package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pelletier/go-toml/v2"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"

	"github.com/zzet/gortex/internal/codegen"
	"github.com/zzet/gortex/internal/codeowners"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/entrypoints"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/fixtures"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/intern"
	"github.com/zzet/gortex/internal/licenses"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/trigram"
	"github.com/zzet/gortex/internal/semantic"
	gortexsql "github.com/zzet/gortex/internal/sql"
	"github.com/zzet/gortex/internal/todos"
)

// IndexResult holds the outcome of an indexing operation.
type IndexResult struct {
	NodeCount int `json:"node_count"`
	EdgeCount int `json:"edge_count"`
	// FileCount is the total number of language files the indexer
	// observed for this repo — i.e. how big the repo is on disk, not
	// how much work this pass did. Stamped onto RepoMetadata so
	// `daemon status` shows a stable file count across both full-track
	// and incremental-reconcile paths.
	FileCount int `json:"file_count"`
	// StaleFileCount is the number of files that were actually
	// re-indexed in this pass (only populated by IncrementalReindex
	// — full-index passes treat every file as stale and would
	// duplicate FileCount). Used by the janitor / reconcile log to
	// report "how much work did the snapshot delta require".
	StaleFileCount int `json:"stale_file_count,omitempty"`
	// FailedFiles lists files an incremental pass could not index even
	// after one retry — a parse error, or a file locked or removed
	// mid-pass. A caller can replay them explicitly. Whether the next
	// incremental pass retries them on its own depends on the failure:
	// a file whose bytes could not even be read (locked, permission
	// denied, removed mid-walk) never gets an mtime recorded, so it
	// stays stale and is retried on every subsequent pass; a file that
	// was read but failed to parse (syntax error, crash-isolation
	// quarantine, extraction timeout) DOES get its current on-disk
	// mtime recorded, so it is retried only once its content changes
	// again — this keeps a warm restart from perpetually treating the
	// whole repo as changed because of one unparseable file. Empty on
	// a clean pass and on full-index passes.
	FailedFiles []string `json:"failed_files,omitempty"`
	// QuarantinedFiles is the number of files held in the parser
	// crash-isolation quarantine after this pass — files that
	// SIGSEGV'd / hung / panicked the parser and were skipped with a
	// Meta["parse_error"] node. Zero unless crash isolation is on.
	QuarantinedFiles int `json:"quarantined_files,omitempty"`
	// SkippedFiles is the number of files skipped by the size cap
	// (MaxFileSize), the per-file extraction timeout (MaxExtractMillis),
	// or the content-admission policy (index.content — oversized documents
	// and, by default, binary/vector data assets). Each is recorded in the
	// graph as a synthetic file node carrying skipped_due_to_size /
	// skipped_due_to_timeout / skipped_due_to_content telemetry. Zero
	// unless one of those gates fires.
	SkippedFiles int `json:"skipped_files,omitempty"`
	// DeletedFileCount is the number of previously-indexed files that
	// were evicted this pass because they no longer exist on disk (only
	// populated by IncrementalReindex). Together with StaleFileCount it
	// lets a batch caller — the daemon warmup loop in particular — decide
	// whether a repo actually changed since the last shutdown: when both
	// are zero across every repo, the persisted graph already carries
	// every resolved / derived edge and the global resolution passes can
	// be skipped entirely (the warm-restart fast path).
	DeletedFileCount int `json:"deleted_file_count,omitempty"`
	// FullRetrack is true when this result came from a whole-repo
	// re-track (IndexCtx) rather than an incremental pass — i.e. the
	// changed-file set is unknown and StaleFileCount does NOT reflect
	// "how many files changed" (it keeps its normal incremental-work
	// meaning and is 0 here). Callers that gate global re-resolution on
	// "did this repo change" must OR in FullRetrack alongside
	// StaleFileCount / DeletedFileCount.
	FullRetrack bool         `json:"full_retrack,omitempty"`
	DurationMs  int64        `json:"duration_ms"`
	Errors      []IndexError `json:"errors,omitempty"`
	// RepoPrefix is the prefix the repo was actually registered under.
	// It usually equals config.ResolvePrefix(entry), but a git worktree
	// tracked as an independent instance gets a derived `<base>@<tag>`
	// prefix that the caller cannot recompute from the (by-value) entry
	// it passed in — callers that need to attach a watcher or report the
	// outcome read it from here. Populated by the multi-repo track and
	// reconcile paths; empty in single-repo mode.
	RepoPrefix string `json:"repo_prefix,omitempty"`
	// DerivedInvalidation is the exact bounded frontier produced by an
	// incremental reconcile. A zero plan proves that no graph-wide derived
	// pass is required; callers must not infer work merely from stale count.
	DerivedInvalidation DerivedInvalidationPlan `json:"derived_invalidation,omitempty"`
}

// EdgeSanityViolated reports the post-reindex sanity-check failure: an
// index pass that observed source files and extracted symbol nodes from
// them, yet produced zero edges. A populated graph with no edges still
// looks indexed but answers every "who calls X" / "what imports Y"
// query with nothing — a wholesale edge-extraction failure (a broken
// grammar, an aborted reindex) worth surfacing rather than shipping
// silently. Even a single one-function file yields containment edges,
// so a real repo only trips this when extraction failed across the
// board.
func (r *IndexResult) EdgeSanityViolated() bool {
	return r != nil && r.FileCount > 0 && r.NodeCount > 0 && r.EdgeCount == 0
}

// IndexError records a per-file parsing failure.
type IndexError struct {
	FilePath string `json:"file_path"`
	Error    string `json:"error"`
}

// Indexer walks a repository and populates the graph.
type Indexer struct {
	graph graph.Store

	// shadowAdmission is shared by every Indexer in the process. Cold repos
	// acquire a weighted lease before constructing an in-memory shadow; when the
	// budget is busy or the repo is too large, SQLite receives the parse stream
	// directly through the bounded graph accumulator.
	shadowAdmission *shadowAdmissionBudget

	// indexCount tracks how many IndexCtx calls this Indexer has
	// completed. Gates the cold-start shadow-swap: each per-repo
	// Indexer in MultiIndexer is fresh (indexCount==0), so all of
	// them take the shadow path regardless of what sibling repos
	// have already drained into the shared disk store. Per-repo-
	// prefixed stub IDs make the concurrent drains conflict-free.
	indexCount atomic.Int32
	registry   *parser.Registry
	resolver   *resolver.Resolver
	search     search.Backend
	config     config.IndexConfig
	transforms *transformPipeline
	// excludes is the compiled ignore matcher, built lazily from
	// config.Exclude. It is hot-swappable rather than build-once: a
	// per-repo Indexer outlives many `.gortex.yaml` edits, and without a
	// swap every incremental / scoped re-index kept walking with the
	// exclude list the Indexer was constructed with. excludeMu guards the
	// build and the swap (and the config.Exclude read/write that goes with
	// them); readers pay only the atomic load.
	excludes      atomic.Pointer[excludes.Matcher]
	excludeMu     sync.Mutex
	dirIgnore     *excludes.Hierarchical
	dirIgnoreOnce sync.Once
	rootPath      string
	// projectName is the repo's own name (go.mod module / package.json /
	// dir), computed once per index. Stripped from the BM25-indexed file
	// path so a query word matching it doesn't earn a useless uniform
	// path-field boost across every document. "" disables the de-weighting.
	projectName string
	logger      *zap.Logger

	// Crash-isolation parser pool, lazily created and then reused
	// across single-file re-indexes so the watcher hot path never
	// forks a worker subprocess per file.
	parsePool   *crashpool.Pool
	parseQuar   *crashpool.Quarantine
	parsePoolMu sync.Mutex

	// Trigram code-search index, lazily built on first GrepText call
	// and rebuilt only when indexGen advances past the build it was
	// made from. indexGen is bumped by every full or incremental
	// index, so a burst of searches between reindexes hits a warm
	// index.
	indexGen        atomic.Uint64
	trigramSearcher *trigram.Searcher
	trigramGen      uint64
	trigramMu       sync.Mutex
	// trigramBudgetOverride scopes the idle/LRU eviction budget to one
	// test rather than the process-wide default. Nil in production.
	trigramBudgetOverride *trigramBudget

	// repoPrefix is set in multi-repo mode to prefix all file paths and node IDs.
	// When empty, the indexer operates in single-repo mode (backward compatible).
	repoPrefix string

	// workspaceID is the hard graph boundary slug for this repo.
	// Stamped onto every node emitted by this indexer via applyRepoPrefix
	// so query-time scoping doesn't have to look it up by repo prefix.
	// Defaults at the MultiIndexer layer to the per-repo `.gortex.yaml`
	// `workspace:` slug, falling back to repoPrefix when no slug is
	// declared (so legacy configs keep working).
	workspaceID string

	// projectID is the soft sub-boundary slug. Defaults to the repo
	// prefix in single-project repos. Monorepos resolve a per-file
	// projectID via the `projects[]` paths-glob mapping in
	// `.gortex.yaml`; until that lookup is wired in, every node from
	// this indexer carries the repo-default value.
	projectID string

	// contractRegistry holds detected API contracts (HTTP routes, gRPC, etc.).
	contractRegistry *contracts.Registry

	// trackedRepoModules maps repo names to Go module paths for cross-repo dependency detection.
	// Populated by MultiIndexer from go.mod files of tracked repos.
	trackedRepoModules map[string]string

	// embedder is the optional embedding provider for semantic search.
	embedder embedding.Provider

	// skipVectorBuild, when true, makes buildSearchIndex populate only
	// the text index and never run the embedding pass — even with an
	// embedder set. The daemon flips it on for the warmup re-index loop
	// when a snapshot already carries the workspace vector index, so
	// the graph is not re-embedded only to have the cached index
	// overwrite it. Off by default; a normal index always builds
	// vectors when an embedder is present.
	skipVectorBuild bool

	// bulkVectorSink holds the disk store captured at the bulk-load
	// shadow swap, so buildSearchIndex can still persist the vector
	// index to the backend while idx.graph points at the in-memory
	// shadow (which does not implement graph.VectorSearcher). Without
	// it the embedding pass under the bulk loader builds vectors only
	// in the in-process HNSW — they never reach the `vectors` table and
	// are lost on the next daemon restart, forcing a paid re-embed.
	// Set during the shadow swap, cleared when idx.graph is restored.
	bulkVectorSink graph.VectorSearcher

	// contentSink mirrors bulkVectorSink for the content full-text index:
	// the disk store captured at the shadow swap, so the per-file content
	// stream reaches content_fts on disk even while idx.graph points at the
	// in-memory shadow (which does not implement graph.ContentSearcher).
	// Set during the shadow swap, cleared when idx.graph is restored.
	contentSink graph.ContentSearcher

	// embedChunkOpts tunes the AST sub-chunking buildSearchIndex applies
	// to large symbols before embedding. The zero value makes the
	// chunker fall back to its package defaults.
	embedChunkOpts embedding.ChunkOptions

	// embedMaxSymbols overrides the built-in cap on how many texts the
	// vector index will hold before buildSearchIndex skips the embed
	// pass. Zero keeps the built-in default.
	embedMaxSymbols int

	// embedAPIConcurrency bounds how many embedding requests run in
	// parallel against an API-backed embedder. Zero keeps the built-in
	// default. Ignored for in-process embedders, which serialise on an
	// inference mutex.
	embedAPIConcurrency int

	// lastVectorBuildErr records why the most recent buildSearchIndex pass
	// shipped text-only instead of a vector index (chunk-embed failure,
	// all-vectors-invalid, or the symbol-count guard). Nil after a build that
	// produced a vector index. Read via LastVectorBuildError once a build has
	// finished — it lets `gortex eval embedders` report the real cause instead
	// of a bare "no vector data".
	lastVectorBuildErr error

	// semanticMgr is the optional semantic enrichment manager.
	semanticMgr *semantic.Manager

	// resolverLSPHelper, when non-nil, is the resolve-time LSP
	// helper installed on idx.resolver. Held here so MultiIndexer
	// can mirror it onto the global post-pass resolver in
	// RunDeferredPassesAll. See SetResolverLSPHelper.
	resolverLSPHelper resolver.LSPHelper

	// npmAliasOnce builds npmAlias lazily on the first resolve-time
	// import-rewrite request. Lazy because the repo root and prefix
	// are set after New(); by the time the resolver runs they are
	// final.
	npmAliasOnce sync.Once
	npmAlias     *npmAliasIndex

	// workspaceMembersOnce builds workspaceMembers lazily on the first
	// resolve-time package-manager-workspace lookup. Lazy for the same
	// reason as npmAliasOnce — the repo root and prefix are final only
	// after New().
	workspaceMembersOnce sync.Once
	workspaceMembers     *workspaceMembershipIndex

	// Mtime tracking and parse error retention for index health diagnostics.
	parseErrors   []IndexError
	fileMtimes    map[string]int64
	lastIndexTime time.Time
	totalDetected int
	mtimeMu       sync.RWMutex

	// contractCache memoizes the contract-extractor output per file.
	// Keyed by graph file path (with repo prefix); value is the file's
	// disk mtime when last extracted plus the contracts that came out.
	// extractContracts replays cache hits to skip the read + 8-extractor
	// run for files that haven't changed since the last extraction —
	// the dominant cost on repos with tens of thousands of source files.
	contractCache   map[string]*contractCacheEntry
	contractCacheMu sync.RWMutex

	// upgradeOnce gates the BM25→Bleve auto-upgrade to exactly one
	// goroutine per indexer lifetime. Without this, every post-threshold
	// IndexCtx — which fires once per tracked repo during multi-repo
	// warmup — would spawn a fresh upgradeSearchToBleve goroutine.
	// Each rebuilds ~N-doc Bleve indexes (≈32 KiB/doc), so overlapping
	// upgrades peak memory far above steady-state and waste CPU on
	// rebuilds that the next Swap immediately discards. Also counts
	// scheduled upgrades so tests can observe gating decisions
	// without relying on log scrapes or timing.
	upgradeOnce      sync.Once
	upgradeSpawnedMu sync.Mutex
	upgradeSpawned   int

	// deferResolve, when set, makes IndexCtx skip the cross-cutting passes
	// (per-repo ResolveAll / semantic enrichment / contract extraction +
	// commit) so the multi-repo orchestrator can run them serially after
	// the parallel fan-out joins. Without this, two goroutines indexing
	// different repos into the shared graph race on Edge.Meta during the
	// resolver's mutation phase vs. the contract pass's graph walk via
	// AllEdges().
	deferResolve       bool
	pendingContractReg *contracts.Registry
	// deferredGoModDone makes go.mod contract materialisation idempotent for
	// one pending contract generation. Coordinated multi-repo cold/warmup runs
	// a pre-enrichment resolve and then the deferred enrichment/contract drain;
	// both stages call runDeferredGoMod, but only the first may do the work.
	deferredGoModDone bool

	// pendingEnrich is raised by an index pass that did real work — IndexCtx
	// that observed files (or a whole-repo re-track) and IncrementalReindex /
	// IncrementalReindexPaths that re-indexed or evicted at least one file. It
	// is cleared only after runDeferredEnrich completes its queued file batch or
	// repository pass without a partial result. The daemon warmup enriches every
	// indexer it collected, so this gates the (multi-minute LSP hover) pass to
	// repos that actually changed: an unchanged repo on a warm restart would
	// otherwise re-confirm nothing for 10+ minutes. A partial / abandoned /
	// failed enrich leaves it set so a later deferred pass retries.
	pendingEnrich atomic.Bool

	// deferredApplyGate, when non-nil, parks every enrichment provider's
	// graph-apply phase until the channel closes. Set by BeginDeferredPasses
	// before the pool launches (so the pool's compute may overlap the warmup
	// resolve without its applies starving the resolver) and cleared by
	// FinishTail.
	deferredApplyGate <-chan struct{}

	// deferredEnrichFiles holds a deduplicated, repo-scoped Go frontier for
	// one deferred batch. Providers receive the whole set at once, so multiple
	// edits become one compiler load rather than N loads or a ./... fallback.
	// Deletions and unknown-language work promote the scope to full. Generation
	// prevents a pass from clearing work queued concurrently while it ran.
	deferredEnrichMu         sync.Mutex
	deferredEnrichFiles      map[string]struct{}
	deferredEnrichFull       bool
	deferredEnrichGeneration uint64

	// fullReindexed is raised by a whole-repo (re-)parse — IndexCtx, reached
	// via a full re-track, a cold TrackRepo, or a snapshot-partial forced full
	// walk — which evicts and re-creates every node and edge for the repo. That
	// drops the LSP hover-enrichment edges a static re-parse cannot reproduce,
	// so the deferred-enrichment pass must re-run even when the persisted
	// completion marker still records the repo's HEAD on a clean tree. It
	// threads Force into RepoEnrichState so enrichMarkerCurrent stops gating the
	// pass out. A fresh Indexer per daemon run starts it false, so it only ever
	// reflects work this run performed.
	fullReindexed atomic.Bool

	// reparsedThisRun is the scoped analogue of fullReindexed: it is raised by a
	// scoped incremental pass that re-parsed at least one stale file this run —
	// IncrementalReindexPaths / IncrementalReindex with a non-zero
	// StaleFileCount. A scoped re-parse evicts and re-creates just the changed
	// files' nodes, dropping THEIR hover-enrichment edges exactly as a whole-repo
	// re-parse drops every node's, so the deferred pass must likewise run past
	// the completion marker at an unchanged clean HEAD — otherwise the re-parsed
	// files' LSP edges stay durably gone until the repo's HEAD moves or its tree
	// goes dirty. runDeferredEnrich ORs it into RepoEnrichState.Force; because the
	// hover provider skips already-stamped nodes, the marker bypass re-hovers only
	// the freshly-unstamped re-parsed files, not the whole repo, so the cost stays
	// bounded. fullReindexed stays clear on the scoped paths — this flag keeps the
	// two claims distinct. A fresh Indexer per daemon run starts it false.
	reparsedThisRun atomic.Bool

	// deferGlobalPasses, when set, makes IndexCtx and IncrementalReindex
	// skip the graph-wide derivation passes (InferImplements,
	// InferOverrides, markTestSymbolsAndEmitEdges). These passes walk the
	// entire shared graph, so running them per-repo inside a batch loop
	// (warmup, ReconcileAll) is O(R · global_size) — quadratic for repo
	// counts in the hundreds. The batch caller is responsible for invoking
	// RunGlobalGraphPasses exactly once at the end. Has no effect on the
	// deferResolve path (multi-repo IndexCtx already skips those passes).
	deferGlobalPasses bool

	// skipResolveInDeferred, when set, makes RunDeferredPasses skip the
	// per-repo resolver.ResolveAll() call. ResolveAll walks the entire
	// shared graph, so paying it once per indexer across hundreds of
	// repos is O(R · E). MultiIndexer.RunDeferredPassesAll sets this
	// flag on every indexer and runs a single resolver.New(graph).ResolveAll
	// once at the end, which picks up every placeholder edge at once.
	// Has no effect on direct (non-batch) callers of RunDeferredPasses.
	skipResolveInDeferred bool

	// codeownersOnce ensures the repo-level CODEOWNERS file is parsed
	// exactly once per indexer lifetime. The rule list is derived
	// from .github/CODEOWNERS / CODEOWNERS / docs/CODEOWNERS at
	// first use and applied per-file by applyCoverageDomains; an
	// absent file produces empty rules and a no-op pass.
	codeownersOnce  sync.Once
	codeownersRules []codeowners.Rule

	// cloneIndex maintains the clone-detection CMS + length-stratified
	// LSH live across single-file edits, so a steady-state reindex
	// updates EdgeSimilarTo edges in O(edited file) instead of the
	// whole-graph detectClonesAndEmitEdges recompute. Constructed empty
	// (built=false) — a batch/global clone pass calls Rebuild to seed it.
	// A watcher edit never seeds it synchronously: until an explicit
	// clone-consuming/global pass rebuilds it, clone completeness is marked
	// pending and the edit hot path stays bounded to the changed file.
	cloneIndex *incrementalCloneIndex

	// prepared caches the watcher's one-shot parsed graph delta until
	// indexFile consumes it. This avoids parsing a structural edit twice
	// (once to classify it and once to apply it); byte equality in
	// takePreparedExtraction prevents stale reuse when another save lands
	// between the probe and graph patch.
	preparedMu sync.Mutex
	prepared   map[string]*preparedExtraction

	// affectedByPasses / affectedByFilesResolved / affectedByDropped
	// count the affected-by re-resolution activity (see affected_by.go):
	// passes that found a signature delta and ran, referencing files
	// re-resolved by them, and files dropped by the fan-out cap.
	// Exposed via AffectedByCounts so tests and diagnostics can observe
	// that a body-only edit triggered no fan-out.
	affectedByPasses        atomic.Int64
	affectedByFilesResolved atomic.Int64
	affectedByDropped       atomic.Int64

	// incrementalResolveFilesHook is a focused test seam for proving a
	// multi-file watcher batch invokes the scoped resolver exactly once. nil in
	// production; the real path calls resolver.ResolveFilesAndIncoming.
	incrementalResolveFilesHook func([]string)
	// incrementalCatchupHook observes resolution-dependent incremental tails.
	// It is nil in production and lets focused tests prove a watcher storm runs
	// each tail once after the bounded mutation batch, never once per chunk.
	incrementalCatchupHook func(kind string, files []string)
}

// contractCacheEntry is a cached contract-extraction result for one file.
type contractCacheEntry struct {
	mtimeNano int64
	contracts []contracts.Contract
}

// New creates an Indexer that writes through the supplied graph.Store.
// Any backend (in-memory, SQLite-on-disk, remote) is acceptable — the
// indexer's mutation paths go through the Store interface methods only,
// so swapping backends is a zero-code-change configuration choice for
// callers.
func New(g graph.Store, reg *parser.Registry, cfg config.IndexConfig, logger *zap.Logger) *Indexer {
	idx := &Indexer{
		graph:           g,
		shadowAdmission: processShadowAdmission,
		registry:        reg,
		resolver:        resolver.New(g),
		// Wrap in Swappable so the auto-upgrade to Bleve at large
		// corpus sizes can happen in a background goroutine without
		// racing with concurrent searches. Subsequent reassignments to
		// idx.search (Hybrid wrap, etc.) should use swap helpers below.
		//
		// When the backing store implements graph.SymbolSearcher
		// (today only store_sqlite), the initial backend is a thin
		// adapter that forwards Search to the store's native FTS.
		// The in-process Bleve / BM25 build path is then bypassed
		// entirely — saving ~100MB heap on a Vscode-scale repo and
		// putting search in the same address space as the rest of
		// the graph queries.
		search:        search.NewSwappable(initialSearchBackend(g)),
		config:        cfg,
		transforms:    newTransformPipeline(cfg.Transforms, logger),
		logger:        logger,
		fileMtimes:    make(map[string]int64),
		contractCache: make(map[string]*contractCacheEntry),
		cloneIndex:    newIncrementalCloneIndex(),
	}
	// Resolve JS/TS imports declared through an npm alias against the
	// local index. The index is built lazily on first use — the repo
	// root and prefix are not final until after New().
	idx.resolver.SetNpmAliasResolver(idx.resolveNpmAliasImport)
	// Expand JS/TS tsconfig/jsconfig path-alias imports (`@/lib/x`)
	// against the local index so cross-directory alias imports resolve
	// to their real file. Same lazy-build rationale.
	idx.resolver.SetPathAliasResolver(idx.resolvePathAliasImport)
	// Break same-named import collisions in favour of the importer's
	// own package-manager workspace member. Same lazy-build rationale.
	idx.resolver.SetWorkspaceMembership(idx.indexerWorkspaceMembership)
	return idx
}

// resolveNpmAliasImport is the resolver.NpmAliasResolver installed on
// this Indexer's resolver. It rewrites a JS/TS import specifier that
// matches an npm-alias dependency key in the importing file's
// nearest-ancestor package.json. Returns "" (no rewrite) when no
// alias applies. The backing npmAliasIndex is built once, lazily.
func (idx *Indexer) resolveNpmAliasImport(callerFile, specifier string) string {
	idx.npmAliasOnce.Do(func() {
		idx.npmAlias = newNpmAliasIndex(map[string]string{idx.repoPrefix: idx.rootPath})
	})
	return idx.npmAlias.Resolve(callerFile, specifier)
}

// swappable returns the search backend cast to *search.Swappable. Panics
// if the invariant (idx.search is always a Swappable) is ever broken —
// that would be a programmer error in this file, not a runtime condition.
func (idx *Indexer) swappable() *search.Swappable {
	if sw, ok := idx.search.(*search.Swappable); ok {
		return sw
	}
	panic("indexer: search backend is not *search.Swappable — invariant violated")
}

// searchIndexFields returns the text fields fed to the BM25 search
// backend for a node. For an ordinary code symbol that is its
// name, file path, and signature. For a KindDoc prose-section node
// the body is what carries the search signal, so the section text
// (Meta["section_text"]) is indexed alongside the breadcrumb name
// -- a prose query then ranks the section, not just a heading match.
func searchIndexFields(n *graph.Node, projectName string) []string {
	// The project-name path segment is stripped from the INDEXED path (not the
	// stored FilePath) so it contributes no uniform path-field boost.
	indexedPath := search.StripProjectNameFromPath(n.FilePath, projectName)
	if n.Kind == graph.KindDoc {
		body, _ := n.Meta["section_text"].(string)
		return []string{n.Name, indexedPath, body}
	}
	// Retrieval-only fields are normalized at the shared extraction boundary.
	// The graph-owned accessor keeps search, embeddings, and MCP presentation
	// on the same fallback contract without exposing metadata keys.
	retrieval := n.RetrievalMetadata()
	// A symbol's doc comment is the natural-language statement of what it
	// does — precisely the vocabulary a task-intent query carries ("union
	// the two sequences", "performs matching on the ignore files") when it
	// names no identifier verbatim. The identifier and signature alone
	// answer name lookups; the doc summary is what lets an intent query
	// reach the definition at all. Only the leading summary is indexed
	// (docSummary), so a long doc block's examples and edge-case prose
	// can't dilute the name/signature tokens under BM25 length
	// normalisation. Empty doc → empty field, dropped by the caller, so a
	// symbol with no doc indexes exactly as before.
	return []string{n.Name, indexedPath, retrieval.QualName, retrieval.Signature, docSummary(retrieval.Doc)}
}

// docSummaryMaxRunes bounds how much of a doc comment enters the search
// document. The leading summary is the highest-signal part; the rest of a
// long doc — parameter lists, example code blocks, edge-case notes — is
// lower-signal and, unbounded, would dominate the token bag and dilute the
// identifier under BM25 length normalisation. The bound keeps the doc's
// contribution on the order of a signature's. It is a generic document-size
// budget, independent of any repository or corpus.
const docSummaryMaxRunes = 280

// docSummary returns the leading summary of a doc comment for the search
// index: the first paragraph (up to the first blank line), trimmed and
// bounded to docSummaryMaxRunes. Doc comments across languages put the
// one-line / one-paragraph summary first and defer detail and examples to
// later paragraphs, so the first paragraph is the highest-signal slice.
// Empty in → empty out.
func docSummary(doc string) string {
	if doc == "" {
		return ""
	}
	// Normalise CRLF so the blank-line cut is newline-style-agnostic.
	if strings.Contains(doc, "\r") {
		doc = strings.ReplaceAll(doc, "\r\n", "\n")
		doc = strings.ReplaceAll(doc, "\r", "\n")
	}
	if i := strings.Index(doc, "\n\n"); i >= 0 {
		doc = doc[:i]
	}
	doc = strings.TrimSpace(doc)
	if r := []rune(doc); len(r) > docSummaryMaxRunes {
		doc = strings.TrimSpace(string(r[:docSummaryMaxRunes]))
	}
	return doc
}

// vectorSearcherDelegate is the search.VectorDelegate-shaped
// adapter the indexer hands to VectorBackend.SetDelegate when the
// underlying store implements graph.VectorSearcher. SimilarTo just
// forwards — search.VectorDelegate is defined to return
// graph.VectorHit slices directly, so there's no translation work
// here, just a small struct so the in-process search package
// doesn't depend on graph.VectorSearcher's full surface.
type vectorSearcherDelegate struct {
	s graph.VectorSearcher
}

func (d *vectorSearcherDelegate) SimilarTo(vec []float32, limit int) ([]graph.VectorHit, error) {
	if d == nil || d.s == nil {
		return nil, nil
	}
	return d.s.SimilarTo(vec, limit)
}

// initialSearchBackend picks the search.Backend the indexer wraps
// in its Swappable on construction. When the underlying store
// implements graph.SymbolSearcher (today only store_sqlite), a
// thin adapter routes Search calls through the store's native FTS
// — the in-process BM25 / Bleve build path is bypassed entirely.
// Otherwise falls through to search.NewAuto which picks BM25 for
// small corpora and auto-upgrades to Bleve once the size warrants
// it.
func initialSearchBackend(g graph.Store) search.Backend {
	if s, ok := g.(graph.SymbolSearcher); ok {
		return search.NewSymbolSearcherBackend(s)
	}
	return search.NewAuto()
}

// isSymbolSearcherBackend reports whether the swappable's currently
// active backend is the SymbolSearcher adapter. Used to suppress
// the Bleve auto-upgrade goroutine — if the active backend is
// already a native FTS, upgrading to Bleve would re-index the same
// corpus into a parallel in-process Bleve and silently swap it in,
// defeating the FTS path and pinning the ~100MB heap the FTS
// integration was meant to release.
func isSymbolSearcherBackend(b search.Backend) bool {
	if b == nil {
		return false
	}
	if sw, ok := b.(*search.Swappable); ok {
		b = sw.Inner()
	}
	_, ok := b.(*search.SymbolSearcherBackend)
	return ok
}

// ftsTokensFor produces the pre-tokenised text the backend FTS path
// indexes. Mirrors searchIndexFields' field selection but joins
// every field through search.Tokenize (camelCase / snake_case /
// path-segment splitter) so the resulting token list matches the
// in-process BM25 corpus contract — the same query produces the
// same recall against either backend. Joined with spaces so the
// downstream COPY FROM sees a single STRING column value.
func ftsTokensFor(n *graph.Node, projectName string) string {
	// searchIndexFields includes the resolver qualifier or its retrieval-only
	// replacement, so both BM25 backends and embeddings see the same token bag.
	fields := searchIndexFields(n, projectName)
	tokens := make([]string, 0, 16)
	for _, f := range fields {
		if f == "" {
			continue
		}
		tokens = append(tokens, search.Tokenize(f)...)
	}
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " ")
}

// shouldIndexForSearch reports whether a node should be added to the
// text search index (BM25/Bleve). File and Import nodes are never
// searchable symbols. Beyond that, config.SkipSearch filters out
// (language, kind) pairs that would only add noise — JSON/YAML/TOML
// keys, CSS tokens, Terraform blocks, shell/build variables. All three
// text-index call sites (buildSearchIndex bulk loop, indexFile
// incremental add, upgradeSearchToBleve repopulate) must go through
// this predicate so they can't drift.
func (idx *Indexer) shouldIndexForSearch(n *graph.Node) bool {
	// Cross-daemon proxy-edge nodes stand in for remote symbols; they
	// are never surfaced in local name search. Inert until
	// edge-minting is enabled.
	if graph.IsProxyNode(n) {
		return false
	}
	if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
		return false
	}
	// KindLocal nodes are intra-function bindings emitted to satisfy
	// rel-table FK constraints on the dataflow edges that target
	// locals. They have a real Name (the variable identifier) but
	// surfacing them in BM25 would flood every search for common
	// names like `err`, `data`, `n`, `i`. Excluded unconditionally.
	if n.Kind == graph.KindLocal {
		return false
	}
	// KindBuiltin nodes are language intrinsics (append / len /
	// string / int / ...). Surfacing them in name search would
	// drown every other hit on common identifiers — agents already
	// know `string` / `append`. They remain queryable by kind and
	// by ID for the analytics passes that care.
	if n.Kind == graph.KindBuiltin {
		return false
	}
	// CONTENT (data_class="content") section nodes live in the dedicated
	// content index (content_fts), never the symbol search — keeping the
	// symbol corpus code-only and bounded. Markdown prose (KindDoc without
	// data_class=content) is unaffected and still honours IndexProse below.
	if isContentNode(n) {
		return false
	}
	// Prose-section nodes are searchable only when prose indexing is
	// enabled (search.index_prose); the rest of the graph is
	// unaffected by the toggle.
	if n.Kind == graph.KindDoc && !idx.config.IndexProse {
		return false
	}
	if config.ShouldSkipSearch(idx.config.SkipSearch, n.Language, string(n.Kind)) {
		return false
	}
	return true
}

// removeFromSearch drops a node from the symbol index, gated on the same
// predicate that admits it. Every eviction path must go through this rather
// than its own Kind check.
//
// The backend's Remove is an unconditional decrement with no membership set,
// so evicting on a broader predicate than Add uses does not just miscount by
// a constant: it subtracts the difference set on every reconcile of the same
// file and never adds it back. KindLocal dominates that set — those bindings
// are persisted only to satisfy dataflow FK constraints, so they are present
// in the prior-node list of every Go file, while shouldIndexForSearch has
// always refused to index them.
//
// Safe on nodes read back from the store: every input the predicate reads
// (Kind, Language, Stub, Origin, data_class) is a persisted column, so it
// evaluates the same on a stored node as on the freshly extracted one that
// was admitted.
func (idx *Indexer) removeFromSearch(n *graph.Node) {
	if idx == nil || idx.search == nil || n == nil {
		return
	}
	if !idx.shouldIndexForSearch(n) {
		return
	}
	idx.search.Remove(n.ID)
}

// upgradeSearchToBleve constructs a Bleve backend from the current graph
// and atomically swaps it in. Designed to run in a background goroutine
// triggered by IndexCtx after the initial index completes. Does nothing
// if Bleve construction fails (caller already hit AutoThreshold but the
// in-memory backend keeps serving correctly, just with worse memory
// characteristics).
// bleveUpgradeEntry is one row of the snapshot the upgrade goroutine
// works from. Snapshotting (id, name, file, signature) up front in
// the foreground — before the goroutine starts reading them — keeps
// the goroutine race-free against subsequent Index calls' Meta-writing
// passes (reach.BuildIndex, ResolveTemporalCalls, ...).
type bleveUpgradeEntry struct {
	id string
	// fields is the BM25 text payload for the node, as produced by
	// searchIndexFields: name + file + signature for a code symbol,
	// name + file + section body for a KindDoc prose section.
	fields []string
}

// snapshotBleveEntries captures every node currently eligible for the
// search index plus its `signature` Meta string. Called synchronously
// from IndexCtx after every Node.Meta mutating pass has returned, so
// the read of n.Meta happens with no concurrent writer.
func (idx *Indexer) snapshotBleveEntries() []bleveUpgradeEntry {
	nodes := graph.RepoCodeNodes(idx.graph, idx.repoPrefix)
	out := make([]bleveUpgradeEntry, 0, len(nodes))
	for _, n := range nodes {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		out = append(out, bleveUpgradeEntry{id: n.ID, fields: searchIndexFields(n, idx.projectName)})
	}
	return out
}

func (idx *Indexer) upgradeSearchToBleve(snapshot []bleveUpgradeEntry) {
	// Defensive early-return: if the active text backend is already
	// Bleve, there is nothing to upgrade. IndexCtx's sync.Once guard
	// prevents re-entry from the auto-upgrade path, but direct
	// callers (tests, manual invocation from tooling) could still
	// hit this function twice; a second run would pointlessly
	// rebuild a full Bleve index and Swap it over an identical one.
	inner := idx.swappable().Inner()
	if _, ok := inner.(*search.BleveBackend); ok {
		return
	}
	if hyb, ok := inner.(*search.HybridBackend); ok {
		if _, ok := hyb.TextBackend().(*search.BleveBackend); ok {
			return
		}
	}

	// Opt-in disk backend. Scorch stores the inverted index on disk
	// (~10-20× less heap than upsidedown+gtreap) at the cost of file
	// I/O during build. Users point GORTEX_BLEVE_DISK_DIR at a
	// writable path; we manage the file lifecycle inside it.
	diskDir := os.Getenv("GORTEX_BLEVE_DISK_DIR")

	var (
		blv *search.BleveBackend
		err error
	)
	if diskDir != "" {
		blv, err = search.NewBleveDisk(diskDir)
		if err != nil {
			idx.logger.Warn("search: bleve disk construction failed, falling back to in-memory",
				zap.String("dir", diskDir), zap.Error(err))
		}
	}
	if blv == nil {
		blv, err = search.NewBleve()
		if err != nil {
			idx.logger.Warn("search: bleve construction failed, staying on in-memory",
				zap.Error(err))
			return
		}
	}

	// Use the foreground snapshot the spawner captured rather than
	// re-walking idx.graph here: the goroutine outlives the spawning
	// IndexCtx call, and subsequent Index calls' Meta-writing passes
	// (reach.BuildIndex, ResolveTemporalCalls, ...) mutate Node.Meta
	// on the same Node objects. Reading sig from a live n.Meta here
	// would race with those writes.
	for _, e := range snapshot {
		blv.Add(e.id, e.fields...)
	}

	// Preserve the vector index if one is wired up. The previous inner
	// is normally a HybridBackend wrapping text + vector + embedder;
	// swapping in raw Bleve would let Swap's old.Close() run on the
	// old Hybrid, which closes only its text side (hybrid.Close) but
	// leaves the resulting inner — raw *BleveBackend — unwrapped, so
	// every downstream hybrid/semantic query silently degrades to
	// BM25 until the daemon restarts. Rewrap the fresh Bleve in a new
	// Hybrid carrying the old vector + embedder. The vector backend
	// itself is never closed by Hybrid.Close, so it stays alive even
	// after the old Hybrid is torn down by Swap.
	sw := idx.swappable()
	var replacement search.Backend = blv
	if oldHybrid, ok := sw.Inner().(*search.HybridBackend); ok {
		replacement = search.NewHybrid(blv, oldHybrid.VectorIndex(), oldHybrid.Embedder())
	}
	sw.Swap(replacement)

	mode := "memory"
	if blv.DiskPath() != "" {
		mode = "disk"
	}
	idx.logger.Info("search: upgraded to Bleve backend (background)",
		zap.Int("symbols", blv.Count()),
		zap.String("mode", mode),
		zap.String("disk_path", blv.DiskPath()))
}

// Graph returns the underlying graph.
func (idx *Indexer) Graph() graph.Store { return idx.graph }

// Search returns the search backend.
func (idx *Indexer) Search() search.Backend { return idx.search }

// Registry returns the parser registry shared across this indexer.
// Exposed for the editor-overlay middleware: the overlay layer-build
// pass parses each pushed buffer through the same per-language
// extractor the indexer uses, ensuring overlay-derived nodes match
// base-derived nodes byte-for-byte for the same input.
func (idx *Indexer) Registry() *parser.Registry { return idx.registry }

// ContractRegistry returns the contract registry populated during indexing.
func (idx *Indexer) ContractRegistry() *contracts.Registry { return idx.contractRegistry }

// SetContractRegistry installs reg as the indexer's contract registry.
// Used by the daemon warmup path to rehydrate the registry from a
// snapshot when IncrementalReindex skipped extraction (no stale files
// → extractContracts never ran → idx.contractRegistry stays nil after
// reconcile, which used to leave multi-repo `contracts` queries silently
// empty). Callers should only install when ContractRegistry() is nil;
// installing over a freshly-extracted registry would roll state backward.
func (idx *Indexer) SetContractRegistry(reg *contracts.Registry) {
	idx.contractRegistry = reg
}

// SetTrackedRepoModules sets the map of tracked repo names to Go module paths.
// This enables the GoModExtractor to detect cross-repo dependencies.
func (idx *Indexer) SetTrackedRepoModules(m map[string]string) { idx.trackedRepoModules = m }

// SetDeferResolve toggles whether IndexCtx defers the cross-cutting passes
// to a later RunDeferredPasses call. See the deferResolve field comment.
func (idx *Indexer) SetDeferResolve(v bool) { idx.deferResolve = v }

// SetSkipResolveInDeferred toggles whether RunDeferredPasses calls
// idx.resolver.ResolveAll. The MultiIndexer batch driver sets this so
// the per-repo resolver pass — which walks the entire shared graph —
// runs exactly once globally instead of R times. See the
// skipResolveInDeferred field comment.
func (idx *Indexer) SetSkipResolveInDeferred(v bool) { idx.skipResolveInDeferred = v }

// SetDeferGlobalPasses toggles whether the graph-wide derivation passes
// (InferImplements, InferOverrides, markTestSymbolsAndEmitEdges) run
// inline at the end of IndexCtx / IncrementalReindex. Set true when the
// caller drives a batch (e.g. daemon warmup) and will invoke
// RunGlobalGraphPasses once at the end. See the deferGlobalPasses field
// comment.
func (idx *Indexer) SetDeferGlobalPasses(v bool) { idx.deferGlobalPasses = v }

// RunGlobalGraphPasses runs the graph-wide derivation passes once
// against the indexer's shared graph. Safe to call against a graph that
// already has these edges — InferImplements / InferOverrides skip
// existing parents, and graph.AddEdge dedupes by edgeKey so EdgeTests
// re-emission is a no-op. Logs counts for telemetry. Use when batching
// multiple per-repo TrackRepoCtx / IncrementalReindex calls under
// SetDeferGlobalPasses(true).
func (idx *Indexer) RunGlobalGraphPasses(ctx context.Context) {
	if idx.graph == nil {
		return
	}
	reporter := progress.FromContext(ctx)
	if added := idx.resolver.InferImplements(); added > 0 {
		idx.logger.Info("inferred implements (global)", zap.Int("added", added))
	}
	if added := idx.resolver.InferOverrides(); added > 0 {
		idx.logger.Info("inferred overrides (global)", zap.Int("added", added))
	}
	marked, emitted := markTestSymbolsAndEmitEdges(idx.graph)
	if marked > 0 || emitted > 0 {
		idx.logger.Info("test edges emitted (global)",
			zap.Int("test_symbols", marked),
			zap.Int("edges", emitted),
		)
	}
	if ctrl := entrypoints.PropagateEntryPointsDownHierarchy(idx.graph); ctrl > 0 {
		idx.logger.Info("entry-point hierarchy stamped (global)", zap.Int("stamped", ctrl))
	}
	if re, ep, fa := synthesizeCapabilityEdges(idx.graph); re > 0 || ep > 0 || fa > 0 {
		idx.logger.Info("capability edges emitted (global)",
			zap.Int("reads_env", re),
			zap.Int("executes_process", ep),
			zap.Int("accesses_field", fa),
		)
	}
	reporter.Report("clone detection pass (global)", 0, 0)
	if cs := detectClonesAndEmitEdgesCtx(ctx, idx.graph, idx.repoPrefix, idx.cloneThreshold()); cs.Items > 0 {
		idx.logger.Info("clone edges emitted (global)",
			zap.Int("items", cs.Items),
			zap.Int("clone_pairs", cs.Pairs),
			zap.Int("edges", cs.Edges),
			zap.Int("skipped_buckets", cs.SkippedBuckets),
			zap.Int("skipped_bucket_items", cs.SkippedBucketItems),
			zap.Int("diffused_pairs", cs.DiffusedPairs),
			zap.Int("diffused_edges", cs.DiffusedEdges),
		)
	}
	// Seed the incremental clone index from the freshly-baselined
	// signatures + sidecar so steady-state single-file edits after this
	// batch go incremental instead of re-running the whole-graph pass.
	if idx.cloneIndex != nil {
		idx.cloneIndex.Rebuild(idx.graph, idx.repoPrefix)
	}
	// Framework dynamic-dispatch synthesis (gRPC stubs, Temporal
	// workflow→activity, in-process / native event channels, native
	// bridges). Runs after InferImplements/InferOverrides (the
	// interface-satisfaction signals several synthesizers depend on) and
	// before DetectCrossRepoEdges so a cross-repo synthesized call gets
	// its parallel cross_repo_calls edge.
	reporter.Report("framework dispatch synthesis (global)", 0, 0)
	if rep := resolver.RunFrameworkSynthesizers(idx.graph); rep.Total > 0 {
		idx.logger.Info("framework dispatch calls synthesized (global)",
			zap.Int("edges", rep.Total),
			zap.Any("per_synthesizer", rep.Per),
		)
	}
	// External-call placeholder synthesis (opt-in). Runs after the
	// resolver and the gRPC/Temporal stub passes so every edge that
	// could land on a real node already has; the leftover external
	// terminals are then materialised into synthetic call-chain nodes.
	reporter.Report("external-call synthesis (global)", 0, 0)
	if extCalls := resolver.SynthesizeExternalCalls(idx.graph, idx.externalCallSynthesisEnabled()); extCalls > 0 {
		idx.logger.Info("external-call placeholders synthesized (global)",
			zap.Int("edges", extCalls),
		)
	}
	// Speculative dynamic-dispatch synthesis (opt-in, default off). Mints
	// best-guess hidden-by-default call edges for blind-spot shapes.
	if spec := resolver.ResolveSpeculativeDispatch(idx.graph, idx.speculativeDispatchEnabled()); spec > 0 {
		idx.logger.Info("speculative dispatch edges synthesized (global)",
			zap.Int("edges", spec),
		)
	}
	// Content -> code "why" links. Runs before DetectCrossRepoEdges so a
	// chunk that motivates a symbol in another repo gets its parallel
	// cross_repo_motivates edge minted by the cross-repo pass below.
	reporter.Report("content links (global)", 0, 0)
	idx.linkContentToCode()
	// Cross-repo edge layer. Runs after InferImplements / InferOverrides
	// so cross-repo implements / extends edges pick up their parallel
	// cross_repo_* edges. No-op on single-repo graphs (no RepoPrefix).
	reporter.Report("cross-repo edges (global)", 0, 0)
	if crossRepoEdges := resolver.DetectCrossRepoEdges(idx.graph); crossRepoEdges > 0 {
		idx.logger.Info("cross-repo edges emitted (global)",
			zap.Int("edges", crossRepoEdges),
		)
	}
	// Reachability index — used to be precomputed here for every
	// impact seed. The eager pass was retired because the breakeven
	// math doesn't work: on a 200 k-seed graph (k8s) the build took
	// ~2000 s of cold-index wall time to save ~10 ms per
	// AnalyzeImpact call, requiring ~200 k queries to pay off — well
	// beyond any realistic agent session. Lookups are now
	// compute-on-first-use; we just invalidate the cache so any
	// surviving stamps from a previous build don't shadow the fresh
	// graph state.
	reach.InvalidateIndex()
}

// cloneThreshold returns the configured Jaccard similarity cutoff for
// clone detection (0 = use the clones package default).
func (idx *Indexer) cloneThreshold() float64 {
	return idx.config.Coverage.ClonesThreshold()
}

// RunDeferredPasses runs the per-repo cross-cutting passes that IndexCtx
// skipped in deferred mode: per-repo ResolveAll, semantic enrichment, and
// contract extraction + commit. Safe to call only after IndexCtx has
// populated the graph for this repo. Idempotent — second calls are a no-op
// because the pending registry is cleared at the end.
//
// The graph-wide derivation passes (InferImplements, InferOverrides,
// markTestSymbolsAndEmitEdges) intentionally do NOT run here. They walk
// the entire shared graph, so the multi-repo orchestrator must invoke
// MultiIndexer.RunGlobalGraphPasses exactly once after every repo has
// finished its deferred per-repo work.
func (idx *Indexer) RunDeferredPasses(ctx context.Context) {
	if idx.pendingContractReg == nil {
		return
	}
	reporter := progress.FromContext(ctx)
	tphase := time.Now()
	var dGoMod, dResolve, dEnrich, dContract time.Duration

	idx.runDeferredGoMod()
	dGoMod = time.Since(tphase)
	tphase = time.Now()

	// Per-repo resolver.ResolveAll walks the entire shared graph; with R
	// repos and E edges that's O(R · E). The MultiIndexer batch driver
	// sets skipResolveInDeferred so this runs exactly once globally
	// (resolver.New(mi.graph).ResolveAll after every per-repo deferred
	// pass has committed contracts). Direct (non-batch) callers leave
	// the flag false and pay the standard single-repo cost.
	if !idx.skipResolveInDeferred {
		reporter.Report("resolving references", 0, 0)
		idx.populateCppIncludeDirs(false)
		idx.resolver.ResolveAll()
	}
	dResolve = time.Since(tphase)
	tphase = time.Now()

	reporter.Report("semantic enrichment", 0, 0)
	idx.runDeferredEnrich()
	dEnrich = time.Since(tphase)
	tphase = time.Now()

	reporter.Report("extracting contracts", 0, 0)
	idx.runDeferredContractsAndReleaseSemanticState()
	dContract = time.Since(tphase)
	idx.logger.Info("DEFERRED-TIMING per-repo",
		zap.String("repo", idx.repoPrefix),
		zap.Duration("gomod", dGoMod),
		zap.Duration("resolve", dResolve),
		zap.Duration("enrich", dEnrich),
		zap.Duration("contract_commit", dContract))
}

// runDeferredGoMod materialises dep::<module> contract nodes from go.mod
// BEFORE ResolveAll so the resolver's import bridge can re-target Go imports
// of declared modules to their dep contract node instead of an external::
// stub. Split out of RunDeferredPasses so the batch driver can run it
// serially across repos ahead of the parallel enrichment phase.
func (idx *Indexer) runDeferredGoMod() {
	if idx.pendingContractReg == nil || idx.deferredGoModDone {
		return
	}
	idx.extractGoModContracts(idx.pendingContractReg)
	idx.deferredGoModDone = true
}

// runDeferredEnrich runs semantic enrichment for this repo. The manager fetches
// a per-repo LSP provider instance (keyed by the repo's workspace); go/types is
// bounded by its cancellation-aware heavyweight gate and publishes only compact
// repo-scoped binding strings; tstypes is stateless; and providers serialize
// graph mutations on the backend resolve mutex.
func (idx *Indexer) runDeferredEnrich() {
	if idx.semanticMgr == nil || !idx.semanticMgr.Enabled() || !idx.semanticMgr.HasProviders() {
		return
	}
	// Gate the pass to repos that actually changed. The daemon warmup collects
	// every indexer and enriches them all, but an unchanged repo's persisted
	// graph already carries its enrichment edges — re-running gopls hover for
	// it confirms nothing over many minutes. GORTEX_WARMUP_FORCE_ENRICH=1
	// bypasses the gate for a full re-enrich.
	forced := os.Getenv("GORTEX_WARMUP_FORCE_ENRICH") == "1"
	if !idx.pendingEnrich.Load() {
		if !forced {
			idx.logger.Info("deferred enrichment skipped",
				zap.String("repo", idx.repoPrefix),
				zap.String("reason", "unchanged"))
			return
		}
		idx.logger.Info("deferred enrichment forced despite no pending changes",
			zap.String("repo", idx.repoPrefix))
	}
	pendingFiles, fullScope, pendingGeneration := idx.deferredEnrichScope()
	if forced {
		pendingFiles = nil
		fullScope = true
	}
	// Lease compiler-backed provider state before enrichment creates it. The
	// matching release runs after this repo's contract pass, including failure
	// unwinding, so a slow sibling in the batch cannot trigger TTL eviction.
	idx.retainDeferredSemanticState()
	// Key by the repo prefix so a repo-scoped provider can scope file
	// selection to this repo (empty in single-repo mode).
	roots := map[string]string{idx.repoPrefix: idx.rootPath}
	// Compute the repo's git freshness ONCE and thread it in so the manager's
	// per-provider skip gate and the completion-marker write agree on the
	// identical (sha, dirty): a provider whose persisted marker still matches
	// HEAD on a clean tree is skipped instead of re-running its hover pass.
	sha, dirty := repoHeadAndDirty(idx.rootPath)
	if !fullScope && len(pendingFiles) > 0 {
		byLanguage := idx.deferredEnrichFrontiers(pendingFiles)
		languages := make([]string, 0, len(byLanguage))
		for language := range byLanguage {
			languages = append(languages, language)
		}
		sort.Strings(languages)
		for _, language := range languages {
			files := byLanguage[language]
			result, err := idx.semanticMgr.EnrichFiles(
				idx.graph, idx.repoPrefix, idx.rootPath, language, files,
			)
			if err != nil {
				idx.logger.Warn("file-scoped deferred semantic enrichment failed",
					zap.String("repo", idx.repoPrefix),
					zap.String("language", language),
					zap.Int("files", len(files)),
					zap.Error(err))
				return
			}
			if result != nil {
				idx.logger.Info("semantic enrichment result",
					zap.String("provider", result.Provider),
					zap.String("language", result.Language),
					zap.Int("confirmed", result.EdgesConfirmed),
					zap.Int("added", result.EdgesAdded),
					zap.Int("refuted", result.EdgesRefuted),
					zap.Float64("coverage", result.CoveragePercent),
				)
				if result.Partial {
					return
				}
			}
		}
		// A file frontier proves only that the affected language/file batches
		// were refreshed. It must never publish the whole-repository completion
		// marker: files and packages outside this exact frontier did not run.
		idx.clearPendingEnrich(pendingGeneration)
		return
	}
	// A re-parse this run evicted the persisted hover edges of the re-parsed
	// files, so force the pass past the completion-marker gate — an unchanged
	// clean HEAD would otherwise skip re-enrichment and leave those files' LSP
	// edges durably gone. fullReindexed covers a whole-repo re-parse (IndexCtx);
	// reparsedThisRun covers a scoped incremental re-parse that dropped only the
	// changed files' edges. Either forces the pass; the provider re-hovers only
	// the freshly-unstamped nodes, so a scoped force stays bounded to those files.
	opts := semantic.EnrichOptions{
		RepoState: map[string]semantic.RepoEnrichState{
			idx.repoPrefix: {SHA: sha, Dirty: dirty, Force: idx.fullReindexed.Load() || idx.reparsedThisRun.Load()},
		},
		MinLanguageNodes: semantic.EnrichmentAdmissionFloor(),
		ApplyGate:        idx.deferredApplyGate,
	}
	results, partialRepos, err := idx.semanticMgr.EnrichAll(idx.graph, roots, opts)
	if err != nil {
		idx.logger.Warn("semantic enrichment failed", zap.Error(err))
		return
	}
	for _, r := range results {
		idx.logger.Info("semantic enrichment result",
			zap.String("provider", r.Provider),
			zap.String("language", r.Language),
			zap.Int("confirmed", r.EdgesConfirmed),
			zap.Int("added", r.EdgesAdded),
			zap.Int("refuted", r.EdgesRefuted),
			zap.Float64("coverage", r.CoveragePercent),
		)
	}
	// Clear the pending marker only when every provider that ran for this repo
	// finished non-partial. A partial / abandoned / failed pass leaves it set
	// so a later deferred pass (or the next restart, once the repo changes
	// again) retries the enrichment rather than trusting an incomplete graph.
	if !partialRepos[idx.repoPrefix] && idx.clearPendingEnrich(pendingGeneration) {
		// Persist a whole-repo completion marker at this HEAD so the next warm
		// restart can tell, with one lookup, that this repo's enrichment finished
		// and MaybeSeedPendingEnrich need not resume it. A partial / abandoned
		// pass takes the other branch and writes no marker, so the absent marker
		// re-arms the pass on the next start. No-op on a dirty tree / empty sha.
		idx.semanticMgr.RecordRepoEnrichmentComplete(idx.graph, idx.repoPrefix, sha, dirty)
	}
}

// MaybeSeedPendingEnrich re-arms the deferred-enrichment gate for a repo whose
// persisted enrichment is known-incomplete at the current clean HEAD, so a warm
// restart resumes a semantic pass a prior process left partial or abandoned.
//
// pendingEnrich otherwise reflects only re-indexing work performed THIS run, so
// an unchanged repo whose first enrichment was cut short by the per-repo
// deadline would short-circuit runDeferredEnrich on every subsequent restart —
// its whole-repo completion marker is absent (a partial pass writes none) yet no
// file changed to raise the flag. The daemon warmup calls this after the parse
// phase, before draining the deferred passes.
//
// Returns whether this repo will enrich (already pending, or newly seeded). A
// no-op — false — for a repo without semantic providers, on a non-git tree (no
// reliable freshness signal), on a backend that does not persist enrichment
// state (it re-indexes from scratch each restart anyway), when the completion
// marker already records the current HEAD, or on a dirty tree (the marker is
// never written or trusted against uncommitted content, and resuming every
// restart while the tree stays dirty would defeat the warm-restart fast path).
func (idx *Indexer) MaybeSeedPendingEnrich() bool {
	if idx.semanticMgr == nil || !idx.semanticMgr.Enabled() || !idx.semanticMgr.HasProviders() {
		return false
	}
	if idx.pendingEnrich.Load() {
		// This run's re-indexing work already armed the gate.
		return true
	}
	// Cheap probe first: only the sha is needed to tell a repo whose marker is
	// already current (the common warm-restart case) from one that must resume,
	// so the slower git status shell-out is deferred to the resume path below.
	sha := repoHead(idx.rootPath)
	if sha == "" {
		return false
	}
	current, persisted := idx.semanticMgr.RepoEnrichmentMarkerState(idx.graph, idx.repoPrefix, sha)
	if !persisted || current {
		return false
	}
	if _, dirty := repoHeadAndDirty(idx.rootPath); dirty {
		return false
	}
	idx.logger.Info("deferred enrichment re-armed: persisted enrichment incomplete",
		zap.String("repo", idx.repoPrefix),
		zap.String("sha", sha))
	idx.markPendingEnrichFull()
	return true
}

// runDeferredContracts extracts and commits this repo's contract nodes and
// clears the pending registration. extractGoModContracts already ran via
// runDeferredGoMod. Mutates the shared graph and walks repo edges, so the
// batch driver runs it serially after the parallel enrichment phase. Compiler
// binding types are resolved in one batch from SQLite, with the provider's
// compact string index as the in-memory-store fallback.
func (idx *Indexer) runDeferredContracts() {
	if idx.pendingContractReg == nil {
		return
	}
	idx.extractExternalModules()
	idx.extractDIContracts(idx.pendingContractReg)
	idx.commitContracts(idx.pendingContractReg)
	idx.pendingContractReg = nil
	idx.deferredGoModDone = false
}

// runDeferredContractsAndReleaseSemanticState keeps the provider's compact
// binding rows available through every contract consumer and releases them on
// every return path. SQLite rows remain persistent for warm restarts; this
// release only bounds the in-memory fallback.
func (idx *Indexer) runDeferredContractsAndReleaseSemanticState() {
	defer idx.releaseDeferredSemanticState()
	idx.runDeferredContracts()
}

// repoSemanticStateRetainer leases repository-scoped compact binding rows
// across the enrichment-to-contract batch barrier so the in-memory fallback is
// released only after this repository's contract pass.
type repoSemanticStateRetainer interface {
	RetainRepoState(repoRoot string) bool
}

// repoSemanticStateReleaser is implemented by semantic providers that retain
// repository-scoped state solely for the deferred contract consumer.
type repoSemanticStateReleaser interface {
	ReleaseRepoState(repoRoot string) bool
}

func (idx *Indexer) retainDeferredSemanticState() {
	if idx.semanticMgr == nil {
		return
	}
	for _, provider := range idx.semanticMgr.AllProviders() {
		if retainer, ok := provider.(repoSemanticStateRetainer); ok {
			retainer.RetainRepoState(idx.rootPath)
		}
	}
}

// releaseDeferredSemanticState runs only after this repository's contract pass
// has returned. Providers not retaining contract-only state are untouched.
func (idx *Indexer) releaseDeferredSemanticState() {
	if idx.semanticMgr == nil {
		return
	}
	for _, provider := range idx.semanticMgr.AllProviders() {
		if releaser, ok := provider.(repoSemanticStateReleaser); ok {
			releaser.ReleaseRepoState(idx.rootPath)
		}
	}
}

// RootPath returns the root path used for relative path computation.
func (idx *Indexer) RootPath() string { return idx.rootPath }

// storeRootPath records the repository root, skipping a redundant
// self-assignment. The watcher goroutine reads idx.rootPath without a
// lock, so re-storing the identical value from a concurrent reindex or
// mtime-census pass would be a data race for no observable change.
func (idx *Indexer) storeRootPath(absRoot string) {
	if idx.rootPath != absRoot {
		idx.rootPath = absRoot
	}
}

// populateCppIncludeDirs reconstructs each C/C++ source file's include search
// path from compile_commands.json and hands it to the resolver, so a quoted
// include binds against the real `-I` dir set (deterministic collision-breaking)
// before the suffix-unique fallback. forceReload drops the cache first, so an
// incremental reindex picks up an edited compile_commands.json without a
// daemon restart. Keys/dirs are prefixed in multi-repo mode to match file IDs.
func (idx *Indexer) populateCppIncludeDirs(forceReload bool) {
	if idx.resolver == nil || idx.rootPath == "" {
		return
	}
	if forceReload {
		clearCppIncludeDirCache(idx.rootPath)
	}
	prefix := ""
	if idx.repoPrefix != "" {
		prefix = idx.repoPrefix + "/"
	}
	prefixDirs := func(dirs []string) []string {
		if prefix == "" || len(dirs) == 0 {
			return dirs
		}
		pd := make([]string, len(dirs))
		for i, d := range dirs {
			pd[i] = prefix + d
		}
		return pd
	}
	tus := loadCompileCommands(idx.rootPath)
	if len(tus) == 0 {
		// No compile DB: fall back to the conventional include-root heuristic
		// so the ordered probe still runs for repos without a compile DB.
		idx.resolver.SetCppIncludeDirs(nil)
		idx.resolver.SetCppFallbackIncludeDirs(prefixDirs(heuristicIncludeDirs(idx.rootPath)))
		return
	}
	perFile := make(map[string][]string, len(tus))
	for f, tu := range tus {
		perFile[prefix+f] = prefixDirs(tu.includeDirs)
	}
	idx.resolver.SetCppIncludeDirs(perFile)
	idx.resolver.SetCppFallbackIncludeDirs(nil)
}

// ResolveFilePath maps a graph file path (repo-relative in single-repo mode)
// to an absolute filesystem path. Returns "" when no root is set so callers
// can refuse rather than open against the daemon process CWD. Implements
// analysis.SourceReader.
func (idx *Indexer) ResolveFilePath(graphPath string) string {
	if graphPath == "" {
		return ""
	}
	if filepath.IsAbs(graphPath) {
		return filepath.Clean(graphPath)
	}
	if idx.rootPath == "" {
		return ""
	}
	// In multi-repo mode the lone Indexer is wrapped by MultiIndexer
	// (which exposes RepoRoot/ResolveFilePath); single-repo callers
	// hit this path directly.
	rel := graphPath
	if idx.repoPrefix != "" {
		rel = strings.TrimPrefix(rel, idx.repoPrefix+"/")
	}
	return filepath.Clean(filepath.Join(idx.rootPath, rel))
}

// relKey reduces an absolute path under the repo root to the canonical
// repo-relative key the graph and the mtime map are indexed by: forward
// slashes, and Unicode NFC.
//
// The NFC fold is load-bearing. A file with a non-ASCII name is handed
// to the indexer in different byte forms depending on the source — the
// filesystem walk (filepath.WalkDir) yields decomposed NFD on macOS,
// while the git watcher decodes `git diff` output that git stored as
// precomposed NFC. Keying the bulk walk under one form and an
// incremental patch under the other would split a single file across
// two graph keys: the watcher's evict would miss the walk's node,
// IncrementalReindex would see the file as both deleted and freshly
// created, and the daemon would carry a stale duplicate. Folding every
// key through one form here removes that whole class of mismatch.
//
// On a path that is not under rootPath (filepath.Rel fails) the input
// is returned slash-normalised and NFC-folded so the result is still a
// stable key, just not repo-relative.
func (idx *Indexer) relKey(absPath string) string {
	rel, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil {
		return pathkey.Normalize(filepath.ToSlash(absPath))
	}
	return pathkey.Normalize(filepath.ToSlash(rel))
}

// graphRelKey reduces an absolute path to the key the GRAPH stores a
// file's nodes under: repo-relative, OS-native separators (the exact
// form the bulk-walk extractor stamps on node IDs / FilePaths),
// NFC-folded. It is the graph-node analogue of relKey — relKey
// slash-normalises for the mtime map, graphRelKey keeps the OS-native
// separators so an incremental re-index's evict lookup actually matches
// the nodes the cold walk created (graph.GetFileNodes / EvictFile key on
// the exact string, with no separator folding). On POSIX the two are
// identical (filepath.Rel already yields '/'); they diverge only on
// Windows, where relKey's ToSlash would key the lookup as
// "repo/a/b.go" while the cold walk stored "repo/a\b.go" — the miss
// leaves the stale nodes un-evicted and the re-parse leaks a duplicate
// set on every save (issue: slash-path duplicate indexing).
func (idx *Indexer) graphRelKey(absPath string) string {
	rel, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil {
		return pathkey.Normalize(absPath)
	}
	return pathkey.Normalize(rel)
}

// RelKey exposes relKey to in-package collaborators (the watcher) that
// hold an absolute filesystem path and need the canonical repo-relative
// graph key for it — e.g. to look up a file's nodes before and after a
// re-index. Going through one helper keeps the watcher's key in lockstep
// with the keys IndexFile / EvictFile write.
func (idx *Indexer) RelKey(absPath string) string { return idx.relKey(absPath) }

// SetRepoPrefix sets the repository prefix for multi-repo mode.
// When non-empty, all node IDs and file paths are prefixed with "<repoPrefix>/".
func (idx *Indexer) SetRepoPrefix(prefix string) { idx.repoPrefix = prefix }

// RepoPrefix returns the current repository prefix.
func (idx *Indexer) RepoPrefix() string { return idx.repoPrefix }

// SetWorkspaceID sets the workspace slug stamped onto nodes emitted
// by this indexer. Empty means "no workspace declared" — the
// applyRepoPrefix path will fall back to RepoPrefix so multi-repo
// configs without `.gortex.yaml::workspace:` keep working.
func (idx *Indexer) SetWorkspaceID(id string) { idx.workspaceID = id }

// WorkspaceID returns the workspace slug this indexer stamps on nodes.
func (idx *Indexer) WorkspaceID() string { return idx.workspaceID }

// SetProjectID sets the project slug stamped onto nodes emitted by
// this indexer. Single-project repos pass their repo name (the
// MultiIndexer default); monorepos compute a per-file slug from the
// `projects[]` mapping (follow-up work).
func (idx *Indexer) SetProjectID(id string) { idx.projectID = id }

// ProjectID returns the project slug this indexer stamps on nodes.
func (idx *Indexer) ProjectID() string { return idx.projectID }

// SetEmbedder sets the embedding provider for semantic search.
// When set, buildSearchIndex will create a HybridBackend with vector search.
func (idx *Indexer) SetEmbedder(p embedding.Provider) { idx.embedder = p }

// SetSkipVectorBuild toggles the embedding pass in buildSearchIndex.
// When true, buildSearchIndex builds only the text index — used by the
// daemon warmup path when a snapshot already carries the workspace
// vector index, so the graph is not needlessly re-embedded. When false
// (the default) an indexer with an embedder set always builds vectors.
func (idx *Indexer) SetSkipVectorBuild(skip bool) { idx.skipVectorBuild = skip }

// SetEmbeddingChunkOptions tunes the AST sub-chunking applied to large
// symbols before embedding (threshold and window line counts). The
// zero value leaves the chunker on its built-in defaults.
func (idx *Indexer) SetEmbeddingChunkOptions(opts embedding.ChunkOptions) {
	idx.embedChunkOpts = opts
}

// SetEmbeddingMaxSymbols overrides the cap on how many texts the vector
// index will hold before buildSearchIndex skips the embed pass. Zero
// keeps the built-in default.
func (idx *Indexer) SetEmbeddingMaxSymbols(n int) { idx.embedMaxSymbols = n }

// SetEmbeddingAPIConcurrency overrides how many embedding requests run
// in parallel against an API-backed embedder. Zero keeps the built-in
// default. Has no effect on in-process embedders.
func (idx *Indexer) SetEmbeddingAPIConcurrency(n int) { idx.embedAPIConcurrency = n }

// LastVectorBuildError returns why the most recent index build produced no
// vector index (chunk-embed failure, all vectors invalid, or the symbol-count
// guard), or nil when a vector index was built or the embedder was unset. Read
// it after an index build completes; it is not safe to call concurrently with
// one.
func (idx *Indexer) LastVectorBuildError() error { return idx.lastVectorBuildErr }

// SetSemanticManager sets the semantic enrichment manager.
// When set, the indexer runs semantic enrichment after resolution.
func (idx *Indexer) SetSemanticManager(m *semantic.Manager) { idx.semanticMgr = m }

// SemanticManager returns the semantic enrichment manager.
func (idx *Indexer) SemanticManager() *semantic.Manager { return idx.semanticMgr }

// SetResolverLSPHelper installs a resolve-time LSP helper on the
// underlying Resolver. The helper is consulted from inside
// resolveEdge for languages whose extensions the helper claims
// (TS/JS/JSX/TSX today via tsserver); see internal/resolver/
// lsp_helper.go for the contract.
//
// Pass nil to detach. Must be called before ResolveAll / ResolveFile;
// the resolver caches no LSP state across passes, so mid-pass swaps
// are racy and not supported.
func (idx *Indexer) SetResolverLSPHelper(h resolver.LSPHelper) {
	if idx.resolver != nil {
		idx.resolver.SetLSPHelper(h)
	}
	idx.resolverLSPHelper = h
}

// ResolverLSPHelper returns the currently installed resolver-time LSP
// helper, or nil. Exported so MultiIndexer can mirror the helper onto
// the global post-pass resolver in RunDeferredPassesAll.
func (idx *Indexer) ResolverLSPHelper() resolver.LSPHelper { return idx.resolverLSPHelper }

// ExportVectorIndex returns the serialized vector index bytes, dims, and count.
// Returns nil, 0, 0 if no vector index is active.
func (idx *Indexer) ExportVectorIndex() ([]byte, int, int) {
	hybrid, ok := idx.swappable().Inner().(*search.HybridBackend)
	if !ok {
		return nil, 0, 0
	}
	vec := hybrid.VectorIndex()
	if vec == nil || vec.Count() == 0 {
		return nil, 0, 0
	}

	var buf bytes.Buffer
	if err := vec.Save(&buf); err != nil {
		idx.logger.Warn("failed to export vector index", zap.Error(err))
		return nil, 0, 0
	}
	return buf.Bytes(), vec.Dims(), vec.Count()
}

// ImportVectorIndex restores a vector index from serialized data and wraps
// the current text search backend into a HybridBackend.
func (idx *Indexer) ImportVectorIndex(data []byte, dims, count int) error {
	if len(data) == 0 || idx.embedder == nil {
		return nil
	}

	// Validate dimensions match the current embedder to avoid mismatches
	// when switching providers (e.g., GloVe 50d → ONNX 384d).
	embedderDims := idx.embedder.Dimensions()
	if embedderDims > 0 && embedderDims != dims {
		idx.logger.Info("vector index dims mismatch, will re-embed",
			zap.Int("cached_dims", dims), zap.Int("embedder_dims", embedderDims))
		return nil // skip import, buildSearchIndex will re-embed
	}

	vec := search.NewVector(dims)
	if err := vec.LoadFrom(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("import vector index: %w", err)
	}
	vec.SetCount(count)

	sw := idx.swappable()
	sw.Swap(search.NewHybrid(sw.Inner(), vec, idx.embedder))
	idx.logger.Info("restored vector index from cache",
		zap.Int("vectors", count), zap.Int("dims", dims))
	return nil
}

// prefixPath prepends the repoPrefix to a relative path when in multi-repo mode.
// Returns the path unchanged when repoPrefix is empty.
func (idx *Indexer) prefixPath(relPath string) string {
	if idx.repoPrefix == "" {
		return relPath
	}
	return idx.repoPrefix + "/" + relPath
}

// graphFilePaths maps reindex file paths (absolute or root-relative, as
// passed to IndexFile) to the canonical graph file-path form
// (prefixPath(relKey)) that GetFileNodes is keyed by — so file-scoped
// passes can look up the just-reindexed files' nodes.
func (idx *Indexer) graphFilePaths(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		abs := f
		if !filepath.IsAbs(abs) && idx.rootPath != "" {
			abs = filepath.Join(idx.rootPath, f)
		}
		out = append(out, idx.prefixPath(idx.graphRelKey(abs)))
	}
	return out
}

// applyRepoPrefix transforms nodes and edges produced by an extractor to include
// the repo prefix in IDs and file paths. Sets Node.RepoPrefix on all nodes.
// This is a no-op when repoPrefix is empty (single-repo mode).
//
// Edge targets beginning with "unresolved::" are a sentinel meaning "the
// resolver will replace this with a real node ID after all files are
// indexed." Prefixing them turns "unresolved::fetchUsers" into
// "web/unresolved::fetchUsers", which the resolver's HasPrefix check on
// "unresolved::" misses — leaving every call edge permanently unresolved
// in multi-repo mode and breaking get_callers / get_call_chain across all
// languages. Skip prefixing on unresolved targets; the resolver will land
// the edge on a real ID that already carries its own correct prefix
// (possibly cross-repo, which the resolver marks explicitly).

// todoTags returns the configured TODO marker set or the default
// (TODO/FIXME/HACK/XXX/NOTE). Reads from the IndexConfig the indexer
// already holds — IsCoverageEnabled gating happens at the call site.
func (idx *Indexer) todoTags() []string {
	if tags := idx.config.Coverage.Todos.Tags; len(tags) > 0 {
		return tags
	}
	return []string{"TODO", "FIXME", "HACK", "XXX", "NOTE"}
}

// todoMaxText returns the configured cap on stored TODO text or the
// 200-char default.
func (idx *Indexer) todoMaxText() int {
	if n := idx.config.Coverage.Todos.MaxText; n > 0 {
		return n
	}
	return 200
}

// loadCodeownersRules lazily parses the repo's CODEOWNERS file. The
// sync.Once guarantees one parse per indexer; applyCoverageDomains
// is then a pure rule-match per file. Errors silently produce an
// empty rule set — the ownership domain is implicitly gated on
// file presence rather than failing extraction when the file is
// missing or malformed.
func (idx *Indexer) loadCodeownersRules() []codeowners.Rule {
	idx.codeownersOnce.Do(func() {
		rules, _, ok := codeowners.LoadFromRepo(idx.rootPath)
		if !ok {
			return
		}
		idx.codeownersRules = rules
	})
	return idx.codeownersRules
}

// applyCoverageDomains runs the per-file coverage extractors
// (todos, licenses, ownership) and applies the post-extraction
// strip pass for domains the language extractor always emits but
// the user has gated off (function_shape). Appended/stripped
// nodes/edges flow through the same applyRepoPrefix / graph.AddNode
// pipeline as the language extractor's output. Called from both
// the bulk index worker pool (IndexCtx) and the incremental
// indexFile path.
//
// relPath is the unprefixed file path; lang is the detected
// language; src is the file bytes.
func (idx *Indexer) applyCoverageDomains(relPath, lang string, src []byte, result *parser.ExtractionResult) {
	if idx.config.Coverage.IsEnabled("todos") {
		findings := todos.Scan(src, idx.todoTags(), idx.todoMaxText())
		todoNodes, todoEdges := todos.BuildGraphArtifacts(relPath, findings, lang)
		result.Nodes = append(result.Nodes, todoNodes...)
		result.Edges = append(result.Edges, todoEdges...)
	}
	if idx.config.Coverage.IsEnabled("licenses") {
		if spdx := licenses.Scan(src); spdx != "" {
			licNodes, licEdges := licenses.BuildGraphArtifacts(relPath, spdx, lang)
			result.Nodes = append(result.Nodes, licNodes...)
			result.Edges = append(result.Edges, licEdges...)
		}
	}
	if idx.config.Coverage.IsEnabled("ownership") {
		if rules := idx.loadCodeownersRules(); len(rules) > 0 {
			if owners := codeowners.MatchFile(relPath, rules); len(owners) > 0 {
				teamNodes, teamEdges := codeowners.BuildGraphArtifacts(relPath, owners, lang)
				result.Nodes = append(result.Nodes, teamNodes...)
				result.Edges = append(result.Edges, teamEdges...)
			}
		}
	}
	if idx.config.Coverage.IsEnabled("codegen") {
		if marker := codegen.Scan(src); marker.Generated {
			// Stamp the marker on the file node when the language
			// extractor produced one. Generated files without a
			// file-shaped result node still get the EdgeGeneratedBy
			// edge so downstream walks pick them up.
			for _, n := range result.Nodes {
				if n.Kind == graph.KindFile && n.FilePath == relPath {
					if n.Meta == nil {
						n.Meta = map[string]any{}
					}
					codegen.MarkFileNode(n.Meta, marker)
					break
				}
			}
			result.Edges = append(result.Edges, codegen.BuildGraphArtifacts(relPath, marker)...)
		}
		// Annotation-driven codegen: Lombok / MapStruct / Kotlin
		// compiler plugins generate members that never appear in
		// source. Flag the annotated symbols so they stay visible.
		if extra, st := codegen.MarkAnnotatedGenerated(result.Nodes, result.Edges); st.NodesMarked > 0 {
			result.Edges = append(result.Edges, extra...)
			// Materialize the actual Lombok accessor members (getX/setX/
			// builder/log) so `obj.getName()` resolves to a real graph node.
			if lnodes, ledges := codegen.MaterializeLombokAccessors(result.Nodes); len(lnodes) > 0 {
				result.Nodes = append(result.Nodes, lnodes...)
				result.Edges = append(result.Edges, ledges...)
			}
		}
	}
	// Framework entry points (Alembic migrations / Next.js pages /
	// ASP.NET host files): symbols reachable only from a runtime.
	// Stamped so dead-code analysis treats them as live roots.
	entrypoints.Detect(relPath, lang, result.Nodes, result.Edges)
	if !idx.config.Coverage.IsEnabled("function_shape") {
		stripFunctionShape(result)
	}
	if !idx.config.Coverage.IsEnabled("type_shape") {
		stripTypeShape(result)
	}
	if !idx.config.Coverage.IsEnabled("constants") {
		revertConstantsToVariables(result)
	}
	if !idx.config.Coverage.IsEnabled("concurrency") {
		stripConcurrencyEdges(result)
	}
	if idx.config.Coverage.IsEnabled("fixtures") {
		applyFixtureClassification(relPath, lang, result)
	}
	if !idx.config.Coverage.IsEnabled("observability") {
		stripObservabilityArtifacts(result)
	}
	if !idx.config.Coverage.IsEnabled("pubsub") {
		stripPubsubArtifacts(result)
	}
	if !idx.config.Coverage.IsEnabled("flags") {
		stripFlagArtifacts(result)
	}
	if !idx.config.Coverage.IsEnabled("configs") {
		stripConfigArtifacts(result)
	}
	// SQL migration / DDL extraction is high-signal — CREATE TABLE in a
	// dedicated migration file or a `gortex db schema` dump is
	// unambiguous, unlike the noisy code-side string-literal SQL the
	// `sql` domain gates. Run it always; the strip below removes only the
	// gated code-side SQL, preserving migration-origin schema nodes.
	applyMigrationExtraction(relPath, src, result)
	if !idx.config.Coverage.IsEnabled("sql") {
		stripSQLArtifacts(result)
	}
	if idx.config.Coverage.IsEnabled("clones") {
		applyCloneSignatures(src, result)
	}
}

// applyMigrationExtraction parses CREATE TABLE DDL out of SQL migration
// files (path under migrate/ or migrations/) and `gortex db schema` dumps
// (recognised by their generated-header marker, wherever they're saved).
// Each declared table becomes a KindTable node with its columns as
// KindColumn nodes (EdgeMemberOf column → table), alongside the synthetic
// KindMigration node for the file, with EdgeProvides edges from migration
// → table so reverse-walk queries answer "which migrations create this
// table". A generated dump carries the real dialect in its header;
// hand-written migrations stay "generic" since the .sql file alone doesn't
// tell us which dialect the application targets. All emitted nodes carry
// Meta["origin"]="migration" so they survive the sql-domain strip — this
// schema DDL is unambiguous and high-signal, unlike the code-side
// string-literal SQL the `sql` coverage gate exists to suppress.
func applyMigrationExtraction(relPath string, src []byte, result *parser.ExtractionResult) {
	generated := gortexsql.IsGeneratedSchema(src)
	if !gortexsql.IsMigrationPath(relPath) && !generated {
		return
	}
	tables := gortexsql.ExtractCreateTablesWithColumns(string(src))
	if len(tables) == 0 {
		return
	}
	dialect := "generic"
	if generated {
		if d := gortexsql.GeneratedSchemaDialect(src); d != "" {
			dialect = d
		}
	}
	migrationID := gortexsql.MigrationNodeID(relPath)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID:       migrationID,
		Kind:     graph.KindMigration,
		Name:     filepath.Base(relPath),
		FilePath: relPath,
		Language: "sql",
		Meta: map[string]any{
			"dialect": dialect,
			"origin":  "migration",
		},
	})
	for _, t := range tables {
		tableID := gortexsql.TableNodeID(dialect, t.Schema, t.Table)
		meta := map[string]any{
			"table":   t.Table,
			"dialect": dialect,
			"origin":  "migration",
		}
		if t.Schema != "" {
			meta["schema"] = t.Schema
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:       tableID,
			Kind:     graph.KindTable,
			Name:     t.Table,
			FilePath: relPath,
			Language: "sql",
			Meta:     meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     migrationID,
			To:       tableID,
			Kind:     graph.EdgeProvides,
			FilePath: relPath,
			Origin:   graph.OriginASTResolved,
		})
		for _, c := range t.Columns {
			colID := gortexsql.ColumnNodeID(dialect, t.Schema, t.Table, c.Name)
			cmeta := map[string]any{
				"table":   t.Table,
				"dialect": dialect,
				"origin":  "migration",
			}
			if t.Schema != "" {
				cmeta["schema"] = t.Schema
			}
			if c.Type != "" {
				cmeta["type"] = c.Type
			}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       colID,
				Kind:     graph.KindColumn,
				Name:     c.Name,
				FilePath: relPath,
				Language: "sql",
				Meta:     cmeta,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From:     colID,
				To:       tableID,
				Kind:     graph.EdgeMemberOf,
				FilePath: relPath,
				Origin:   graph.OriginASTResolved,
			})
		}
	}
}

// stripSQLArtifacts drops KindTable + KindMigration nodes plus
// the matching EdgeQueries / EdgeProvides edges when the sql
// coverage domain is gated off. Mirrors the strip passes for
// flags / configs / observability — endpoint-aware so any
// leftover edges to stripped nodes are pruned. SQL extraction
// defaults off because string-literal pattern matching against
// db.Get / db.Query / db.Exec produces false positives when
// domain code shares method names (cache.Get, etc.).
func stripSQLArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if (n.Kind == graph.KindTable || n.Kind == graph.KindMigration) && !isMigrationOriginNode(n) {
			stripped[n.ID] = struct{}{}
			continue
		}
		// SQL-context KindString registry nodes are the short-circuit
		// input for the SQL extractor; gated off alongside the rest
		// of the SQL domain so a disabled gate leaves no SQL-related
		// residue in the graph.
		if n.Kind == graph.KindString {
			if ctx, _ := n.Meta["context"].(string); ctx == "sql" {
				stripped[n.ID] = struct{}{}
				continue
			}
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeQueries {
			continue
		}
		if _, ok := stripped[e.From]; ok {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// stripConfigArtifacts drops KindConfigKey nodes plus
// EdgeReadsConfig / EdgeWritesConfig edges when the configs
// coverage domain is gated off. Endpoint-aware so any leftover
// edges to stripped key nodes are pruned.
//
// Infrastructure-origin config keys (Meta["origin"] in {"k8s",
// "dockerfile"}) are preserved because they are emitted by the K8s
// manifest, Kustomize, and Dockerfile extractors, which have no
// dedicated coverage flag and always run. Stripping them would
// also strip the EdgeUsesEnv edges those extractors produce (which
// target the same node IDs), defeating the cross-ref between
// container env declarations and code-side `os.Getenv` reads.
func stripConfigArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if n.Kind == graph.KindConfigKey && !isInfraOriginConfigKey(n) && !isDocFrontmatterConfigKey(n) {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeReadsConfig || e.Kind == graph.EdgeWritesConfig {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// isInfraOriginConfigKey reports whether a KindConfigKey node was
// emitted by the K8s / Kustomize / Dockerfile extractors. These
// nodes carry Meta["origin"] = "k8s" or "dockerfile" by convention.
// The code-side extractors (Go os.Getenv, Python os.environ, viper,
// struct-tag, …) leave Meta["origin"] empty.
// isMigrationOriginNode reports whether a node was emitted by migration /
// live-DB DDL extraction (Meta["origin"]=="migration"). These schema nodes
// are high-signal and survive the sql-domain strip — the gate only removes
// the noisy code-side string-literal SQL.
func isMigrationOriginNode(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	o, _ := n.Meta["origin"].(string)
	return o == "migration"
}

func isInfraOriginConfigKey(n *graph.Node) bool {
	if n == nil || n.Kind != graph.KindConfigKey || n.Meta == nil {
		return false
	}
	origin, _ := n.Meta["origin"].(string)
	return origin == "k8s" || origin == "dockerfile"
}

// isDocFrontmatterConfigKey reports whether a KindConfigKey node is a
// document's frontmatter metadata (Quarto .qmd, …) rather than code /
// infra configuration. These keys ride with the document / prose ingest,
// which is independent of the `configs` coverage domain, so they survive
// the strip the same way infra-origin keys do — keeping a .qmd's declared
// title / format / params searchable by default. Add new frontmatter
// sources to the switch as prose extractors gain frontmatter support.
func isDocFrontmatterConfigKey(n *graph.Node) bool {
	if n == nil || n.Kind != graph.KindConfigKey || n.Meta == nil {
		return false
	}
	switch src, _ := n.Meta["source"].(string); src {
	case "quarto_frontmatter":
		return true
	}
	return false
}

// stripFlagArtifacts drops KindFlag nodes and EdgeTogglesFlag
// edges when the flags coverage domain is gated off. Mirrors the
// observability strip — endpoint-aware so any leftover edges that
// pointed to a removed flag node are also dropped.
func stripFlagArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFlag {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeTogglesFlag {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// stripObservabilityArtifacts drops the log/metric/trace KindEvent
// nodes and their EdgeEmits edges when the observability coverage
// domain is gated off. Used for the same reason as the function-shape
// and type-shape strips: the language extractor always emits, and the
// indexer prunes per-file before applyRepoPrefix so the gate stays a
// pure-config dial without parser plumbing.
//
// Pub/sub KindEvent nodes (Meta["event_kind"]="pubsub") are a
// separately-gated domain — they share the KindEvent kind and the
// EdgeEmits edge (publish side) but belong to the `pubsub` coverage
// domain, so this pass leaves them and any EdgeEmits/EdgeListensOn
// edge targeting them untouched. stripPubsubArtifacts owns those.
func stripObservabilityArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	pubsubNodes := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if isPubsubEventNode(n) {
			pubsubNodes[n.ID] = struct{}{}
			keptNodes = append(keptNodes, n)
			continue
		}
		if n.Kind == graph.KindEvent {
			stripped[n.ID] = struct{}{}
			continue
		}
		// log_message-context KindString registry nodes are the
		// string-side shadow of log KindEvent emissions. They gate
		// alongside the rest of the observability domain so a
		// disabled gate leaves no log residue in the graph.
		if n.Kind == graph.KindString {
			if ctx, _ := n.Meta["context"].(string); ctx == "log_message" {
				stripped[n.ID] = struct{}{}
				continue
			}
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if _, ok := stripped[e.To]; ok {
			continue
		}
		// The observability gate strips every EdgeEmits — the publish
		// side of the log/metric/trace layer. The one exception is an
		// EdgeEmits whose target is a pub/sub topic node: that's the
		// publish side of the separately-gated pubsub domain, so it
		// survives here and is owned by stripPubsubArtifacts.
		if e.Kind == graph.EdgeEmits {
			if _, ok := pubsubNodes[e.To]; !ok {
				continue
			}
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// stripPubsubArtifacts drops the pub/sub KindEvent topic nodes
// (Meta["event_kind"]="pubsub"), every EdgeListensOn edge (a
// pubsub-only edge kind), and any EdgeEmits edge whose target is a
// pub/sub topic node, when the pubsub coverage domain is gated off.
// Endpoint-aware so a publish edge into a stripped topic node doesn't
// dangle. Mirrors stripObservabilityArtifacts — the two domains share
// KindEvent + EdgeEmits but are toggled independently.
func stripPubsubArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if isPubsubEventNode(n) {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeListensOn {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// isPubsubEventNode reports whether a node is a pub/sub topic node — a
// KindEvent carrying Meta["event_kind"]="pubsub". Distinguishes the
// pub/sub domain from observability (log/metric/trace) events, which
// share KindEvent but are gated separately.
func isPubsubEventNode(n *graph.Node) bool {
	if n == nil || n.Kind != graph.KindEvent || n.Meta == nil {
		return false
	}
	kind, _ := n.Meta["event_kind"].(string)
	return kind == "pubsub"
}

// applyFixtureClassification reclassifies the language extractor's
// emitted file node from KindFile to KindFixture when the file
// lives under a testdata/ directory. When the language extractor
// produced no file node (file types without a registered
// extractor), a standalone KindFixture node is emitted instead.
//
// Reference edges from test functions to fixtures are out of scope
// for v1 — agents can already filter by kind to enumerate fixtures.
func applyFixtureClassification(relPath, lang string, result *parser.ExtractionResult) {
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFile && n.FilePath == relPath {
			if fixtures.ReclassifyFileToFixture(n) {
				return
			}
			break
		}
	}
	result.Nodes = append(result.Nodes, fixtures.BuildGraphArtifacts(relPath, lang)...)
}

// stripConcurrencyEdges removes the EdgeSpawns / EdgeSends /
// EdgeRecvs edges introduced by the concurrency coverage domain.
// EdgeCalls is left in place — spawns are emitted in addition to
// the corresponding call edge, so dropping just the spawn edge
// reverts to the pre-coverage call graph without losing reachability.
func stripConcurrencyEdges(result *parser.ExtractionResult) {
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		switch e.Kind {
		case graph.EdgeSpawns, graph.EdgeSends, graph.EdgeRecvs:
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// revertConstantsToVariables re-classifies KindConstant /
// KindEnumMember nodes back to KindVariable when the constants
// coverage domain is gated off. Unlike stripFunctionShape /
// stripTypeShape this is a re-classification, not a removal —
// users who disable the domain still want their `const` and `iota`
// declarations in the graph, just under the original kind that
// pre-coverage code expected.
func revertConstantsToVariables(result *parser.ExtractionResult) {
	for _, n := range result.Nodes {
		switch n.Kind {
		case graph.KindConstant, graph.KindEnumMember:
			n.Kind = graph.KindVariable
		}
	}
}

// stripTypeShape removes the alias / composition edges introduced
// by the type_shape coverage domain (EdgeAliases, EdgeComposes).
// EdgeExtends is left in place — it's an existing edge kind whose
// newtype-derived emissions fall under the spec's "EdgeExtends
// continues to mean newtype / inheritance / interface extension"
// guarantee, not a new domain signal.
func stripTypeShape(result *parser.ExtractionResult) {
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		switch e.Kind {
		case graph.EdgeAliases, graph.EdgeComposes:
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// stripFunctionShape removes the param/closure/generic_param nodes
// and their associated edges from a per-file extraction result.
// Used when the function_shape coverage domain is gated off — the
// language extractor always emits these for resolution-internal
// reasons, and we drop them after the extractor returns rather than
// wire a config dependency through the parser layer.
func stripFunctionShape(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if isFunctionShapeNode(n.Kind) {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if isFunctionShapeEdge(e.Kind) {
			continue
		}
		if _, ok := stripped[e.From]; ok {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

func isFunctionShapeNode(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindParam, graph.KindClosure, graph.KindGenericParam:
		return true
	}
	return false
}

func isFunctionShapeEdge(kind graph.EdgeKind) bool {
	switch kind {
	case graph.EdgeParamOf, graph.EdgeReturns, graph.EdgeTypedAs, graph.EdgeCaptures:
		return true
	}
	return false
}

func (idx *Indexer) applyRepoPrefix(nodes []*graph.Node, edges []*graph.Edge) {
	// Stamp WorkspaceID / ProjectID on every node emitted by this
	// indexer regardless of mode — single-repo and multi-repo both
	// need the boundary slugs for query scoping and contract
	// matching. Single-repo callers can leave them empty; the
	// MultiIndexer path always sets them via SetWorkspaceID /
	// SetProjectID before calling Index.
	if idx.workspaceID != "" || idx.projectID != "" {
		for _, n := range nodes {
			if idx.workspaceID != "" && n.WorkspaceID == "" {
				n.WorkspaceID = idx.workspaceID
			}
			if idx.projectID != "" && n.ProjectID == "" {
				n.ProjectID = idx.projectID
			}
		}
	}
	if idx.repoPrefix == "" {
		return
	}
	prefix := idx.repoPrefix + "/"
	const unresolvedMarker = "unresolved::"
	// Intern every minted string. A node ID is referenced once on the
	// node and again on every edge endpoint that points at it; a file
	// path recurs on every node and edge in that file. Without
	// interning each reference is a distinct `prefix + s` allocation —
	// interning collapses them to one shared backing array, and edge
	// endpoints end up sharing storage with the node ID they name.
	//
	// The file path is identical for every node and edge extracted from
	// the same file — thousands of them. Concatenating `prefix + path`
	// per reference would mint thousands of throwaway strings before
	// interning collapses their storage. This per-call cache computes
	// the interned prefixed path once per distinct raw path, so a file
	// with N symbols pays one concatenation instead of N.
	prefixedPath := make(map[string]string)
	internFilePath := func(raw string) string {
		if c, ok := prefixedPath[raw]; ok {
			return c
		}
		c := intern.String(prefix + raw)
		prefixedPath[raw] = c
		return c
	}
	for _, n := range nodes {
		n.ID = intern.String(prefix + n.ID)
		n.FilePath = internFilePath(n.FilePath)
		n.RepoPrefix = idx.repoPrefix
		// Name, Language and Kind are low-cardinality and recur across
		// thousands of nodes — method/function names like String, New,
		// Get…, the ~20 distinct languages, and the fixed set of node
		// kinds. Interning collapses each to a single backing array; it
		// also shrinks the byName secondary index, whose keys are these
		// same strings.
		n.Name = intern.String(n.Name)
		n.Language = intern.String(n.Language)
		n.Kind = graph.NodeKind(intern.String(string(n.Kind)))
	}
	for _, e := range edges {
		e.From = intern.String(prefix + e.From)
		if strings.HasPrefix(e.To, unresolvedMarker) {
			// Unresolved targets carry no prefix, but many edges name
			// the same unresolved symbol — still worth interning.
			e.To = intern.String(e.To)
		} else {
			e.To = intern.String(prefix + e.To)
		}
		e.FilePath = internFilePath(e.FilePath)
	}
}

// Index walks root and populates the graph using a concurrent worker pool.
//
// This is the backwards-compatible entry point; it delegates to IndexCtx with
// a background context. Callers wanting progress notifications or cancellation
// should use IndexCtx directly.
func (idx *Indexer) Index(root string) (*IndexResult, error) {
	return idx.IndexCtx(context.Background(), root)
}

// IndexCtx is Index with a context, enabling progress reporting. The reporter
// is pulled from ctx via progress.FromContext — attach one with
// progress.WithReporter to receive stage updates. If no reporter is attached,
// stage calls are silently dropped.
// clampParseWeight maps a file size to a parse-admission weight bounded to
// [1, budget]. A file larger than the whole budget is admitted alone
// (weight == budget) so the bytes-in-flight semaphore can never deadlock.
func clampParseWeight(size, budget int64) int64 {
	if size < 1 {
		return 1
	}
	if size > budget {
		return budget
	}
	return size
}

func (idx *Indexer) IndexCtx(ctx context.Context, root string) (result *IndexResult, retErr error) {
	start := time.Now()
	reporter := progress.FromContext(ctx)

	// Cold/full-index GC tuning: raise the GC percent window and install a
	// cgroup-aware soft memory limit for the duration of the index, then
	// restore the prior settings on every exit path. The knobs affect only GC
	// timing and peak RSS during the allocation burst of a full index — the
	// graph content is identical with them on or off. Disable for A/B runs
	// with GORTEX_INDEX_GC_TUNE=0; see gc_tune.go.
	restoreGCTuning := applyIndexGCTuning(idx.logger)
	defer restoreGCTuning()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.rootPath = absRoot
	idx.projectName = search.DetectProjectName(absRoot)

	reporter.Report("walking files", 0, 0)

	// Collect files. Files over IndexConfig.MaxFileSize are skipped
	// during the walk — they're nearly always generated/minified code
	// that dominates parse time without contributing useful signal.
	// A single summary warning reports how many were skipped so the
	// user knows when the cap is biting.
	//
	// Each surviving file is captured with its walk-time ModTime so
	// the worker (contract-cache mtime stamp) and the post-parse
	// fileMtimes loop don't have to os.Stat again. d.Info() is one
	// syscall per file regardless; trading one walk-time stat for two
	// later stats is the net win.
	maxSize := idx.config.MaxFileSize
	// Corpus-admission gate: drops oversized document assets and (by
	// default) binary/vector data artifacts at the walk, before they are
	// read and extracted, so a content-heavy repo can't pull gigabytes of
	// non-source files into the parse pipeline and OOM (#120). Inert for
	// all-code repos.
	contentGate := idx.newContentAdmissionGate()
	// Git-aware admission (opt-in): when index.skip_untracked_assets is on,
	// drop asset-class files git does not track — uncommitted RAG corpora /
	// datasets / build outputs that .gitignore can't catch (#120). Inert
	// when off, on a non-git repo, or when git is unavailable.
	untrackedGate := idx.newUntrackedAssetGate(ctx, absRoot)
	var files []walkedFile
	var skippedLarge int
	var skippedBytes int64
	var skippedBySize []skippedFile
	var skippedByContent []skippedFile
	var skippedContentBytes int64
	var parseFailedFiles []skippedFile
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldPruneDir(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		lang, ok := idx.effectiveLanguage(path, nil)
		if !ok {
			return nil
		}
		if idx.shouldExclude(path, absRoot, false) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			// Couldn't read FileInfo (race with deletion, broken
			// symlink, …). Skip — the worker would fail too.
			return nil
		}
		if maxSize > 0 && info.Size() > maxSize {
			skippedLarge++
			skippedBytes += info.Size()
			rel, _ := filepath.Rel(absRoot, path)
			skippedBySize = append(skippedBySize, skippedFile{
				relPath: pathkey.Normalize(rel), lang: lang, size: info.Size(),
			})
			return nil
		}
		if reason, skip := untrackedGate.skip(lang, path); skip {
			skippedContentBytes += info.Size()
			rel, _ := filepath.Rel(absRoot, path)
			skippedByContent = append(skippedByContent, skippedFile{
				relPath: pathkey.Normalize(rel), lang: lang, size: info.Size(), reason: reason,
			})
			return nil
		}
		if reason, skip := contentGate.skip(lang, info.Size()); skip {
			skippedContentBytes += info.Size()
			rel, _ := filepath.Rel(absRoot, path)
			skippedByContent = append(skippedByContent, skippedFile{
				relPath: pathkey.Normalize(rel), lang: lang, size: info.Size(), reason: reason,
			})
			return nil
		}
		files = append(files, walkedFile{
			path:      path,
			lang:      lang,
			size:      info.Size(),
			mtimeNano: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if skippedLarge > 0 {
		idx.logger.Info("indexer: skipped large files over MaxFileSize",
			zap.Int("count", skippedLarge),
			zap.Int64("total_bytes", skippedBytes),
			zap.Int64("limit_bytes", maxSize))
	}
	if len(skippedByContent) > 0 {
		idx.logger.Info("indexer: skipped content/data assets by admission policy",
			zap.Int("count", len(skippedByContent)),
			zap.Int64("total_bytes", skippedContentBytes))
	}
	reporter.Report("walking files", len(files), len(files))

	// In-memory shadow for cold-start indexing on disk-backed stores.
	// Disk backends pay ms-level per-call cost on every read; running
	// the resolver against the disk store turns its ~100k+ point
	// lookups into many minutes of wall time. Instead, swap idx.graph
	// to an in-memory *Graph for the whole IndexCtx pipeline — parse,
	// resolve, all subpasses, every per-edge MERGE/MATCH stays in
	// memory at nanosecond latency. At the end, dump the final state
	// to the disk backend via one BulkLoad cycle, so the disk has the
	// post-resolve graph and the bench's query workload runs against
	// the persisted state.
	//
	// Guards:
	//   - Backend must implement graph.BulkLoader (the on-disk backend opts in).
	//   - Store must be empty (NodeCount == 0 && EdgeCount == 0). The
	//     final dump is BulkLoad's INSERT-only fast path — running it
	//     against a non-empty store would corrupt or duplicate.
	//     Incremental / re-index flows fall through to the per-call
	//     AddBatch path against the disk store directly.
	//   - File count is below the shadow-max threshold (see
	//     shadowMaxFileCount). Above the threshold the shadow's RAM
	//     footprint would exceed available memory — Linux / Firefox
	//     at full scale (~10M+ edges) would push the shadow past
	//     20GB. Override with GORTEX_SHADOW_MAX_FILES.
	//   - The swap happens before the parse worker pool starts and is
	//     committed before IndexCtx returns. retErr from the named
	//     return suppresses the commit when the pipeline errored —
	//     the disk store stays empty rather than capturing partial
	//     state.
	var diskTarget graph.Store
	var inMemShadow *graph.Graph
	bl, blOK := idx.graph.(graph.BulkLoader)
	// Per-Indexer sentinel: each *Indexer is constructed fresh
	// (per-repo in MultiIndexer, once in single-repo daemons), so
	// "this Indexer has indexed before" is the right question to
	// gate the shadow-swap on. The legacy gate looked at the
	// disk store's NodeCount, but in MultiIndexer the disk store
	// holds data from sibling repos that already drained — the
	// gate would mis-fire and force the big repo onto the per-row
	// path. With per-repo-prefixed stub IDs (internal/graph/stub.go)
	// concurrent shadow drains no longer conflict on PRIMARY KEY,
	// so disk-non-empty is safe.
	firstIndex := idx.indexCount.Load() == 0
	belowShadowMax := len(files) <= shadowMaxFileCount()
	// The file-count ceiling is blind to the few-huge-files shape: a
	// content repo of a few hundred PDFs / text dumps / spreadsheets is
	// far under shadowMaxFileCount yet holds multiple GB that explode
	// into hundreds of thousands of section nodes. Gate the in-memory
	// shadow on total input bytes too, so such a repo falls through to
	// the bounded per-call disk path instead of pinning the whole
	// post-parse graph in RAM and OOMing (see #120).
	var totalFileBytes int64
	for i := range files {
		totalFileBytes += files[i].size
	}
	maxShadowBytes := shadowMaxBytes()
	belowShadowBytes := totalFileBytes <= maxShadowBytes
	shadowWeight := shadowAdmissionWeight(len(files), totalFileBytes)
	shadowLocallyEligible := blOK && firstIndex && belowShadowMax && belowShadowBytes
	admission := idx.shadowAdmission
	if admission == nil {
		admission = processShadowAdmission
	}
	var shadowLease *shadowAdmissionLease
	shadowBudgetGranted := false
	if shadowLocallyEligible {
		shadowLease, shadowBudgetGranted = admission.tryAcquire(shadowWeight)
	}
	shadowTaken := shadowLocallyEligible && shadowBudgetGranted
	if shadowLease != nil {
		// Registered before the shadow-drain defer below. Defers execute in
		// reverse order, so the lease remains charged through every success,
		// error, and cancellation drain/restore path and releases afterwards.
		defer shadowLease.Release()
	}
	shadowBudgetCapacity, shadowBudgetUsed, shadowBudgetPeak := admission.snapshot()
	preNodes := idx.graph.NodeCount()
	preEdges := idx.graph.EdgeCount()
	idx.logger.Info("indexer: shadow-swap decision",
		zap.String("repo", idx.RepoPrefix()),
		zap.Bool("bulk_loader", blOK),
		zap.Bool("first_index", firstIndex),
		zap.Int("pre_nodes", preNodes),
		zap.Int("pre_edges", preEdges),
		zap.Int("files", len(files)),
		zap.Int("shadow_max_files", shadowMaxFileCount()),
		zap.Bool("below_shadow_max", belowShadowMax),
		zap.Int64("total_file_bytes", totalFileBytes),
		zap.Int64("shadow_max_bytes", maxShadowBytes),
		zap.Bool("below_shadow_bytes", belowShadowBytes),
		zap.Int64("shadow_weight_bytes", shadowWeight),
		zap.Int64("shadow_process_budget_bytes", shadowBudgetCapacity),
		zap.Int64("shadow_process_used_bytes", shadowBudgetUsed),
		zap.Int64("shadow_process_peak_bytes", shadowBudgetPeak),
		zap.Bool("shadow_budget_granted", shadowBudgetGranted),
		zap.Bool("shadow_taken", shadowTaken),
	)
	if shadowTaken {
		// Warm-restart safety. `firstIndex` is a PER-INDEXER sentinel, and
		// a fresh per-repo Indexer is constructed on every daemon restart,
		// so firstIndex is true on every restart — even when the
		// persistent disk store already holds this repo's nodes from a
		// prior run. The shadow drain below ends in BulkLoad's INSERT-only
		// COPY, which (per this function's own contract) "running against a
		// non-empty store would corrupt or duplicate". A duplicate-primary-
		// key bulk load against the persisted rows would fail warmup, and
		// because the repo's mtimes never get persisted when warmup dies
		// first, the failure re-fires on the next restart: a crash loop.
		// Evicting the repo's existing rows first makes the bulk load land
		// on a clean slate. EvictRepo self-guards with a count query, so this is a
		// cheap no-op for the genuine first-index cases (true cold start,
		// a newly-tracked repo) where the disk store has no rows for this
		// prefix. preNodes>0 short-circuits the call entirely on the
		// first repo of a cold start (empty store).
		if preNodes > 0 {
			if n, e := idx.graph.EvictRepo(idx.RepoPrefix()); n > 0 || e > 0 {
				idx.logger.Info("indexer: evicted stale repo rows before bulk reload (warm restart)",
					zap.String("repo", idx.RepoPrefix()),
					zap.Int("nodes", n), zap.Int("edges", e))
			}
		}
		idx.indexCount.Add(1)
		diskTarget = idx.graph
		inMemShadow = graph.New()
		idx.graph = inMemShadow
		// Capture the disk store as the vector sink: buildSearchIndex runs
		// while idx.graph is the shadow (no VectorSearcher), so without this
		// the embedded vectors never land on disk. The `vectors` table has no
		// FK to `nodes`, so upserting before FlushBulk persists the nodes is
		// safe. Cleared when idx.graph is restored below.
		idx.bulkVectorSink, _ = diskTarget.(graph.VectorSearcher)
		// Same capture for the content index: the per-file content stream
		// must reach content_fts on disk while idx.graph is the shadow.
		idx.contentSink, _ = diskTarget.(graph.ContentSearcher)
		// The resolver was constructed at indexer.New with the disk
		// Store. Redirect it at the shadow too, otherwise ResolveAll
		// reads from the empty disk Store, finds no pending edges,
		// and short-circuits — silently disabling every resolver pass
		// (module attribution, relative imports, edge in-place
		// resolution, …) for any backend that takes the shadow path.
		if idx.resolver != nil {
			idx.resolver.SetGraph(inMemShadow)
		}
		defer func() {
			if retErr != nil {
				idx.graph = diskTarget
				idx.bulkVectorSink = nil
				idx.contentSink = nil
				if idx.resolver != nil {
					idx.resolver.SetGraph(diskTarget)
				}
				return
			}
			reporter.Report("persisting bulk graph", 0, 0)
			drainStart := time.Now()
			shadowNodeCount := inMemShadow.NodeCount()
			shadowEdgeCount := inMemShadow.EdgeCount()
			idx.logger.Info("indexer: drain start (shadow → disk)",
				zap.String("repo", idx.RepoPrefix()),
				zap.Int("shadow_nodes", shadowNodeCount),
				zap.Int("shadow_edges", shadowEdgeCount),
			)
			bl.BeginBulkLoad()
			// Transfer the shadow in explicit row/byte-capped batches. The
			// process-wide shadow admission caps all live shadows at 1 GiB; these
			// much smaller buffers prevent persistence from double-buffering an
			// admitted repository while SQLite consumes it. Each destructive
			// batch is locally key-ordered and released before the next batch.
			const (
				persistChunkRows     = 8192
				persistChunkBytes    = 16 << 20
				persistFTSChunkRows  = 2048
				persistFTSChunkBytes = 4 << 20
			)
			searcher, hasFTS := diskTarget.(graph.SymbolSearcher)
			ftsBatcher, hasFTSBatcher := diskTarget.(graph.SymbolFTSBatchUpserter)
			ftsResetter, hasFTSResetter := diskTarget.(graph.SymbolFTSRepoResetter)
			ftsReady := hasFTS && hasFTSBatcher && hasFTSResetter
			ftsItemCount := 0
			if hasFTS {
				reporter.Report("building symbol fts", 0, 0)
				switch {
				case !ftsReady:
					retErr = fmt.Errorf("indexer: symbol FTS backend lacks bounded reset/upsert capabilities")
				case retErr == nil:
					if err := ftsResetter.ResetSymbolFTS(idx.RepoPrefix()); err != nil {
						retErr = fmt.Errorf("indexer: reset symbol FTS: %w", err)
						ftsReady = false
					}
				}
			}

			for nodes := range inMemShadow.DrainNodeBatches(persistChunkRows, persistChunkBytes) {
				diskTarget.AddBatch(nodes, nil)
				if !ftsReady || retErr != nil {
					continue
				}
				ftsItems := make([]graph.SymbolFTSItem, 0, min(len(nodes), persistFTSChunkRows))
				var ftsBytes uint64
				flushFTS := func() bool {
					if len(ftsItems) == 0 {
						return true
					}
					if err := ftsBatcher.BatchUpsertSymbolFTS(ftsItems); err != nil {
						retErr = fmt.Errorf("indexer: append symbol FTS batch: %w", err)
						ftsReady = false
						return false
					}
					ftsItemCount += len(ftsItems)
					ftsItems = make([]graph.SymbolFTSItem, 0, min(len(nodes), persistFTSChunkRows))
					ftsBytes = 0
					return true
				}
				for _, node := range nodes {
					if !idx.shouldIndexForSearch(node) {
						continue
					}
					tokens := ftsTokensFor(node, idx.projectName)
					nextBytes := uint64(len(node.ID) + len(tokens) + 32)
					if len(ftsItems) > 0 && (len(ftsItems) >= persistFTSChunkRows || ftsBytes+nextBytes > persistFTSChunkBytes) {
						if !flushFTS() {
							break
						}
					}
					ftsItems = append(ftsItems, graph.SymbolFTSItem{NodeID: node.ID, Tokens: tokens})
					ftsBytes += nextBytes
					if len(ftsItems) >= persistFTSChunkRows || ftsBytes >= persistFTSChunkBytes {
						if !flushFTS() {
							break
						}
					}
				}
				if ftsReady {
					flushFTS()
				}
			}
			for edges := range inMemShadow.DrainEdgeBatches(persistChunkRows, persistChunkBytes) {
				diskTarget.AddBatch(nil, edges)
			}

			flushStart := time.Now()
			idx.logger.Info("indexer: FlushBulk start",
				zap.String("repo", idx.RepoPrefix()),
				zap.Duration("drain_elapsed", flushStart.Sub(drainStart)),
			)
			if ferr := bl.FlushBulk(); ferr != nil && retErr == nil {
				retErr = fmt.Errorf("indexer: persist bulk graph: %w", ferr)
			}
			idx.logger.Info("indexer: FlushBulk complete",
				zap.String("repo", idx.RepoPrefix()),
				zap.Duration("flush_elapsed", time.Since(flushStart)),
				zap.Duration("total_drain", time.Since(drainStart)),
				zap.Int("nodes", shadowNodeCount),
				zap.Int("edges", shadowEdgeCount),
				zap.Int("fts_items", ftsItemCount),
			)
			if retErr == nil {
				if serr := persistShadowCompactSidecars(
					inMemShadow, diskTarget, idx.RepoPrefix(),
				); serr != nil {
					retErr = fmt.Errorf("indexer: persist compact shadow sidecars: %w", serr)
				}
			}
			if hasFTS {
				if retErr == nil && ftsReady {
					if ferr := searcher.BuildSymbolIndex(); ferr != nil {
						retErr = fmt.Errorf("indexer: finalize backend FTS: %w", ferr)
					}
				}
				reporter.Report("building symbol fts", 1, 1)
			}
			reporter.Report("persisting bulk graph", 1, 1)
			idx.graph = diskTarget
			idx.bulkVectorSink = nil
			idx.contentSink = nil
			// Mirror of the SetGraph(inMemShadow) above: the resolver
			// must follow the graph pointer back to the disk store, or
			// every post-index per-file resolve (the watcher save path,
			// incremental reindex) reads the drained — now empty —
			// shadow and silently resolves nothing.
			if idx.resolver != nil {
				idx.resolver.SetGraph(diskTarget)
			}
		}()
	} else if diskTarget == nil && idx.graph.NodeCount() == 0 && idx.graph.EdgeCount() == 0 {
		if _, isBulk := idx.graph.(graph.BulkLoader); isBulk && firstIndex && (!belowShadowMax || !belowShadowBytes || !shadowBudgetGranted) {
			idx.logger.Info("indexer: skipping in-memory shadow; building against disk store (bounded RAM)",
				zap.Int("files", len(files)),
				zap.Int("file_threshold", shadowMaxFileCount()),
				zap.Bool("over_file_count", !belowShadowMax),
				zap.Int64("total_file_bytes", totalFileBytes),
				zap.Int64("byte_threshold", maxShadowBytes),
				zap.Bool("over_byte_budget", !belowShadowBytes),
				zap.Int64("shadow_weight_bytes", shadowWeight),
				zap.Int64("shadow_process_budget_bytes", shadowBudgetCapacity),
				zap.Int64("shadow_process_used_bytes", shadowBudgetUsed),
				zap.Bool("shadow_budget_unavailable", shadowLocallyEligible && !shadowBudgetGranted))
		}
	}

	// Content-index rebuild strategy. The crash-safe path (on-disk store)
	// deletes each file's prior content rows as that file re-streams
	// (contentWipeFile, invoked at the per-file AddBatch sites below) and
	// sweeps every OTHER content row after the authoritative mtime replace —
	// so a mid-parse kill leaves a mix of old+new content per file instead of
	// the empty table a repo-wide pre-wipe would leave. contentStreamedFiles
	// records which files actually streamed content this walk (the wipe's own
	// argument, i.e. the node FilePath content_fts carries); the end sweep
	// keeps exactly that set, so files that vanished from disk AND files that
	// still exist but no longer yield content sections (doc emptied,
	// classification changed) are both reaped in one scan — the transitions
	// the old repo-wide pre-wipe used to cover. Backends without the per-file
	// capability keep that old behaviour: one repo-wide wipe up front (a
	// cold-store no-op; a cheap per-repo DELETE on a warm one).
	var (
		contentWipeFile      func(filePath string)
		contentRecordFile    func(filePath string)
		contentStreamedMu    sync.Mutex
		contentStreamedFiles map[string]struct{}
	)
	if cs := idx.contentSearcher(); cs != nil {
		repoPrefix := idx.RepoPrefix()
		contentStreamedFiles = make(map[string]struct{})
		contentRecordFile = func(filePath string) {
			if filePath == "" {
				return
			}
			contentStreamedMu.Lock()
			contentStreamedFiles[filePath] = struct{}{}
			contentStreamedMu.Unlock()
		}
		wipeFile, projectionErr, perFile := prepareFullContentFileWiper(cs, repoPrefix, contentRecordFile)
		if perFile {
			if projectionErr != nil {
				idx.logger.Warn("indexer: content repo presence projection failed; retaining per-file wipes",
					zap.String("repo", repoPrefix), zap.Error(projectionErr))
			}
			contentWipeFile = func(filePath string) {
				if err := wipeFile(filePath); err != nil {
					idx.logger.Warn("indexer: per-file content wipe failed", zap.Error(err))
				}
			}
		} else if err := cs.WipeContent(repoPrefix); err != nil {
			idx.logger.Warn("indexer: content index wipe failed", zap.Error(err))
		}
	}

	// Worker pool.
	workers := idx.config.Workers
	if workers <= 0 {
		workers = 1
	}
	// idx.config.Workers defaults to the host's runtime.NumCPU(); inside a
	// CPU-limited container that exceeds the cgroup CPU quota and the pool
	// over-subscribes, so CFS throttling drags throughput down. Clamp the
	// effective pool size to the quota when one is present (lowers only, floor
	// of 1, host-identical when unquotaed). GORTEX_INDEX_CPU_CLAMP=0 opts out.
	if cpuClampEnabled() {
		workers = clampWorkersToCPUQuota(workers, cgroupCPUQuota())
	}

	// Optional crash isolation: run tree-sitter extraction in worker
	// subprocesses so a grammar SIGSEGV / OOM / hang on one
	// pathological file is contained — the bad file is quarantined and
	// the pass still completes. Off unless index.crash_isolation /
	// GORTEX_PARSER_ISOLATION is set.
	var parsePool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		quarantine = crashpool.LoadQuarantine(filepath.Join(absRoot, ".gortex", "parser-quarantine.json"))
		if p, perr := idx.newParsePool(workers); perr != nil {
			idx.logger.Warn("indexer: crash isolation requested but parser pool unavailable; parsing in-process",
				zap.Error(perr))
		} else {
			parsePool = p
			defer parsePool.Close()
			idx.logger.Info("indexer: parser crash isolation enabled", zap.Int("workers", workers))
		}
	}

	// Workers parse files, write the resulting nodes/edges to the
	// sharded graph, and run per-file contract extractors on the same
	// src bytes — all in one pass. Reusing src avoids the 10k+ disk
	// re-reads the old "parse then extractContracts" flow did; running
	// the contract extractors per-worker parallelises what used to be
	// a serial post-pass; language-filtered dispatch skips extractors
	// that can't match (HTTP on .css, OpenAPI on .ts, etc.).
	const parseReportEvery = 50
	totalFiles := len(files)

	_, contractExtractorsByLang := idx.buildPerFileContractExtractors()
	contractReg := contracts.NewRegistry()
	var contractMu sync.Mutex

	var errMu sync.Mutex
	var errors []IndexError
	var processed int64
	var fileCount int64
	var skippedByTimeout int64
	var skippedByMinified int64
	// Parse-subphase instrumentation. The per-stage numbers are SUMMED
	// worker nanoseconds — read/extract/batch overlap across the pool, so
	// their sum legitimately exceeds the critical-path wall emitted beside
	// them; sem_wait is pure admission queuing. One log line per repo at
	// parse completion, so a parse-phase regression is attributable to a
	// stage instead of guessed at.
	parseWallStart := time.Now()
	var parseSemWaitNS, parseReadNS, parseExtractNS, parseBatchNS int64

	// Bound peak parse memory: a weighted, bytes-in-flight semaphore
	// admits each worker by its file size before it reads + extracts, so
	// a cluster of large files (PDFs / office docs in a content repo)
	// serialises instead of all workers materialising whole files and
	// their parse trees at once. Code files are tiny and flow freely;
	// only genuinely large inputs queue. budget <= 0 disables the cap.
	parseBudget := idx.config.MaxParseBytesInFlight
	var parseSem *semaphore.Weighted
	if parseBudget > 0 {
		parseSem = semaphore.NewWeighted(parseBudget)
	}

	// In addition to the bytes-in-flight budget above, cap how many
	// genuinely large files are *read* concurrently: a few huge PDFs /
	// spreadsheets / vector artifacts can dominate RSS before extraction
	// even starts. Small source files bypass the gate and keep full
	// throughput.
	largeReadGate := make(chan struct{}, largeFileReadParallelism(workers))
	readFile := func(wf walkedFile) ([]byte, error) {
		if wf.size < largeFileReadThresholdBytes {
			return os.ReadFile(wf.path)
		}
		largeReadGate <- struct{}{}
		defer func() { <-largeReadGate }()
		return os.ReadFile(wf.path)
	}

	// recordStreamedMtime persists a file's mtime incrementally, in batches,
	// as its nodes land on disk during a full track — so a first-ever index
	// killed mid-parse RESUMES on the next boot (the flushed files reconcile
	// clean, the remainder re-parses) instead of re-tracking a large repo
	// from scratch every time it dies under memory pressure.
	//
	// INVARIANT: an mtime row must land only AFTER its file's nodes are
	// durably in the store, or a fresh mtime would falsely imply indexed
	// nodes. The `idx.graph.(graph.FileMtimeWriter)` assertion is what
	// enforces it: it succeeds ONLY on the direct-to-disk path, where
	// idx.graph is the on-disk store and AddBatch has already made the
	// file's nodes durable. On the whole-repo in-memory shadow and the
	// streaming-flush per-chunk shadow, idx.graph is a plain graph.Graph
	// (no FileMtimeWriter) whose nodes reach disk only at a later drain, so
	// this self-skips and those paths persist at their own drain points (the
	// final ReplaceFileMtimes / the streaming-flush per-chunk persist). The
	// final ReplaceFileMtimes still runs and is authoritative — it also
	// prunes deleted files — so these batches only bound the work a crash
	// can waste; the leftover under-threshold tail rides that final replace.
	var (
		streamMtimeMu sync.Mutex
		streamMtimes  = make(map[string]int64, mtimeStreamPersistEvery)
	)
	recordStreamedMtime := func(absPath string, mtimeNano int64) {
		if mtimeNano <= 0 {
			return
		}
		w, ok := idx.graph.(graph.FileMtimeWriter)
		if !ok {
			return
		}
		var flush map[string]int64
		streamMtimeMu.Lock()
		streamMtimes[idx.relKey(absPath)] = mtimeNano
		if len(streamMtimes) >= mtimeStreamPersistEvery {
			flush = streamMtimes
			streamMtimes = make(map[string]int64, mtimeStreamPersistEvery)
		}
		streamMtimeMu.Unlock()
		if flush != nil {
			if err := w.BulkSetFileMtimes(idx.repoPrefix, flush); err != nil {
				idx.logger.Warn("indexer: incremental mtime batch persist failed",
					zap.String("repo", idx.repoPrefix), zap.Error(err))
			}
		}
	}
	flushStreamedMtimes := func() {
		w, ok := idx.graph.(graph.FileMtimeWriter)
		if !ok {
			return
		}
		streamMtimeMu.Lock()
		flush := streamMtimes
		streamMtimes = make(map[string]int64, mtimeStreamPersistEvery)
		streamMtimeMu.Unlock()
		if len(flush) == 0 {
			return
		}
		if err := w.BulkSetFileMtimes(idx.repoPrefix, flush); err != nil {
			idx.logger.Warn("indexer: final incremental mtime batch persist failed",
				zap.String("repo", idx.repoPrefix), zap.Error(err))
		}
	}

	// parseChunk runs the per-file worker pool over the supplied
	// slice. Closure over outer state (errors, counters, contract
	// registry, parsePool, quarantine) so it can be called multiple
	// times — once for the non-streaming path, repeatedly for the
	// streaming-flush large-repo path where each call processes a
	// bounded slice into a per-chunk in-memory shadow.
	parseChunk := func(chunkFiles []walkedFile) {
		sidecars := newParseSidecarBatch(idx)
		contentBatch := newParseContentBatch(idx)
		graphBatch := newParseGraphBatch(idx.graph)
		fileCh := make(chan walkedFile, workers*4)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var localContracts []contracts.Contract
				for wf := range fileCh {
					path := wf.path
					p := atomic.AddInt64(&processed, 1)
					if p == 1 || p%parseReportEvery == 0 {
						reporter.Report("parsing", int(p), totalFiles)
					}

					// Admit this file into extraction under the
					// bytes-in-flight budget before reading it, so large
					// content files serialise instead of all workers
					// materialising whole files at once.
					var weight int64
					if parseSem != nil {
						weight = clampParseWeight(wf.size, parseBudget)
						semStart := time.Now()
						if aerr := parseSem.Acquire(ctx, weight); aerr != nil {
							return
						}
						atomic.AddInt64(&parseSemWaitNS, int64(time.Since(semStart)))
					}

					relPath, _ := filepath.Rel(absRoot, path)
					// Streaming content extractors (PDF / office docs) read the
					// file themselves — one page/slide/sheet at a time — instead
					// of materialising the whole file. Only the in-process route
					// streams; the crash-isolation subprocess route keeps bytes.
					if walkExt, found := idx.registry.GetByLanguage(wf.lang); found && parsePool == nil {
						if se, ok := walkExt.(parser.StreamingExtractor); ok {
							result, serr := idx.extractStreaming(se, path, relPath)
							if parseSem != nil {
								parseSem.Release(weight)
							}
							if serr != nil {
								errMu.Lock()
								errors = append(errors, IndexError{FilePath: path, Error: serr.Error()})
								if result == nil {
									parseFailedFiles = append(parseFailedFiles, skippedFile{relPath: relPath, lang: wf.lang, cause: serr.Error()})
								}
								errMu.Unlock()
							}
							if result == nil {
								continue
							}
							idx.applyRepoPrefix(result.Nodes, result.Edges)
							contentHandled := false
							if contentBatch != nil {
								durablePath, durableMtime := path, wf.mtimeNano
								contentHandled = contentBatch.add(result.Nodes, result.Edges, func() {
									recordStreamedMtime(durablePath, durableMtime)
								})
								if contentHandled && contentRecordFile != nil {
									contentRecordFile(firstContentFilePath(result.Nodes))
								}
							}
							if !contentHandled {
								if contentWipeFile != nil {
									contentWipeFile(firstContentFilePath(result.Nodes))
								}
								idx.streamContentSections(result.Nodes)
								durablePath, durableMtime := path, wf.mtimeNano
								if !graphBatch.add(result.Nodes, result.Edges, func() {
									recordStreamedMtime(durablePath, durableMtime)
								}) {
									idx.graph.AddBatch(result.Nodes, result.Edges)
									recordStreamedMtime(durablePath, durableMtime)
								}
							}
							sidecars.addConstValues(result)
							continue
						}
					}

					readStart := time.Now()
					src, err := readFile(wf)
					atomic.AddInt64(&parseReadNS, int64(time.Since(readStart)))
					if err != nil {
						errMu.Lock()
						errors = append(errors, IndexError{FilePath: path, Error: err.Error()})
						errMu.Unlock()
						if parseSem != nil {
							parseSem.Release(weight)
						}
						continue
					}

					// Reuse the walk-time language. The walk's
					// effectiveLanguage call already consulted shebang
					// bytes via readSniffPrefix (512-byte probe), so a
					// re-detect against the full src would change the
					// answer only on the vanishingly rare case where a
					// language marker lives past byte 512 — and any such
					// case is content-sniffing-by-luck rather than spec'd
					// behaviour. The fallback below covers the truly
					// pathological case where the walk-time language has
					// no extractor registered (effectively dead code).
					lang := wf.lang
					ext, _ := idx.registry.GetByLanguage(lang)
					if ext == nil {
						if relang, ok := idx.effectiveLanguage(path, src); ok {
							lang = relang
							ext, _ = idx.registry.GetByLanguage(lang)
						}
					}
					if ext == nil {
						if parseSem != nil {
							parseSem.Release(weight)
						}
						continue
					}

					// Pre-ingestion transforms: rewrite the bytes before
					// extraction (BOM strip, minified-bundle expansion, a
					// PDF→markdown command, …).
					src = idx.transforms.run(relPath, src)

					extractStart := time.Now()
					result, skipped, err := idx.extractFile(parsePool, quarantine, path, relPath, lang, ext, src)
					atomic.AddInt64(&parseExtractNS, int64(time.Since(extractStart)))
					if parseSem != nil {
						parseSem.Release(weight)
					}
					if err != nil {
						errMu.Lock()
						errors = append(errors, IndexError{FilePath: path, Error: err.Error()})
						errMu.Unlock()
					}
					if result == nil {
						// A full-index parse failure that produced no nodes:
						// record it for a skip-node post-pass so the file
						// stays visible instead of vanishing. (The live-modify
						// path never reaches here — it keeps a file's prior
						// nodes through a transient parse failure.)
						if err != nil {
							errMu.Lock()
							parseFailedFiles = append(parseFailedFiles, skippedFile{relPath: relPath, lang: lang, cause: err.Error()})
							errMu.Unlock()
						}
						continue
					}
					if skipped && len(result.Nodes) > 0 {
						if _, ok := result.Nodes[0].Meta["skipped_due_to_timeout"]; ok {
							atomic.AddInt64(&skippedByTimeout, 1)
						}
						if _, ok := result.Nodes[0].Meta["skipped_due_to_minified"]; ok {
							atomic.AddInt64(&skippedByMinified, 1)
						}
					}

					// Append coverage artifacts (todos / licenses /
					// ownership) before applyRepoPrefix so they get the
					// same multi-repo namespacing treatment as
					// language-extractor output. Skipped for quarantined /
					// timed-out files — the coverage scanners would re-read
					// a source the parser could not survive.
					if !skipped {
						idx.applyCoverageDomains(relPath, lang, src, result)
					}

					idx.applyRepoPrefix(result.Nodes, result.Edges)

					// Find the file node (if the extractor produced one)
					// and collect its outgoing edges — contract extractors
					// take the file-scope edge set (imports, etc.), not
					// every intra-file edge.
					var fileNodeID, fileGraphPath string
					for _, n := range result.Nodes {
						if n.Kind == graph.KindFile {
							fileNodeID = n.ID
							fileGraphPath = n.FilePath
							break
						}
					}
					var fileScopeEdges []*graph.Edge
					if fileNodeID != "" {
						for _, e := range result.Edges {
							if e.From == fileNodeID {
								fileScopeEdges = append(fileScopeEdges, e)
							}
						}
					}

					// Stream this file's content (data_class=content) section
					// bodies into the dedicated content index and lean the
					// nodes to a snippet BEFORE AddBatch, so the bulk text
					// never enters the graph, the symbol search, or the
					// materialising code passes.
					contentHandled := false
					if contentBatch != nil {
						durablePath, durableMtime := path, wf.mtimeNano
						contentHandled = contentBatch.add(result.Nodes, result.Edges, func() {
							recordStreamedMtime(durablePath, durableMtime)
						})
						if contentHandled && contentRecordFile != nil {
							contentRecordFile(firstContentFilePath(result.Nodes))
						}
					}
					if !contentHandled {
						if contentWipeFile != nil {
							contentWipeFile(firstContentFilePath(result.Nodes))
						}
						idx.streamContentSections(result.Nodes)
					}

					// In-memory shadows keep the existing per-file shard-grouped
					// AddBatch. Direct SQLite writes enter a bounded cross-file
					// accumulator, amortising transaction/statement setup without
					// retaining a repository-sized graph in Go memory.
					if !contentHandled {
						batchStart := time.Now()
						durablePath, durableMtime := path, wf.mtimeNano
						if !graphBatch.add(result.Nodes, result.Edges, func() {
							recordStreamedMtime(durablePath, durableMtime)
						}) {
							idx.graph.AddBatch(result.Nodes, result.Edges)
							recordStreamedMtime(durablePath, durableMtime)
						}
						atomic.AddInt64(&parseBatchNS, int64(time.Since(batchStart)))
					}
					sidecars.add(relPath, src, result)

					if !skipped && fileGraphPath != "" {
						exts := contractExtractorsByLang[lang]
						if len(exts) > 0 {
							c := idx.runContractExtractorsForFile(
								fileGraphPath, src, result.Nodes, fileScopeEdges, exts, result.Tree)
							localContracts = append(localContracts, c...)

							// Populate the per-file contract cache so a
							// later IncrementalReindex can skip this file
							// on a cache hit. Mtime comes from the walk-
							// time d.Info() — no extra stat here.
							if wf.mtimeNano > 0 {
								idx.contractCacheMu.Lock()
								idx.contractCache[fileGraphPath] = &contractCacheEntry{
									mtimeNano: wf.mtimeNano,
									contracts: c,
								}
								idx.contractCacheMu.Unlock()
							}
						}
					}
					// Release the parse tree now that the per-file
					// contract pass is done. Post-passes that need a
					// tree for this file (cross-file handler resolution)
					// re-parse on demand. Nil-safe. Deliberately not a
					// defer: this is a per-file loop body inside a
					// long-lived worker goroutine, so a defer would pin
					// every tree in the chunk until the worker exits.
					result.ReleaseTree()
					atomic.AddInt64(&fileCount, 1)
				}
				if len(localContracts) > 0 {
					contractMu.Lock()
					for _, c := range localContracts {
						contractReg.Add(c)
					}
					contractMu.Unlock()
				}
			}()
		}

	dispatch:
		for _, f := range chunkFiles {
			select {
			case fileCh <- f:
			case <-ctx.Done():
				break dispatch
			}
		}
		close(fileCh)
		wg.Wait()
		// Flush every successfully parsed file even when ctx was cancelled
		// while another worker waited for admission. This preserves the old
		// per-file durability boundary and makes the next cold attempt resume
		// from the mtimes whose callbacks run after this commit.
		graphBatch.flush()
		contentBatch.flush()
		sidecars.flush()
	}

	// Dispatch the largest files first. Both dispatch paths below
	// consume this slice in order: the plain path feeds it straight to
	// the worker pool, and the streaming-flush path carves it into
	// chunks from the front. Feeding the biggest file to the workers
	// first lets its long parse overlap with the tail of small files,
	// instead of the whole index waiting on a large file that would
	// otherwise be dispatched last. The sort is stable and a pure
	// permutation, so the set of indexed files and the resulting graph
	// are identical — only the dispatch order changes.
	sortBySizeDesc(files)

	// Streaming-flush path: above shadowMaxFileCount with a
	// BulkLoader-capable backend, we can't fit the whole shadow in
	// RAM but we can still amortise the per-file disk-write cost by
	// chunking. Each chunk runs against its own throwaway shadow,
	// then flushes via BulkLoad to disk. Resolve runs against the
	// disk store afterwards (per-call, slower than the shadow path
	// but bounded RAM). Activated by GORTEX_STREAMING_FLUSH=1; off
	// by default since it requires the disk-only resolver path
	// (~tens of minutes on huge repos) that we haven't yet
	// optimised end-to-end.
	if diskTarget == nil && streamingFlushActive(idx.graph, len(files)) {
		bl, _ := idx.graph.(graph.BulkLoader)
		streamingDisk := idx.graph
		chunkSize := streamingChunkSize()
		idx.logger.Info("indexer: streaming-flush parse",
			zap.Int("files", len(files)),
			zap.Int("chunk_size", chunkSize))
		for chunkStart := 0; chunkStart < len(files); chunkStart += chunkSize {
			chunkEnd := min(chunkStart+chunkSize, len(files))
			chunkShadow := graph.New()
			idx.graph = chunkShadow
			parseChunk(files[chunkStart:chunkEnd])
			if err := ctx.Err(); err != nil {
				// This chunk has not crossed the durable boundary yet. Drop it
				// and retain only prior fully-flushed chunks; never stamp mtimes
				// for files the cancelled dispatch did not parse.
				idx.graph = streamingDisk
				return nil, err
			}
			// Flush the chunk to disk through the same explicit caps as the
			// admitted whole-repository shadow. Node and edge buffers never
			// overlap, and compact sidecars drain independently afterwards.
			bl.BeginBulkLoad()
			const (
				streamPersistRows  = 8192
				streamPersistBytes = 16 << 20
			)
			for nodes := range chunkShadow.DrainNodeBatches(streamPersistRows, streamPersistBytes) {
				streamingDisk.AddBatch(nodes, nil)
			}
			for edges := range chunkShadow.DrainEdgeBatches(streamPersistRows, streamPersistBytes) {
				streamingDisk.AddBatch(nil, edges)
			}
			if err := bl.FlushBulk(); err != nil {
				return nil, fmt.Errorf("indexer: streaming-flush chunk %d..%d: %w", chunkStart, chunkEnd, err)
			}
			if err := persistShadowCompactSidecarChunk(
				chunkShadow, streamingDisk, idx.RepoPrefix(),
			); err != nil {
				return nil, fmt.Errorf("indexer: streaming-flush compact sidecars %d..%d: %w", chunkStart, chunkEnd, err)
			}
			// This chunk's nodes are durable on disk now, so persist its
			// files' mtimes — a kill mid-track then resumes from this chunk
			// boundary instead of re-tracking the whole large first index
			// from scratch. The in-worker recordStreamedMtime is a no-op on
			// this path (idx.graph was the throwaway per-chunk shadow, not a
			// FileMtimeWriter); the persist happens here, after the drain,
			// keeping the "fresh mtime implies durable nodes" invariant.
			if w, ok := streamingDisk.(graph.FileMtimeWriter); ok {
				batch := make(map[string]int64, chunkEnd-chunkStart)
				for _, f := range files[chunkStart:chunkEnd] {
					if f.mtimeNano > 0 {
						batch[idx.relKey(f.path)] = f.mtimeNano
					}
				}
				if len(batch) > 0 {
					if err := w.BulkSetFileMtimes(idx.repoPrefix, batch); err != nil {
						idx.logger.Warn("indexer: streaming-flush chunk mtime persist failed",
							zap.String("repo", idx.repoPrefix), zap.Error(err))
					}
				}
			}
		}
		// After all chunks, idx.graph points at the disk store so
		// the resolver and subpasses read/mutate the merged state.
		idx.graph = streamingDisk
	} else {
		parseChunk(files)
		if err := ctx.Err(); err != nil {
			// Direct SQLite batches have committed all completed files at this
			// point. Persist their under-threshold mtime tail before returning;
			// the authoritative all-file replace below must not run on a
			// cancelled walk. Shadow paths self-skip this writer capability and
			// their deferred error restore discards the partial shadow.
			flushStreamedMtimes()
			return nil, err
		}
	}

	// Finalise the content index after the per-file streaming appends so
	// its FTS5 segments are merged before the first content query.
	if cs := idx.contentSearcher(); cs != nil {
		if err := cs.BuildContentIndex(); err != nil {
			idx.logger.Warn("indexer: content index build failed", zap.Error(err))
		}
	}

	if processed > 0 {
		reporter.Report("parsing", int(processed), totalFiles)
	}

	// Emit synthetic file nodes for files dropped by the size cap so
	// they stay visible in the graph with skip telemetry attached
	// instead of vanishing silently.
	idx.emitSizeSkipNodes(skippedBySize)
	idx.emitContentSkipNodes(skippedByContent)
	idx.emitParseFailedSkipNodes(parseFailedFiles)

	// Populate fileMtimes for all detected files. Keyed through
	// relKey so the mtime map agrees with the graph's file-node keys
	// (and with the incremental / git-watcher paths) on the NFC form
	// of every non-ASCII filename. Mtimes are the walk-time values
	// captured via d.Info(); no per-file os.Stat round-trip here.
	idx.mtimeMu.Lock()
	idx.fileMtimes = make(map[string]int64, len(files))
	for _, f := range files {
		if f.mtimeNano > 0 {
			idx.fileMtimes[idx.relKey(f.path)] = f.mtimeNano
		}
	}
	mtimeSnapshot := make(map[string]int64, len(idx.fileMtimes))
	for k, v := range idx.fileMtimes {
		mtimeSnapshot[k] = v
	}
	idx.mtimeMu.Unlock()

	// Persist the per-file mtimes through the store's optional
	// FileMtime sidecar table. On the on-disk backend this lets warm
	// restarts seed ReconcileRepoCtx without having to read them back
	// out of the gob+gzip metadata snapshot; on the in-memory
	// backend the capability isn't implemented and the assertion
	// short-circuits.
	//
	// Multi-repo bug: when the shadow-swap path is active, idx.graph
	// is the in-memory shadow graph at this point — graph.Graph does
	// NOT implement FileMtimeWriter, so the type assertion fails and
	// persistence is silently skipped. The actual disk store is
	// the local diskTarget variable; checking it first ensures warm-
	// restart-skip-reindex actually works. The defer that swaps
	// idx.graph back to diskTarget runs LATER, when IndexCtx returns,
	// so we can't rely on it here. Falls through to idx.graph for the
	// non-shadow path.
	idx.logger.Info("indexer: parse subphases",
		zap.String("repo", idx.repoPrefix),
		zap.Duration("wall", time.Since(parseWallStart)),
		zap.Duration("sem_wait_workers", time.Duration(atomic.LoadInt64(&parseSemWaitNS))),
		zap.Duration("read_workers", time.Duration(atomic.LoadInt64(&parseReadNS))),
		zap.Duration("extract_workers", time.Duration(atomic.LoadInt64(&parseExtractNS))),
		zap.Duration("batch_workers", time.Duration(atomic.LoadInt64(&parseBatchNS))),
		zap.Int("workers", workers),
		zap.Int("files", totalFiles))
	mtimeTarget := graph.Store(idx.graph)
	if diskTarget != nil {
		mtimeTarget = diskTarget
	}
	// Full-index persist is AUTHORITATIVE: replace the repo's entire mtime
	// set so files deleted since the last index are pruned. An upsert-only
	// write (BulkSetFileMtimes) leaves deleted-file rows behind, and warm-
	// restart reconcile then detects them as phantom deletions on every
	// restart — forcing a full re-track that never converges. Prefer the
	// replace capability; fall back to upsert for backends without it.
	if len(mtimeSnapshot) > 0 {
		var perr error
		persisted := false
		if r, ok := mtimeTarget.(graph.FileMtimeReplacer); ok {
			perr, persisted = r.ReplaceFileMtimes(idx.repoPrefix, mtimeSnapshot), true
		} else if w, ok := mtimeTarget.(graph.FileMtimeWriter); ok {
			perr, persisted = w.BulkSetFileMtimes(idx.repoPrefix, mtimeSnapshot), true
		}
		if persisted {
			if perr != nil {
				idx.logger.Warn("persist file mtimes failed",
					zap.String("repo", idx.repoPrefix), zap.Error(perr))
			} else {
				idx.logger.Info("persisted file mtimes",
					zap.String("repo", idx.repoPrefix),
					zap.Int("count", len(mtimeSnapshot)))
			}
		}

		// Crash-safe content path: reap every content row this walk did NOT
		// re-stream. keep is contentStreamedFiles — the files that actually
		// produced content sections this run — NOT the surviving-file mtime
		// set: a file can survive on disk yet stop yielding content (doc
		// emptied, classification changed), and keying keep off mtimes would
		// protect its stale rows forever. Recorded keys are the wipe's own
		// argument (the node FilePath content_fts carries), so the comparison
		// matches the stored rows in single- and multi-repo form alike. A walk
		// that streamed NO content falls back to the repo-wide wipe: the repo
		// has zero content files now, and the sweep's empty-keep guard (a
		// never-wipe-from-empty safety net) would otherwise no-op and leave
		// every stale row behind. Only when the per-file wipe path is active;
		// on the repo-wide-wipe fallback the up-front pre-wipe already cleared
		// both transitions. Runs only under the completed-walk guard above
		// (len(mtimeSnapshot) > 0), so a killed parse never triggers it.
		if contentWipeFile != nil {
			contentStreamedMu.Lock()
			keep := contentStreamedFiles
			contentStreamedMu.Unlock()
			if len(keep) == 0 {
				if cs := idx.contentSearcher(); cs != nil {
					if err := cs.WipeContent(idx.RepoPrefix()); err != nil {
						idx.logger.Warn("indexer: content wipe of contentless repo failed", zap.Error(err))
					}
				}
			} else if sw, ok := idx.contentSearcher().(interface {
				DeleteContentFilesForRepoNotIn(repoPrefix string, keep map[string]struct{}) error
			}); ok {
				if err := sw.DeleteContentFilesForRepoNotIn(idx.repoPrefix, keep); err != nil {
					idx.logger.Warn("indexer: content sweep of stale files failed", zap.Error(err))
				}
			}
		}
	}

	// Retain parse errors and record index metadata.
	idx.parseErrors = errors
	idx.totalDetected = len(files)
	idx.lastIndexTime = time.Now()

	if idx.deferResolve {
		// Multi-repo orchestrator runs these serially after wg.Wait()
		// to avoid races on the shared graph between this goroutine's
		// ResolveAll mutation phase and a sibling goroutine's contract
		// pass walking AllEdges. See SetDeferResolve.
		idx.pendingContractReg = contractReg
		idx.deferredGoModDone = false
	} else {
		// Materialise dep::<module> contract nodes from go.mod BEFORE
		// ResolveAll so the resolver's import bridge can re-target Go
		// imports of declared modules to their dep contract node.
		idx.extractGoModContracts(contractReg)

		reporter.Report("resolving references", 0, 0)
		// Resolve cross-file references.
		idx.populateCppIncludeDirs(true)
		idx.resolver.ResolveAll()

		// Infer structural interface satisfaction + method-level
		// overrides. Skipped under deferGlobalPasses so a batch caller
		// (warmup, ReconcileAll) can run them once at the end against
		// the final shared graph instead of paying the O(global) walk
		// per repo. InferOverrides depends on InferImplements running
		// first.
		if !idx.deferGlobalPasses {
			reporter.Report("inferring interfaces", 0, 0)
			idx.resolver.InferImplements()
			idx.resolver.InferOverrides()
		}

		// Semantic enrichment (SCIP, go/types, LSP).
		if idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders() {
			reporter.Report("semantic enrichment", 0, 0)
			// Key by the repo prefix so a repo-scoped provider can scope
			// file selection to this repo (empty in single-repo mode).
			roots := map[string]string{idx.repoPrefix: absRoot}
			// The inline full-index path does not gate or persist enrichment
			// markers (no RepoState): the deferred warmup path owns the
			// skip-on-restart optimisation. The admission floor still applies —
			// this is index-time enrichment of a whole repo, the exact workload
			// the floor protects.
			results, _, err := idx.semanticMgr.EnrichAll(idx.graph, roots, semantic.EnrichOptions{
				MinLanguageNodes: semantic.EnrichmentAdmissionFloor(),
			})
			if err != nil {
				idx.logger.Warn("semantic enrichment failed", zap.Error(err))
			} else if len(results) > 0 {
				for _, r := range results {
					idx.logger.Info("semantic enrichment result",
						zap.String("provider", r.Provider),
						zap.String("language", r.Language),
						zap.Int("confirmed", r.EdgesConfirmed),
						zap.Int("added", r.EdgesAdded),
						zap.Int("refuted", r.EdgesRefuted),
						zap.Float64("coverage", r.CoveragePercent),
					)
				}
			}
		}
	}

	reporter.Report("building search index", 0, 0)
	// Build search index.
	idx.buildSearchIndex()

	if !idx.deferResolve {
		// Contracts were already extracted inline during parse (per file,
		// per worker). Here we just finish up. extractGoModContracts
		// already ran (see the !deferResolve branch above) so dep
		// nodes were available during ResolveAll's import-bridge pass;
		// commitContracts is idempotent for those.
		reporter.Report("extracting contracts", 0, 0)
		idx.extractExternalModules()
		idx.extractDIContracts(contractReg)
		idx.commitContracts(contractReg)

		// Test-edge pass — runs once the call graph is final. Skipped
		// under deferGlobalPasses so a batch caller can fold this into
		// one global pass after the per-repo loop.
		if !idx.deferGlobalPasses {
			reporter.Report("test edge pass", 0, 0)
			marked, emitted := markTestSymbolsAndEmitEdges(idx.graph)
			if marked > 0 || emitted > 0 {
				idx.logger.Info("test edges emitted",
					zap.Int("test_symbols", marked),
					zap.Int("edges", emitted),
				)
			}
			if ctrl := entrypoints.PropagateEntryPointsDownHierarchy(idx.graph); ctrl > 0 {
				idx.logger.Info("entry-point hierarchy stamped", zap.Int("stamped", ctrl))
			}
			if re, ep, fa := synthesizeCapabilityEdges(idx.graph); re > 0 || ep > 0 || fa > 0 {
				idx.logger.Info("capability edges emitted",
					zap.Int("reads_env", re),
					zap.Int("executes_process", ep),
					zap.Int("accesses_field", fa),
				)
			}
			reporter.Report("clone detection pass", 0, 0)
			if cs := detectClonesAndEmitEdgesCtx(ctx, idx.graph, idx.repoPrefix, idx.cloneThreshold()); cs.Items > 0 {
				idx.logger.Info("clone edges emitted",
					zap.Int("items", cs.Items),
					zap.Int("clone_pairs", cs.Pairs),
					zap.Int("edges", cs.Edges),
					zap.Int("skipped_buckets", cs.SkippedBuckets),
					zap.Int("skipped_bucket_items", cs.SkippedBucketItems),
					zap.Int("diffused_pairs", cs.DiffusedPairs),
					zap.Int("diffused_edges", cs.DiffusedEdges),
				)
			}
			// Seed the incremental clone index from the freshly-baselined
			// signatures + sidecar so steady-state single-file edits go
			// incremental (EvictFuncs/UpdateFuncs) instead of re-running
			// this whole-graph pass per file. The batch pass remains the
			// re-baseline (corrects CMS drift) and owns diffusion.
			if idx.cloneIndex != nil {
				idx.cloneIndex.Rebuild(idx.graph, idx.repoPrefix)
			}
			// Framework dynamic-dispatch synthesis — runs once the call
			// graph and interface inference are final. Skipped under
			// deferGlobalPasses; the batch caller folds it into
			// RunGlobalGraphPasses.
			reporter.Report("framework dispatch synthesis", 0, 0)
			if rep := resolver.RunFrameworkSynthesizers(idx.graph); rep.Total > 0 {
				idx.logger.Info("framework dispatch calls synthesized",
					zap.Int("edges", rep.Total),
					zap.Any("per_synthesizer", rep.Per),
				)
			}
			// External-call placeholder synthesis (opt-in) — runs after
			// the resolver and stub passes so only genuinely un-indexed
			// external targets are left to materialise.
			reporter.Report("external-call synthesis", 0, 0)
			if extCalls := resolver.SynthesizeExternalCalls(idx.graph, idx.externalCallSynthesisEnabled()); extCalls > 0 {
				idx.logger.Info("external-call placeholders synthesized",
					zap.Int("edges", extCalls),
				)
			}
			if spec := resolver.ResolveSpeculativeDispatch(idx.graph, idx.speculativeDispatchEnabled()); spec > 0 {
				idx.logger.Info("speculative dispatch edges synthesized",
					zap.Int("edges", spec),
				)
			}
			// Reachability index — used to be precomputed for every
			// impact seed here. The eager pass was retired because the
			// breakeven was untenable on monorepo graphs (k8s:
			// ~2000 s build to save ~10 ms per query, ~200 k-query
			// breakeven). reach.Lookup now computes the BFS on first
			// access per seed and caches the result. The
			// InvalidateIndex call bumps the build counter so any
			// stale stamps from a prior build (e.g. snapshot reload
			// before a partial mutation) no longer shadow the live
			// graph state.
			reach.InvalidateIndex()
		}
	}

	// Auto-upgrade to Bleve if above threshold. Run in the background
	// so the foreground IndexCtx returns immediately — populating
	// Bleve with 50k+ symbols takes 30-60s and adding that to the
	// initial-index latency was the dominant tail. Searches against
	// idx.search keep hitting the in-memory backend until the swap
	// completes; nothing observes a half-built Bleve.
	//
	// upgradeOnce gates the spawn so multi-repo warmup, which calls
	// IndexCtx once per tracked repo, doesn't launch one upgrade
	// goroutine per post-threshold repo. One per indexer lifetime.
	//
	// Skip the upgrade when the active search backend is the
	// SymbolSearcher adapter: the disk store's native FTS is
	// already serving search at engine-native latency, and
	// spawning a parallel Bleve build would (a) waste ~100MB heap
	// re-indexing the same corpus and (b) silently swap the
	// adapter out for Bleve on completion — defeating the whole
	// FTS path. The Swappable's current backend tells us which
	// branch we're on.
	if !isSymbolSearcherBackend(idx.search) && idx.search.Count() >= search.AutoThreshold {
		idx.upgradeOnce.Do(func() {
			reporter.Report("scheduling search backend upgrade", 0, 0)
			idx.upgradeSpawnedMu.Lock()
			idx.upgradeSpawned++
			idx.upgradeSpawnedMu.Unlock()
			// Snapshot upfront so the background goroutine doesn't
			// read Node.Meta concurrently with subsequent Index
			// calls' Meta-writing passes (reach.BuildIndex,
			// ResolveTemporalCalls, ...).
			snapshot := idx.snapshotBleveEntries()
			go idx.upgradeSearchToBleve(snapshot)
		})
	}

	// Persist the parser quarantine so a file that crashed the parser
	// stays skipped across daemon restarts until its content changes.
	if quarantine != nil {
		if err := quarantine.Save(); err != nil {
			idx.logger.Warn("indexer: failed to persist parser quarantine", zap.Error(err))
		} else if n := quarantine.Len(); n > 0 {
			idx.logger.Info("indexer: parser quarantine", zap.Int("files", n))
		}
	}

	reporter.Report("indexing complete", int(fileCount), len(files))

	// Persist the Merkle baseline so the next incremental pass diffs
	// against content hashes rather than re-indexing the whole repo.
	workspaceFP := ""
	if idx.merkleEnabled() {
		paths := make([]string, len(files))
		for i, wf := range files {
			paths[i] = wf.path
		}
		workspaceFP = idx.saveMerkleBaseline(absRoot, paths)
	}
	idx.indexGen.Add(1) // invalidate the trigram search cache

	nodes, edges := idx.repoNodeEdgeCount()
	idx.persistRepoIndexState(diskTarget, absRoot, workspaceFP, nodes, edges)
	result = &IndexResult{
		NodeCount:        nodes,
		EdgeCount:        edges,
		FileCount:        int(fileCount),
		QuarantinedFiles: quarantine.Len(),
		SkippedFiles:     len(skippedBySize) + len(skippedByContent) + int(skippedByTimeout) + int(skippedByMinified),
		DurationMs:       time.Since(start).Milliseconds(),
		Errors:           errors,
	}
	idx.warnIfEdgeSanityViolated(result)
	// A whole-repo (re-)track that observed files did work: mark the repo for
	// deferred semantic enrichment. FullRetrack is stamped by the multi-repo
	// caller after this returns, so FileCount carries the signal here.
	if result.FileCount > 0 || result.FullRetrack {
		idx.markPendingEnrichFull()
		// IndexCtx re-parsed every file, dropping this repo's hover-enrichment
		// edges — force the deferred pass past the completion-marker gate so
		// they are restored even at an unchanged clean HEAD.
		idx.fullReindexed.Store(true)
	}
	return result, nil
}

// repoNodeEdgeCount returns this indexer's contribution to the graph,
// scoped to its repoPrefix in multi-repo mode. In single-repo mode
// (empty prefix) every node carries an empty RepoPrefix anyway, so the
// graph totals equal the repo's contribution and we use the cheap
// global accessors. The multi-repo path uses RepoMemoryEstimate which
// walks only this repo's byRepo bucket — O(repo size), not O(graph) —
// so callers that stamp RepoMetadata.NodeCount/EdgeCount no longer
// freeze the workspace-wide total at TrackRepo time.
func (idx *Indexer) repoNodeEdgeCount() (int, int) {
	if idx.repoPrefix == "" {
		return idx.graph.NodeCount(), idx.graph.EdgeCount()
	}
	est := idx.graph.RepoMemoryEstimate(idx.repoPrefix)
	return est.NodeCount, est.EdgeCount
}

// warnIfEdgeSanityViolated logs a loud warning when an index pass
// produced files and symbol nodes but no edges — see
// IndexResult.EdgeSanityViolated.
func (idx *Indexer) warnIfEdgeSanityViolated(r *IndexResult) {
	if r.EdgeSanityViolated() {
		idx.logger.Warn("indexer: edge-sanity check failed — index has files and nodes but zero edges; edge extraction likely failed wholesale",
			zap.Int("files", r.FileCount),
			zap.Int("nodes", r.NodeCount),
			zap.Int("edges", r.EdgeCount))
	}
}

// IndexFile parses a single file and patches the graph (evict then
// add), including per-file resolver work for cross-file references.
// Use in the single-event fsnotify path where each edit is isolated.
func (idx *Indexer) IndexFile(filePath string) error {
	return idx.indexFile(filePath, true, nil)
}

// IndexFileNoResolve is IndexFile minus the per-file resolver call.
// Callers in batch paths (storm mode, branch-switch reconcile, git
// diff dispatch) use this when they will run resolver.ResolveAll()
// once at the end of the batch; otherwise a 500-file checkout pays
// the per-file resolver cost 500 times instead of once.
func (idx *Indexer) IndexFileNoResolve(filePath string) error {
	return idx.indexFile(filePath, false, nil)
}

func (idx *Indexer) indexFile(
	filePath string,
	resolve bool,
	markerBatches ...*reparsePendingEnrichmentBatch,
) error {
	var markerBatch *reparsePendingEnrichmentBatch
	if len(markerBatches) > 0 {
		markerBatch = markerBatches[0]
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}

	// Two keys for the same file, deliberately distinct on Windows.
	// mtimeKey (relKey: slash form + NFC) is the fileMtimes map key.
	// relPath (graphRelKey: OS-native separators + NFC) is what the
	// graph stores a file's nodes under — the exact form the cold bulk
	// walk stamped on their IDs / FilePaths. The evict below MUST use
	// the graph form: a slash-keyed lookup misses the backslash-keyed
	// cold nodes on Windows, so the re-parse would leak a duplicate node
	// set on every save. On POSIX the two keys are identical
	// (filepath.Rel already yields '/'), so this split is a Windows-only
	// correction. Both drive an FSEvents-NFD / git-watcher-NFC path onto
	// the same NFC key so a re-index still lands on the bulk-walk key.
	mtimeKey := idx.relKey(absPath)
	relPath := idx.graphRelKey(absPath)

	// In multi-repo mode, the graph stores prefixed file paths.
	graphPath := idx.prefixPath(relPath)
	indexFileStarted := time.Now()
	var snapshotDuration, commitDuration, resolveDuration time.Duration
	var refFactsDuration, affectedDuration, enrichDuration time.Duration

	// Parse-then-swap: we must NOT evict the file's existing nodes/edges
	// and search entries until we hold a usable parse result. Evicting
	// first leaves the file at zero nodes whenever the on-disk bytes are
	// transiently unparseable (a save mid-edit) — a failed extraction
	// then returns early and the symbols stay nuked. Capturing the old
	// state up front and deferring the actual eviction to evictExisting()
	// keeps the file stale-but-present on failure (stale beats empty) and
	// shrinks the no-nodes window to the gap between evict and AddBatch.
	//
	// oldFuncIDs holds this file's function/method node IDs so the
	// incremental clone index can drop their CMS/LSH contributions —
	// EvictFile removes the nodes (and their clone_sig) from the graph,
	// so it must be captured before evictExisting runs.
	var oldFuncIDs []string
	evictExisting := func() {
		for _, n := range idx.graph.GetFileNodes(graphPath) {
			idx.removeFromSearch(n)
			if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
				oldFuncIDs = append(oldFuncIDs, n.ID)
			}
		}
		idx.restubIncomingRefs(graphPath)
		idx.graph.EvictFile(graphPath)
	}

	src, readVersion, err := readFileWithVersion(absPath)
	if err != nil {
		return err
	}

	lang, ok := idx.effectiveLanguage(absPath, src)
	if !ok {
		return nil
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return nil
	}

	// Honour the size cap on the incremental path too: an over-cap
	// file gets a synthetic skip node, not a parse — matching the
	// bulk IndexCtx walk. This IS a successful result, so it evicts the
	// prior state and installs the synthetic node, same as before.
	if maxSize := idx.config.MaxFileSize; maxSize > 0 && int64(len(src)) > maxSize {
		n := sizeSkipNode(skippedFile{
			relPath: relPath, lang: lang, size: int64(len(src)),
		}, maxSize)
		idx.applyRepoPrefix([]*graph.Node{n}, nil)
		evictExisting()
		idx.graph.AddBatch([]*graph.Node{n}, nil)
		if !idx.recordFileReadVersion(mtimeKey, absPath, readVersion) {
			return errFileVersionChanged
		}
		return nil
	}

	// Honour the content-admission gate on the incremental path too, so a
	// document over its cap (or a data asset, by default) the cold walk
	// would skip doesn't get parsed back in when the watcher re-indexes it
	// — same synthetic-skip-node treatment as the size cap above.
	if reason, skip := idx.newContentAdmissionGate().skip(lang, int64(len(src))); skip {
		n := contentSkipNode(skippedFile{
			relPath: relPath, lang: lang, size: int64(len(src)), reason: reason,
		})
		idx.applyRepoPrefix([]*graph.Node{n}, nil)
		evictExisting()
		idx.graph.AddBatch([]*graph.Node{n}, nil)
		if !idx.recordFileReadVersion(mtimeKey, absPath, readVersion) {
			return errFileVersionChanged
		}
		return nil
	}

	// Pre-ingestion transforms — same pipeline as the bulk path.
	src = idx.transforms.run(relPath, src)

	// Crash isolation for the incremental path: a file the user just
	// saved that SIGSEGVs the parser is quarantined instead of taking
	// the daemon down with it. The pool is long-lived and shared, so
	// the watcher hot path never forks a worker subprocess per file.
	var pool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		pool, quarantine = idx.sharedParsePool()
	}
	result, prepared := idx.takePreparedExtraction(absPath, relPath, lang, src)
	skipped := false
	if !prepared {
		result, skipped, err = idx.extractFile(pool, quarantine, absPath, relPath, lang, ext, src)
		if quarantine != nil && quarantine.Len() > 0 {
			_ = quarantine.Save()
		}
	}
	// The tree-sitter tree behind result.Tree is C memory the Go GC
	// cannot reclaim, and this function has many early returns below.
	// Release it on every exit path — nothing after this point reads
	// the tree (the incremental contract pass parses its own), so the
	// defer is the whole lifetime.
	defer result.ReleaseTree()
	if result == nil {
		// No usable parse result (transient parse failure, quarantine,
		// timeout). Do NOT evict — the file's prior nodes/edges/search
		// entries stay intact. A stale-but-present file beats an empty
		// one, and the next successful re-index swaps cleanly.
		//
		// The bytes were read successfully (src above), so this is a
		// stable fact about the file's current on-disk content, not a
		// transient "couldn't even open it" failure (that case returns
		// earlier, before relPath/mtime bookkeeping, and deliberately
		// leaves the mtime unrecorded so it keeps retrying). Recording
		// the mtime here keeps a warm restart's HasChangesSinceMtimes
		// from perpetually seeing this one unparseable file as "the
		// repo changed" and routing the whole repo through the
		// expensive shadow re-track path on every restart.
		fresh := idx.recordFileReadVersion(mtimeKey, absPath, readVersion)
		if err == nil && !fresh {
			return errFileVersionChanged
		}
		return err
	}

	// Affected-by snapshot: the symbol shapes and reverse-reference
	// sources the post-resolve signature-delta pass compares against,
	// captured BEFORE eviction — EvictFile drops in-edges from
	// unchanged files and replaces this file's nodes, so neither is
	// recoverable afterwards. A watcher-deferred batch captures the same compact
	// snapshot and merges only its affected-file plan into the outer catch-up;
	// it never retains this parse result across chunks.
	//
	// Also skipped for a quarantined / timed-out / minified-skipped file:
	// its synthetic result carries zero symbols, so the delta would read
	// every prior symbol as removed and fan out to re-resolve the whole
	// reverse graph on a transient parse failure. A failure that yields no
	// symbols is not the same as a symbol genuinely deleted from source.
	var abSnap *affectedBySnapshot
	var reuseIdx map[reuseKey]*reuseVal
	var priorUnresolved []*graph.Edge
	deferredResolverCatchup := markerBatch != nil && markerBatch.deferResolverCatchup
	if (resolve || deferredResolverCatchup) && !idx.deferGlobalPasses && !skipped {
		snapshotStarted := time.Now()
		abSnap = idx.snapshotAffectedBy(graphPath)
		// Snapshot the file's outgoing edges before eviction: resolved ones so
		// the re-parse recovers unchanged resolutions (reuseIdx), and the ones
		// still unresolved so the forward pass can skip re-trying them
		// (priorUnresolved). Together this makes a save re-resolve only the
		// references it actually changed instead of the whole file.
		reuseIdx, priorUnresolved = captureIncrementalState(idx.graph, graphPath)
		snapshotDuration = time.Since(snapshotStarted)
	}

	// We hold a usable result: evict the old state now, then add the
	// new — the window where the file has no nodes is just this gap.
	commitStarted := time.Now()
	evictExisting()

	// Coverage extractors (todos, licenses, ownership). A prepared watcher
	// delta already ran this exact pipeline; reusing it avoids both a second
	// parse and duplicate coverage artifacts.
	if !skipped {
		if !prepared {
			idx.applyCoverageDomains(relPath, lang, src, result)
		}
		// Persist the canonical raw-extraction identity on the file node.
		// Future watcher probes compare against it and skip only when every
		// graph artifact — nodes, edges, locations and metadata — is equal.
		stampExtractionGraphFingerprint(result)
	}

	idx.applyRepoPrefix(result.Nodes, result.Edges)

	// Reuse prior resolutions for edges whose source-side shape is unchanged
	// (the common case on a small edit), so the resolver below only handles
	// genuinely-new references instead of re-resolving the whole file. The
	// about-to-be-added node IDs let the reuse recover same-file targets that
	// eviction removed and this AddBatch re-adds under identical IDs — without
	// them a same-file call's resolution + tier is lost to a full re-resolve on
	// every structural save.
	newNodeIDs := make(map[string]struct{}, len(result.Nodes))
	for _, n := range result.Nodes {
		if n != nil {
			newNodeIDs[n.ID] = struct{}{}
		}
	}
	if reused := applyResolvedOutEdges(idx.graph, result.Edges, reuseIdx, newNodeIDs); reused > 0 {
		idx.logger.Debug("indexer: reused prior resolutions",
			zap.Int("edges", reused), zap.String("file", graphPath))
	}

	// Content (incremental): clear this file's prior content rows, then
	// re-stream + lean — mirrors the full-index per-file path so an edited
	// content file leaves no stale rows and doesn't revert to full text on
	// the node.
	idx.replaceContentSections(graphPath, result.Nodes, false)

	idx.graph.AddBatch(result.Nodes, result.Edges)
	idx.persistConstValues(result)
	idx.persistFileMeta(relPath, src, result)

	// Add new symbols to search index. shouldIndexForSearch enforces
	// the same SkipSearch filter used by the bulk and upgrade paths.
	// When the backing store implements graph.SymbolSearcher we
	// also mirror each upsert into its native FTS, so an
	// incremental reindex doesn't fall out of sync with the
	// bulk-built corpus.
	batcher, _ := idx.graph.(graph.SymbolFTSBatchUpserter)
	var ftsItems []graph.SymbolFTSItem
	if batcher != nil {
		ftsItems = make([]graph.SymbolFTSItem, 0, len(result.Nodes))
	}
	for _, n := range result.Nodes {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		idx.search.Add(n.ID, searchIndexFields(n, idx.projectName)...)
		if batcher != nil {
			ftsItems = append(ftsItems, graph.SymbolFTSItem{
				NodeID: n.ID,
				Tokens: ftsTokensFor(n, idx.projectName),
			})
		}
	}
	if len(ftsItems) > 0 {
		if err := batcher.BatchUpsertSymbolFTS(ftsItems); err != nil {
			idx.logger.Debug("indexer: backend FTS batch upsert failed",
				zap.Int("symbols", len(ftsItems)),
				zap.Error(err))
		}
	}
	commitDuration = time.Since(commitStarted)

	if resolve {
		// Forward pass (this file's outgoing references) plus the
		// reverse pass binding callers in OTHER files that reference a
		// symbol (re)defined here — a symbol newly defined or changed
		// here leaves callers elsewhere pointing at the unresolved stub
		// restubIncomingRefs left when the prior concrete node was
		// evicted. Scoped to this file's names — not a whole-graph
		// ResolveAll — and run as one combined pass so the resolver's
		// per-pass indexes are built once per save, not twice.
		//
		// Skip re-resolving references that were already unresolved before the
		// edit and are unchanged: they stay parked on their stubs for the
		// incoming pass, so a small edit to a reference-heavy file no longer
		// re-runs the candidate cascade on thousands of stdlib/external calls.
		idx.resolver.SetIncrementalSkip(priorUnresolved)
		resolveStarted := time.Now()
		idx.resolver.ResolveFileAndIncoming(graphPath)
		resolveDuration = time.Since(resolveStarted)
		idx.resolver.SetIncrementalSkip(nil)
		// CPG-lite dataflow placeholders for this file: inter-
		// procedural callees may have just been lifted by
		// ResolveFile, so re-run the dataflow materialisation pass
		// to keep arg_of / returns_to edges in sync with the
		// freshly resolved EdgeCalls graph. Scoped to this file's
		// out-edges — not a whole-graph AllEdges scan — so an
		// incremental edit stays O(file), not O(all edges).
		idx.materializeDataflowParamsForFile(graphPath, result.Edges)
		// Clone detection. EvictFile above removed this file's
		// EdgeSimilarTo edges in both directions. When the incremental
		// clone index is built, re-bank just this file's bodies
		// (EvictFuncs the old ids, UpdateFuncs the fresh nodes) — an
		// O(edited file) update that restores the same edge set the
		// whole-graph pass would. Until a batch/global pass has seeded
		// the index (built=false) we fall back to the full recompute.
		// Skipped under deferGlobalPasses — a batch caller (ReconcileAll,
		// warmup) runs the global pass once at the end.
		if !idx.deferGlobalPasses {
			newCloneFuncs := cloneFuncNodes(result.Nodes)
			// A file with no old or new function nodes cannot change clone
			// topology. In particular, do not make the first JSON/Markdown/data
			// watcher event after a warm load seed the graph-wide clone index.
			// The first real function edit still performs the normal one-time
			// rebuild before its incremental update.
			if len(oldFuncIDs) > 0 || len(newCloneFuncs) > 0 {
				if idx.cloneIndex != nil && idx.cloneIndex.Ready() {
					idx.cloneIndex.EvictFuncs(idx.graph, oldFuncIDs)
					idx.cloneIndex.UpdateFuncs(idx.graph, idx.repoPrefix, newCloneFuncs, idx.cloneThreshold())
				} else if idx.cloneIndex != nil {
					// Never turn the first edit after a warm restart into a hidden
					// whole-repo clone scan. Mark the clone view incomplete; an
					// explicit clone-consuming/global pass rebuilds it later.
					idx.cloneIndex.MarkPending()
				}
			}
		}
		// in-memory backend. Skipped for a quarantined / timed-out /
		// minified file: its synthetic result yields no facts, so a
		// delete-then-set would durably drop the file's real facts on a
		// transient parse failure and leave them gone until a clean
		// reparse — abSnap is nil here too, so the affected-by pass that
		// would also fan out is already a no-op.
		if !skipped {
			refFactsStarted := time.Now()
			idx.persistRefFactsForFiles([]string{graphPath})
			refFactsDuration = time.Since(refFactsStarted)
			// Affected-by re-resolution: if this save changed a symbol's
			// signature or kind, or removed a symbol, re-resolve the files
			// that referenced it — bounded, synchronous, and gated on the
			// signature delta so a body-only edit fans out to nothing.
			affectedStarted := time.Now()
			idx.reresolveAffectedBy(graphPath, abSnap, result.Nodes)
			affectedDuration = time.Since(affectedStarted)

			// Incremental semantic enrichment for this single file. Mirrors the
			// full-index EnrichAll call but scoped to the saved file, so a
			// watcher save re-runs the type resolvers (and any watch-enabled
			// LSP / compiler provider) instead of leaving the file's edges at
			// their pre-enrichment tier until the next full reindex. This caller
			// enforces Config.EnrichOnWatch; deferred batches use EnrichFiles.
			providersPresent := idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders()
			watchEnrichment := providersPresent && !idx.deferGlobalPasses && idx.semanticMgr.EnrichesOnWatch()
			reEnriched := false
			if watchEnrichment {
				enrichStarted := time.Now()
				if _, err := idx.semanticMgr.EnrichFile(idx.graph, idx.rootPath, graphPath); err != nil {
					idx.logger.Debug("indexer: incremental semantic enrichment failed",
						zap.String("file", graphPath),
						zap.Error(err))
				} else {
					reEnriched = true
				}
				enrichDuration = time.Since(enrichStarted)
			}
			// Record whether this live re-parse left the file below the
			// enrichment tier: providers exist (so there IS an lsp/ast tier to
			// fall short of) but the save did not re-run enrichment. When set,
			// find_usages / get_callers flag their default text_matched
			// suppression as re-verification-pending so a hidden-but-real usage
			// is diagnosable rather than silently dropped. Cleared when
			// enrichment did re-run for the file.
			pendingReparseEnrichment := providersPresent && !reEnriched
			if markerBatch == nil {
				idx.setReparsePendingEnrichment(graphPath, pendingReparseEnrichment)
			} else if markerBatch.add(graphPath, pendingReparseEnrichment) {
				idx.flushReparsePendingEnrichment(markerBatch)
			}
		}
	} else if deferredResolverCatchup && !skipped {
		// Exceptional files that could not enter the prepared structural batch
		// still defer every resolution-dependent tail to the watcher batch's one
		// outer catch-up. Preserve the compact pre-evict affected-by frontier, but
		// do not retain parse trees or run a per-file resolver/dataflow/fact pass.
		markerBatch.mergeDeferredAffected(idx.planAffectedByStages([]*incrementalBatchStage{{
			graphPath: graphPath,
			result:    result,
			abSnap:    abSnap,
		}}))
		providersPresent := idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders()
		if markerBatch.add(graphPath, providersPresent) {
			idx.flushReparsePendingEnrichment(markerBatch)
		}
	}

	// Update mtime for this file, keyed by mtimeKey (the relKey slash
	// form), NOT relPath — relPath is the OS-native graph key, whereas
	// the fileMtimes map and IsStale / TrackedFileState all key on the
	// slash form.
	if !idx.recordFileReadVersion(mtimeKey, absPath, readVersion) {
		return errFileVersionChanged
	}
	if elapsed := time.Since(indexFileStarted); elapsed >= 2*time.Second {
		idx.logger.Warn("indexer: slow incremental stages",
			zap.String("file", graphPath),
			zap.Duration("total", elapsed),
			zap.Duration("snapshot_capture", snapshotDuration),
			zap.Duration("commit_fts", commitDuration),
			zap.Duration("resolver", resolveDuration),
			zap.Duration("ref_facts", refFactsDuration),
			zap.Duration("affected_by", affectedDuration),
			zap.Duration("enrich_file", enrichDuration),
		)
	}

	return nil
}

// recordFileMtime restamps the recorded mtime for relPath (a canonical
// relKey, not repo-prefixed) from absPath's current on-disk mtime, both
// in the in-memory map and — when the backend supports it — the store's
// FileMtime sidecar. Per-file write is ~1ms on the on-disk backend;
// trivial under steady-state file-watcher load. A missing/unstatable
// file is a no-op; a persist error is logged but non-fatal, since the
// in-memory map (which the current process trusts) is already correct.
func (idx *Indexer) recordFileMtime(relPath, absPath string) {
	info, err := os.Stat(absPath)
	if err != nil {
		return
	}
	idx.recordFileMtimeValue(relPath, info.ModTime().UnixNano())
}

// recordFileReadVersion records only the version whose bytes were parsed. If
// the file changed after the stable read, the prior receipt remains stale so a
// queued event or the poller retries instead of treating newer bytes as done.
func (idx *Indexer) recordFileReadVersion(relPath, absPath string, version fileReadVersion) bool {
	if !version.valid {
		return false
	}
	current, err := os.Stat(absPath)
	if err != nil || !sameFileVersion(version.info, current) {
		return false
	}
	idx.recordFileMtimeValue(relPath, version.mtime)
	return true
}

func (idx *Indexer) recordFileMtimeValue(relPath string, mtime int64) {
	idx.mtimeMu.Lock()
	idx.fileMtimes[relPath] = mtime
	idx.mtimeMu.Unlock()
	if w, ok := idx.graph.(graph.FileMtimeWriter); ok {
		if err := w.BulkSetFileMtimes(idx.repoPrefix, map[string]int64{relPath: mtime}); err != nil {
			idx.logger.Warn("persist file mtime failed",
				zap.String("repo", idx.repoPrefix), zap.String("file", relPath), zap.Error(err))
		}
	}
}

// StructuralSymbols parses a file from its current on-disk content and
// returns the structural symbols it defines — functions, methods,
// types, interfaces, constants, variables, fields, enum members — and
// nothing else. It is a read-only probe: the graph and the search
// index are left completely untouched, no mtime is stamped, and no
// resolver runs. The watcher uses it to decide whether a save is
// structurally inert (a comment / whitespace / config-value edit that
// changes no symbol) and can skip the destructive evict + reindex.
//
// The second return reports whether the file was parseable at all: a
// file with no detectable language, an over-cap file, or a read error
// yields (nil, false). A genuinely empty source file yields
// (empty-slice, true).
func (idx *Indexer) StructuralSymbols(filePath string) ([]*graph.Node, bool) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, false
	}
	relPath, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil {
		relPath = filePath
	}

	src, err := os.ReadFile(absPath)
	if err != nil {
		return nil, false
	}

	lang, ok := idx.effectiveLanguage(absPath, src)
	if !ok {
		return nil, false
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return nil, false
	}

	// An over-cap file is never structurally parsed on the indexing
	// path either (it gets a synthetic skip node), so the watcher
	// cannot prove inertness for it — fall through to a real reindex.
	if maxSize := idx.config.MaxFileSize; maxSize > 0 && int64(len(src)) > maxSize {
		return nil, false
	}

	// Same pre-ingestion transforms as indexFile so the probe parses
	// exactly the bytes the real index pass would.
	src = idx.transforms.run(relPath, src)

	var pool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		pool, quarantine = idx.sharedParsePool()
	}
	result, skipped, err := idx.extractFile(pool, quarantine, absPath, relPath, lang, ext, src)
	if quarantine != nil && quarantine.Len() > 0 {
		_ = quarantine.Save()
	}
	// This probe only ever reads result.Nodes, so the parse tree is
	// dead the moment extraction returns. Release it on both exits —
	// the inertness probe runs on every watcher event, so a retained
	// tree here leaks C memory at save frequency.
	defer result.ReleaseTree()
	// A skipped (quarantined / timed-out) file produces only a
	// synthetic node — not the real symbol set — so inertness cannot
	// be proven and the caller must reindex normally.
	if result == nil || skipped || err != nil {
		return nil, false
	}

	out := make([]*graph.Node, 0, len(result.Nodes))
	for _, n := range result.Nodes {
		if isStructuralKind(n.Kind) {
			out = append(out, n)
		}
	}
	return out, true
}

// isStructuralKind reports whether a node kind represents a structural
// code symbol — the kinds whose presence, name, or signature define a
// file's graph shape. File and import nodes (graph bookkeeping),
// params, closures, and the coverage-domain kinds (todos, licenses,
// strings, …) are deliberately excluded: a change confined to those
// does not alter the structural graph the watcher cares about.
func isStructuralKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindVariable, graph.KindConstant,
		graph.KindField, graph.KindEnumMember:
		return true
	default:
		return false
	}
}

// ResolveAll re-runs the global cross-file reference resolver and
// interface-implementation inference. Exposed for batch paths that
// defer per-file resolver work until the end of a batch.
func (idx *Indexer) ResolveAll() {
	idx.populateCppIncludeDirs(true)
	idx.resolver.ResolveAll()
	idx.resolver.InferImplements()
	idx.resolver.InferOverrides()
	// Framework dynamic-dispatch synthesis (gRPC / Temporal / event
	// channels / native bridges) depends on InferImplements (the
	// interface-satisfaction signals) having run first.
	resolver.RunFrameworkSynthesizers(idx.graph)
	// External-call placeholder synthesis (opt-in) — runs after the
	// resolver and stub passes so only genuinely un-indexed external
	// targets remain to materialise.
	resolver.SynthesizeExternalCalls(idx.graph, idx.externalCallSynthesisEnabled())
	resolver.ResolveSpeculativeDispatch(idx.graph, idx.speculativeDispatchEnabled())
	// CPG-lite dataflow rewriting must run after the call resolver
	// has lifted unresolved:: targets; arg_of edges then point at
	// real function/method nodes whose param nodes can be found,
	// and returns_to placeholders join cleanly against the
	// now-resolved EdgeCalls edge at the same caller+line.
	idx.materializeDataflowParams()

	// Seed the durable reference-facts sidecar from the fully-resolved graph
	// (no-op on the in-memory backend).
	idx.persistAllRefFacts()
}

// EvictFile removes all nodes and edges belonging to filePath.
//
// filePath may arrive in any Unicode form — the git watcher derives it
// from `git diff` output (NFC), while an FSEvents-driven evict carries
// the filesystem's form (NFD on macOS). relKey folds both to the
// canonical NFC key the graph indexed the file under, so the eviction
// actually finds the file's nodes rather than silently no-opping and
// leaving a stale subtree behind.
func (idx *Indexer) EvictFile(filePath string) (int, int) {
	absPath := filePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(idx.rootPath, filePath)
	}
	// graphRelKey (OS-native + NFC), not relKey (slash + NFC): the graph
	// stores nodes under OS-native separators, so a slash-form lookup
	// would miss them on Windows and leave a stale subtree behind.
	relPath := idx.graphRelKey(absPath)
	// In multi-repo mode, the graph stores prefixed file paths.
	graphPath := idx.prefixPath(relPath)
	// Remove from search index.
	for _, n := range idx.graph.GetFileNodes(graphPath) {
		idx.removeFromSearch(n)
	}
	idx.restubIncomingRefs(graphPath)
	idx.evictEnrichment(graphPath)
	idx.deleteRefFactsForFiles(idx.repoPrefix, []string{graphPath})
	return idx.graph.EvictFile(graphPath)
}

// ReresolveFileScoped forces the scoped re-resolution + LSP re-verify a normal
// IndexFile performs, WITHOUT re-parsing and WITHOUT the IsStale gate — used by
// the watcher's shape-degradation self-heal, where the file's mtime is already
// current (IndexFile just ran) yet its resolved edges came out degraded, so a
// plain IncrementalReindexPaths would stale-gate it out. Re-runs only this
// file's forward + incoming resolve and, when a watch-enabled provider is
// wired, its incremental enrichment. O(file), no whole-graph pass. No-op when
// the file has no nodes (evicted since it was enqueued).
func (idx *Indexer) ReresolveFileScoped(filePath string) error {
	absPath := filePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(idx.rootPath, filePath)
	}
	// graphRelKey (OS-native + NFC): the graph keys nodes under OS-native
	// separators, so a relKey slash-form graphPath would miss them on
	// Windows and wrongly report the file as evicted.
	graphPath := idx.prefixPath(idx.graphRelKey(absPath))
	if len(idx.graph.GetFileNodes(graphPath)) == 0 {
		return nil // file gone / evicted; nothing to re-resolve
	}
	idx.resolver.ResolveFileAndIncoming(graphPath)
	providersPresent := idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders()
	watchEnrichment := providersPresent && idx.semanticMgr.EnrichesOnWatch()
	reEnriched := false
	if watchEnrichment {
		if _, err := idx.semanticMgr.EnrichFile(idx.graph, idx.rootPath, graphPath); err != nil {
			idx.logger.Debug("indexer: forced scoped enrichment failed",
				zap.String("file", graphPath), zap.Error(err))
		} else {
			reEnriched = true
		}
	}
	// Keep the find_usages staleness marker consistent with the enrichment
	// that actually ran during this forced re-resolve (mirrors indexFile).
	idx.setReparsePendingEnrichment(graphPath, providersPresent && !reEnriched)
	return nil
}

// restubIncomingRefs rewrites every resolved reference edge that points
// INTO a symbol of graphPath from a surviving (other-file) source back
// to an `unresolved::<Name>` stub, in place, BEFORE the file's nodes are
// evicted. Graph eviction otherwise drops those incoming caller edges
// wholesale (it removes the edge from the surviving source's out-edge
// bucket) and nothing recreates them until a cold reindex — so editing
// or deleting a definition silently strips its callers' edges and
// find_usages / get_callers go blank. Re-stubbing detaches the edges
// from the soon-to-be-evicted nodes so they survive as pending stubs;
// ResolveIncomingForFile (after a re-index) rebinds them to the file's
// fresh symbols, or they stay unresolved — the correct state once the
// symbol is gone. Only name-resolvable reference kinds are re-stubbed;
// structural and enrichment edges are left to be dropped. Backend-
// agnostic: GetInEdges + ReindexEdges are the same Store primitives the
// resolver uses, so this behaves identically on the in-memory and disk
// stores.
// evictEnrichment drops the per-node enrichment sidecar rows (churn,
// coverage, release, blame — change A) for a file's nodes on the
// delete/rename paths only, so a removed file leaves no orphan
// enrichment. Capability-gated. A modify re-indexes the same node IDs
// (enrichment stays valid) so it is NOT cascaded there.
func (idx *Indexer) evictEnrichment(graphPath string) {
	nodes := idx.graph.GetFileNodes(graphPath)
	if len(nodes) == 0 {
		return
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	if w, ok := idx.graph.(graph.ChurnEnrichmentWriter); ok {
		_ = w.DeleteChurn(ids)
	}
	if w, ok := idx.graph.(graph.CoverageEnrichmentWriter); ok {
		_ = w.DeleteCoverage(ids)
	}
	if w, ok := idx.graph.(graph.ReleaseEnrichmentWriter); ok {
		_ = w.DeleteReleases(ids)
	}
	if w, ok := idx.graph.(graph.BlameEnrichmentWriter); ok {
		_ = w.DeleteBlame(ids)
	}
}

func (idx *Indexer) restubIncomingRefs(graphPath string) {
	nodes := idx.graph.GetFileNodes(graphPath)
	if len(nodes) == 0 {
		return
	}
	evicted := make(map[string]struct{}, len(nodes))
	refIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		evicted[n.ID] = struct{}{}
		if n.Name != "" && graph.IsReferenceableSymbol(n.Kind) {
			refIDs = append(refIDs, n.ID)
		}
	}
	if len(refIDs) == 0 {
		return
	}
	// Fetch the whole changed file's incoming adjacency in one bounded store
	// operation. On SQLite this is chunked under the parameter limit; more
	// importantly, it avoids one transaction/query per definition in the file.
	inEdges := idx.graph.GetInEdgesByNodeIDs(refIDs)
	var batch []graph.EdgeReindex
	for _, n := range nodes {
		if n == nil || n.Name == "" || !graph.IsReferenceableSymbol(n.Kind) {
			continue
		}
		stub := graph.UnresolvedMarker + n.Name
		for _, e := range inEdges[n.ID] {
			if e == nil || !graph.IsResolvableRefEdge(e.Kind) {
				continue
			}
			if _, fromEvicted := evicted[e.From]; fromEvicted {
				continue // intra-file edge: the source is evicted too
			}
			if graph.IsUnresolvedTarget(e.To) {
				continue // already a pending stub
			}
			oldTo := e.To
			// Stash + clear the edge's resolved provenance before restubbing:
			// an `unresolved::` stub must not keep advertising a resolved
			// tier. The incoming-resolve pass restores it verbatim if the stub
			// rebinds to the same target (idempotent re-parse); a deleted or
			// moved target leaves the stub honestly unresolved.
			graph.StashRestubProvenance(e)
			e.To = stub
			batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(batch) > 0 {
		idx.graph.ReindexEdges(batch)
	}
}

// embeddingDimsOrDefault returns the embedder's reported vector width,
// falling back to a neutral placeholder only when the provider cannot
// state its width yet (Dimensions() == 0, the APIProvider-before-first-
// call case). The fallback is never persisted: buildSearchIndex and
// ImportVectorIndex both overwrite it with the true width taken from a
// real vector / the cached header. Kept as a named helper so the
// vector-dimension default has one definition instead of a scattered
// magic number.
func embeddingDimsOrDefault(p embedding.Provider) int {
	if p == nil {
		return 0
	}
	if d := p.Dimensions(); d > 0 {
		return d
	}
	// Provider has not committed to a width. 384 matches the default
	// transformer backend (MiniLM-L6-v2); a static GloVe provider
	// always reports its real 50 and never reaches this branch.
	return 384
}

// collectEmbedTexts walks the nodes and produces the parallel texts /
// ids slices the embedding pass consumes, plus a chunkMap recording
// which synthetic IDs are chunks of which symbol.
//
// A symbol whose source span exceeds the configured chunk threshold is
// read from disk and split into AST windows by embedding.ChunkSymbol;
// each window contributes one text (its body, prefixed with the
// symbol's kind + name for a little lexical grounding) under a
// synthetic ID "<symbolID>#chunkK", and chunkMap[syntheticID] = symbolID.
// A symbol below the threshold — or one whose file can't be read —
// contributes a single metadata text under its own ID, exactly as the
// pre-chunking pipeline did. The returned skipped count is the number
// of nodes dropped by the SkipEmbed rules.
func (idx *Indexer) collectEmbedTexts(nodes []*graph.Node) (texts []string, ids []string, chunkMap map[string]string, skipped int) {
	chunkMap = make(map[string]string)
	opts := idx.embedChunkOpts
	threshold := opts.ThresholdLines
	if threshold <= 0 {
		threshold = embedding.DefaultChunkThresholdLines
	}
	// fileCache memoizes one os.ReadFile per source file — many symbols
	// share a file, and the chunker only needs the bytes once.
	fileCache := make(map[string][]byte)
	readFile := func(graphPath string) []byte {
		if cached, ok := fileCache[graphPath]; ok {
			return cached
		}
		var data []byte
		if abs := idx.ResolveFilePath(graphPath); abs != "" {
			if b, err := os.ReadFile(abs); err == nil {
				data = b
			}
		}
		fileCache[graphPath] = data // cache misses too (nil) — don't re-stat
		return data
	}

	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		// CONTENT section bodies are served by the content index, not the
		// vector store — excluding them keeps the embed-text count (and the
		// 100k auto-disable check) code-only, so a content-heavy repo no
		// longer drowns the embedding pass in hundreds of thousands of
		// section texts.
		if isContentNode(n) {
			skipped++
			continue
		}
		if config.ShouldSkipEmbed(idx.config.SkipEmbed, n.Language, string(n.Kind)) {
			skipped++
			continue
		}
		sig, _ := n.Meta["signature"].(string)
		metaText := fmt.Sprintf("%s %s %s %s", n.Kind, n.Name, sig, n.FilePath)

		// Decide whether to sub-chunk: the symbol must declare a
		// multi-line span past the threshold and its file must be
		// readable. Anything else falls back to the metadata vector.
		span := n.EndLine - n.StartLine + 1
		body := extractSymbolBody(n, readFile, threshold)
		if span <= threshold || len(body) == 0 {
			texts = append(texts, metaText)
			ids = append(ids, n.ID)
			continue
		}

		windows := embedding.ChunkSymbol(body, n.Language, n.ID, opts)
		if len(windows) <= 1 {
			// The chunker decided one window was enough (short body,
			// no splitter, parse failure) — embed it as a single
			// metadata + body vector under the symbol's own ID.
			texts = append(texts, metaText+" "+windows[0].Text)
			ids = append(ids, n.ID)
			continue
		}
		for _, w := range windows {
			chunkID := fmt.Sprintf("%s#chunk%d", n.ID, w.WindowIndex)
			texts = append(texts, fmt.Sprintf("%s %s %s", n.Kind, n.Name, w.Text))
			ids = append(ids, chunkID)
			chunkMap[chunkID] = n.ID
		}
	}
	return texts, ids, chunkMap, skipped
}

// extractSymbolBody returns the source text of a symbol's span, read
// from its file via readFile and sliced by the node's 1-based
// StartLine..EndLine. Returns nil when the file is unreadable, the
// line range is unusable, or the symbol is at or below the threshold
// (small symbols never need their body — the caller embeds metadata).
func extractSymbolBody(n *graph.Node, readFile func(string) []byte, threshold int) []byte {
	if n.StartLine <= 0 || n.EndLine < n.StartLine {
		return nil
	}
	if n.EndLine-n.StartLine+1 <= threshold {
		return nil
	}
	data := readFile(n.FilePath)
	if len(data) == 0 {
		return nil
	}
	return sliceLines(data, n.StartLine, n.EndLine)
}

// sliceLines returns the bytes of the 1-based inclusive line range
// [start,end] of src. An out-of-range request is clamped; an empty
// result is returned for a range that lands entirely past EOF.
func sliceLines(src []byte, start, end int) []byte {
	if start < 1 {
		start = 1
	}
	line := 1
	startByte := -1
	endByte := len(src)
	for i := 0; i < len(src); i++ {
		if line == start && startByte < 0 {
			startByte = i
		}
		if src[i] == '\n' {
			line++
			if line == end+1 {
				endByte = i + 1 // include the trailing newline
				break
			}
		}
	}
	if startByte < 0 {
		return nil
	}
	if endByte < startByte {
		endByte = len(src)
	}
	return src[startByte:endByte]
}

// defaultEmbedAPIConcurrency bounds parallel embedding requests
// against an API-backed embedder when embedding.api_concurrency is
// unset. Four is a conservative default that overlaps round-trips
// without tripping typical hosted-API rate limits.
const defaultEmbedAPIConcurrency = 4

// embedChunkBatch is one unit of work for the embedding pool: the
// texts of one chunk plus the index that fixes where its vectors land
// in the result slice. Carrying the index makes completion order
// irrelevant — workers write by index, never append.
type embedChunkBatch struct {
	index int
	texts []string
}

// embedAllChunks embeds every text, returning the vectors in the same
// order as texts. The work is split into batches of batchSize texts.
//
// For an API-backed embedder the batches run through a bounded worker
// pool — a hosted embedding round-trip dominates index time, so
// overlapping requests is a real speedup. In-process embedders
// (Hugot / ONNX / GoMLX / static) serialise on an inference mutex, so
// concurrency buys them nothing and they keep the simple serial path.
//
// The abort-on-any-error contract is preserved in both modes: the
// first batch failure cancels the group and embedAllChunks returns the
// error with no partial result, exactly as the old serial loop did.
// embedFn already layers the deadline-halving retry on top of each
// batch.
func (idx *Indexer) embedAllChunks(
	texts []string,
	batchSize int,
	embedFn func(ctx context.Context, items []string) ([][]float32, error),
) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Split into batches up front so both the serial and parallel
	// paths iterate the same units.
	var batches []embedChunkBatch
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batches = append(batches, embedChunkBatch{index: len(batches), texts: texts[start:end]})
	}

	// Per-batch result slots, pre-sized so workers write by index and
	// completion order never matters.
	results := make([][][]float32, len(batches))

	// Only run the pool for an embedder that declares itself safe and
	// worthwhile to call concurrently — the API-backed provider, where
	// overlapped HTTP round-trips are a real win. In-process backends
	// (Hugot / ONNX / GoMLX) hold an inference mutex, so a pool would
	// only add scheduling overhead; they keep the serial path.
	apiBacked := false
	if c, ok := idx.embedder.(interface{ Concurrent() bool }); ok {
		apiBacked = c.Concurrent()
	}
	concurrency := idx.embedAPIConcurrency
	if concurrency <= 0 {
		concurrency = defaultEmbedAPIConcurrency
	}
	if concurrency > len(batches) {
		concurrency = len(batches)
	}

	if !apiBacked || concurrency <= 1 {
		// Serial path — unchanged behaviour for in-process embedders.
		ctx := context.Background()
		for _, b := range batches {
			vecs, err := embedFn(ctx, b.texts)
			if err != nil {
				return nil, err
			}
			results[b.index] = vecs
		}
		return flattenEmbedResults(results), nil
	}

	// Parallel path — bounded worker pool for an API-backed embedder.
	// A cancellable group context means the first failure stops every
	// in-flight worker; the indexer's existing per-batch retry still
	// runs underneath embedFn.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan embedChunkBatch)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel() // abort siblings on the first error
		})
	}

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				if ctx.Err() != nil {
					return // group already aborted
				}
				vecs, err := embedFn(ctx, b.texts)
				if err != nil {
					fail(err)
					return
				}
				// Write into the pre-sized slot — no shared append, so
				// no lock and order is fixed by b.index.
				results[b.index] = vecs
			}
		}()
	}
	idx.logger.Info("embedding vector index with a concurrent API pool",
		zap.Int("workers", concurrency),
		zap.Int("batches", len(batches)))

	for _, b := range batches {
		if ctx.Err() != nil {
			break // stop feeding once aborted
		}
		select {
		case jobs <- b:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return flattenEmbedResults(results), nil
}

// flattenEmbedResults concatenates per-batch vector slices back into a
// single slice aligned with the original texts order.
func flattenEmbedResults(results [][][]float32) [][]float32 {
	total := 0
	for _, r := range results {
		total += len(r)
	}
	out := make([][]float32, 0, total)
	for _, r := range results {
		out = append(out, r...)
	}
	return out
}

// buildSearchIndex populates the search backend from the current graph.
// When an embedder is set, also builds a vector index and wraps both
// in a HybridBackend with RRF fusion.
//
// In multi-repo mode the search backend is shared across every repo
// (Indexer.search is wired to MultiIndexer.search at construction).
// Re-reading every graph node would mean each freshly-tracked repo pays an
// O(workspace) re-index pass over all
// previously-tracked repos' nodes — quadratic in repo count and the
// dominant cost of warming up a 260-repo workspace. So when this
// indexer carries a non-empty repoPrefix we walk only that repo's
// byRepo bucket; the other repos' entries are already in the shared
// backend from when they were tracked. Single-repo mode uses the store's
// non-content projection with an empty repo namespace.
func (idx *Indexer) buildSearchIndex() {
	// Start every build from a clean vector-build error: the degraded vector
	// paths below set it, and a successful build (or a benign skip / no embedder)
	// leaves it nil, so LastVectorBuildError always reflects the current pass.
	idx.lastVectorBuildErr = nil

	// Code-only enumeration: content (data_class=content) sections live in
	// the content index, never the symbol search or the vector store, so the
	// FTS loop below and collectEmbedTexts both skip them anyway. Fetching
	// the non-content set up front means a content-heavy repo's hundreds of
	// thousands of sections never enter memory here (the disk backend filters
	// them in SQL), instead of being materialised only to be skipped.
	nodes := graph.RepoCodeNodes(idx.graph, idx.repoPrefix)

	// Install the learned sub-word boundary table on the BM25 layer
	// before populating postings, so the optional sparse-ngram
	// tokenizer's split points are data-driven and — crucially —
	// identical on the index path here and the query path later. The
	// table must be in place before the first Add for postings and
	// queries to agree; it is a no-op for non-BM25 backends and a no-op
	// for search overall unless GORTEX_SPARSE_NGRAM is set.
	search.InstallNgramBoundaries(idx.search, search.BuildNgramBoundaries(idx.graph))

	// Build text index. The SkipSearch filter (wired through
	// idx.shouldIndexForSearch) drops config-key-style variable nodes
	// that would only pad the index — see docs on IndexConfig.SkipSearch.
	for _, n := range nodes {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		idx.search.Add(n.ID, searchIndexFields(n, idx.projectName)...)
	}

	// Build vector index if embedder is available.
	if idx.embedder == nil {
		return
	}

	// skipVectorBuild short-circuits the embedding pass: the text index
	// above is fully populated, but a caller (the daemon warmup loop
	// after a snapshot restore) has signalled that the workspace vector
	// index will be supplied separately, so re-embedding here would be
	// wasted work immediately overwritten by the cached index.
	if idx.skipVectorBuild {
		return
	}

	// Provisional dimensionality: trust the embedder's own report.
	// A provider that can't state its width yet (an APIProvider before
	// its first call returns 0) gets a neutral placeholder — the value
	// is overwritten below from the first real vector, so it never
	// reaches the persisted index. The old hard-coded 300 was wrong for
	// the default static GloVe provider (50d) and misrepresented the
	// index width in the interim; deriving from Dimensions() keeps it
	// honest for every provider.
	dims := embeddingDimsOrDefault(idx.embedder)

	// Collect texts and IDs for batch embedding. Nodes matching
	// Semantic.SkipEmbed (e.g. CSS custom properties, terraform blocks,
	// YAML/TOML/shell config vars) are kept in the text index but
	// excluded from the vector index — embedding them is pure cost
	// with no semantic payoff and on big monorepos dominates RAM.
	//
	// A symbol whose source span exceeds the chunk threshold is split
	// into AST windows: each window is embedded as its own vector under
	// a synthetic ID ("<symbolID>#chunkK"), and chunkMap records the
	// chunk → parent mapping so query-time de-chunking maps a chunk hit
	// back to the symbol. A small symbol stays a single metadata-only
	// vector under its own ID. chunkMap is empty when nothing was split.
	texts, ids, chunkMap, skipped := idx.collectEmbedTexts(nodes)
	if skipped > 0 {
		idx.logger.Info("skipped embedding for low-value nodes",
			zap.Int("count", skipped),
			zap.Int("embedded", len(texts)))
	}

	if len(texts) == 0 {
		return
	}

	// Embedding scaling guards. Hard-cap the vector index for repos
	// big enough that the cost no longer pays off — BM25 alone is a
	// fine fallback and an OOM during initial index is much worse than
	// missing the semantic boost. Chunk the EmbedBatch calls so any
	// single API request stays small (matters for hosted embedders
	// with per-request token limits).
	//
	// embedChunkTimeout is generous because ONNX inference (Hugot) has
	// long tail latency: a 60s budget made one in ~30 chunks miss its
	// deadline, which under the old fail-fast policy threw away every
	// already-embedded chunk and silently degraded to BM25 with no
	// signal to the user. 5 minutes covers observed worst-case spikes
	// without changing steady-state behaviour. On a true hang the
	// caller can still cancel the parent indexing call.
	const (
		defaultEmbedMaxSymbols = 100_000
		embedChunkSize         = 500
		embedChunkTimeout      = 5 * time.Minute
	)

	// The cap is over the embeddable-text count, which with AST
	// sub-chunking can exceed the symbol count. embedding.max_symbols
	// overrides the built-in default for users with memory headroom.
	embedMaxSymbols := defaultEmbedMaxSymbols
	if idx.embedMaxSymbols > 0 {
		embedMaxSymbols = idx.embedMaxSymbols
	}
	// Env override wins over both the default and the config-wired cap —
	// a reliable knob independent of the config plumbing. Lets an operator
	// lift the vector-index size guard for a large repo without editing
	// (or debugging) the layered config.
	if env := os.Getenv("GORTEX_EMBEDDINGS_MAX_SYMBOLS"); env != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(env)); err == nil && n > 0 {
			embedMaxSymbols = n
		}
	}
	if len(texts) > embedMaxSymbols {
		idx.logger.Warn("vector index disabled — embedding text count exceeds threshold",
			zap.Int("texts", len(texts)),
			zap.Int("threshold", embedMaxSymbols),
			zap.String("hint", "BM25 text search remains active; raise embedding.max_symbols if you have memory headroom"))
		idx.lastVectorBuildErr = fmt.Errorf("embedding text count %d exceeds threshold %d (raise embedding.max_symbols)", len(texts), embedMaxSymbols)
		return
	}

	// embedWithRetry runs one chunk under ctx; on a context-deadline
	// failure it splits the chunk in half and retries each half once.
	// A single slow batch shouldn't throw away every already-embedded
	// chunk and silently demote the backend to BM25. ctx is the group
	// context, so once one chunk fails everywhere the in-flight retries
	// here see the cancellation and stop too.
	var embedWithRetry func(ctx context.Context, items []string) ([][]float32, error)
	embedWithRetry = func(ctx context.Context, items []string) ([][]float32, error) {
		chunkCtx, cancel := context.WithTimeout(ctx, embedChunkTimeout)
		out, err := idx.embedder.EmbedBatch(chunkCtx, items)
		cancel()
		if err == nil {
			return out, nil
		}
		// Only retry on deadline-style failures; auth/protocol errors
		// won't get better with smaller batches. A cancellation from
		// the group context (a sibling chunk already failed) is not a
		// retry case either.
		if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || len(items) <= 1 {
			return nil, err
		}
		idx.logger.Warn("embed chunk timed out, retrying with halved batch",
			zap.Int("size", len(items)),
			zap.Error(err))
		mid := len(items) / 2
		left, lerr := embedWithRetry(ctx, items[:mid])
		if lerr != nil {
			return nil, lerr
		}
		right, rerr := embedWithRetry(ctx, items[mid:])
		if rerr != nil {
			return nil, rerr
		}
		return append(left, right...), nil
	}

	// Embed every chunk. For an API-backed embedder the chunks are run
	// through a bounded worker pool (a hosted round-trip dominates
	// indexing time, so overlapping requests is a real win); local
	// in-process backends serialise on an inference mutex, so they keep
	// the simple serial path. Either way the abort-on-any-error
	// contract holds — one chunk failure means no vector index ships.
	vectors, err := idx.embedAllChunks(texts, embedChunkSize, embedWithRetry)
	if err != nil {
		// A partial vector index would mis-score later queries (some
		// symbols semantically findable, others not) — bail to
		// text-only search rather than ship an inconsistent hybrid
		// backend.
		idx.logger.Warn("vector index aborted on chunk failure", zap.Error(err))
		idx.lastVectorBuildErr = fmt.Errorf("chunk embedding failed: %w", err)
		return
	}

	// Detect actual dimensions from first vector.
	if len(vectors) > 0 && len(vectors[0]) > 0 {
		dims = len(vectors[0])
	}

	vecBackend := search.NewVector(dims)
	// VectorSearcher capability bridging: if the underlying store
	// has a native HNSW, install it as the in-process backend's
	// delegate — Add becomes a no-op, Search forwards to the
	// engine, and we don't allocate `dim × 4 × N` bytes of heap
	// for a parallel in-process HNSW. The indexer still drives
	// the writes (BulkUpsertEmbeddings below) so the engine
	// index lands with the same corpus the in-process one would
	// have built.
	vecSearcher, _ := idx.graph.(graph.VectorSearcher)
	if vecSearcher != nil {
		vecBackend.SetDelegate(&vectorSearcherDelegate{s: vecSearcher})
	}
	// persistSink is where the embedded vectors are written to the backend
	// so they survive a restart. Normally it is the active graph (a disk
	// store). Under the bulk loader idx.graph is the in-memory shadow, which
	// does not implement VectorSearcher; fall back to the disk store captured
	// at the shadow swap (bulkVectorSink) so the vector index still reaches
	// the `vectors` table instead of living only in the in-process HNSW and
	// vanishing on the next restart (which would force a paid re-embed).
	persistSink := vecSearcher
	if persistSink == nil {
		persistSink = idx.bulkVectorSink
	}
	var backendItems []graph.VectorItem
	if persistSink != nil {
		backendItems = make([]graph.VectorItem, 0, len(vectors))
	}
	// Add only well-formed vectors. A nil or wrong-width vector would poison
	// the index — a mis-scored or panicking query — so drop it, count it, and
	// keep a sample of offending node IDs for the log.
	var droppedVectors int
	var droppedSample []string
	for i, vec := range vectors {
		if len(vec) != dims {
			droppedVectors++
			if len(droppedSample) < 5 {
				droppedSample = append(droppedSample, ids[i])
			}
			continue
		}
		vecBackend.Add(ids[i], vec)
		if persistSink != nil {
			backendItems = append(backendItems, graph.VectorItem{
				NodeID: ids[i],
				Vec:    vec,
			})
		}
	}
	// If every vector was invalid there is nothing to search on. Ship text-only
	// rather than a silently empty vector index that mis-scores every query —
	// the same all-or-nothing contract as the chunk-failure abort above.
	if len(vectors) > 0 && vecBackend.Count() == 0 {
		idx.logger.Warn("vector index aborted — all embeddings invalid",
			zap.Int("dropped", droppedVectors),
			zap.Int("dimensions", dims),
			zap.Strings("sample_ids", droppedSample))
		idx.lastVectorBuildErr = fmt.Errorf("all %d embedding vectors were invalid (want width %d)", droppedVectors, dims)
		return
	}
	if persistSink != nil && len(backendItems) > 0 {
		if err := persistSink.BulkUpsertEmbeddings(backendItems); err != nil {
			idx.logger.Warn("indexer: backend vector bulk upsert failed",
				zap.Error(err))
		} else if err := persistSink.BuildVectorIndex(dims); err != nil {
			idx.logger.Warn("indexer: backend vector index build failed",
				zap.Error(err))
		}
	}
	// Install the chunk → parent-symbol mapping so HybridBackend can
	// de-chunk vector hits back to symbols at query time. Empty when no
	// symbol was large enough to split.
	if len(chunkMap) > 0 {
		vecBackend.SetChunkMap(chunkMap)
	}

	// Wrap text + vector into hybrid backend, swapping it in atomically
	// so any concurrent searches keep seeing a coherent backend.
	//
	// Unwrap any existing HybridBackend to its text side before
	// re-wrapping. Without this, buildSearchIndex called again (e.g.
	// once per tracked repo during daemon warmup) would stack a fresh
	// Hybrid on top of the previous one — nested Hybrids retain all
	// their stale vector indexes, ballooning live memory by an order
	// of magnitude. The text backend (BM25 or Bleve) has already been
	// updated with every node via idx.search.Add above; a single
	// Hybrid wrapping it + the latest vecBackend is all we need.
	sw := idx.swappable()
	inner := sw.Inner()
	if hyb, ok := inner.(*search.HybridBackend); ok {
		inner = hyb.TextBackend()
	}
	sw.Swap(search.NewHybrid(inner, vecBackend, idx.embedder))
	if droppedVectors > 0 {
		idx.logger.Warn("indexer: dropped invalid embedding vectors",
			zap.Int("dropped", droppedVectors),
			zap.Int("dimensions", dims),
			zap.Strings("sample_ids", droppedSample))
	}
	fields := []zap.Field{
		zap.Int("vectors", vecBackend.Count()),
		zap.Int("chunk_vectors", len(chunkMap)),
		zap.Int("dimensions", dims),
		zap.Int("dropped", droppedVectors),
	}
	// Surface the actual token spend of a paid embedding pass when the
	// backend reports usage (API providers do; in-process ones don't).
	// Without this the cost of an embedding run is invisible after the fact.
	if acc, ok := idx.embedder.(interface{ TokensUsed() int64 }); ok {
		if tokens := acc.TokensUsed(); tokens > 0 {
			fields = append(fields, zap.Int64("embed_tokens", tokens))
		}
	}
	idx.logger.Info("vector index built", fields...)
}

// dirIgnoreFiles are the per-directory ignore-file basenames honored by
// the index walk, siblings to .gitignore: Gortex's own .gortexignore
// plus ripgrep's .ignore and .rgignore. Patterns in each file are
// scoped to the directory that contains it; later filenames win, so a
// directory's .rgignore overrides its .ignore on a conflicting path.
var dirIgnoreFiles = []string{".gortexignore", ".ignore", ".rgignore"}

// shouldExclude reports whether a path is excluded by the effective
// ignore list. The flat matcher is built lazily from idx.config.Exclude,
// which is populated by ConfigManager.GetRepoConfig with the full
// layered list (builtin + global + RepoEntry + workspace). A path is
// also excluded by any per-directory ignore file (dirIgnoreFiles)
// present in one of its ancestor directories. isDir lets a trailing-
// slash pattern prune a directory subtree instead of only its files.
func (idx *Indexer) shouldExclude(path, root string, isDir bool) bool {
	// .claude/ and .kiro/ are Builtin-excluded wholesale, but may hold an
	// MCP server config the MCP-config-as-graph feature targets (the
	// extractor's own docs name .kiro/mcp.json). Descend those subtrees
	// and index only the MCP config files within them — everything else
	// stays excluded, so the agent-state noise never reaches the graph.
	if rel, err := filepath.Rel(root, path); err == nil && excludes.InAgentConfigDir(rel) {
		if isDir {
			return false
		}
		return !excludes.IsMCPConfigFile(rel)
	}
	if m := idx.excludeMatcher(); m != nil && m.MatchAbsDir(path, root, isDir) {
		return true
	}
	return idx.dirIgnoreMatcher(root).Match(path, isDir)
}

// shouldPruneDir reports whether the index walk may skip a directory
// subtree wholesale (filepath.SkipDir) instead of descending it. A
// directory is prunable only when it is excluded AND no re-include ("!")
// pattern targets anything beneath it. go-gitignore's "*" matches across
// "/", so a blanket like "wp-content/plugins/*" reports the parent
// directory "wp-content/plugins" itself as excluded; pruning it would
// skip a later "!wp-content/plugins/foo/" re-include before the walk ever
// reaches the child. Mirroring git, we keep descending such a directory
// and let the per-file shouldExclude check filter its contents.
func (idx *Indexer) shouldPruneDir(path, root string) bool {
	if !idx.shouldExclude(path, root, true) {
		return false
	}
	if m := idx.excludeMatcher(); m != nil {
		if rel, err := filepath.Rel(root, path); err == nil && m.HasNegatedDescendant(filepath.ToSlash(rel)) {
			return false
		}
	}
	if idx.dirIgnoreMatcher(root).HasNegatedDescendant(path) {
		return false
	}
	return true
}

// dirIgnoreMatcher returns the per-directory ignore matcher, built lazily
// against the repo root the index walk is anchored at.
func (idx *Indexer) dirIgnoreMatcher(root string) *excludes.Hierarchical {
	idx.dirIgnoreOnce.Do(func() {
		idx.dirIgnore = excludes.NewHierarchical(root, dirIgnoreFiles...)
	})
	return idx.dirIgnore
}

func (idx *Indexer) excludeMatcher() *excludes.Matcher {
	if m := idx.excludes.Load(); m != nil {
		return m
	}
	idx.excludeMu.Lock()
	defer idx.excludeMu.Unlock()
	if m := idx.excludes.Load(); m != nil {
		return m
	}
	m := excludes.New(effectiveExcludePatterns(idx.config.Exclude))
	idx.excludes.Store(m)
	return m
}

// SetExcludePatterns installs a new effective ignore list on a live
// Indexer and rebuilds the matcher, so the next walk honours it. Called
// when a repo's `.gortex.yaml` is re-read (daemon reload / re-track):
// the per-repo Indexer is long-lived and is reused by every incremental
// and scoped re-index, so without this an exclude added after the repo
// was tracked did not take effect until the daemon restarted.
func (idx *Indexer) SetExcludePatterns(patterns []string) {
	idx.excludeMu.Lock()
	defer idx.excludeMu.Unlock()
	idx.config.Exclude = patterns
	idx.excludes.Store(excludes.New(effectiveExcludePatterns(patterns)))
}

// effectiveExcludePatterns falls back to the builtin baseline when the
// list is empty. A nil/empty list from upstream means "no layering was
// applied" (e.g. a direct caller of indexer.New without ConfigManager),
// not "index everything" — the walk should still skip the obvious
// non-source dirs.
func effectiveExcludePatterns(patterns []string) []string {
	if len(patterns) == 0 {
		return excludes.Builtin
	}
	return patterns
}

// ParseErrors returns the parse errors from the last full index.
func (idx *Indexer) ParseErrors() []IndexError {
	return idx.parseErrors
}

// FileMtimes returns a copy of the file modification time map.
func (idx *Indexer) FileMtimes() map[string]int64 {
	idx.mtimeMu.RLock()
	defer idx.mtimeMu.RUnlock()
	out := make(map[string]int64, len(idx.fileMtimes))
	for k, v := range idx.fileMtimes {
		out[k] = v
	}
	return out
}

// trackedFileCount reports how many files this indexer currently holds
// mtime records for — the repo's whole file set, independent of any one
// pass's scope. A scoped pass adds and evicts its own files within scope
// before reading this, so the count stays current. Kept separate from
// FileMtimes() so callers that only need the size do not copy the map.
func (idx *Indexer) trackedFileCount() int {
	idx.mtimeMu.RLock()
	defer idx.mtimeMu.RUnlock()
	return len(idx.fileMtimes)
}

// RefreshFileMtime restamps the recorded modification time for a file
// from its current on-disk mtime, without re-indexing it. The watcher
// calls this when a save turned out to be structurally inert and the
// reindex was skipped: the graph is already correct, but the recorded
// mtime must advance past the save so the poller's mtime sweep does
// not keep re-flagging the same untouched file. A file absent from
// disk or never indexed is a no-op.
//
// The in-memory map is what the current process's poller and
// IsStale checks trust, but a warm restart trusts the persisted
// FileMtime sidecar instead — without also writing through to it here,
// a single inert save during a session left the persisted row at its
// pre-save value, so the next restart's HasChangesSinceMtimes saw this
// file as changed and re-tracked the whole repo. Mirrors the per-file
// indexFile persist (recordFileMtime); a no-op on the in-memory backend.
func (idx *Indexer) RefreshFileMtime(filePath string) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return
	}
	// relKey (slash + NFC) so the lookup hits the same fileMtimes
	// entry the index walk created for a non-ASCII filename.
	key := idx.relKey(absPath)
	mtime := info.ModTime().UnixNano()
	idx.mtimeMu.Lock()
	_, tracked := idx.fileMtimes[key]
	if tracked {
		idx.fileMtimes[key] = mtime
	}
	idx.mtimeMu.Unlock()
	if !tracked {
		return
	}
	if w, ok := idx.graph.(graph.FileMtimeWriter); ok {
		if err := w.BulkSetFileMtimes(idx.repoPrefix, map[string]int64{key: mtime}); err != nil {
			idx.logger.Warn("persist file mtime failed",
				zap.String("repo", idx.repoPrefix), zap.String("file", key), zap.Error(err))
		}
	}
}

// pruneDeletedFileMtimes drops the persisted mtime rows for files the
// incremental reindex just confirmed deleted. The in-memory map is already
// pruned by the caller; this keeps the store's FileMtime sidecar in step so
// a later warm restart does not re-discover them as phantom deletions and
// force a full re-track. A no-op when the backend lacks the capability
// (the in-memory backend) or the list is empty.
func (idx *Indexer) pruneDeletedFileMtimes(deleted []string) {
	if len(deleted) == 0 {
		return
	}
	if d, ok := idx.graph.(graph.FileMtimeDeleter); ok {
		if err := d.DeleteFileMtimes(idx.repoPrefix, deleted); err != nil {
			idx.logger.Warn("prune deleted file mtimes failed",
				zap.String("repo", idx.repoPrefix), zap.Error(err))
		}
	}
}

// SetFileMtimes restores the file modification time map from a persisted snapshot.
func (idx *Indexer) SetFileMtimes(mtimes map[string]int64) {
	idx.mtimeMu.Lock()
	defer idx.mtimeMu.Unlock()
	idx.fileMtimes = make(map[string]int64, len(mtimes))
	for k, v := range mtimes {
		idx.fileMtimes[k] = v
	}
}

// SetRootPath sets the root path for relative path computation.
func (idx *Indexer) SetRootPath(root string) {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	idx.storeRootPath(abs)
}

// IncrementalReindexPaths re-indexes only the files reachable from the
// supplied paths, instead of walking the whole repository root.
//
// Each path may be absolute or relative to root, and may be a file or a
// directory; directories are walked recursively with the same
// exclude / language filters as a full pass. Within that scoped file
// set the behaviour matches IncrementalReindex: only files that are
// stale (mtime or, in Merkle mode, content) are re-indexed, and a file
// previously tracked under one of the scoped paths but now absent from
// disk is evicted.
//
// When paths is empty the call degrades to IncrementalReindex(root) —
// callers can therefore pass an optional path list unconditionally.
func (idx *Indexer) IncrementalReindexPaths(root string, paths []string) (*IndexResult, error) {
	return idx.incrementalReindexPaths(root, paths, true)
}

// incrementalDiscoverPaths discovers and refreshes files beneath paths without
// treating absent tracked files as deletions. Watcher directory-create scans
// use this mode because their job is to recover creates that happened before a
// nested watch was attached. A concurrent file-delete event must remain the
// sole owner of eviction so it can publish the pre-delete symbol snapshot.
func (idx *Indexer) incrementalDiscoverPaths(root string, paths []string) (*IndexResult, error) {
	return idx.incrementalReindexPaths(root, paths, false)
}

func (idx *Indexer) incrementalReindexPaths(
	root string,
	paths []string,
	detectDeletions bool,
	markerBatches ...*reparsePendingEnrichmentBatch,
) (*IndexResult, error) {
	if len(paths) == 0 {
		// An empty scope means the repository root. detectDeletions decides
		// whether absent persisted paths are evicted; keeping that choice here
		// lets IncrementalReindex share this bounded implementation without a
		// recursive wrapper call.
		paths = []string{root}
	}

	start := time.Now()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.storeRootPath(absRoot)

	// scopeRels holds the repo-relative slash-paths the caller asked to
	// reindex — used both to drive the discovery walk and to bound
	// deletion detection to the scoped subtree.
	scopeRels := make(map[string]bool)

	// diskFiles is the set of in-scope language files currently on
	// disk; staleFiles is the subset that changed since the last pass.
	diskFiles := make(map[string]bool)
	var staleFiles []string

	merkleMode := idx.merkleEnabled()

	for _, p := range paths {
		absPath := p
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(absRoot, filepath.FromSlash(p))
		}
		absPath = filepath.Clean(absPath)

		// A path outside the repo root is rejected: scoping is a
		// narrowing operation, never an escape hatch to index files
		// the repo doesn't own.
		rel, relErr := filepath.Rel(absRoot, absPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("incremental reindex: path %q is outside repository root %q", p, absRoot)
		}
		// Canonical key (slash + NFC) so scopeRels matches fileMtimes
		// keys when deletion detection intersects the two below.
		scopeRels[idx.relKey(absPath)] = true

		info, statErr := os.Stat(absPath)
		if statErr != nil {
			// A path that no longer exists is not an error: it may be
			// a deleted file the caller still wants evicted. Deletion
			// detection below handles it via scopeRels.
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("incremental reindex: stat %q: %w", p, statErr)
		}

		if info.IsDir() {
			walkErr := filepath.WalkDir(absPath, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if idx.shouldPruneDir(path, absRoot) {
						return filepath.SkipDir
					}
					return nil
				}
				if _, ok := idx.effectiveLanguage(path, nil); !ok && !idx.isIncrementalContractManifest(path) {
					return nil
				}
				if idx.shouldExclude(path, absRoot, false) {
					return nil
				}
				// relKey (slash + NFC) keeps the disk set keyed
				// consistently with fileMtimes for non-ASCII names.
				relPath := idx.relKey(path)
				diskFiles[relPath] = true
				if !merkleMode && idx.IsStale(relPath) {
					staleFiles = append(staleFiles, path)
				}
				return nil
			})
			if walkErr != nil {
				return nil, walkErr
			}
			continue
		}

		// Single file. Apply the same language / exclude gate so a
		// caller can't force a non-source or excluded file in.
		if _, ok := idx.effectiveLanguage(absPath, nil); !ok && !idx.isIncrementalContractManifest(absPath) {
			continue
		}
		if idx.shouldExclude(absPath, absRoot, false) {
			continue
		}
		// relKey (slash + NFC) — same canonical key the graph and
		// fileMtimes use, so a non-ASCII path passed in here matches
		// regardless of the Unicode form the caller supplied.
		relPath := idx.relKey(absPath)
		diskFiles[relPath] = true
		if !merkleMode && idx.IsStale(relPath) {
			staleFiles = append(staleFiles, absPath)
		}
	}

	// In Merkle mode the per-file mtime check is skipped; the stale set
	// comes from a content-addressed tree diff over the whole repo,
	// then intersected back down to the requested scope.
	if merkleMode {
		for _, abs := range idx.merkleStaleFiles(absRoot, diskFiles) {
			rel, relErr := filepath.Rel(absRoot, abs)
			if relErr != nil {
				continue
			}
			if diskFiles[filepath.ToSlash(rel)] {
				staleFiles = append(staleFiles, abs)
			}
		}
	}

	var deletedFiles []string
	if detectDeletions {
		// Deletion detection is deliberately opt-in. General scoped
		// reconciles need it, while directory-create discovery must never
		// consume a concurrent delete before the watcher can notify clients.
		idx.mtimeMu.RLock()
		var candidates []string
		for relPath := range idx.fileMtimes {
			if diskFiles[relPath] {
				continue
			}
			if relPathInScope(relPath, scopeRels) {
				candidates = append(candidates, relPath)
			}
		}
		idx.mtimeMu.RUnlock()

		for _, relPath := range candidates {
			absPath := filepath.Join(absRoot, filepath.FromSlash(relPath))
			_, statErr := os.Stat(absPath)
			if statErr == nil {
				// Present-but-excluded must be purged (same as full IncrementalReindex).
				if idx.shouldExclude(absPath, absRoot, false) {
					deletedFiles = append(deletedFiles, relPath)
				}
				continue
			}
			if errors.Is(statErr, os.ErrNotExist) {
				deletedFiles = append(deletedFiles, relPath)
				continue
			}
			idx.logger.Warn("incremental reindex: stat failed during scoped deletion detection, preserving",
				zap.String("rel", relPath), zap.Error(statErr))
		}
	}

	// Capture surviving dependents before deletion evicts the target symbols and
	// their incoming adjacency. The helper performs one batched node/edge
	// frontier read for the whole deletion set.
	deletedDependencyFiles := idx.semanticDependencyFrontierForDeletedFiles(deletedFiles)
	sourceStaleFiles, manifestFiles := splitIncrementalContractManifests(idx, staleFiles)
	markerBatch := &reparsePendingEnrichmentBatch{}
	if len(markerBatches) > 0 && markerBatches[0] != nil {
		markerBatch = markerBatches[0]
	}
	invalidation, reparsedFiles, failedFiles := idx.reindexIncrementalFilesBatched(
		sourceStaleFiles, deletedFiles, markerBatch,
	)
	manifestPlan, manifestFailed := idx.refreshIncrementalContractManifests(manifestFiles)
	invalidation.Merge(manifestPlan)
	failedFiles = appendUniqueSorted(failedFiles, manifestFailed...)
	invalidation.Files = appendUniqueSorted(invalidation.Files, idx.graphFilePaths(reparsedFiles)...)
	invalidation.Files = appendUniqueSorted(invalidation.Files, deletedDependencyFiles...)
	idx.pruneDeletedFileMtimes(deletedFiles)
	idx.flushReparsePendingEnrichment(markerBatch)

	// Structural and metadata refreshes maintain both the in-memory search
	// backend and persistent FTS one symbol at a time; deletions remove the
	// prior symbols above. Rebuilding the whole corpus after a tiny edit would
	// re-embed every unchanged symbol. The only rebuild retained here is the
	// zero-delta bootstrap for a non-persistent backend restored beside an
	// already-populated graph.
	if len(staleFiles) == 0 && len(deletedFiles) == 0 && !isSymbolSearcherBackend(idx.search) {
		idx.buildSearchIndex()
	}

	if len(staleFiles) > 0 || len(deletedFiles) > 0 {
		// Contract extraction is file-bounded even for body-only and metadata
		// deltas: endpoint literals, response envelopes and source locations can
		// change without changing declaration/call topology. Only an effective
		// contract-set change requests the workspace reconciliation pass.
		contractRefresh := idx.refreshContractsForFiles(invalidation.Files)
		if contractRefresh.Changed {
			invalidation.Flags |= DerivedInvalidatesContracts
			invalidation.ContractGroups = mergeContractGroups(
				invalidation.ContractGroups, contractRefresh.Groups...,
			)
			invalidation.ContractSymbolIDs = appendUniqueSorted(
				invalidation.ContractSymbolIDs, contractRefresh.SymbolIDs...,
			)
		}
		if contractRefresh.LegacyFallback {
			invalidation.LegacyFallback = true
		}
		idx.indexGen.Add(1) // files changed — invalidate the trigram cache
	}

	nodes, edges := idx.repoNodeEdgeCount()
	// FileCount must describe the repo, never this pass. diskFiles holds
	// only the files under the caller's scope, so len(diskFiles) is the
	// size of the batch — stamping that onto RepoMetadata is what made
	// `daemon status` report an actively-edited repo's file count as the
	// size of its last changed-file batch. How much work this pass did is
	// already carried by StaleFileCount / DeletedFileCount.
	fileCount := idx.trackedFileCount()
	if fileCount == 0 {
		// No prior full pass populated the mtime map (a virgin indexer
		// reaching the scoped path). The scope is all we know about.
		fileCount = len(diskFiles)
	}
	result := &IndexResult{
		NodeCount:           nodes,
		EdgeCount:           edges,
		FileCount:           fileCount,
		StaleFileCount:      len(staleFiles),
		DeletedFileCount:    len(deletedFiles),
		FailedFiles:         failedFiles,
		DurationMs:          time.Since(start).Milliseconds(),
		DerivedInvalidation: invalidation,
	}
	idx.warnIfEdgeSanityViolated(result)
	// Partial work always queues the exact changed/deleted/dependent graph-file
	// frontier. runDeferredEnrich partitions surviving files by language in one
	// read; deleted files need no provider call after their graph state is evicted.
	// A partial pass never publishes or requires a whole-repository marker.
	if len(staleFiles) > 0 || len(deletedFiles) > 0 {
		idx.markPendingEnrichFiles(invalidation.Files)
	}
	return result, nil
}

// relPathInScope reports whether a repo-relative slash-path falls under
// any of the scoped paths — either an exact file match or anywhere
// inside a scoped directory.
func relPathInScope(relPath string, scope map[string]bool) bool {
	if scope[relPath] {
		return true
	}
	for s := range scope {
		if s == "." {
			return true
		}
		if strings.HasPrefix(relPath, s+"/") {
			return true
		}
	}
	return false
}

// IncrementalReindex walks the file tree and re-indexes only files that changed
// since the last snapshot. It also evicts nodes for deleted files.
func (idx *Indexer) IncrementalReindex(root string) (*IndexResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.storeRootPath(absRoot)

	// Collect files currently on disk.
	diskFiles := make(map[string]bool)
	var staleFiles []string

	// Merkle mode replaces per-file mtime staleness with a
	// content-addressed Merkle-tree diff computed after the walk.
	merkleMode := idx.merkleEnabled()

	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldPruneDir(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := idx.effectiveLanguage(path, nil); !ok && !idx.isIncrementalContractManifest(path) {
			return nil
		}
		if idx.shouldExclude(path, absRoot, false) {
			return nil
		}

		// relKey (slash + NFC) so the disk set is keyed identically
		// to fileMtimes — otherwise a non-ASCII file the snapshot
		// stored under one Unicode form and this walk observes under
		// another would be seen as both deleted and newly created.
		relPath := idx.relKey(path)
		diskFiles[relPath] = true

		if !merkleMode && idx.IsStale(relPath) {
			staleFiles = append(staleFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// In Merkle mode the per-file mtime check above is skipped; the
	// stale set comes from a content-addressed tree diff instead.
	if merkleMode {
		staleFiles = idx.merkleStaleFiles(absRoot, diskFiles)
	}

	// Detect deleted files. A file that's tracked in fileMtimes but
	// absent from the current discovery walk is a candidate, but
	// "absent from discovery" is not always "absent from disk":
	//
	//   - Newly excluded (user/config intent): treat as deleted so the
	//     graph does not retain permanent orphans (#321).
	//   - Language extractor Extensions() shrank: file still on disk and
	//     not excluded — preserve graph state.
	//   - WalkDir swallowed a transient error (EACCES, EIO, NFS hiccup,
	//     ELOOP) — preserve on non-ENOENT stat failures.
	//
	// Stat the candidate: ENOENT => deleted; exists+excluded => deleted;
	// exists+not-excluded => preserve; other errors => preserve.
	idx.mtimeMu.RLock()
	var candidates []string
	for relPath := range idx.fileMtimes {
		if !diskFiles[relPath] {
			candidates = append(candidates, relPath)
		}
	}
	idx.mtimeMu.RUnlock()

	var deletedFiles []string
	for _, relPath := range candidates {
		absPath := filepath.Join(absRoot, relPath)
		_, err := os.Stat(absPath)
		if err == nil {
			// File still exists on disk but was not admitted by this walk.
			// If the admission set now excludes it, treat it like a deletion
			// so exclude-list refinements do not leave permanent orphans
			// (file_count drops while node_count/search stay stale). See #321.
			// If it is not excluded (e.g. extension no longer detected),
			// preserve graph state.
			if idx.shouldExclude(absPath, absRoot, false) {
				deletedFiles = append(deletedFiles, relPath)
			}
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			deletedFiles = append(deletedFiles, relPath)
			continue
		}
		// Transient error — preserve to be safe.
		idx.logger.Warn("incremental reindex: stat failed during deletion detection, preserving",
			zap.String("rel", relPath), zap.Error(err))
	}

	// Capture surviving dependents while the deleted symbols and their incoming
	// adjacency are still present. Keep the exact deleted paths too so contract
	// registries can remove their stale entries after graph eviction.
	deletedDependencyFiles := idx.semanticDependencyFrontierForDeletedFiles(deletedFiles)
	partialFrontier := appendUniqueSorted(nil, deletedDependencyFiles...)
	for _, relPath := range deletedFiles {
		partialFrontier = append(partialFrontier, idx.prefixPath(filepath.FromSlash(relPath)))
	}
	partialFrontier = appendUniqueSorted(nil, partialFrontier...)

	// Evict only files that are truly absent from disk.
	for _, relPath := range deletedFiles {
		// relPath is a fileMtimes key (relKey: slash + NFC); the graph
		// keys nodes under OS-native separators, so convert before the
		// evict or a deleted file's nodes leak on Windows. FromSlash is
		// a no-op on POSIX.
		graphPath := idx.prefixPath(filepath.FromSlash(relPath))
		idx.restubIncomingRefs(graphPath)
		idx.evictEnrichment(graphPath)
		idx.graph.EvictFile(graphPath)
		idx.mtimeMu.Lock()
		delete(idx.fileMtimes, relPath)
		idx.mtimeMu.Unlock()
	}
	// Prune the persisted mtime rows for deleted files too, so the next
	// warm restart does not see them as phantom deletions (the in-memory
	// delete above does not reach the store's sidecar table).
	idx.pruneDeletedFileMtimes(deletedFiles)

	// Re-index stale files. A file that fails — most often because it
	// was locked or mid-write when the walk caught it — is collected
	// and retried once below. A failure that survives the retry is
	// surfaced on IndexResult.FailedFiles so the caller can replay it.
	// A file whose bytes couldn't be read at all keeps no mtime
	// recorded, so it stays stale for the next incremental pass; a
	// file that read but failed to parse gets its mtime recorded (see
	// indexFile's result==nil branch), so it stops being retried until
	// its content changes again — see IndexResult.FailedFiles.
	sourceStaleFiles, manifestFiles := splitIncrementalContractManifests(idx, staleFiles)
	markerBatch := &reparsePendingEnrichmentBatch{}
	var failedFiles []string
	var reparsedFiles []string
	for _, f := range sourceStaleFiles {
		if err := idx.indexFile(f, true, markerBatch); err != nil {
			idx.logger.Debug("incremental reindex: failed to index file",
				zap.String("file", f), zap.Error(err))
			failedFiles = append(failedFiles, f)
		} else {
			reparsedFiles = append(reparsedFiles, f)
		}
	}
	if len(failedFiles) > 0 {
		retry := failedFiles
		failedFiles = nil
		for _, f := range retry {
			if err := idx.indexFile(f, true, markerBatch); err != nil {
				idx.logger.Warn("incremental reindex: file failed after retry",
					zap.String("file", f), zap.Error(err))
				failedFiles = append(failedFiles, f)
			} else {
				reparsedFiles = append(reparsedFiles, f)
			}
		}
	}

	partialFrontier = appendUniqueSorted(partialFrontier, idx.graphFilePaths(reparsedFiles)...)
	manifestPlan, manifestFailed := idx.refreshIncrementalContractManifests(manifestFiles)
	failedFiles = appendUniqueSorted(failedFiles, manifestFailed...)
	partialFrontier = appendUniqueSorted(partialFrontier, manifestPlan.Files...)
	idx.flushReparsePendingEnrichment(markerBatch)

	// Re-infer interface implementations (edges may have been lost
	// during eviction). Skipped under deferGlobalPasses so a batch
	// caller (ReconcileAll, warmup) can run a single global pass at
	// the end instead of paying O(global) per repo.
	if !idx.deferGlobalPasses {
		// Scoped inference passes re-derive only the affected types/interfaces
		// (add-parity with the full pass); fall back to whole-graph when off.
		if !idx.runScopedInferencePasses(staleFiles) {
			idx.resolver.InferImplements()
			idx.resolver.InferOverrides()
		}
		// Capability edges (reads_env / executes_process / accesses_field)
		// and framework dynamic-dispatch synthesis are whole-graph recomputes
		// that derive edges only from structural nodes in the changed files.
		// When nothing structural changed — no stale code file (a doc/config
		// edit or a true zero-change reconcile) and no deletion — they can
		// produce no new edge, so re-running them is pure cost: the dominant
		// waste in a no-op IncrementalReindex, which the periodic janitor runs
		// per repo per tick. Gate them on the same predicate the path-scoped
		// IncrementalReindexPaths already uses. Deletions still trigger a
		// re-run because an evicted file may have been a dispatch endpoint.
		if len(deletedFiles) > 0 || idx.staleFilesAffectDerivedEdges(staleFiles) {
			synthesizeCapabilityEdges(idx.graph)
			resolver.RunFrameworkSynthesizers(idx.graph)
		}
		// External-call synthesis (opt-in) — file-scoped to the reindexed
		// files (O(edited files)), not a full-graph recompute. Eviction
		// already dropped a removed file's synthetic edges; a re-indexed
		// file's fresh external terminals are re-materialised here.
		resolver.SynthesizeExternalCallsForFiles(idx.graph, idx.externalCallSynthesisEnabled(), idx.graphFilePaths(staleFiles))
		// Clone detection is not re-run here: each stale file was
		// re-indexed through IndexFile above, whose resolve pass
		// already recomputed EdgeSimilarTo against the fresh graph,
		// and deleted files self-clean via EvictFile's bidirectional
		// edge removal. Under deferGlobalPasses the batch caller runs
		// the global clone pass once at the end.
	}

	// Rebuild search index to ensure consistency — but skip it on a
	// zero-change reconcile against a backend that persists its search
	// structures natively (the on-disk backend). See the matching guard
	// in the other incremental path: re-embedding is wasted work and
	// there is nothing to rebuild when no file changed.
	if len(staleFiles) > 0 || len(deletedFiles) > 0 || !isSymbolSearcherBackend(idx.search) {
		idx.buildSearchIndex()
	}

	// Update totalDetected so index_health reports correctly after cache restore.
	if idx.totalDetected == 0 {
		idx.totalDetected = len(diskFiles)
	}

	// Refresh contracts only for the changed/deleted/dependent frontier. The
	// registry bootstrap and cross-file expansion remain contract-scoped; this
	// path never scans the repository merely because one file changed.
	if len(staleFiles) > 0 || len(deletedFiles) > 0 {
		idx.refreshContractsForFiles(partialFrontier)
		idx.indexGen.Add(1) // files changed — invalidate the trigram cache
	}

	nodes, edges := idx.repoNodeEdgeCount()
	result := &IndexResult{
		NodeCount:        nodes,
		EdgeCount:        edges,
		FileCount:        len(diskFiles),
		StaleFileCount:   len(staleFiles),
		DeletedFileCount: len(deletedFiles),
		FailedFiles:      failedFiles,
		DurationMs:       time.Since(start).Milliseconds(),
	}
	idx.warnIfEdgeSanityViolated(result)
	// Queue only the exact changed/deleted/dependent file frontier. The deferred
	// runner handles all surviving languages in batches and clears this generation
	// only after every selected provider succeeds.
	if len(staleFiles) > 0 || len(deletedFiles) > 0 {
		idx.markPendingEnrichFiles(partialFrontier)
	}
	return result, nil
}

// LastIndexTime returns the timestamp of the last full index.
func (idx *Indexer) LastIndexTime() time.Time {
	return idx.lastIndexTime
}

// TotalDetected returns the total number of files detected during the last full index.
func (idx *Indexer) TotalDetected() int {
	return idx.totalDetected
}

// buildPerFileContractExtractors returns the set of extractors that
// operate on a single source file (everything except GoModExtractor,
// which runs once against go.mod at the repo root) plus a language →
// [extractors] map so callers can skip extractors whose
// SupportedLanguages() doesn't include a given file's language.
// Building the language map once avoids doing the string-membership
// check per file.
func (idx *Indexer) buildPerFileContractExtractors() ([]contracts.Extractor, map[string][]contracts.Extractor) {
	extractors := []contracts.Extractor{
		&contracts.HTTPExtractor{ClientAliases: idx.config.HTTPClientAliases},
		&contracts.GRPCExtractor{},
		&contracts.ThriftExtractor{},
		&contracts.GraphQLExtractor{},
		&contracts.TRPCExtractor{},
		&contracts.OpenAPIExtractor{},
		&contracts.TopicExtractor{},
		&contracts.WebSocketExtractor{},
		&contracts.NestMicroserviceExtractor{},
		&contracts.EnvVarExtractor{},
	}
	// Config-driven event bus: only registered when the user declared
	// boundaries (index.event_bus / CODEGRAPH_EVENT_CONFIG), so the default
	// extractor set is unchanged.
	if b := idx.eventBusBoundaries(); len(b) > 0 {
		extractors = append(extractors, &contracts.EventBusExtractor{Boundaries: b})
	}
	byLang := make(map[string][]contracts.Extractor)
	for _, ex := range extractors {
		for _, lang := range ex.SupportedLanguages() {
			byLang[lang] = append(byLang[lang], ex)
		}
	}
	return extractors, byLang
}

// runContractExtractorsForFile applies the given extractors to a single
// file and returns the raw contracts (with RepoPrefix already set).
// Called both inline from parse workers and from the full-walk
// extractContracts path — they share the same per-file work.
func (idx *Indexer) runContractExtractorsForFile(
	graphPath string,
	src []byte,
	fileNodes []*graph.Node,
	fileEdges []*graph.Edge,
	exts []contracts.Extractor,
	tree *parser.ParseTree,
) []contracts.Contract {
	if len(exts) == 0 {
		return nil
	}
	// Contracts from synthetic test/bench fixtures are kept (so drift
	// checks can flag a stale test pinned to an obsolete production
	// contract) but tagged with is_test=true and a test_source
	// category so the dashboard can filter them out by default.
	testSource := fixtures.TestContractSource(graphPath)
	var out []contracts.Contract
	// The graph store backs graph-wide constant resolution for endpoint
	// arguments (a route path / queue / topic referenced by a const). Resolved
	// once per call; nil when the backend can't satisfy the reader (const
	// dereference is then disabled and store-aware extractors degrade to their
	// tree-aware behaviour).
	var endpointStore contracts.EndpointConstStore
	if es, ok := idx.graph.(contracts.EndpointConstStore); ok {
		endpointStore = es
	}
	for _, ex := range exts {
		var found []contracts.Contract
		if sae, ok := ex.(contracts.StoreAwareExtractor); ok {
			found = sae.ExtractWithStore(graphPath, src, fileNodes, fileEdges, tree, endpointStore, idx.repoPrefix)
		} else if tae, ok := ex.(contracts.TreeAwareExtractor); ok && tree != nil {
			found = tae.ExtractWithTree(graphPath, src, fileNodes, fileEdges, tree)
		} else {
			found = ex.Extract(graphPath, src, fileNodes, fileEdges)
		}
		for i := range found {
			found[i].RepoPrefix = idx.repoPrefix
			// Stamp the workspace / project slugs alongside the repo
			// prefix so the matcher's boundary check has the data it
			// needs without a second registry walk. Empty slugs
			// default to RepoPrefix at Match time via
			// Contract.EffectiveWorkspace / EffectiveProject.
			if idx.workspaceID != "" {
				found[i].WorkspaceID = idx.workspaceID
			}
			if idx.projectID != "" {
				found[i].ProjectID = idx.projectID
			}
			if testSource != "" {
				if found[i].Meta == nil {
					found[i].Meta = map[string]any{}
				}
				found[i].Meta["is_test"] = true
				found[i].Meta["test_source"] = testSource
			}
		}
		out = append(out, found...)
	}
	return out
}

func (idx *Indexer) upgradeContractBareTypeRefs(reg *contracts.Registry) {
	if reg == nil {
		return
	}
	bareTypeSet := make(map[string]struct{})
	for _, contract := range reg.All() {
		for _, key := range []string{"request_type", "response_type"} {
			name, _ := contract.Meta[key].(string)
			if name != "" && !strings.Contains(name, "::") {
				bareTypeSet[name] = struct{}{}
			}
		}
	}
	bareTypeNames := make([]string, 0, len(bareTypeSet))
	for name := range bareTypeSet {
		bareTypeNames = append(bareTypeNames, name)
	}
	sort.Strings(bareTypeNames)
	typeNodesByName := make(map[string][]*graph.Node)
	if len(bareTypeNames) > 0 {
		typeNodesByName = idx.graph.FindNodesByNames(bareTypeNames)
	}
	reg.UpgradeBareTypeRefs(func(name, repoHint string) []string {
		var same, others []string
		for _, node := range typeNodesByName[name] {
			if node.Kind != graph.KindType {
				continue
			}
			if repoHint != "" && strings.HasPrefix(node.ID, repoHint+"/") {
				same = append(same, node.ID)
				continue
			}
			others = append(others, node.ID)
		}
		if len(same) > 0 {
			return same
		}
		return others
	})
}

// commitContracts writes contract nodes + provides/consumes edges for
// every contract in reg, and sets idx.contractRegistry to reg. Called
// once per index pass after all per-file contracts have been collected
// (inline from parse workers) plus go.mod has been processed.
func (idx *Indexer) commitContracts(reg *contracts.Registry) {
	// Upgrade bare type names in contract Meta (e.g. "UserResp") to
	// full symbol IDs (e.g. "pkg/resp.go::UserResp") now that the
	// graph is complete. During extraction the enricher only saw
	// the handler's file-scoped node list, so types declared in a
	// sibling file stayed as bare names.
	idx.upgradeContractBareTypeRefs(reg)

	// Cross-file handler resolution. When a route is registered with
	// a handler identifier that the file-scoped extractor couldn't
	// resolve (`h.ServeArchive` in router.go wiring a method defined
	// in archive_handler.go), the contract's SymbolID fell back to
	// the enclosing router function and schema extraction ran
	// against the router's body — which has every route's bindings
	// piled on top of each other. Re-run enrichment with the
	// correct per-handler scope now that the graph is complete.
	idx.resolveProviderHandlers(reg)

	// Cross-file route-prefix joining. A FastAPI APIRouter declared in
	// one file (`router = APIRouter(prefix="/users")`) is mounted under
	// a second prefix elsewhere (`app.include_router(router,
	// prefix="/api")`), so a route declared `@router.get("/{id}")`
	// belongs at /api/users/{id}, not /{id}. Rewrite the affected
	// provider contract IDs to the joined path before the matcher pairs
	// them with consumers. Also handles Express app.use mounts and
	// NestJS @Controller class prefixes. Reads source straight off disk
	// (cached per file) — the same access pattern resolveProviderHandlers
	// uses for cross-file handler bodies.
	//
	// Mount sites (a main.py that only calls include_router) often carry
	// no route contracts, so the scan-file set comes from the graph's
	// py/ts/js file nodes, not the registry — but only when at least one
	// prefix-eligible route contract exists, so non-FastAPI/Express/Nest
	// repos pay nothing.
	if scanFiles := idx.routerPrefixScanFiles(reg); len(scanFiles) > 0 {
		srcCache := make(map[string][]byte)
		contracts.JoinRouterPrefixes(reg, scanFiles, func(filePath string) []byte {
			if data, ok := srcCache[filePath]; ok {
				return data
			}
			data := idx.contractFileSrc(filePath)
			srcCache[filePath] = data
			return data
		})
	}

	// Spring application(-profile)?.{yml,properties} config-key graph: emit the
	// value-redacted config-key nodes and reads_config edges from the @Value /
	// @ConfigurationProperties beans the Java extractor stamped. Cheap to skip
	// on non-Spring repos (no config files + no stamped beans = no work).
	contracts.BindSpringConfig(idx.graph, contracts.SpringConfigScope{
		RepoPrefix:  idx.repoPrefix,
		RepoRoot:    idx.rootPath,
		WorkspaceID: idx.workspaceID,
	})

	// Trace response variables back to their call-site return types.
	// Handles `source, err := h.svc.Get(...)` → response_type is
	// whatever `h.svc.Get` returns. The enricher can't do this
	// without graph access; this pass reads each method's signature
	// directly off the graph node, parses the first non-error
	// return type, and resolves it to a symbol ID.
	idx.resolveCallReturnTypes(reg)

	// Snapshot field-level shapes for every type that's referenced as
	// a contract's request / response body. This is Stage 2 — without
	// per-field data Stage 3 (validation, breaking-change detection)
	// has nothing to diff. We de-duplicate by symbol ID so heavy
	// fan-in types (a User DTO used by 40 routes) only get parsed
	// once per index pass.
	idx.snapshotContractShapes(reg)

	// Fold each type's snapshotted Shape into the envelope rows that
	// reference it. The dashboard renders these rows as the response
	// JSON shape (e.g. `{ workspace: { id: string }, repos: [{ name: string }] }`)
	// instead of the bare type-symbol-ID, which answers nothing about
	// the wire format.
	idx.inlineEnvelopeShapes(reg)

	all := reg.All()
	nodes := make([]*graph.Node, 0, len(all))
	edges := make([]*graph.Edge, 0, len(all))
	for _, c := range all {
		// dep::<module> nodes were materialised by extractGoModContracts
		// before ResolveAll (so the import bridge could find them);
		// re-emitting them here would PK-collide on backends whose bulk
		// load is INSERT-only (the on-disk backend). The pre-pass is the single
		// writer for that contract type.
		if c.Type == contracts.ContractDependency {
			continue
		}
		nodes = append(nodes, &graph.Node{
			ID:          c.ID,
			Kind:        graph.KindContract,
			Name:        c.ID,
			FilePath:    c.FilePath,
			Language:    "contract",
			RepoPrefix:  c.RepoPrefix,
			WorkspaceID: c.EffectiveWorkspace(),
			ProjectID:   c.EffectiveProject(),
			Meta: map[string]any{
				"type":          string(c.Type),
				"role":          string(c.Role),
				"symbol_id":     c.SymbolID,
				"line":          c.Line,
				"confidence":    c.Confidence,
				"contract_meta": c.Meta,
			},
		})

		if c.SymbolID == "" {
			continue
		}
		edgeKind := graph.EdgeProvides
		if c.Role == contracts.RoleConsumer {
			edgeKind = graph.EdgeConsumes
		}
		edges = append(edges, &graph.Edge{
			From:     c.SymbolID,
			To:       c.ID,
			Kind:     edgeKind,
			FilePath: c.FilePath,
			Line:     c.Line,
			Meta:     contractOwnerEdgeMeta(c),
		})
		// Framework-layer EdgeHandlesRoute. Emitted alongside
		// EdgeProvides for HTTP / gRPC / WS / GraphQL / topic
		// providers so `analyze kind=routes` and other
		// framework-aware tools walk one targeted edge instead
		// of filtering EdgeProvides by contract type. Consumers
		// (callers of routes) and non-route contract types (env,
		// OpenAPI specs, DI tokens) intentionally skip this
		// edge — they aren't route handlers.
		if c.Role == contracts.RoleProvider && isRouteContractType(c.Type) {
			routeMeta := contractOwnerEdgeMeta(c)
			routeMeta["contract_type"] = string(c.Type)
			edges = append(edges, &graph.Edge{
				From:     c.SymbolID,
				To:       c.ID,
				Kind:     graph.EdgeHandlesRoute,
				FilePath: c.FilePath,
				Line:     c.Line,
				Meta:     routeMeta,
			})
		}
	}

	bulkStart := time.Now()
	idx.bulkCommit(nodes, edges)
	bulkElapsed := time.Since(bulkStart)

	idx.contractRegistry = reg
	repo := idx.rootPath
	if idx.repoPrefix != "" {
		repo = idx.repoPrefix
	}
	idx.logger.Info("contracts extracted",
		zap.String("repo", repo),
		zap.Int("count", len(all)),
		zap.Duration("commit_bulk_elapsed", bulkElapsed))
}

// bulkCommit writes nodes + edges in one AddBatch call. The bulk
// load path is intentionally NOT used here: contract IDs often
// coincide with existing source-symbol IDs (a route handler shows
// up as both a Go function and an HTTP-contract anchor), and the
// on-disk backend's bulk load is INSERT-only on the node table so
// any collision fails the whole batch. AddBatch's non-bulk path
// upserts every row so duplicates are absorbed in place; the
// per-call cost is amortised by the chunked write path the backend
// uses internally.
func (idx *Indexer) bulkCommit(nodes []*graph.Node, edges []*graph.Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	idx.graph.AddBatch(nodes, edges)
}

// routerPrefixScanFiles returns the set of source files
// JoinRouterPrefixes must scan for router definitions and mount sites
// (APIRouter / include_router / app.use). Returns nil when no HTTP
// contract uses a prefix-joining framework (FastAPI / Express / NestJS),
// so unrelated repos skip the file enumeration entirely. When eligible,
// it enumerates py / ts / js / tsx / jsx file nodes from the graph — the
// mount file (a FastAPI main.py) frequently has no route contract of its
// own and so can't be discovered from the registry alone.
func (idx *Indexer) routerPrefixScanFiles(reg *contracts.Registry) []string {
	eligible := false
	for _, c := range reg.All() {
		if c.Type != contracts.ContractHTTP || c.Meta == nil {
			continue
		}
		switch fw, _ := c.Meta["framework"].(string); fw {
		case "fastapi/flask", "express", "nestjs":
			eligible = true
		}
		if eligible {
			break
		}
	}
	if !eligible {
		return nil
	}

	return graph.ReadRepoFilePaths(
		idx.graph,
		idx.repoPrefix,
		idx.workspaceID,
		[]string{"python", "typescript", "javascript"},
		[]string{".py", ".ts", ".tsx", ".js", ".jsx"},
	)
}

// contractFileSrc reads the on-disk source for a contract FilePath
// (which is repo-prefixed when the indexer uses a repo prefix). Returns
// nil when the file can't be read. Mirrors the disk-resolution logic in
// resolveProviderHandlers so cross-file passes share one access pattern.
func (idx *Indexer) contractFileSrc(filePath string) []byte {
	diskPath := filePath
	if idx.repoPrefix != "" && strings.HasPrefix(diskPath, idx.repoPrefix+"/") {
		diskPath = strings.TrimPrefix(diskPath, idx.repoPrefix+"/")
	}
	diskPath = filepath.Join(idx.rootPath, diskPath)
	data, err := os.ReadFile(diskPath)
	if err != nil {
		return nil
	}
	return data
}

// isRouteContractType reports whether a ContractType corresponds to a
// real network-route handler (HTTP / gRPC / WebSocket / GraphQL /
// topic). Used to gate EdgeHandlesRoute emission so the framework-layer
// edge stays focused on actual handlers and excludes env / OpenAPI /
// dependency / DI-token contracts that share the EdgeProvides edge but
// aren't routes in the agent-asks-which-handler-serves-X sense.
func isRouteContractType(t contracts.ContractType) bool {
	switch t {
	case contracts.ContractHTTP,
		contracts.ContractGRPC,
		contracts.ContractThrift,
		contracts.ContractGraphQL,
		contracts.ContractTopic,
		contracts.ContractWS:
		return true
	}
	return false
}

// resolveProviderHandlers finds the actual handler for every HTTP
// provider contract whose per-file extraction couldn't resolve the
// handler identifier (typically routers in one file wiring handlers
// defined in sibling files). For each such contract:
//
//   - Take Meta["handler_trail"] — the full expression between the
//     HandleFunc parens, which carries every handler candidate
//     (wrappers + inner handler). Fall back to "handler_ident"
//     when no trail was captured (older contracts, simple consumer
//     patterns).
//   - Enumerate candidates in source order and look each up in the
//     graph; take the innermost (last) one that resolves. That
//     picks h.ServeArchive out of WithAuth(h.ServeArchive) instead
//     of the WithAuth wrapper.
//   - Re-run EnrichHTTPContract against the handler's file with the
//     handler's line range so the enricher sees its actual body
//     instead of the router's.
//   - Drop `handler_ident` / `handler_trail` from meta afterwards —
//     they were internal resolution hints.
func (idx *Indexer) resolveProviderHandlers(reg *contracts.Registry) {
	type pending struct {
		contractID string
		trail      string
		fallback   string
		repoHint   string
		// srcDir is the directory of the contract's registration site
		// (the file with the HandleFunc call). Used by lookupHandler
		// as a tie-breaker when two same-repo functions share a name
		// across packages — e.g. `Handler.handleContracts` in the
		// `server` pkg vs `Server.handleContracts` in `mcp`. A
		// `recv.method` call from inside `server/handler.go` resolves
		// to the same-package method, not the cross-package one.
		srcDir string
	}
	var todo []pending
	for _, c := range reg.All() {
		if c.Role != contracts.RoleProvider || c.Type != contracts.ContractHTTP {
			continue
		}
		trail, _ := c.Meta["handler_trail"].(string)
		fallback, _ := c.Meta["handler_ident"].(string)
		if trail == "" && fallback == "" {
			continue
		}
		// Skip contracts where schema is already populated — the
		// initial file-scoped pass worked.
		if src, _ := c.Meta["schema_source"].(string); src == "extracted" || src == "partial" {
			continue
		}
		todo = append(todo, pending{
			contractID: c.ID,
			trail:      trail,
			fallback:   fallback,
			repoHint:   c.RepoPrefix,
			srcDir:     filepath.Dir(c.FilePath),
		})
	}
	// Always strip the internal handler hints from Meta at the end of
	// this pass — successful or not. They were only ever intended as
	// per-pass resolution scratchpad: when the cross-file lookup
	// succeeds we delete them in the patched-contract loop below; when
	// it fails (no candidate, ambiguous, etc.) they used to leak to
	// the dashboard as values like `handler_trail: "/users", listUsers`
	// — useless to a reader. This cleanup runs unconditionally so
	// downstream consumers never see internal extractor state.
	defer func() {
		for _, c := range reg.All() {
			if c.Meta == nil {
				continue
			}
			if _, hasIdent := c.Meta["handler_ident"]; !hasIdent {
				if _, hasTrail := c.Meta["handler_trail"]; !hasTrail {
					continue
				}
			}
			items := reg.ByID(c.ID)
			for i := range items {
				if items[i].Meta == nil {
					continue
				}
				delete(items[i].Meta, "handler_ident")
				delete(items[i].Meta, "handler_trail")
			}
			reg.ReplaceByID(c.ID, items)
		}
	}()

	if len(todo) == 0 {
		return
	}

	// Resolve every distinct handler name in one store call. A router can
	// contain hundreds of endpoints that repeat the same handful of handlers;
	// issuing FindNodesByName for every trail segment turns SQLite into an N+1
	// loop even though the candidate set is identical.
	var handlerNames []string
	seenHandlerNames := make(map[string]struct{})
	for _, p := range todo {
		candidates := contracts.HandlerCandidatesInTrail(p.trail)
		if len(candidates) == 0 && p.fallback != "" {
			candidates = []string{p.fallback}
		}
		for _, ident := range candidates {
			name := handlerLookupName(ident)
			if name == "" {
				continue
			}
			if _, seen := seenHandlerNames[name]; seen {
				continue
			}
			seenHandlerNames[name] = struct{}{}
			handlerNames = append(handlerNames, name)
		}
	}
	handlerCandidates := idx.graph.FindNodesByNames(handlerNames)

	type resolvedPending struct {
		pending pending
		handler *graph.Node
	}
	resolvedTodo := make([]resolvedPending, 0, len(todo))
	var handlerPaths []string
	seenHandlerPaths := make(map[string]struct{})
	for _, p := range todo {
		handler := idx.resolveInnermostHandlerFromCandidates(
			p.trail, p.fallback, p.repoHint, p.srcDir, handlerCandidates,
		)
		if handler == nil {
			continue
		}
		resolvedTodo = append(resolvedTodo, resolvedPending{pending: p, handler: handler})
		if handler.FilePath == "" {
			continue
		}
		if _, seen := seenHandlerPaths[handler.FilePath]; seen {
			continue
		}
		seenHandlerPaths[handler.FilePath] = struct{}{}
		handlerPaths = append(handlerPaths, handler.FilePath)
	}
	handlerFileNodes := idx.graph.GetFileNodesByPaths(handlerPaths)

	// Cache file source per file path — a single router often refers to
	// dozens of handlers in the same sibling file. Handler nodes themselves
	// were fetched above in one store batch.
	fileSrc := make(map[string][]byte)

	// fileTrees caches per-file ParseTree handles parsed lazily below
	// so a router referencing many handlers in the same sibling file
	// only parses that file once.
	fileTrees := make(map[string]*parser.ParseTree)
	defer func() {
		for _, t := range fileTrees {
			t.Release()
		}
	}()
	resolved := 0
	for _, item := range resolvedTodo {
		p := item.pending
		handlerNode := item.handler
		src, ok := fileSrc[handlerNode.FilePath]
		if !ok {
			diskPath := handlerNode.FilePath
			if idx.repoPrefix != "" && strings.HasPrefix(diskPath, idx.repoPrefix+"/") {
				diskPath = strings.TrimPrefix(diskPath, idx.repoPrefix+"/")
			}
			diskPath = filepath.Join(idx.rootPath, diskPath)
			data, err := os.ReadFile(diskPath)
			if err != nil {
				fileSrc[handlerNode.FilePath] = nil
				continue
			}
			fileSrc[handlerNode.FilePath] = data
			src = data
		}
		if src == nil {
			continue
		}
		nodes := handlerFileNodes[handlerNode.FilePath]

		lang := detectLangFromPath(handlerNode.FilePath)
		tree, treeReady := fileTrees[handlerNode.FilePath]
		if !treeReady {
			tree = contracts.ParseTreeForLang(lang, src)
			fileTrees[handlerNode.FilePath] = tree
		}

		// Re-run enrichment. EnrichHTTPContractWithTree reads the
		// contract's SymbolID to locate the handler body range — swap
		// it in temporarily to the resolved handler so the lookup
		// works. With a tree the AST overlay runs after the regex
		// pass and overrides Meta keys it can confidently produce.
		matches := reg.ByID(p.contractID)
		if len(matches) == 0 {
			continue
		}
		for i, c := range matches {
			if c.Role != contracts.RoleProvider {
				continue
			}
			// Operate on a copy; Registry entries are values.
			patched := c
			patched.SymbolID = handlerNode.ID
			patched.FilePath = handlerNode.FilePath
			if patched.Meta == nil {
				patched.Meta = map[string]any{}
			}
			// Drop prior path_params so the enricher's fresh pass
			// repopulates consistently (path hasn't changed, but we
			// want the call-path to be identical to Stage 1).
			lines := splitLines(src)
			contracts.EnrichHTTPContractWithTree(&patched, lines, nodes, lang, tree)
			delete(patched.Meta, "handler_ident")
			delete(patched.Meta, "handler_trail")
			matches[i] = patched
			resolved++
		}
		// Write back the mutated set. The registry doesn't have an
		// "update" API; we use AddAll semantics via Set-like
		// operations. Simpler: clear then re-add all roles to this ID.
		reg.ReplaceByID(p.contractID, matches)
	}
	if resolved > 0 {
		idx.logger.Info("resolved cross-file provider handlers",
			zap.Int("count", resolved),
			zap.Int("considered", len(todo)))
	}
}

func (idx *Indexer) resolveInnermostHandlerFromCandidates(
	trail, fallback, repoHint, srcDir string,
	byName map[string][]*graph.Node,
) *graph.Node {
	candidates := contracts.HandlerCandidatesInTrail(trail)
	if len(candidates) == 0 && fallback != "" {
		candidates = []string{fallback}
	}
	var best *graph.Node
	for _, ident := range candidates {
		var n *graph.Node
		if byName == nil {
			n = idx.lookupHandler(ident, repoHint, srcDir)
		} else {
			n = lookupHandlerFromCandidates(ident, repoHint, srcDir, byName)
		}
		if n != nil {
			best = n
		}
	}
	return best
}

// lookupHandler maps a raw identifier from a route pattern to the
// graph node for the handler function / method.
//
//   - "h.ServeArchive" → method named "ServeArchive", prefer same repo.
//   - "ServeArchive"   → function or method of that name.
//   - "pkg.Foo"        → same as first form, package-qualified call.
//
// Returns nil when no candidate resolves unambiguously.
func (idx *Indexer) lookupHandler(ident, repoHint, srcDir string) *graph.Node {
	name := handlerLookupName(ident)
	if name == "" {
		return nil
	}
	return pickHandlerCandidate(idx.graph.FindNodesByName(name), repoHint, srcDir)
}

func handlerLookupName(ident string) string {
	// Strip a leading receiver / package qualifier — "h.ServeArchive"
	// → "ServeArchive".
	if i := strings.LastIndex(ident, "."); i >= 0 {
		ident = ident[i+1:]
	}
	return ident
}

func lookupHandlerFromCandidates(
	ident, repoHint, srcDir string,
	byName map[string][]*graph.Node,
) *graph.Node {
	name := handlerLookupName(ident)
	if name == "" {
		return nil
	}
	return pickHandlerCandidate(byName[name], repoHint, srcDir)
}

func pickHandlerCandidate(candidates []*graph.Node, repoHint, srcDir string) *graph.Node {
	if len(candidates) == 0 {
		return nil
	}
	var sameRepo, other []*graph.Node
	for _, n := range candidates {
		if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
			continue
		}
		if repoHint != "" && strings.HasPrefix(n.ID, repoHint+"/") {
			sameRepo = append(sameRepo, n)
			continue
		}
		other = append(other, n)
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	if len(sameRepo) == 0 && len(other) == 1 {
		return other[0]
	}
	// Multiple candidates — try same-package tie-break before giving up.
	// A `recv.method` call inside `pkg/foo.go` resolves to a method
	// declared in the same package; cross-package lookalikes (e.g.
	// `Server.handleContracts` in `mcp` vs `Handler.handleContracts`
	// in `server`) are filtered out. Without this, both routers and
	// MCP-side handlers compete for the same name and the resolver
	// falls back to the enclosing function (`registerRoutes`).
	if srcDir != "" {
		pool := sameRepo
		if len(pool) == 0 {
			pool = other
		}
		var samePkg []*graph.Node
		for _, n := range pool {
			if filepath.Dir(n.FilePath) == srcDir {
				samePkg = append(samePkg, n)
			}
		}
		if len(samePkg) == 1 {
			return samePkg[0]
		}
	}
	return nil // ambiguous
}

func splitLines(src []byte) []string {
	return strings.Split(string(src), "\n")
}

// detectLangFromPath mirrors internal/contracts.detectLanguage so the
// enricher's language-gate fires correctly for the handler's own file.
func detectLangFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".go"):
		return "go"
	case strings.HasSuffix(path, ".ts"), strings.HasSuffix(path, ".tsx"):
		return "typescript"
	case strings.HasSuffix(path, ".js"), strings.HasSuffix(path, ".jsx"):
		return "javascript"
	case strings.HasSuffix(path, ".py"):
		return "python"
	case strings.HasSuffix(path, ".java"):
		return "java"
	case strings.HasSuffix(path, ".kt"), strings.HasSuffix(path, ".kts"):
		return "kotlin"
	case strings.HasSuffix(path, ".dart"):
		return "dart"
	}
	return ""
}

// responseHelperCallRe pulls the third argument out of a JSON-response
// helper call, e.g. `respondJSON(w, http.StatusOK, source)` → "source",
// `WriteJSON(w, 200, &result)` → "result". Matches every helper name
// the Go enricher knows about so the two pipes stay in sync.
var responseHelperCallRe = regexp.MustCompile(
	`(?:[A-Za-z_]\w*\.)?(?:[Rr]espond|[Ww]rite|[Ss]end|[Rr]ender)(?:JSON|Json)\(\s*\w+\s*,\s*[^,]+?\s*,\s*&?([A-Za-z_]\w*)\s*\)`,
)

// receiverMatchesHint decides whether a method node could plausibly
// be the target of a call whose receiver chain includes `hint` as
// its penultimate segment. For `h.tucks.Update`, the hint is
// "tucks" and we accept receivers whose name (stripped of pointer
// marker) contains "tucks" case-insensitively:
//
//	*TucksStore.Update       ✓  (receiver "TucksStore" contains "tucks")
//	*PostgresTuckStore.Update ✓  (contains "tuck")
//	*EmailSources.Update     ✗  (no "tucks")
//
// The hint may itself be the receiver variable (`h` in `h.Update(...)`)
// when the call has only two segments; in that case any same-repo
// method named `Update` passes — but the upstream `len(matches) != 1`
// check still demands uniqueness, which is the real guard.
func receiverMatchesHint(n *graph.Node, hint string) bool {
	if hint == "" {
		return true
	}
	// Method ID looks like "<repo>/<file>::Receiver.Method". Extract
	// "Receiver" by splitting once on `::` then taking the type part
	// before the last `.`.
	idParts := strings.Split(n.ID, "::")
	if len(idParts) < 2 {
		return true // conservative: no receiver info available → don't filter out
	}
	last := idParts[len(idParts)-1]
	dot := strings.LastIndex(last, ".")
	if dot < 0 {
		return true // plain function, no receiver
	}
	recv := strings.TrimPrefix(last[:dot], "*")
	// Handle both singular and plural forms: "tucks" in hint matches
	// "TuckStore" by containing "tuck", and "tuck" hint matches
	// "Tucks" too. Strip a trailing `s` from the longer side to let
	// singular/plural pairs match.
	return strings.Contains(strings.ToLower(recv), strings.ToLower(hint)) ||
		strings.Contains(strings.ToLower(recv), strings.ToLower(strings.TrimSuffix(hint, "s"))) ||
		strings.Contains(strings.ToLower(recv), strings.ToLower(hint+"s"))
}

// parseFirstNonErrorReturnType walks a Go function signature and
// returns the first return type that isn't `error`. Signatures as
// stored in the graph have the form:
//
//	func ((s *Store)) Get(args) (*EmailSource, error)
//	func list() []*User
//	func save(x Foo) error
//
// Regex-based extraction struggles with the receiver's `((...))`
// nesting and with multi-paren return groups — we parse with an
// explicit bracket-depth counter so every shape above is handled
// the same way.

// resolveCallReturnTypes is the graph-aware companion to the
// regex-based schema enricher. For every HTTP provider contract whose
// response couldn't be pinned syntactically (response_expr is set,
// response_type is empty), we:
//
//   - Pull the bound variable's name out of the helper-call expression.
//   - Read the handler's body from disk.
//   - Find the variable's declaration line and parse the RHS call.
//   - Look up the called method by name in the graph (preferring the
//     same file / same repo).
//   - Parse the method's signature meta for the first non-error
//     return type, strip `*` / `[]`, resolve to a type node's ID.
//   - Patch the contract's meta in place.
//
// This is the proper tracing the name-based heuristic only
// approximated — it follows the variable to its definition instead
// of guessing from its name.
func (idx *Indexer) resolveCallReturnTypes(reg *contracts.Registry) {
	resolved := 0
	bfCache := newBodyFactsCache(idx)
	defer bfCache.Close()
	resolution := idx.prepareContractTypeResolution(reg, bfCache)

	for _, c := range reg.All() {
		if c.Role != contracts.RoleProvider || c.Type != contracts.ContractHTTP {
			continue
		}

		// Path 1: bare-variable response (`return WriteJSON(w, code, resp)`).
		// Trace the variable to its binding call's return type — and,
		// failing that, the literal/builtin shape of its declaration —
		// then stamp response_type / response_repeated. Accepts
		// response_expr in two forms:
		//   - Bare identifier ("result")             — emitted by the
		//     AST overlay (or the post-fix Go enricher) when the value
		//     is a plain var.
		//   - Full helper call ("WriteJSON(w, …)")   — older
		//     extraction output, kept compatible by extracting the
		//     third arg via responseHelperCallRe.
		if rt, _ := c.Meta["response_type"].(string); rt == "" {
			varName := contractResponseVarName(c)
			if varName != "" {
				typeID, repeated := idx.lookupVarTypeForContract(c, varName, resolution)
				if typeID != "" {
					items := reg.ByID(c.ID)
					changed := false
					for i := range items {
						if items[i].Role != contracts.RoleProvider || items[i].SymbolID != c.SymbolID {
							continue
						}
						if items[i].Meta == nil {
							items[i].Meta = map[string]any{}
						}
						items[i].Meta["response_type"] = typeID
						if repeated {
							items[i].Meta["response_repeated"] = true
						}
						items[i].Meta["schema_source"] = "extracted"
						delete(items[i].Meta, "response_expr")
						changed = true
					}
					if changed {
						reg.ReplaceByID(c.ID, items)
						resolved++
					}
				}
			}
		}

		// Path 2: envelope response (`map[string]any{"workspace": ws,
		// "repos": repos}`). For each row that didn't resolve a type
		// syntactically, trace its expression to a binding call's
		// return type and patch the row in place. Pulled out as a
		// separate pass so a contract whose top-level response_type
		// stays unresolvable can still get per-field signal — which is
		// the whole point of the envelope view.
		envRaw, ok := c.Meta["response_envelope"].([]map[string]any)
		if !ok {
			continue
		}
		if !envelopeNeedsResolution(envRaw) {
			continue
		}
		envChanged := false
		for ri := range envRaw {
			if t, _ := envRaw[ri]["type"].(string); t != "" {
				continue
			}
			expr, _ := envRaw[ri]["expr"].(string)
			if expr == "" {
				continue
			}
			// Strip a leading `&` / `*` so the binding lookup sees
			// the underlying identifier.
			ident := strings.TrimLeft(expr, "&*")
			if !isLikelyIdentifier(ident) {
				continue
			}
			typeID, repeated := idx.lookupVarTypeForContract(c, ident, resolution)
			if typeID != "" {
				envRaw[ri]["type"] = typeID
				if repeated {
					envRaw[ri]["repeated"] = true
				}
				envChanged = true
			}
		}
		if !envChanged {
			continue
		}
		// Promote schema_source to "extracted" if every row now has a
		// type (or this is a single-key envelope whose lone field
		// resolved). Otherwise leave it as "partial" — we have more
		// info than before but it's not exhaustive.
		items := reg.ByID(c.ID)
		patched := false
		for i := range items {
			if items[i].Role != contracts.RoleProvider || items[i].SymbolID != c.SymbolID {
				continue
			}
			if items[i].Meta == nil {
				items[i].Meta = map[string]any{}
			}
			items[i].Meta["response_envelope"] = envRaw
			if envelopeFullyTyped(envRaw) {
				items[i].Meta["schema_source"] = "extracted"
			}
			patched = true
		}
		if patched {
			reg.ReplaceByID(c.ID, items)
			resolved++
		}
	}
	if resolved > 0 {
		idx.logger.Info("resolved response types from call signatures",
			zap.Int("count", resolved))
	}
}

func envelopeNeedsResolution(env []map[string]any) bool {
	for _, row := range env {
		if t, _ := row["type"].(string); t == "" {
			return true
		}
	}
	return false
}

func envelopeFullyTyped(env []map[string]any) bool {
	if len(env) == 0 {
		return false
	}
	for _, row := range env {
		if t, _ := row["type"].(string); t == "" {
			return false
		}
	}
	return true
}

// isLikelyIdentifier accepts the bare-identifier and dotted-path
// forms that traceVarTypeFromBody can match against a binding line.
// Compound expressions ("len(repos)", "&Foo{}") are out of scope —
// they'd need a more thorough RHS parser than the regex chain here.
func isLikelyIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			continue
		case i > 0 && r >= '0' && r <= '9':
			continue
		case r == '.' && i > 0:
			continue
		default:
			return false
		}
	}
	return true
}

type contractBindingKey struct {
	repoPrefix string
	filePath   string
	symbolID   string
	name       string
}

type contractCallKey struct {
	callExpr   string
	repoPrefix string
}

type contractTypeNameKey struct {
	typeName   string
	repoPrefix string
}

type rawContractCallType struct {
	typeName string
	repeated bool
	pointer  bool
}

type resolvedContractCallType struct {
	typeID   string
	repeated bool
	pointer  bool
}

type contractTypeResolution struct {
	bindings      map[contractBindingKey]contracts.Binding
	semanticTypes map[graph.SemanticBindingSite]string
	upgradedTypes map[contractTypeNameKey]string
	callTypes     map[contractCallKey]resolvedContractCallType
}

func contractBindingKeyFor(c contracts.Contract, name string) contractBindingKey {
	return contractBindingKey{
		repoPrefix: c.RepoPrefix,
		filePath:   c.FilePath,
		symbolID:   c.SymbolID,
		name:       name,
	}
}

func contractResponseVarName(c contracts.Contract) string {
	respExpr, _ := c.Meta["response_expr"].(string)
	switch {
	case respExpr == "":
		return ""
	case isLikelyIdentifier(respExpr):
		return respExpr
	default:
		if m := responseHelperCallRe.FindStringSubmatch(respExpr); len(m) >= 2 {
			return m[1]
		}
		return ""
	}
}

func unresolvedContractBindingNames(c contracts.Contract) []string {
	seen := make(map[string]struct{})
	add := func(name string) {
		if name != "" {
			seen[name] = struct{}{}
		}
	}

	if rt, _ := c.Meta["response_type"].(string); rt == "" {
		add(contractResponseVarName(c))
	}
	if env, ok := c.Meta["response_envelope"].([]map[string]any); ok {
		for _, row := range env {
			if typeName, _ := row["type"].(string); typeName != "" {
				continue
			}
			expr, _ := row["expr"].(string)
			ident := strings.TrimLeft(expr, "&*")
			if isLikelyIdentifier(ident) {
				add(ident)
			}
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func semanticTypeForBinding(
	key contractBindingKey,
	binding contracts.Binding,
	types map[graph.SemanticBindingSite]string,
) string {
	if binding.Line <= 0 {
		return ""
	}
	site := graph.SemanticBindingSite{
		RepoPrefix: key.repoPrefix,
		FilePath:   key.filePath,
		Line:       binding.Line,
		Name:       key.name,
	}
	if typeName := types[site]; typeName != "" {
		return typeName
	}
	site.Name = ""
	return types[site]
}

func (idx *Indexer) prepareContractTypeResolution(
	reg *contracts.Registry,
	bfCache *bodyFactsCache,
) *contractTypeResolution {
	resolution := &contractTypeResolution{
		bindings:      make(map[contractBindingKey]contracts.Binding),
		semanticTypes: make(map[graph.SemanticBindingSite]string),
		upgradedTypes: make(map[contractTypeNameKey]string),
		callTypes:     make(map[contractCallKey]resolvedContractCallType),
	}
	if reg == nil {
		return resolution
	}

	allContracts := reg.All()
	handlerSet := make(map[string]struct{})
	for _, c := range allContracts {
		if c.Role != contracts.RoleProvider || c.Type != contracts.ContractHTTP || len(unresolvedContractBindingNames(c)) == 0 {
			continue
		}
		if c.SymbolID != "" {
			handlerSet[c.SymbolID] = struct{}{}
		}
	}
	handlerIDs := make([]string, 0, len(handlerSet))
	for id := range handlerSet {
		handlerIDs = append(handlerIDs, id)
	}
	sort.Strings(handlerIDs)
	handlers := make(map[string]*graph.Node)
	if len(handlerIDs) > 0 {
		handlers = idx.graph.GetNodesByIDs(handlerIDs)
	}

	siteSet := make(map[graph.SemanticBindingSite]struct{})
	for _, c := range allContracts {
		if c.Role != contracts.RoleProvider || c.Type != contracts.ContractHTTP {
			continue
		}
		names := unresolvedContractBindingNames(c)
		if len(names) == 0 {
			continue
		}
		bf := bfCache.For(handlers[c.SymbolID])
		for _, name := range names {
			key := contractBindingKeyFor(c, name)
			if _, exists := resolution.bindings[key]; exists {
				continue
			}
			binding := bf.VarBinding(name)
			resolution.bindings[key] = binding
			if binding.Line <= 0 {
				continue
			}
			site := graph.SemanticBindingSite{
				RepoPrefix: c.RepoPrefix,
				FilePath:   c.FilePath,
				Line:       binding.Line,
				Name:       name,
			}
			siteSet[site] = struct{}{}
			site.Name = ""
			siteSet[site] = struct{}{}
		}
	}

	sites := make([]graph.SemanticBindingSite, 0, len(siteSet))
	for site := range siteSet {
		sites = append(sites, site)
	}
	sort.Slice(sites, func(i, j int) bool {
		a, b := sites[i], sites[j]
		if a.RepoPrefix != b.RepoPrefix {
			return a.RepoPrefix < b.RepoPrefix
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Name < b.Name
	})
	resolution.semanticTypes = idx.readSemanticBindingTypes(sites)

	callSet := make(map[contractCallKey]struct{})
	typeNameSet := make(map[contractTypeNameKey]struct{})
	for key, binding := range resolution.bindings {
		if typeName := semanticTypeForBinding(key, binding, resolution.semanticTypes); typeName != "" {
			typeNameSet[contractTypeNameKey{typeName: typeName, repoPrefix: key.repoPrefix}] = struct{}{}
			continue
		}
		switch binding.Kind {
		case contracts.BindingMethodCall, contracts.BindingFuncCall:
			if binding.CallExpr != "" {
				callSet[contractCallKey{callExpr: binding.CallExpr, repoPrefix: key.repoPrefix}] = struct{}{}
			}
		default:
			if binding.TypeID != "" {
				typeNameSet[contractTypeNameKey{typeName: binding.TypeID, repoPrefix: key.repoPrefix}] = struct{}{}
			}
		}
	}

	callKeys := make([]contractCallKey, 0, len(callSet))
	for key := range callSet {
		callKeys = append(callKeys, key)
	}
	sort.Slice(callKeys, func(i, j int) bool {
		if callKeys[i].repoPrefix != callKeys[j].repoPrefix {
			return callKeys[i].repoPrefix < callKeys[j].repoPrefix
		}
		return callKeys[i].callExpr < callKeys[j].callExpr
	})
	rawCalls := idx.resolveCallExprTypeNames(callKeys)
	for key, raw := range rawCalls {
		typeNameSet[contractTypeNameKey{typeName: raw.typeName, repoPrefix: key.repoPrefix}] = struct{}{}
	}

	typeNameKeys := make([]contractTypeNameKey, 0, len(typeNameSet))
	for key := range typeNameSet {
		typeNameKeys = append(typeNameKeys, key)
	}
	resolution.upgradedTypes = idx.upgradeBareTypeNames(typeNameKeys)
	for key, raw := range rawCalls {
		typeID := resolution.upgradedTypes[contractTypeNameKey{typeName: raw.typeName, repoPrefix: key.repoPrefix}]
		if typeID == "" {
			typeID = raw.typeName
		}
		resolution.callTypes[key] = resolvedContractCallType{
			typeID:   typeID,
			repeated: raw.repeated,
			pointer:  raw.pointer,
		}
	}
	return resolution
}

func (idx *Indexer) readSemanticBindingTypes(sites []graph.SemanticBindingSite) map[graph.SemanticBindingSite]string {
	resolved := make(map[graph.SemanticBindingSite]string, len(sites))
	if len(sites) == 0 {
		return resolved
	}

	seen := make(map[graph.SemanticBindingSite]struct{}, len(sites))
	missing := make([]graph.SemanticBindingSite, 0, len(sites))
	for _, site := range sites {
		if _, exists := seen[site]; exists {
			continue
		}
		seen[site] = struct{}{}
		missing = append(missing, site)
	}
	sort.Slice(missing, func(i, j int) bool {
		a, b := missing[i], missing[j]
		if a.RepoPrefix != b.RepoPrefix {
			return a.RepoPrefix < b.RepoPrefix
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Name < b.Name
	})

	readers := make([]graph.SemanticBindingTypeReader, 0, 2)
	if reader, ok := idx.graph.(graph.SemanticBindingTypeReader); ok {
		readers = append(readers, reader)
	}
	if bindingResolver := contracts.CurrentBindingResolver(); bindingResolver != nil {
		if reader, ok := bindingResolver.(graph.SemanticBindingTypeReader); ok {
			readers = append(readers, reader)
		}
	}

	for readerIndex, reader := range readers {
		if len(missing) == 0 {
			break
		}
		found, err := reader.SemanticBindingTypes(missing)
		if err != nil && idx.logger != nil {
			idx.logger.Debug("semantic binding batch lookup failed",
				zap.Int("reader_index", readerIndex),
				zap.Int("site_count", len(missing)),
				zap.Error(err))
		}
		nextMissing := missing[:0]
		for _, site := range missing {
			if typeName := found[site]; typeName != "" {
				resolved[site] = typeName
				continue
			}
			nextMissing = append(nextMissing, site)
		}
		missing = nextMissing
	}
	return resolved
}

func (idx *Indexer) upgradeBareTypeNames(keys []contractTypeNameKey) map[contractTypeNameKey]string {
	resolved := make(map[contractTypeNameKey]string, len(keys))
	unique := make(map[contractTypeNameKey]struct{}, len(keys))
	nameSet := make(map[string]struct{})
	for _, key := range keys {
		if key.typeName == "" {
			continue
		}
		unique[key] = struct{}{}
		if !strings.Contains(key.typeName, "::") {
			nameSet[key.typeName] = struct{}{}
		}
	}

	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	candidatesByName := make(map[string][]*graph.Node)
	if len(names) > 0 {
		candidatesByName = idx.graph.FindNodesByNames(names)
	}

	for key := range unique {
		if strings.Contains(key.typeName, "::") {
			resolved[key] = key.typeName
			continue
		}
		var fallback *graph.Node
		for _, node := range candidatesByName[key.typeName] {
			if node.Kind != graph.KindType {
				continue
			}
			if key.repoPrefix != "" && strings.HasPrefix(node.ID, key.repoPrefix+"/") {
				fallback = node
				break
			}
			if fallback == nil {
				fallback = node
			}
		}
		if fallback != nil {
			resolved[key] = fallback.ID
		} else {
			resolved[key] = key.typeName
		}
	}
	return resolved
}

func (idx *Indexer) resolveCallExprTypeNames(keys []contractCallKey) map[contractCallKey]rawContractCallType {
	resolved := make(map[contractCallKey]rawContractCallType, len(keys))
	type callInfo struct {
		methodName   string
		receiverHint string
	}
	infos := make(map[contractCallKey]callInfo, len(keys))
	methodSet := make(map[string]struct{})
	for _, key := range keys {
		if key.callExpr == "" {
			continue
		}
		parts := strings.Split(key.callExpr, ".")
		methodName := parts[len(parts)-1]
		if methodName == "" {
			continue
		}
		info := callInfo{methodName: methodName}
		if len(parts) >= 2 {
			info.receiverHint = parts[len(parts)-2]
		}
		infos[key] = info
		methodSet[methodName] = struct{}{}
	}

	methodNames := make([]string, 0, len(methodSet))
	for name := range methodSet {
		methodNames = append(methodNames, name)
	}
	sort.Strings(methodNames)
	candidatesByName := make(map[string][]*graph.Node)
	if len(methodNames) > 0 {
		candidatesByName = idx.graph.FindNodesByNames(methodNames)
	}

	for key, info := range infos {
		var matches []*graph.Node
		for _, node := range candidatesByName[info.methodName] {
			if node.Kind != graph.KindMethod && node.Kind != graph.KindFunction {
				continue
			}
			if key.repoPrefix != "" && !strings.HasPrefix(node.ID, key.repoPrefix+"/") {
				continue
			}
			if info.receiverHint != "" && !receiverMatchesHint(node, info.receiverHint) {
				continue
			}
			matches = append(matches, node)
		}
		if len(matches) == 0 {
			continue
		}

		var returnType string
		ambiguous := false
		for _, match := range matches {
			if match.Meta == nil {
				continue
			}
			signature, _ := match.Meta["signature"].(string)
			typeName := parseFirstNonErrorReturnType(signature)
			if typeName == "" {
				continue
			}
			if returnType == "" {
				returnType = typeName
				continue
			}
			if typeName != returnType {
				ambiguous = true
				break
			}
		}
		if ambiguous || returnType == "" {
			continue
		}

		repeated := strings.HasPrefix(returnType, "[]")
		pointer := strings.HasPrefix(returnType, "*") ||
			(repeated && strings.HasPrefix(returnType[2:], "*"))
		returnType = strings.TrimLeft(returnType, "*[]")
		if dot := strings.LastIndex(returnType, "."); dot >= 0 {
			returnType = returnType[dot+1:]
		}
		if returnType != "" {
			resolved[key] = rawContractCallType{
				typeName: returnType,
				repeated: repeated,
				pointer:  pointer,
			}
		}
	}
	return resolved
}

// lookupVarTypeForContract resolves a variable to its return type
// using BodyFacts (AST-driven, structurally correct). Returns
// (typeID, repeated) or ("", false) when the binding can't be
// resolved.
//
// AST-only: phase 1b deleted the body-text regex fallback. Languages
// without a BodyFactsFactory get nopBodyFacts (which returns empty
// Bindings), so this function is a no-op for non-Go contracts.
// Their per-file regex enricher in schema_enrich_<lang>.go still runs
// and populates request_type / response_type via the framework
// detectors — only the post-pass cross-handler trace is AST-only.
func (idx *Indexer) lookupVarTypeForContract(
	c contracts.Contract,
	varName string,
	resolution *contractTypeResolution,
) (string, bool) {
	if resolution == nil {
		return "", false
	}
	key := contractBindingKeyFor(c, varName)
	b, ok := resolution.bindings[key]
	if !ok {
		return "", false
	}

	// Highest tier: one compiler-resolved batch loaded before this pass. The
	// SQLite reader serves warm restarts; the compact Go provider serves stores
	// without persistent binding rows. Named rows disambiguate same-line
	// declarations, while the blank-name row preserves legacy line parity.
	if typeName := semanticTypeForBinding(key, b, resolution.semanticTypes); typeName != "" {
		resolvedType := resolution.upgradedTypes[contractTypeNameKey{typeName: typeName, repoPrefix: c.RepoPrefix}]
		if resolvedType == "" {
			resolvedType = typeName
		}
		return resolvedType, b.Repeated
	}

	switch b.Kind {
	case contracts.BindingMethodCall, contracts.BindingFuncCall:
		if resolved, ok := resolution.callTypes[contractCallKey{callExpr: b.CallExpr, repoPrefix: c.RepoPrefix}]; ok && resolved.typeID != "" {
			return resolved.typeID, resolved.repeated
		}
	default:
		// Composite / slice / map / literal / path-value /
		// header-value / form-value / query-get — already typed.
		if b.TypeID != "" {
			resolvedType := resolution.upgradedTypes[contractTypeNameKey{typeName: b.TypeID, repoPrefix: c.RepoPrefix}]
			if resolvedType == "" {
				resolvedType = b.TypeID
			}
			return resolvedType, b.Repeated
		}
	}
	return "", false
}

func parseFirstNonErrorReturnType(sig string) string {
	sig = strings.TrimSpace(sig)
	if !strings.HasPrefix(sig, "func") {
		return ""
	}
	sig = strings.TrimSpace(strings.TrimPrefix(sig, "func"))

	// Optional receiver. Two forms to recognise:
	//   `((*Recv)) Name(params)`  — gortex's stored double-paren form
	//   `(r *Recv) Name(params)`  — standard Go source form
	// In both cases a function name follows the receiver parens.
	// Anonymous function types (`func(a, b) (c, d)`) have no
	// receiver — the first `(` opens the parameter list and is
	// followed by another `(` for the return group or end-of-string.
	// Disambiguate by peeking past the first balanced `(...)` group
	// for an identifier letter.
	if strings.HasPrefix(sig, "(") {
		end := findBalancedParenEnd(sig)
		if end < 0 {
			return ""
		}
		afterFirstGroup := strings.TrimSpace(sig[end+1:])
		if len(afterFirstGroup) > 0 {
			r := afterFirstGroup[0]
			isIdent := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_'
			if isIdent {
				sig = afterFirstGroup
			}
		}
	}

	// Skip the function name — everything up to the parameter list's
	// opening `(`.
	if i := strings.Index(sig, "("); i >= 0 {
		sig = sig[i:]
	} else {
		return ""
	}

	// Skip parameter list.
	end := findBalancedParenEnd(sig)
	if end < 0 {
		return ""
	}
	sig = strings.TrimSpace(sig[end+1:])
	if sig == "" {
		return ""
	}

	// Return clause — either `(T1, T2, ...)` or a bare single type.
	var inner string
	if strings.HasPrefix(sig, "(") {
		end := findBalancedParenEnd(sig)
		if end < 0 {
			return ""
		}
		inner = sig[1:end]
	} else {
		inner = sig
	}
	return firstNonErrorReturnField(inner)
}

// findBalancedParenEnd returns the index of the `)` that closes the
// `(` at s[0]. Returns -1 when the parens don't balance.
func findBalancedParenEnd(s string) int {
	if len(s) == 0 || s[0] != '(' {
		return -1
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// firstNonErrorReturnField splits a return-clause body by top-level
// commas and returns the first type expression that isn't `error`.
// Named return parameters (`result *User`) are handled by taking the
// last whitespace-separated token as the type.
func firstNonErrorReturnField(inner string) string {
	var fields []string
	depth := 0
	start := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				fields = append(fields, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	if last := strings.TrimSpace(inner[start:]); last != "" {
		fields = append(fields, last)
	}
	for _, f := range fields {
		t := f
		// Named return: `ctx context.Context`, `result *User`. Grab
		// the last whitespace-separated token — that's the type.
		if parts := strings.Fields(f); len(parts) > 1 {
			t = parts[len(parts)-1]
		}
		if t == "error" || strings.HasSuffix(t, ".error") {
			continue
		}
		return t
	}
	return ""
}

// snapshotContractShapes walks every request_type / response_type
// reference in the registry, loads each referenced type node's source,
// and attaches the extracted Shape to the node's Meta["shape"].
//
// We:
//   - Collect the unique set of symbol IDs — a popular DTO might be a
//     request/response on dozens of routes and we want to parse its
//     source once.
//   - Read each file once (cached in the source map).
//   - Skip nodes whose ID doesn't look like a symbol (bare names that
//     couldn't be upgraded) — those have nothing to dereference.
//   - Skip type nodes that already have a shape attached from a prior
//     pass on the same session (ETag-style short-circuit).
func (idx *Indexer) snapshotContractShapes(reg *contracts.Registry) {
	symbols := make(map[string]struct{})
	for _, c := range reg.All() {
		for _, key := range []string{"request_type", "response_type"} {
			v, _ := c.Meta[key].(string)
			if v == "" || !strings.Contains(v, "::") {
				continue
			}
			symbols[v] = struct{}{}
		}
		// Envelope rows reference types the dashboard wants expanded
		// just as much as the top-level response_type does — without
		// snapshotting them here, the inlineEnvelopeShapes pass below
		// finds no shape to fold into the row.
		if env, ok := c.Meta["response_envelope"].([]map[string]any); ok {
			for _, row := range env {
				v, _ := row["type"].(string)
				if v == "" || !strings.Contains(v, "::") {
					continue
				}
				symbols[v] = struct{}{}
			}
		}
	}
	if len(symbols) == 0 {
		return
	}
	symbolIDs := make([]string, 0, len(symbols))
	for id := range symbols {
		symbolIDs = append(symbolIDs, id)
	}
	sort.Strings(symbolIDs)
	typeNodes := idx.graph.GetNodesByIDs(symbolIDs)
	srcCache := make(map[string][]byte)
	attached := 0
	var updatedNodes []*graph.Node
	for _, id := range symbolIDs {
		node := typeNodes[id]
		if node == nil {
			continue
		}
		// Accept both KindType and KindInterface — TypeScript /
		// Java / Kotlin model their type defs as interfaces, and
		// the dashboard wants their fields expanded just like Go
		// struct types. Limiting to KindType silently dropped every
		// TS interface shape extraction.
		if node.Kind != graph.KindType && node.Kind != graph.KindInterface {
			continue
		}
		if _, done := node.Meta["shape"]; done {
			continue
		}
		src, ok := srcCache[node.FilePath]
		if !ok {
			// File paths in the graph are repo-prefixed; trim the
			// prefix for disk I/O.
			diskPath := node.FilePath
			if idx.repoPrefix != "" && strings.HasPrefix(diskPath, idx.repoPrefix+"/") {
				diskPath = strings.TrimPrefix(diskPath, idx.repoPrefix+"/")
			}
			diskPath = filepath.Join(idx.rootPath, diskPath)
			data, err := os.ReadFile(diskPath)
			if err != nil {
				srcCache[node.FilePath] = nil
				continue
			}
			srcCache[node.FilePath] = data
			src = data
		}
		if src == nil {
			continue
		}
		shape := contracts.ExtractShape(node.FilePath, src, node.StartLine, node.EndLine)
		if shape == nil {
			continue
		}
		if node.Meta == nil {
			node.Meta = map[string]any{}
		}
		node.Meta["shape"] = shape
		updatedNodes = append(updatedNodes, node)
		attached++
	}
	if attached > 0 {
		// GetNodesByIDs returns detached values on SQLite. Persist the compact
		// shape updates in one batch so the following hydration pass observes
		// identical data on both backends and warm restarts can reuse it.
		idx.graph.AddBatch(updatedNodes, nil)
		idx.logger.Info("contract shapes snapshotted",
			zap.Int("types", attached),
			zap.Int("examined", len(symbols)))
	}
}

// inlineEnvelopeShapes folds each type node's snapshotted shape onto
// every response_envelope row that references it. After this pass an
// envelope row carries the full JSON-rendering data:
//
//	{
//	  "name":  "repos",
//	  "expr":  "repos",
//	  "type":  "<repo>/service.go::Repo",
//	  "shape": { "kind": "struct", "fields": [...] }
//	}
//
// so the dashboard can render the actual response shape instead of a
// bare type-symbol-ID. Idempotent: rows that already carry "shape"
// are skipped, which lets cross-pass calls (re-extraction, snapshot
// restore) re-run cheaply.
//
// Also handles the top-level response_type / request_type: the
// referenced type's shape is mirrored onto Meta["response_shape"] /
// Meta["request_shape"] so plain-typed responses (no map envelope)
// also expose their JSON object shape on the dashboard.
func (idx *Indexer) inlineEnvelopeShapes(reg *contracts.Registry) {
	contractsAll := reg.All()
	typeIDs := make(map[string]struct{})
	collectTypeID := func(raw any) {
		id, _ := raw.(string)
		if id != "" && strings.Contains(id, "::") {
			typeIDs[id] = struct{}{}
		}
	}
	for _, c := range contractsAll {
		if c.Meta == nil {
			continue
		}
		collectTypeID(c.Meta["response_type"])
		collectTypeID(c.Meta["request_type"])
		if env, ok := c.Meta["response_envelope"].([]map[string]any); ok {
			for _, row := range env {
				collectTypeID(row["type"])
			}
		}
	}
	ids := make([]string, 0, len(typeIDs))
	for id := range typeIDs {
		ids = append(ids, id)
	}
	typeNodes := idx.graph.GetNodesByIDs(ids)
	lookupShape := func(raw any) any {
		id, ok := raw.(string)
		if !ok || id == "" || !strings.Contains(id, "::") {
			return nil
		}
		node := typeNodes[id]
		if node == nil || node.Meta == nil {
			return nil
		}
		shape, ok := node.Meta["shape"]
		if !ok || shape == nil {
			return nil
		}
		return shape
	}
	inlined := 0
	for _, c := range contractsAll {
		changed := false

		// Envelope rows.
		if env, ok := c.Meta["response_envelope"].([]map[string]any); ok && len(env) > 0 {
			for ri, row := range env {
				if _, has := row["shape"]; has {
					continue
				}
				if shape := lookupShape(row["type"]); shape != nil {
					env[ri]["shape"] = shape
					changed = true
				}
			}
			if changed {
				items := reg.ByID(c.ID)
				for i := range items {
					if items[i].Role != contracts.RoleProvider || items[i].SymbolID != c.SymbolID {
						continue
					}
					if items[i].Meta == nil {
						items[i].Meta = map[string]any{}
					}
					items[i].Meta["response_envelope"] = env
				}
				reg.ReplaceByID(c.ID, items)
			}
		}

		// Top-level request/response shapes — same idea, applied to a
		// plain `response_type: "<id>::Foo"` so the schema view can
		// render the JSON object even when there's no envelope wrapper.
		topChanged := false
		for metaKey, shapeKey := range map[string]string{
			"response_type": "response_shape",
			"request_type":  "request_shape",
		} {
			if _, has := c.Meta[shapeKey]; has {
				continue
			}
			if shape := lookupShape(c.Meta[metaKey]); shape != nil {
				items := reg.ByID(c.ID)
				for i := range items {
					if items[i].Role != contracts.RoleProvider || items[i].SymbolID != c.SymbolID {
						continue
					}
					if items[i].Meta == nil {
						items[i].Meta = map[string]any{}
					}
					items[i].Meta[shapeKey] = shape
				}
				reg.ReplaceByID(c.ID, items)
				topChanged = true
			}
		}

		if changed || topChanged {
			inlined++
		}
	}
	if inlined > 0 {
		idx.logger.Info("response envelopes hydrated with shapes",
			zap.Int("contracts", inlined))
	}
}

// extractExternalModules parses the repo's go.mod once and writes
// KindModule nodes plus EdgeDependsOnModule edges into the graph.
// A synthetic KindFile node is emitted for go.mod itself so the
// edges have a real source endpoint that survives applyRepoPrefix.
// Safe to call when no go.mod exists. Other manifest formats
// (package.json, pnpm-lock, requirements.txt, Cargo.toml, …) are
// future additions — each lands as another switch case here.
//
// Import-node → module-node edges (per the broader coverage spec)
// are deferred to v2; the v1 file-level edge is already enough for
// agents asking "what does this repo depend on".
func (idx *Indexer) extractExternalModules() {
	if !idx.config.Coverage.IsEnabled("modules") {
		return
	}
	// Walk known manifest formats at the repo root. Each manifest
	// produces an independent Spec list and gets its own synthetic
	// file node — the file→module edge stays scoped to the
	// originating manifest so cross-ecosystem queries (e.g. "what
	// does package.json declare") don't bleed into go.mod's
	// answer.
	manifests := []struct {
		path           string
		parse          func([]byte) []modules.Spec
		ownPathFromSrc func([]byte) string
	}{
		{
			path:           "go.mod",
			parse:          modules.ParseGoMod,
			ownPathFromSrc: readGoModModulePath,
		},
		{
			path:           "package.json",
			parse:          modules.ParsePackageJSON,
			ownPathFromSrc: readPackageJSONOwnName,
		},
		{
			path:           "package-lock.json",
			parse:          modules.ParsePackageLockJSON,
			ownPathFromSrc: nil, // package-lock has no own-name notion separate from package.json
		},
		{
			path:           "yarn.lock",
			parse:          modules.ParseYarnLock,
			ownPathFromSrc: nil,
		},
		{
			path:           "pnpm-lock.yaml",
			parse:          modules.ParsePnpmLock,
			ownPathFromSrc: nil,
		},
		{
			path:           "pyproject.toml",
			parse:          modules.ParsePyProject,
			ownPathFromSrc: readPyProjectOwnName,
		},
		{
			path:           "requirements.txt",
			parse:          modules.ParseRequirementsTxt,
			ownPathFromSrc: nil, // requirements.txt has no own-name notion
		},
		{
			path:           "Cargo.toml",
			parse:          modules.ParseCargoToml,
			ownPathFromSrc: readCargoTomlOwnName,
		},
		{
			path:           "pom.xml",
			parse:          modules.ParsePomXML,
			ownPathFromSrc: readPomXMLOwnName,
		},
		{
			path:           "composer.json",
			parse:          modules.ParseComposerJSON,
			ownPathFromSrc: modules.ComposerOwnName,
		},
		{
			path:  "composer.lock",
			parse: modules.ParseComposerLock,
			// The lockfile has no own-name notion separate from composer.json.
			ownPathFromSrc: nil,
		},
	}

	for _, m := range manifests {
		idx.extractOneModuleManifest(m.path, m.parse, m.ownPathFromSrc)
	}

	// After per-manifest module extraction, detect whether this repo's
	// root is a package-manager workspace and materialise its
	// root→member edges.
	idx.extractPackageWorkspace()
}

// extractOneModuleManifest reads a single manifest file from the
// repo root, parses it via the supplied parser, and writes the
// resulting nodes/edges + import-link edges into the graph. Used
// from extractExternalModules's per-manifest dispatch.
func (idx *Indexer) extractOneModuleManifest(relPath string, parse func([]byte) []modules.Spec, ownPathFromSrc func([]byte) string) {
	manifestAbs := filepath.Join(idx.rootPath, relPath)
	src, err := os.ReadFile(manifestAbs)
	if err != nil {
		return
	}
	idx.extractOneModuleManifestSource(relPath, src, parse, ownPathFromSrc)
}

// extractOneModuleManifestSource materialises a manifest from caller-owned
// bytes. Incremental refresh uses this form so the graph mutation and the
// post-commit mtime receipt refer to the exact same file version; the cold path
// above retains its single os.ReadFile with no extra stat/hash overhead.
func (idx *Indexer) extractOneModuleManifestSource(
	relPath string,
	src []byte,
	parse func([]byte) []modules.Spec,
	ownPathFromSrc func([]byte) string,
) {
	specs := parse(src)
	nodes, edges := modules.BuildGraphArtifacts(relPath, specs)
	// Synthetic file node for the manifest — it isn't represented
	// through the language-extractor pipeline (no extractor is
	// registered for the .mod extension; package.json may have one
	// but the JSON walker doesn't emit a synthetic file node we
	// can reuse), so the EdgeDependsOnModule edges would otherwise
	// dangle from a missing source endpoint after applyRepoPrefix
	// runs in multi-repo mode.
	manifestNode := &graph.Node{
		ID:       relPath,
		Kind:     graph.KindFile,
		Name:     filepath.Base(relPath),
		FilePath: relPath,
		Language: manifestLanguage(relPath),
	}
	// composer.json's autoload map is the only statement a PHP repo makes
	// about which namespaces are its own. Carried on the manifest node so the
	// resolver can tell a first-party `use` from a dependency's without
	// re-reading the file.
	if filepath.Base(relPath) == "composer.json" {
		if roots := modules.ComposerAutoloadRoots(src); len(roots) > 0 {
			manifestNode.Meta = map[string]any{"php_autoload_roots": encodeComposerAutoloadRoots(roots)}
		}
	}
	allNodes := append([]*graph.Node{manifestNode}, nodes...)
	idx.applyRepoPrefix(allNodes, edges)
	idx.graph.AddBatch(allNodes, edges)

	// Connect each KindImport node to its matching module via
	// longest-prefix path resolution. Repo-internal imports (the
	// own-module path) are filtered inside LinkImports — when the
	// manifest doesn't expose an own-name, the filter is a no-op
	// which is safe (no own-module imports to match against).
	var ownModulePath string
	if ownPathFromSrc != nil {
		ownModulePath = ownPathFromSrc(src)
	}
	// Scope the walk to this repo's own import nodes. The unscoped
	// An unscoped import walk is O(R · N) under a warmup loop across
	// hundreds of repos. The per-repo byRepo bucket
	// keeps this O(repo size).
	importIDs := graph.ReadRepoNodeIDsByKinds(
		idx.graph, []string{idx.repoPrefix}, []graph.NodeKind{graph.KindImport},
	)
	importsByID := idx.graph.GetNodesByIDs(importIDs)
	importNodes := make([]*graph.Node, 0, len(importIDs))
	for _, id := range importIDs {
		if node := importsByID[id]; node != nil {
			importNodes = append(importNodes, node)
		}
	}
	modules.LinkImportsIn(idx.graph, importNodes, specs, ownModulePath)
}

// encodeComposerAutoloadRoots flattens the PSR-4 / PSR-0 map into the
// `Prefix=>dir,dir;Prefix=>dir` form graph Meta can hold (Meta is gob-encoded,
// so a nested map costs a codec entry per shape). Sorted for determinism.
func encodeComposerAutoloadRoots(roots map[string][]string) string {
	prefixes := make([]string, 0, len(roots))
	for p := range roots {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	parts := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		parts = append(parts, p+"=>"+strings.Join(roots[p], ","))
	}
	return strings.Join(parts, ";")
}

// manifestLanguage returns the language tag stamped on a manifest's
// synthetic file node. Used purely for Brief listings — the kind
// field carries the structural type.
func manifestLanguage(relPath string) string {
	switch filepath.Base(relPath) {
	case "go.mod":
		return "go"
	case "package.json", "package-lock.json":
		return "json"
	case "yarn.lock":
		return "yarn"
	case "pnpm-lock.yaml":
		return "yaml"
	case "pyproject.toml", "Cargo.toml":
		return "toml"
	case "requirements.txt":
		return "text"
	case "pom.xml":
		return "xml"
	case "composer.json", "composer.lock":
		// "json", never "php": the synthetic manifest node shares its ID with
		// the JSON extractor's file node, and a php-tagged file would vouch
		// for PHP presence in a repo that holds no PHP source.
		return "json"
	}
	return ""
}

// readPomXMLOwnName builds the project's own Maven coordinate
// (groupId:artifactId) so LinkImports can filter self-references.
// Java workspace setups where a sibling module imports the parent
// project shouldn't accidentally resolve to an external dep with
// the same coordinate. Returns "" when either field is missing —
// the LinkImports filter treats "" as no own-module filter, which
// is safe.
func readPomXMLOwnName(src []byte) string {
	var manifest struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
	}
	if err := xml.Unmarshal(src, &manifest); err != nil {
		return ""
	}
	if manifest.GroupID == "" || manifest.ArtifactID == "" {
		return ""
	}
	return manifest.GroupID + ":" + manifest.ArtifactID
}

// readCargoTomlOwnName reads the crate's own name from
// `[package] name`. Used for LinkImports's own-module filter so
// workspace-internal crate references don't accidentally match
// external crates with the same name.
func readCargoTomlOwnName(src []byte) string {
	var manifest struct {
		Package struct {
			Name string `toml:"name"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(src, &manifest); err != nil {
		return ""
	}
	return manifest.Package.Name
}

// readPyProjectOwnName returns the package's own name from the
// pyproject.toml `[project] name` field. Used by LinkImports's
// own-module filter so a workspace package's internal imports
// don't accidentally collide with external pypi names. Returns ""
// on parse error or when the field is absent.
func readPyProjectOwnName(src []byte) string {
	var manifest struct {
		Project struct {
			Name string `toml:"name"`
		} `toml:"project"`
	}
	if err := toml.Unmarshal(src, &manifest); err != nil {
		return ""
	}
	return manifest.Project.Name
}

// readPackageJSONOwnName extracts the manifest's `name` field — the
// npm equivalent of go.mod's `module` directive. Returns "" on
// missing or malformed JSON; LinkImports treats "" as "no own-
// module filter", which is safe because internal-package matches
// (e.g. workspaces) won't accidentally collide with external deps
// at the longest-prefix scan.
func readPackageJSONOwnName(src []byte) string {
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(src, &manifest); err != nil {
		return ""
	}
	return manifest.Name
}

// readGoModModulePath extracts the `module ` directive value from
// go.mod source. Mirrors the inline parse in coverage.ReadModulePath
// — we keep both copies tiny rather than introducing a one-import
// shared helper that would force a layering compromise (coverage
// shouldn't depend on indexer; indexer shouldn't depend on coverage).
func readGoModModulePath(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// extractGoModContracts runs the go.mod-specific extractor once against
// the repo root (go.mod isn't represented as a file node in the graph).
// Results are added to reg. Safe to call when no go.mod exists.
//
// Also materialises the dep::<module> contracts as graph nodes
// immediately, so the resolver's import-bridge (Resolver.lookupDepModule)
// can find them during ResolveAll. commitContracts later AddNode is
// idempotent — it skips nodes that already exist — so this doesn't
// double-emit. We only do this for type=dependency; everything else
// goes through the normal commit path which depends on a resolved
// graph (UpgradeBareTypeRefs, resolveProviderHandlers).
func (idx *Indexer) extractGoModContracts(reg *contracts.Registry) {
	goModPath := filepath.Join(idx.rootPath, "go.mod")
	goModSrc, err := os.ReadFile(goModPath)
	if err != nil {
		return
	}
	goModExtractor := &contracts.GoModExtractor{TrackedRepos: idx.trackedRepoModules}
	goModFilePath := "go.mod"
	if idx.repoPrefix != "" {
		goModFilePath = idx.repoPrefix + "/go.mod"
	}
	found := goModExtractor.Extract(goModFilePath, goModSrc, nil, nil)
	reg.AddAllScoped(found, idx.repoPrefix, idx.workspaceID, idx.projectID)

	dependencyIDs := make([]string, 0, len(found))
	for i := range found {
		if found[i].Type == contracts.ContractDependency {
			dependencyIDs = append(dependencyIDs, found[i].ID)
		}
	}
	existing := idx.graph.GetNodesByIDs(dependencyIDs)

	var nodes []*graph.Node
	for i := range found {
		c := found[i]
		if c.Type != contracts.ContractDependency {
			continue
		}
		if existing[c.ID] != nil {
			continue
		}
		nodes = append(nodes, &graph.Node{
			ID:         c.ID,
			Kind:       graph.KindContract,
			Name:       c.ID,
			FilePath:   c.FilePath,
			Language:   "contract",
			RepoPrefix: idx.repoPrefix,
			Meta:       map[string]any{"type": string(c.Type), "role": string(c.Role)},
		})
	}
	if len(nodes) > 0 {
		idx.graph.AddBatch(nodes, nil)
	}
}

// extractContracts scans all file nodes in the graph and runs contract
// extractors to detect API contracts (HTTP routes, gRPC services,
// GraphQL, topics, etc.). Detected contracts are added as graph nodes
// with provides/consumes edges.
//
// This full-walk path is used by IncrementalReindex (where many files
// are already cached). IndexCtx instead runs the per-file work inline
// with parsing — see the worker loop — and skips this function.
func (idx *Indexer) extractContracts() {
	reg := contracts.NewRegistry()
	_, byLang := idx.buildPerFileContractExtractors()

	// Track which file nodes we saw this pass so we can prune stale
	// cache entries afterwards (files that left the graph).
	seenFiles := make(map[string]struct{})

	// Multi-repo mode: walk only this repo's nodes. The previous
	// AllNodes()-then-filter pass paid an O(global) walk per repo,
	// which compounded with hundreds of tracked siblings.
	nodes := graph.RepoCodeNodes(idx.graph, idx.repoPrefix)

	// Pre-bucket the already-fetched node slice by FilePath so the
	// per-file body can look up its co-located nodes in O(1) instead
	// of firing a fresh GetFileNodes query per file. Likewise pre-
	// fetch every out-edge whose source is in this repo as ONE backend
	// call and bucket by From so the per-file body can replace
	// GetOutEdges(fileNode.ID) — on disk backends the per-file query
	// path was the second-largest source of round-trips in
	// deferred_passes (after the DI walk).
	nodesByFile := make(map[string][]*graph.Node, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		nodesByFile[n.FilePath] = append(nodesByFile[n.FilePath], n)
	}
	var edgesByFrom map[string][]*graph.Edge
	if idx.repoPrefix != "" {
		repoEdges := idx.graph.GetRepoEdges(idx.repoPrefix)
		edgesByFrom = make(map[string][]*graph.Edge, len(nodes))
		for _, e := range repoEdges {
			if e == nil {
				continue
			}
			edgesByFrom[e.From] = append(edgesByFrom[e.From], e)
		}
	} else {
		nodeIDs := make([]string, 0, len(nodes))
		for _, n := range nodes {
			if n != nil {
				nodeIDs = append(nodeIDs, n.ID)
			}
		}
		edgesByFrom = idx.graph.GetOutEdgesByNodeIDs(nodeIDs)
	}

	for _, fileNode := range nodes {
		if fileNode.Kind != graph.KindFile {
			continue
		}

		// In multi-repo mode the byRepo bucket is already scoped, but
		// the path-prefix strip below still needs to run.
		relPath := fileNode.FilePath
		if idx.repoPrefix != "" {
			if !strings.HasPrefix(relPath, idx.repoPrefix+"/") {
				continue
			}
			relPath = strings.TrimPrefix(relPath, idx.repoPrefix+"/")
		}

		absPath := filepath.Join(idx.rootPath, relPath)
		info, statErr := os.Stat(absPath)
		if statErr != nil {
			continue
		}
		seenFiles[fileNode.FilePath] = struct{}{}
		currentMtime := info.ModTime().UnixNano()

		// Cache hit: replay the previously-extracted contracts without
		// re-reading the file or re-running the 8 extractors. This is
		// the dominant savings path on repos with many files where most
		// haven't changed since the last extraction (e.g. live re-index
		// after a single-file edit).
		idx.contractCacheMu.RLock()
		cached, ok := idx.contractCache[fileNode.FilePath]
		idx.contractCacheMu.RUnlock()
		if ok && cached.mtimeNano == currentMtime {
			for _, c := range cached.contracts {
				reg.Add(c)
			}
			continue
		}

		src, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}

		fileNodes := nodesByFile[fileNode.FilePath]
		fileEdges := edgesByFrom[fileNode.ID]

		// Language-filtered dispatch: skip extractors that don't list
		// this file's language in SupportedLanguages(). On big repos
		// with lots of .css / .svg / .json etc. this cuts a lot of
		// no-op extractor calls.
		exts := byLang[fileNode.Language]
		// Re-parse for AST overlay — the language extractor's tree
		// from the original index pass was closed when the file was
		// first added. Cheap (~milliseconds per file) and cleanly
		// scoped to this contract-pass invocation.
		tree := contracts.ParseTreeForLang(fileNode.Language, src)
		fileContracts := idx.runContractExtractorsForFile(
			fileNode.FilePath, src, fileNodes, fileEdges, exts, tree)
		tree.Release()
		for _, c := range fileContracts {
			reg.Add(c)
		}

		idx.contractCacheMu.Lock()
		idx.contractCache[fileNode.FilePath] = &contractCacheEntry{
			mtimeNano: currentMtime,
			contracts: fileContracts,
		}
		idx.contractCacheMu.Unlock()
	}

	// Prune cache entries for files that are no longer in the graph
	// (deleted, or repo untracked). Otherwise the cache grows unboundedly
	// across the lifetime of the daemon.
	idx.contractCacheMu.Lock()
	for path := range idx.contractCache {
		if _, ok := seenFiles[path]; !ok {
			delete(idx.contractCache, path)
		}
	}
	idx.contractCacheMu.Unlock()

	idx.extractGoModContracts(reg)
	idx.extractDIContracts(reg)
	idx.commitContracts(reg)
}

// IsStale returns true if the file at relPath has been modified on disk since
// it was last indexed, based on comparing stored mtime against current disk mtime.
//
// relPath is folded to the canonical key (slash form, Unicode NFC)
// before lookup so a caller passing a non-ASCII path in a different
// Unicode form than fileMtimes was keyed with still resolves — without
// the fold the lookup would miss and the file be reported permanently
// stale, re-indexing it under a second key on every pass.
// HasChangesSinceMtimes reports whether any indexable file under root
// changed (mtime differs or is new) or was deleted, relative to the
// indexer's currently-loaded fileMtimes. It runs the SAME walk +
// staleness + deletion logic as IncrementalReindex but writes nothing.
//
// The daemon warmup uses it to choose a reconcile strategy for a
// reopened repo: a repo with zero changes takes the fast no-op
// IncrementalReindex path, while a repo that changed while the daemon
// was down is routed through the shadow/bulk re-track path instead.
// That routing matters because IncrementalReindex re-resolves changed
// files through per-edge graph.ReindexEdges, and the per-edge write
// path against a freshly reopened disk store is slow and unreliable.
// The shadow path resolves entirely in an in-memory graph and commits
// the result in one bulk load, so it never issues a per-edge write to
// the reopened store. It re-indexes the whole repo (more work than a
// true incremental pass), but it is reliable, and only repos that
// actually changed during downtime pay the cost.
//
// Conservative on error: anything it can't determine (bad root, walk
// error) returns true so the caller re-indexes rather than silently
// serving a stale graph.
func (idx *Indexer) HasChangesSinceMtimes(root string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return true
	}
	idx.storeRootPath(absRoot)

	diskFiles := make(map[string]bool)
	errStop := errors.New("stop-walk")
	walkErr := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldPruneDir(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := idx.effectiveLanguage(path, nil); !ok {
			return nil
		}
		if idx.shouldExclude(path, absRoot, false) {
			return nil
		}
		rel := idx.relKey(path)
		diskFiles[rel] = true
		if idx.IsStale(rel) {
			return errStop // a single changed/new file is enough
		}
		return nil
	})
	if errors.Is(walkErr, errStop) {
		return true
	}
	if walkErr != nil {
		return true
	}

	// Deletion check: a previously-indexed file absent from the walk and
	// confirmed gone from disk counts as a change (its edges must drop).
	idx.mtimeMu.RLock()
	var candidates []string
	for rel := range idx.fileMtimes {
		if !diskFiles[rel] {
			candidates = append(candidates, rel)
		}
	}
	idx.mtimeMu.RUnlock()
	for _, rel := range candidates {
		if _, err := os.Stat(filepath.Join(absRoot, filepath.FromSlash(rel))); errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// ChangedSinceMtimes is the accumulating variant of HasChangesSinceMtimes:
// it runs the SAME walk + IsStale staleness predicate + deletion-confirm
// logic but, instead of short-circuiting on the first stale file, returns
// the full changed and deleted sets as in-repo relative slash-paths.
//
// The warm-restart reconcile router uses the census to size the reconcile:
// an empty result takes the fast no-op path, a small delta is scoped to
// exactly those files (re-index the changed, evict the deleted), and only a
// large-fraction churn falls back to a whole-repo re-track.
//
// changed holds files that are new or whose mtime drifted since the last
// pass; deleted holds previously-indexed files confirmed gone from disk. A
// walk (or abs-path) error returns a non-nil err with nil slices — the
// caller treats that as "unknown, do a full re-track", exactly as
// HasChangesSinceMtimes conservatively returns true on the same condition.
func (idx *Indexer) ChangedSinceMtimes(root string) (changed []string, deleted []string, err error) {
	absRoot, absErr := filepath.Abs(root)
	if absErr != nil {
		return nil, nil, absErr
	}
	idx.storeRootPath(absRoot)

	diskFiles := make(map[string]bool)
	walkErr := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldPruneDir(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := idx.effectiveLanguage(path, nil); !ok {
			return nil
		}
		if idx.shouldExclude(path, absRoot, false) {
			return nil
		}
		rel := idx.relKey(path)
		diskFiles[rel] = true
		if idx.IsStale(rel) {
			changed = append(changed, rel)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}

	// Deletion check: a previously-indexed file absent from the walk and
	// confirmed gone from disk counts as a change (its edges must drop).
	idx.mtimeMu.RLock()
	var candidates []string
	for rel := range idx.fileMtimes {
		if !diskFiles[rel] {
			candidates = append(candidates, rel)
		}
	}
	idx.mtimeMu.RUnlock()
	for _, rel := range candidates {
		if _, statErr := os.Stat(filepath.Join(absRoot, filepath.FromSlash(rel))); errors.Is(statErr, os.ErrNotExist) {
			deleted = append(deleted, rel)
		}
	}
	return changed, deleted, nil
}

func (idx *Indexer) IsStale(relPath string) bool {
	relPath = pathkey.Normalize(filepath.ToSlash(relPath))

	idx.mtimeMu.RLock()
	storedMtime, ok := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	if !ok {
		// Unknown file — treat as stale.
		return true
	}

	absPath := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
	info, err := os.Stat(absPath)
	if err != nil {
		// Can't stat — treat as stale.
		return true
	}

	return info.ModTime().UnixNano() != storedMtime
}

// IsTrackedStale reports whether a file that IS in the index has changed
// on disk since it was indexed. Unlike IsStale it returns false for an
// untracked path (a new file, a non-source path, or a path-form
// mismatch), so a freshness signal never false-positives on a file the
// index legitimately does not cover.
func (idx *Indexer) IsTrackedStale(relPath string) bool {
	return idx.TrackedFileState(relPath) == FileStale
}

// FileFreshness is the verdict TrackedFileState returns for a tracked file:
// its indexed view is current, has drifted, or the file is gone from disk.
type FileFreshness string

const (
	// FileFresh means the file is not tracked (so no claim is made) or its
	// on-disk mtime still matches what was indexed.
	FileFresh FileFreshness = "fresh"
	// FileStale means the file is tracked and its on-disk mtime differs from
	// the indexed mtime — the graph view lags the working tree.
	FileStale FileFreshness = "stale"
	// FileMissing means the file is tracked in the graph but no longer exists
	// on disk — it was deleted or moved since indexing. A plain staleness
	// check folds this into "not stale"; the distinct verdict lets a list
	// result flag hits that point at vanished files.
	FileMissing FileFreshness = "missing"
)

// TrackedFileState classifies one repo-relative file against the indexer's
// recorded mtimes: fresh (untracked or unchanged), stale (mtime drift), or
// missing (tracked but absent on disk). Splitting the stat-failure branch out
// of IsTrackedStale is what lets the freshness rider distinguish a stale hit
// from one whose underlying file has been deleted.
func (idx *Indexer) TrackedFileState(relPath string) FileFreshness {
	relPath = pathkey.Normalize(filepath.ToSlash(relPath))

	idx.mtimeMu.RLock()
	storedMtime, ok := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	if !ok {
		return FileFresh
	}

	absPath := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return FileMissing
		}
		// A transient / permission stat error is not evidence of drift —
		// don't cry wolf for a file we simply could not read.
		return FileFresh
	}
	if info.ModTime().UnixNano() != storedMtime {
		return FileStale
	}
	return FileFresh
}
