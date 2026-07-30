package main

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
)

func init() {
	// gob requires every concrete type that lands inside a map[string]any
	// (Node.Meta / Edge.Meta) to be registered before the first Encode.
	// The persistence package registers the same set, but the daemon
	// snapshot path is a separate gob stream — registering here keeps the
	// coupling explicit so a stripped-down build (or a new caller that
	// drops the persistence import) cannot silently regress.
	gob.Register(map[string]any{})
	gob.Register(map[string]string{})
	gob.Register([]any{})
	gob.Register([]string{})
	gob.Register([]int{})
	gob.Register([]map[string]string{})
	gob.Register([]map[string]any{})
}

// snapshotRepo carries the per-repo metadata needed to reconcile a
// restarting daemon with the filesystem: specifically, FileMtimes so
// IncrementalReindexPaths can skip unchanged files and evict deleted ones.
// Added additively — absent in v≤2 snapshots, where RepoCount decodes
// as zero and the repo section is empty.
type snapshotRepo struct {
	RepoPrefix string
	RootPath   string
	FileMtimes map[string]int64
	// NodeCount / EdgeCount are the per-repo graph shape at save time. Added
	// additively — a snapshot written before this field decodes them as zero,
	// which the boot shape-degradation guard reads as "no baseline to compare"
	// and skips. Used on reload to catch a persisted graph that came back
	// materially short of what it held when saved.
	NodeCount int
	EdgeCount int
}

// snapshotContract is the wire form of contracts.Contract. Persisted so
// per-repo contract registries survive daemon restarts without having to
// re-run extractContracts during warmup — in steady state IncrementalReindexPaths
// skips the extraction step entirely, which used to leave the registry nil
// for every repo whose mtimes hadn't drifted. Isolates the wire schema from
// unrelated evolution of the runtime Contract type: an additive field on
// contracts.Contract does not force a snapshot migration so long as the two
// shapes stay aligned on the fields we care about persisting.
type snapshotContract struct {
	ID         string
	Type       string
	Role       string
	SymbolID   string
	FilePath   string
	Line       int
	RepoPrefix string
	Meta       map[string]any
	Confidence float64
}

// snapshotLoadResult reports the outcome of loadSnapshot. Partial is
// true when any record was skipped (corrupt, dangling, or structurally
// invalid) so warmup can decide whether to force a fuller reconcile.
type snapshotLoadResult struct {
	Loaded    bool
	Partial   bool
	Repos     map[string]*snapshotRepo
	Contracts map[string][]contracts.Contract
	// Vector carries the workspace-global vector-search index restored
	// from the snapshot. Index is nil when the snapshot predates schema
	// v3 or embeddings were disabled when it was written. Warmup uses
	// it to skip re-embedding the whole graph on restart.
	Vector snapshotVector
}

// isStaleAbsPathID reports whether a node ID begins with an absolute
// filesystem path — a leftover from a prior-version code path that wrote
// abs paths into IDs instead of repo-prefixed relative ones. Current
// indexing never produces such IDs, so any found in a snapshot are stale
// duplicates of a properly-prefixed node and must be dropped on load.
func isStaleAbsPathID(id string) bool {
	return strings.HasPrefix(id, "/")
}

// snapshotHeader is the first record in a streamed snapshot. NodeCount
// and EdgeCount let the loader pre-size its work and detect truncation.
//
// The encoded layout is: header → node × NodeCount → edge × EdgeCount.
// Each item is encoded as its own gob value, so the encoder never has
// to buffer the full graph in memory before writing to the gzip stream.
// On a 5M-edge graph that drops peak memory from ~500 MB (old
// "encode-then-write" path) to roughly the size of one node/edge plus
// the gzip window — a few hundred KB.
type snapshotHeader struct {
	SchemaVersion int
	Version       string
	// BinaryMtimeUnix is the Unix epoch (seconds) of the daemon binary
	// that wrote this snapshot. Added additively — older snapshots decode
	// as zero and skip the binary-mtime check entirely. Set on save via
	// os.Stat(os.Executable()); used on load to discard snapshots written
	// by a different build of the same `version` string (i.e. every
	// `go build` rebuild during development). Without this, a buggy
	// resolver's mis-resolved edges persist across local rebuilds forever
	// because per-repo ResolveAll only revisits files whose mtime changed.
	BinaryMtimeUnix int64
	NodeCount       int
	EdgeCount       int
	// RepoCount is the number of snapshotRepo records that follow the
	// nodes and edges sections. Added additively in the resilience work;
	// older snapshots decode this as zero (gob skips unknown fields),
	// so a newer daemon reading an older snapshot simply gets no
	// per-repo reconciliation metadata and falls back to full re-index.
	RepoCount int
	// ContractCount is the number of snapshotContract records that follow
	// the repo section. Added additively: older snapshots decode this as
	// zero and the loader emits an empty Contracts map, which warmup
	// treats as "re-extract on next stale file" — identical to the
	// pre-contracts-persistence behaviour.
	ContractCount int

	// VectorIndex is the serialized HNSW semantic-search vector index
	// for the whole workspace. The daemon's search backend is shared
	// across every tracked repo, so there is one global vector index,
	// not one per repo — it is carried on the header rather than on
	// snapshotRepo. Nil when embeddings are disabled or no vectors were
	// built. Persisting it lets a default-on daemon skip re-embedding
	// the entire graph on every restart (re-embedding 30k+ symbols
	// otherwise dominates warmup). Added in schema v3.
	VectorIndex []byte
	// VectorDims is the embedding dimensionality of VectorIndex (0 when
	// no vector index is present).
	VectorDims int
	// VectorCount is the number of vectors in VectorIndex.
	VectorCount int
}

// snapshotVector bundles the workspace-global vector-search index for
// threading through saveSnapshotTo / loadSnapshotFrom. Kept as a small
// struct (rather than three loose parameters) so the save/load
// signatures stay readable as the snapshot grows.
type snapshotVector struct {
	// Index is the serialized HNSW vector index. Nil disables vector
	// persistence for this snapshot.
	Index []byte
	// Dims is the embedding dimensionality; Count is the vector count.
	Dims  int
	Count int
}

// snapshotSchemaVersion is bumped whenever daemonSnapshot's shape or
// semantics change in a way that older snapshots can no longer be
// interpreted. v2 introduced the streaming layout (header + per-item
// records); v1 was a single gob struct holding the whole graph. v3
// added the workspace-global vector-search index fields to
// snapshotHeader — purely additive (gob decodes a v2 header with the
// new fields zero), so the v2→v3 migration is a verbatim stream copy
// that exists only to keep canMigrate from discarding v2 snapshots.
//
// ──────────────────────────── Wire contract ─────────────────────────────
// graph.Node, graph.Edge, snapshotHeader, and snapshotRepo are wire
// contracts. Daemons in the wild write v_n snapshots; daemons at v_{n+k}
// must still load them. Rules:
//
//   - Additive field changes (new field, unused by older readers) do
//     NOT require a schema bump — gob decodes unknown fields as zero,
//     and newer fields on older writers stay zero on newer readers.
//
//   - Renames, type changes, or removals on existing fields DO require
//     a schema bump + migration entry in snapshotMigrations. The gob
//     stream is field-name-tagged; renaming breaks decode silently.
//
//   - CI guard: TestWireContractFingerprint (wire_contract_test.go)
//     hashes the exported fields of the four wire types above and
//     fails any PR that drifts the fingerprint without updating the
//     pinned golden. Runs as part of the normal `go test ./...` sweep.
//
// We explicitly chose graceful degradation + additive discipline over
// a heavy migration framework that would ossify these structs
// prematurely.
const snapshotSchemaVersion = 3

// snapshotMigration runs when an on-disk snapshot is at a lower
// schema version than the daemon. It reads the old-format gob stream
// from `in`, rewrites it as the next version's layout, and writes the
// result to `out`. Chained by loadSnapshot when a version gap spans
// multiple steps. Start empty — premature migration frameworks encode
// the wrong abstractions; we add entries only on genuine breaking
// changes.
type snapshotMigration func(in io.Reader, out io.Writer) error

// snapshotMigrations is the in-process migration registry. Keyed by
// the source schema version: migrations[N] turns an N-format snapshot
// into (N+1)-format. Absence of a migration for some version in the
// gap → fall through to rebuild (current behaviour unchanged).
var snapshotMigrations = map[int]snapshotMigration{
	// v2 → v3: schema v3 only adds zero-valued vector-index fields to
	// snapshotHeader, which gob already decodes as zero from a v2
	// stream. The migration therefore re-stamps the header's
	// SchemaVersion to 3 and copies the node/edge/repo/contract
	// records through byte-for-byte — no record reshaping needed.
	2: migrateSnapshotV2toV3,
}

// migrateSnapshotV2toV3 rewrites a v2 snapshot stream as v3. Both
// `in` and `out` are the raw (already gunzipped) gob streams. The
// record shapes (graph.Node, graph.Edge, snapshotRepo,
// snapshotContract) are identical across the two versions — only the
// header gained additive fields — so the migration decodes every
// record with the current types and re-encodes it through a fresh
// encoder, with the header's SchemaVersion bumped to 3. A full
// decode/re-encode (rather than a raw byte copy of the tail) keeps the
// gob type-id table internally consistent regardless of how the v2
// writer interleaved type definitions.
func migrateSnapshotV2toV3(in io.Reader, out io.Writer) error {
	dec := gob.NewDecoder(in)
	enc := gob.NewEncoder(out)

	var header snapshotHeader
	if err := dec.Decode(&header); err != nil {
		return fmt.Errorf("migrate v2→v3: decode header: %w", err)
	}
	header.SchemaVersion = 3
	if err := enc.Encode(header); err != nil {
		return fmt.Errorf("migrate v2→v3: encode header: %w", err)
	}
	for i := 0; i < header.NodeCount; i++ {
		var n graph.Node
		if err := dec.Decode(&n); err != nil {
			return fmt.Errorf("migrate v2→v3: decode node %d: %w", i, err)
		}
		if err := enc.Encode(n); err != nil {
			return fmt.Errorf("migrate v2→v3: encode node %d: %w", i, err)
		}
	}
	for i := 0; i < header.EdgeCount; i++ {
		var e graph.Edge
		if err := dec.Decode(&e); err != nil {
			return fmt.Errorf("migrate v2→v3: decode edge %d: %w", i, err)
		}
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("migrate v2→v3: encode edge %d: %w", i, err)
		}
	}
	for i := 0; i < header.RepoCount; i++ {
		var r snapshotRepo
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("migrate v2→v3: decode repo %d: %w", i, err)
		}
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("migrate v2→v3: encode repo %d: %w", i, err)
		}
	}
	for i := 0; i < header.ContractCount; i++ {
		var c snapshotContract
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("migrate v2→v3: decode contract %d: %w", i, err)
		}
		if err := enc.Encode(c); err != nil {
			return fmt.Errorf("migrate v2→v3: encode contract %d: %w", i, err)
		}
	}
	return nil
}

// canMigrate reports whether a migration chain exists that bridges
// `from` → `to`. Used by loadSnapshot to decide between "migrate" and
// "discard the cache." Today this always returns false because the
// registry is empty; wired up so adding a migration doesn't require
// touching the loader's conditional.
func canMigrate(from, to int) bool {
	if from >= to {
		return false
	}
	for v := from; v < to; v++ {
		if _, ok := snapshotMigrations[v]; !ok {
			return false
		}
	}
	return true
}

// migrateSnapshotFile re-reads the snapshot at `path`, decompresses
// it, and runs every registered migration step from `fromVersion` up
// to snapshotSchemaVersion. It returns an in-memory reader positioned
// at the start of the fully-migrated (uncompressed) gob stream. The
// caller (loadSnapshotFrom) has already verified canMigrate covers the
// whole gap, so a missing step here is an internal inconsistency and
// surfaces as an error.
func migrateSnapshotFile(path string, fromVersion int) (io.Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reopen snapshot for migration: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip reader for migration: %w", err)
	}
	defer gz.Close()

	// Buffer the raw uncompressed stream once, then fold each migration
	// step over it. Each step reads its input and writes the next
	// version's layout into a fresh buffer.
	var cur bytes.Buffer
	if _, err := io.Copy(&cur, gz); err != nil {
		return nil, fmt.Errorf("read snapshot stream for migration: %w", err)
	}
	for v := fromVersion; v < snapshotSchemaVersion; v++ {
		migrate, ok := snapshotMigrations[v]
		if !ok {
			return nil, fmt.Errorf("no migration registered for schema v%d", v)
		}
		var next bytes.Buffer
		if err := migrate(&cur, &next); err != nil {
			return nil, fmt.Errorf("migration v%d→v%d: %w", v, v+1, err)
		}
		cur = next
	}
	return &cur, nil
}

// saveSnapshot streams a gob+gzip snapshot of the graph to the daemon's
// snapshot path. Called from the daemon's shutdown hook. Errors are
// logged but never propagated — a failed snapshot write should never
// block clean shutdown. The repos slice carries per-repo FileMtimes so
// the next warmup can use IncrementalReindexPaths instead of a full re-scan.
// The contracts slice carries per-repo contract entries so the warmup
// can rehydrate each indexer's contracts.Registry without re-running the
// extractors — IncrementalReindexPaths skips extraction in steady state, so
// without this the registries came back nil after every restart.
// The vec argument carries the workspace-global vector-search index so
// a default-on daemon does not re-embed the whole graph on restart.
func saveSnapshot(g *graph.Graph, repos []snapshotRepo, snapContracts []snapshotContract, vec snapshotVector, version string, logger *zap.Logger) {
	// Memory backend: the gob+gzip dump IS the persistence layer, so
	// route to the per-backend path so a future disk-backed daemon
	// can't accidentally pick up this snapshot at startup. See
	// daemon.BackendSnapshotPath for the memory ↔ disk-backend switch
	// rationale.
	_ = saveSnapshotTo(g, repos, snapContracts, vec, version, daemon.BackendSnapshotPath("memory"), logger)
}

// saveSnapshotTo writes the snapshot to an explicit path. Used by the
// daemon's snapshot writer. Returns an error when the path can't be written so the
// caller can fail the job; the daemon's saveSnapshot wrapper still
// swallows errors because a failed snapshot must never block clean
// shutdown.
func saveSnapshotTo(g *graph.Graph, repos []snapshotRepo, snapContracts []snapshotContract, vec snapshotVector, version string, path string, logger *zap.Logger) error {
	if g == nil {
		return errors.New("snapshot: nil graph")
	}
	if err := daemon.EnsureParentDir(path); err != nil {
		logger.Warn("snapshot: parent dir", zap.Error(err))
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		logger.Warn("snapshot: create tmp", zap.Error(err))
		return err
	}

	gz := gzip.NewWriter(f)
	enc := gob.NewEncoder(gz)

	// Snapshot the slices once so the encode loop sees a consistent
	// view even if a late event slips in (the graph's RWMutex protects
	// each AllNodes/AllEdges call individually).
	nodes := g.AllNodes()
	edges := g.AllEdges()

	header := snapshotHeader{
		SchemaVersion:   snapshotSchemaVersion,
		Version:         version,
		BinaryMtimeUnix: currentBinaryMtimeUnix(),
		NodeCount:       len(nodes),
		EdgeCount:       len(edges),
		RepoCount:       len(repos),
		ContractCount:   len(snapContracts),
		VectorIndex:     vec.Index,
		VectorDims:      vec.Dims,
		VectorCount:     vec.Count,
	}

	// Helper to clean up after any failure.
	abort := func(stage string, e error) error {
		logger.Warn("snapshot: "+stage, zap.Error(e))
		_ = gz.Close()
		_ = f.Close()
		_ = os.Remove(tmp)
		return e
	}

	if err := enc.Encode(header); err != nil {
		return abort("encode header", err)
	}
	for _, n := range nodes {
		if err := enc.Encode(n); err != nil {
			return abort("encode node", err)
		}
	}
	for _, e := range edges {
		if err := enc.Encode(e); err != nil {
			return abort("encode edge", err)
		}
	}
	for i := range repos {
		if err := enc.Encode(repos[i]); err != nil {
			return abort("encode repo", err)
		}
	}
	for i := range snapContracts {
		if err := enc.Encode(snapContracts[i]); err != nil {
			return abort("encode contract", err)
		}
	}
	if err := gz.Close(); err != nil {
		logger.Warn("snapshot: gzip close", zap.Error(err))
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		logger.Warn("snapshot: file close", zap.Error(err))
		_ = os.Remove(tmp)
		return err
	}
	// Shrink-guard: the new snapshot is fully written to tmp but not
	// yet swapped in. If it has collapsed against the snapshot already
	// on disk — a sign the in-memory graph is the product of a partial
	// or failed index — refuse the swap and keep the prior good
	// snapshot. A modest shrink (deleted files) still goes through;
	// only a suspicious collapse is blocked. Returning nil rather than
	// an error is deliberate: keeping the good snapshot is the guard's
	// success outcome, not a write failure for the caller to surface.
	if snapshotWouldCollapse(path, header.NodeCount, header.EdgeCount, logger) {
		_ = os.Remove(tmp)
		return nil
	}
	// Atomic swap so a concurrent crash can never leave a truncated
	// snapshot on disk.
	if err := os.Rename(tmp, path); err != nil {
		logger.Warn("snapshot: rename", zap.Error(err))
		return err
	}
	logger.Info("snapshot: wrote",
		zap.String("path", path),
		zap.Int("nodes", header.NodeCount),
		zap.Int("edges", header.EdgeCount),
		zap.Int("repos", header.RepoCount),
		zap.Int("contracts", header.ContractCount),
		zap.Int("vectors", header.VectorCount))
	return nil
}

// snapshotShrinkFloorPercent is the share of the prior snapshot's node
// and edge counts the new snapshot must retain to be allowed to
// overwrite it. Below this floor the new graph is treated as the
// product of a partial or failed index and the swap is refused.
//
// 50% is chosen to sit firmly between two regimes. A legitimate
// shrink is incremental: deleting files trims a slice of one repo,
// and the daemon tracks several repos at once, so even an aggressive
// cleanup rarely halves the whole graph in a single save. A failed
// index collapses it far harder — the load path elsewhere in this
// file documents a real incident where a half-loaded graph came back
// as 47k nodes against an expected 146k, a ~68% drop. A 50% floor
// clears the worst plausible honest shrink while still catching that
// class of collapse.
const snapshotShrinkFloorPercent = 50

// readSnapshotHeader decodes just the snapshotHeader from the snapshot
// at path — the header is the first gob record, so this stops after
// it without touching the (potentially huge) node/edge stream. ok is
// false when there is no usable baseline to compare against: a missing
// file, an unreadable / truncated stream, or a schema-version
// mismatch (an older header's counts are not comparable).
func readSnapshotHeader(path string) (hdr snapshotHeader, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return snapshotHeader{}, false
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return snapshotHeader{}, false
	}
	defer gz.Close()

	var header snapshotHeader
	if err := gob.NewDecoder(gz).Decode(&header); err != nil {
		return snapshotHeader{}, false
	}
	if header.SchemaVersion != snapshotSchemaVersion {
		return snapshotHeader{}, false
	}
	return header, true
}

// snapshotWouldCollapse reports whether overwriting the snapshot at
// path with one carrying newNodes / newEdges would replace a healthy
// snapshot with a drastically smaller one. It returns true — and logs
// a warning — only for a suspicious collapse; a missing / unreadable
// baseline, a previously-empty snapshot, or a modest shrink all return
// false so the save proceeds.
func snapshotWouldCollapse(path string, newNodes, newEdges int, logger *zap.Logger) bool {
	prev, ok := readSnapshotHeader(path)
	if !ok {
		// No comparable baseline — nothing to protect, allow the write.
		return false
	}
	// A prior snapshot with no nodes or no edges gives no meaningful
	// ratio (and is not a "good" snapshot worth protecting anyway).
	if prev.NodeCount == 0 || prev.EdgeCount == 0 {
		return false
	}
	// Retaining at least the floor share of BOTH counts is required;
	// a collapse in either nodes or edges is enough to refuse. Compare
	// by cross-multiplication to avoid floating-point rounding:
	//   new < prev * floor/100   ⇔   new*100 < prev*floor
	nodesCollapsed := newNodes*100 < prev.NodeCount*snapshotShrinkFloorPercent
	edgesCollapsed := newEdges*100 < prev.EdgeCount*snapshotShrinkFloorPercent
	if !nodesCollapsed && !edgesCollapsed {
		return false
	}
	logger.Warn("snapshot: refusing to overwrite — new snapshot has shrunk drastically, keeping the prior good snapshot",
		zap.String("path", path),
		zap.Int("prev_nodes", prev.NodeCount),
		zap.Int("new_nodes", newNodes),
		zap.Int("prev_edges", prev.EdgeCount),
		zap.Int("new_edges", newEdges),
		zap.Int("shrink_floor_percent", snapshotShrinkFloorPercent))
	return true
}

// bootShapeShortfall reports whether a warm-reloaded repo's live node/edge
// counts have collapsed against what the snapshot recorded for it — the
// per-repo, boot-time analogue of snapshotWouldCollapse (which guards the
// whole graph at save time). Only a drastic drop trips (the same floor share
// as the persist-side shrink guard); a snapshot with no recorded counts
// (legacy / newly tracked, both zero) returns false, since there is nothing
// trustworthy to compare against.
func bootShapeShortfall(snap map[string]*snapshotRepo, prefix string, liveNodes, liveEdges int) bool {
	rec, ok := snap[prefix]
	if !ok || rec == nil || rec.NodeCount == 0 || rec.EdgeCount == 0 {
		return false
	}
	// Cross-multiply to avoid floating-point rounding:
	//   live < rec * floor/100   ⇔   live*100 < rec*floor
	nodesShort := liveNodes*100 < rec.NodeCount*snapshotShrinkFloorPercent
	edgesShort := liveEdges*100 < rec.EdgeCount*snapshotShrinkFloorPercent
	return nodesShort || edgesShort
}

// toSnapshotContract flattens a contracts.Contract into its wire form.
// The runtime type alias members (ContractType, Role) are stringified so
// the snapshot struct carries only primitive-typed fields and the
// migration rules stay predictable.
func toSnapshotContract(c contracts.Contract) snapshotContract {
	return snapshotContract{
		ID:         c.ID,
		Type:       string(c.Type),
		Role:       string(c.Role),
		SymbolID:   c.SymbolID,
		FilePath:   c.FilePath,
		Line:       c.Line,
		RepoPrefix: c.RepoPrefix,
		Meta:       c.Meta,
		Confidence: c.Confidence,
	}
}

// fromSnapshotContract rebuilds the runtime Contract from its wire form.
// Unknown Type / Role strings are passed through — the extractors wrote
// them, and rejecting a value we still understand structurally would
// silently drop real contracts in an edge case we have no reason to
// force.
func fromSnapshotContract(s snapshotContract) contracts.Contract {
	return contracts.Contract{
		ID:         s.ID,
		Type:       contracts.ContractType(s.Type),
		Role:       contracts.Role(s.Role),
		SymbolID:   s.SymbolID,
		FilePath:   s.FilePath,
		Line:       s.Line,
		RepoPrefix: s.RepoPrefix,
		Meta:       s.Meta,
		Confidence: s.Confidence,
	}
}

// loadSnapshot streams the snapshot at daemon.SnapshotPath() into g.
// Returns (Loaded=false) when no snapshot exists — that's the expected
// first-run / post-reset case, not an error. Schema mismatches are
// logged and treated as absent so we don't try to interpret bytes we
// don't understand.
//
// Per-record decode failures do not abort the load — they're logged and
// counted, the whole record is dropped, and the graph state is
// structurally validated before return (dangling edges pruned). This
// trades "one bad byte poisons the entire cache" for "N bad records
// cost at most N files being re-indexed on next warmup."
func loadSnapshot(g *graph.Graph, logger *zap.Logger) (snapshotLoadResult, error) {
	// Memory backend reads from its own backend-tagged path. Falls
	// back transparently to the legacy unsuffixed daemon.gob.gz when
	// the override env is set or the new file doesn't exist yet, so
	// users upgrading across this change don't have to re-warm.
	res, err := loadSnapshotFrom(g, daemon.BackendSnapshotPath("memory"), logger)
	if err == nil && (res.Loaded || res.Partial) {
		return res, nil
	}
	return loadSnapshotFrom(g, daemon.SnapshotPath(), logger)
}

// loadSnapshotFrom is loadSnapshot with an explicit path argument.
// Used by `gortex server --snapshot <path>` so a per-workspace
// process can boot from a specific snapshot file produced by the
// cloud indexer worker.
func loadSnapshotFrom(g graph.Store, path string, logger *zap.Logger) (snapshotLoadResult, error) {
	// Allocate Contracts up front so every early-return path (missing
	// file, gzip error, header decode error, schema mismatch) hands the
	// caller a safe-to-read zero-value instead of a nil map. The warmup
	// path `range state.snapshotContracts` over a nil map is fine in Go,
	// but a nil result is a gotcha other call sites have hit before.
	result := snapshotLoadResult{
		Contracts: make(map[string][]contracts.Contract),
	}
	if g == nil {
		return result, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("open snapshot: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return result, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	dec := gob.NewDecoder(gz)
	var header snapshotHeader
	if err := dec.Decode(&header); err != nil {
		return result, fmt.Errorf("decode snapshot header: %w", err)
	}
	if header.SchemaVersion != snapshotSchemaVersion {
		// Schema gap — try the migration chain. When a chain exists,
		// re-read the file from scratch, run every migration step up to
		// the current version, and decode the migrated stream;
		// otherwise the snapshot is discarded (treated as absent) so we
		// never interpret bytes we can't be sure of.
		if canMigrate(header.SchemaVersion, snapshotSchemaVersion) {
			migrated, err := migrateSnapshotFile(path, header.SchemaVersion)
			if err != nil {
				logger.Warn("snapshot: schema migration failed, ignoring",
					zap.Int("on_disk", header.SchemaVersion),
					zap.Int("expected", snapshotSchemaVersion),
					zap.Error(err))
				return result, nil
			}
			logger.Info("snapshot: migrated to current schema",
				zap.Int("from", header.SchemaVersion),
				zap.Int("to", snapshotSchemaVersion))
			dec = gob.NewDecoder(migrated)
			if err := dec.Decode(&header); err != nil {
				logger.Warn("snapshot: decode migrated header failed, ignoring", zap.Error(err))
				return result, nil
			}
		} else {
			logger.Info("snapshot: schema mismatch, ignoring",
				zap.Int("on_disk", header.SchemaVersion),
				zap.Int("expected", snapshotSchemaVersion))
			return result, nil
		}
	}

	// Binary-version gate. The snapshot persists already-resolved edges
	// (e.g. `runQuery → Node.Inner`). When the resolver changes between
	// daemon versions — bug fixes, new edge kinds, tighter scope rules —
	// edges that were correctly resolved by the OLD resolver may now
	// look stale, and edges that were misresolved by the OLD resolver
	// will keep their wrong targets forever (per-repo ResolveAll only
	// rewrites edges whose source file's mtime changed, and most files
	// stay untouched across daemon restarts). Bumping any resolver
	// behaviour without bumping snapshotSchemaVersion silently degrades
	// query quality until the user thinks to wipe ~/.gortex/cache.
	//
	// Cheap fix: if the binary that wrote the snapshot has a different
	// version string than the binary loading it, discard. Cost is one
	// full re-index per daemon upgrade — measured at ~2 minutes for a
	// 100k-node workspace, an entirely fair tax for a stale-cache class
	// of bugs that's otherwise invisible.
	if header.Version != "" && header.Version != version {
		logger.Info("snapshot: binary version mismatch, discarding to force a fresh resolve",
			zap.String("on_disk", header.Version),
			zap.String("running", version))
		return result, nil
	}

	// Same-version rebuild gate. A `go build` of the same `version` string
	// produces a binary with a newer mtime; if the snapshot was written by
	// an earlier build, discard. Critical for developer workflow where
	// resolver/indexer changes ship without a version bump — without this,
	// every rebuild silently inherits the previous build's potentially
	// stale or buggy resolutions.
	//
	// Legacy snapshots written before this field existed decode as zero;
	// we treat that as "can't verify, don't trust" and discard exactly
	// once. The cost is one full re-index for every user upgrading past
	// this commit, which is the right cost — those users are precisely
	// the ones carrying stale resolutions from older resolver behaviour.
	if header.BinaryMtimeUnix == 0 {
		logger.Info("snapshot: legacy (no binary mtime stamp), discarding once to force a fresh resolve")
		return result, nil
	}
	if mt := currentBinaryMtimeUnix(); mt > 0 && header.BinaryMtimeUnix != mt {
		logger.Info("snapshot: binary rebuilt since last save, discarding to force a fresh resolve",
			zap.Int64("snapshot_binary_mtime", header.BinaryMtimeUnix),
			zap.Int64("running_binary_mtime", mt))
		return result, nil
	}

	// Carry the workspace-global vector index off the header. It is
	// present only for schema-v3+ snapshots written with embeddings
	// enabled; warmup decides whether to restore it (an embedder must
	// be configured and its dims must match).
	result.Vector = snapshotVector{
		Index: header.VectorIndex,
		Dims:  header.VectorDims,
		Count: header.VectorCount,
	}

	// Snapshots can carry stale nodes whose IDs begin with an absolute
	// filesystem path — leftovers from prior-version indexing bugs. Drop
	// them on load; re-indexing the tracked repos recreates clean
	// repo-prefixed replacements. Edges pointing at dropped nodes are
	// skipped so the graph never contains dangling references.
	droppedNodes := make(map[string]struct{})
	var skippedNodes, skippedEdges, corruptNodes, corruptEdges, corruptRepos, corruptContracts int
	loadedIDs := make(map[string]struct{})
	for i := 0; i < header.NodeCount; i++ {
		var n graph.Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// Truncation is irrecoverable — the remaining records
				// are gone. Validate what we have and return partial.
				logger.Warn("snapshot: truncated during nodes",
					zap.Int("expected", header.NodeCount),
					zap.Int("read", i),
					zap.Error(err))
				result.Partial = true
				goto validate
			}
			// A single corrupt record in an otherwise-valid stream:
			// skip it, keep going. Surviving the bad byte is the whole
			// point of per-record decode; the alternative is dropping
			// millions of good nodes over one bad one.
			corruptNodes++
			result.Partial = true
			continue
		}
		if n.ID == "" {
			corruptNodes++
			result.Partial = true
			continue
		}
		if isStaleAbsPathID(n.ID) {
			droppedNodes[n.ID] = struct{}{}
			skippedNodes++
			continue
		}
		g.AddNode(&n)
		loadedIDs[n.ID] = struct{}{}
	}
	for i := 0; i < header.EdgeCount; i++ {
		var e graph.Edge
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				logger.Warn("snapshot: truncated during edges",
					zap.Int("expected", header.EdgeCount),
					zap.Int("read", i),
					zap.Error(err))
				result.Partial = true
				goto validate
			}
			corruptEdges++
			result.Partial = true
			continue
		}
		if _, drop := droppedNodes[e.From]; drop {
			skippedEdges++
			continue
		}
		if _, drop := droppedNodes[e.To]; drop {
			skippedEdges++
			continue
		}
		// Structural validation: drop edges whose endpoints weren't
		// loaded (either corrupt-skipped or never in the snapshot).
		if _, ok := loadedIDs[e.From]; !ok {
			skippedEdges++
			continue
		}
		if _, ok := loadedIDs[e.To]; !ok {
			skippedEdges++
			continue
		}
		g.AddEdge(&e)
	}

	if header.RepoCount > 0 {
		result.Repos = make(map[string]*snapshotRepo, header.RepoCount)
		for i := 0; i < header.RepoCount; i++ {
			var r snapshotRepo
			if err := dec.Decode(&r); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					logger.Warn("snapshot: truncated during repos",
						zap.Int("expected", header.RepoCount),
						zap.Int("read", i),
						zap.Error(err))
					result.Partial = true
					goto validate
				}
				corruptRepos++
				result.Partial = true
				continue
			}
			if r.RepoPrefix == "" {
				corruptRepos++
				result.Partial = true
				continue
			}
			result.Repos[r.RepoPrefix] = &r
		}
	}

	if header.ContractCount > 0 {
		for i := 0; i < header.ContractCount; i++ {
			var sc snapshotContract
			if err := dec.Decode(&sc); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					logger.Warn("snapshot: truncated during contracts",
						zap.Int("expected", header.ContractCount),
						zap.Int("read", i),
						zap.Error(err))
					result.Partial = true
					goto validate
				}
				corruptContracts++
				result.Partial = true
				continue
			}
			if sc.ID == "" {
				corruptContracts++
				result.Partial = true
				continue
			}
			result.Contracts[sc.RepoPrefix] = append(result.Contracts[sc.RepoPrefix], fromSnapshotContract(sc))
		}
	}

validate:
	// The load reached here either cleanly or via a truncation goto —
	// in both cases validate what's in the graph before returning.

	totalContracts := 0
	for _, cs := range result.Contracts {
		totalContracts += len(cs)
	}
	// If the load shed any stale records, discard the snapshot entirely
	// and fall through to a clean from-scratch index. Dropped edges
	// signal that the persisted resolution state is corrupt — and we've
	// learned the hard way that mixing partial snapshot state with
	// incremental re-extraction silently leaves the graph in a worse
	// state than starting fresh (observed: per-repo TrackRepoCtx on top
	// of a half-loaded graph produced 47k nodes instead of the expected
	// 146k, and most methods ended up with zero callers despite
	// obviously having dozens). One full re-index per partial-load
	// detection is the right tax — it converges in 1-2 minutes and the
	// next snapshot writes from a known-good state.
	// Distinguish two skip causes that look similar in the counters:
	//   - skippedNodes accompanied by their dependent edges: that's the
	//     intentional stale-abs-path cleanup (a few nodes, a few edges).
	//   - a large number of edges whose targets vanished WITHOUT a
	//     matching node-drop wave: that's persisted-resolution
	//     corruption — the snapshot has resolved edges pointing at
	//     node IDs that no longer exist, which means the resolver
	//     state in this snapshot is no longer trustworthy.
	//
	// 5% is the threshold: empirically the abs-path cleanup sheds a
	// handful of edges; real corruption sheds tens of thousands. The
	// gap is wide enough that 5% comfortably separates them.
	corruptDetected := header.EdgeCount > 100 && skippedEdges*20 > header.EdgeCount
	if corruptDetected {
		// Wipe the partial graph the per-record loop populated above so
		// the caller's `g` is empty when we return Loaded=false. Without
		// this the daemon would warmup with a half-graph plus a from-
		// scratch index running over the same node IDs — exactly the
		// duplicate-edges failure mode this whole resilience layer was
		// built to avoid. EvictRepo per discovered repo prefix is the
		// most surgical wipe available (Graph has no `Reset()`).
		for _, prefix := range g.RepoPrefixes() {
			g.EvictRepo(prefix)
		}
		// Also delete the snapshot file so a subsequent restart (e.g.
		// `gortex daemon restart` immediately after) doesn't re-encounter
		// the same partial-load loop. The next save will write a fresh,
		// clean snapshot.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			logger.Warn("snapshot: could not delete partial snapshot file",
				zap.String("path", path), zap.Error(rmErr))
		}
		logger.Info("snapshot: discarded due to partial load — forcing fresh index",
			zap.String("path", path),
			zap.Int("stale_nodes_dropped", skippedNodes),
			zap.Int("stale_edges_dropped", skippedEdges))
		return snapshotLoadResult{Loaded: false}, nil
	}
	logger.Info("snapshot: loaded",
		zap.String("path", path),
		zap.Int("nodes", header.NodeCount-skippedNodes-corruptNodes),
		zap.Int("edges", header.EdgeCount-skippedEdges-corruptEdges),
		zap.Int("repos", len(result.Repos)),
		zap.Int("contracts", totalContracts),
		zap.Int("stale_nodes_dropped", skippedNodes),
		zap.Int("stale_edges_dropped", skippedEdges),
		zap.Int("corrupt_nodes_skipped", corruptNodes),
		zap.Int("corrupt_edges_skipped", corruptEdges),
		zap.Int("corrupt_repos_skipped", corruptRepos),
		zap.Int("corrupt_contracts_skipped", corruptContracts))
	result.Loaded = true
	return result, nil
}

// currentBinaryMtimeUnix returns the Unix timestamp (seconds) of the
// daemon executable's mtime. Used in the snapshot header to invalidate
// caches across `go build` rebuilds that don't bump the version string.
// Returns 0 on any error so the load-time check can skip the comparison
// rather than risk false-positive cache discards.
func currentBinaryMtimeUnix() int64 {
	exe, err := os.Executable()
	if err != nil {
		return 0
	}
	// Follow symlinks — homebrew installs gortex as a symlink to the
	// real binary and we want the real binary's mtime, not the symlink's.
	resolved, err := os.Readlink(exe)
	if err == nil && resolved != "" {
		if !strings.HasPrefix(resolved, "/") {
			resolved = exe + "/../" + resolved
		}
		exe = resolved
	}
	info, err := os.Stat(exe)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}
