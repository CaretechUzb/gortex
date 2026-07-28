package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/cochange"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/coverage"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/releases"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// realController is the production daemon.Controller implementation. It
// wraps the MultiIndexer and ConfigManager so track/untrack/reload/status
// operations go through the same code paths the current `gortex mcp`
// command uses.
//
// Methods are serialized via a mutex — track/reload can race with status
// otherwise. The mutex is coarse; finer locking is a later optimization.
type realController struct {
	mu            sync.Mutex
	graph         graph.Store
	indexer       *indexer.Indexer
	multiIndexer  *indexer.MultiIndexer
	configManager *config.ConfigManager
	// multiWatcher is an atomic pointer, not a mu-guarded field: the daemon's
	// teardown hook reads it, and reading it under mu is what kept `daemon
	// stop` queued behind a running track / reload / enrichment. One writer
	// (AttachWatcher, during warmup); read via watcher().
	multiWatcher atomic.Pointer[indexer.MultiWatcher]
	logger       *zap.Logger

	// liveRouter is the multi-server Router currently wired into the
	// dispatch path (nil for a local-only daemon with no roster).
	// localExecute + publishRouter let ReloadServers build and publish
	// a router live when the first remote is added after startup, or
	// tear it down when the last remote is removed — all without a
	// daemon restart. Guarded by mu.
	liveRouter    *daemon.Router
	localExecute  daemon.LocalExecutor
	publishRouter func(*daemon.Router)

	// onShutdown is invoked by the Shutdown method. Used by the daemon
	// main to flush savings, close the snapshot store, etc.
	//
	// Guarded by its own mutex, deliberately NOT by mu. Shutdown is the
	// one control RPC that has to work when the daemon is at its least
	// healthy, and mu is held for the entire duration of a track / reload /
	// enrichment — minutes on a large workspace. Reading this field under
	// mu made `gortex daemon stop` queue behind exactly the condition it
	// exists to escape.
	shutdownMu sync.Mutex
	onShutdown func() error

	// toolSurface reports the active tool-surface preset + mode and the
	// per-workspace learned-promotion count for `gortex daemon status`.
	// Nil when the MCP server isn't wired (control-only daemon).
	toolSurface func() (preset, mode string, learned int)

	// ready flips to true once references are resolved and the graph is
	// queryable — find_usages / get_callers return complete results from
	// this point. The socket accepts connections before this; queries
	// against not-yet-resolved repos return partial results until ready.
	// warmupSeconds records how long the parse + resolve stage took.
	//
	// enriched flips to true once the slow semantic-enrichment pass and the
	// graph-wide derivation passes finish in the background, after ready.
	// Background timers that must not fight the enrichment pipeline for
	// shard locks (the periodic snapshotter) gate on enriched, not ready.
	// enrichSeconds records the full warmup duration.
	ready         atomic.Bool
	warmupSeconds atomic.Int64
	enriched      atomic.Bool
	enrichSeconds atomic.Int64
}

// Track indexes a new repository and persists it to the global config.
// Path is resolved to an absolute form before the MultiIndexer sees it.
func (c *realController) Track(ctx context.Context, p daemon.TrackParams) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}
	absPath, err := filepath.Abs(p.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	entry := config.RepoEntry{Path: absPath, Name: p.Name, Ref: p.Ref, AsWorktree: p.AsWorktree}
	result, err := c.multiIndexer.TrackRepoCtx(ctx, entry)
	if err != nil {
		return nil, err
	}
	if result == nil {
		// Already tracked — idempotent.
		return json.RawMessage(fmt.Sprintf(`{"status":"already_tracked","path":%q}`, absPath)), nil
	}
	// TrackRepoCtx may have derived a worktree-instance prefix that the
	// by-value entry above can't see — read the prefix it actually
	// registered under for the watcher attach and the response.
	prefix := result.RepoPrefix
	if prefix == "" {
		prefix = config.ResolvePrefix(entry)
	}

	// Project association from TrackParams.Project isn't wired yet — the
	// config package doesn't expose an AddRepoToProject helper. Callers
	// who need project scoping can edit ~/.gortex/config.yaml and
	// run `gortex daemon reload`; track from the daemon-v1 surface just
	// adds to the top-level repo list.

	// Attach a watcher to the newly-tracked repo so file edits in it
	// flow back into the graph live without a manual reload. Failures
	// here are logged but don't fail the track — an indexed-but-
	// unwatched repo is still queryable, just stale if edited.
	if mw := c.watcher(); mw != nil && c.configManager != nil {
		wcfg := c.configManager.GetRepoConfig(prefix).Watch
		if err := mw.AddRepo(prefix, wcfg); err != nil {
			c.logger.Warn("track: attach watcher failed",
				zap.String("prefix", prefix), zap.Error(err))
		}
	}

	// Persist the config change. TrackRepoCtx mutates the in-memory
	// GlobalConfig via AddRepo but does not flush to disk; without this
	// Save the new repo vanishes on daemon restart. Mirrors Untrack.
	if c.configManager != nil {
		if err := c.configManager.Global().Save(); err != nil {
			c.logger.Warn("track: save config failed", zap.Error(err))
		}
	}

	return json.Marshal(map[string]any{
		"status":     "tracked",
		"path":       absPath,
		"prefix":     prefix,
		"file_count": result.FileCount,
		"node_count": result.NodeCount,
		"edge_count": result.EdgeCount,
	})
}

// EnrichChurn runs the churn enricher in-process against the daemon's
// graph. We hold c.mu for the duration so a concurrent Track/Untrack
// can't reshape the set of files while the enricher walks them. The
// caller (CLI / git hook) picks the params; an empty Path means "every
// tracked repo", an empty Branch means "resolve each repo's default
// branch from its working tree".
func (c *realController) EnrichChurn(ctx context.Context, p daemon.EnrichChurnParams) (daemon.EnrichChurnResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.graph == nil {
		return daemon.EnrichChurnResult{}, fmt.Errorf("graph not initialized")
	}
	if c.multiIndexer == nil {
		return daemon.EnrichChurnResult{}, fmt.Errorf("multi-repo indexer not initialized")
	}

	// Resolve the set of repo roots the call targets. Empty Path =
	// every tracked repo. A path or prefix narrows to one.
	type target struct {
		prefix string
		root   string
	}
	var targets []target
	want := strings.TrimSpace(p.Path)
	for prefix, meta := range c.multiIndexer.AllMetadata() {
		if want != "" && want != prefix && want != meta.RootPath {
			continue
		}
		targets = append(targets, target{prefix: prefix, root: meta.RootPath})
	}
	if len(targets) == 0 {
		return daemon.EnrichChurnResult{}, fmt.Errorf("no tracked repo matches %q", p.Path)
	}

	started := time.Now()
	var combined daemon.EnrichChurnResult
	for _, t := range targets {
		branch := strings.TrimSpace(p.Branch)
		if branch == "" {
			branch = gitDefaultBranch(t.root)
		}
		if branch == "" {
			c.logger.Warn("enrich churn: no default branch resolved",
				zap.String("prefix", t.prefix), zap.String("root", t.root))
			continue
		}
		res, err := churn.EnrichGraph(ctx, c.graph, t.root, churn.Options{Branch: branch})
		if err != nil {
			return daemon.EnrichChurnResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Files += res.Files
		combined.Symbols += res.Symbols
		combined.Branch = res.Branch
		combined.HeadSHA = res.HeadSHA
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// EnrichReleases runs the per-file release enricher against the
// daemon's graph. Mirrors EnrichChurn — c.mu is held for the duration,
// targets resolve via the multi-indexer, and an empty Branch lets
// each repo's default branch be resolved on demand (so feature-branch
// tags don't leak into the timeline).
func (c *realController) EnrichReleases(ctx context.Context, p daemon.EnrichReleasesParams) (daemon.EnrichReleasesResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.graph == nil {
		return daemon.EnrichReleasesResult{}, fmt.Errorf("graph not initialized")
	}
	if c.multiIndexer == nil {
		return daemon.EnrichReleasesResult{}, fmt.Errorf("multi-repo indexer not initialized")
	}

	type target struct {
		prefix string
		root   string
	}
	var targets []target
	want := strings.TrimSpace(p.Path)
	for prefix, meta := range c.multiIndexer.AllMetadata() {
		if want != "" && want != prefix && want != meta.RootPath {
			continue
		}
		targets = append(targets, target{prefix: prefix, root: meta.RootPath})
	}
	if len(targets) == 0 {
		return daemon.EnrichReleasesResult{}, fmt.Errorf("no tracked repo matches %q", p.Path)
	}
	_ = ctx // graph mutation is synchronous; no cancellation surface today

	started := time.Now()
	var combined daemon.EnrichReleasesResult
	for _, t := range targets {
		branch := strings.TrimSpace(p.Branch)
		if branch == "" {
			branch = gitDefaultBranch(t.root)
			// Empty branch is still legal — releases.EnrichGraphForBranch
			// treats "" as "every tag", which is the right default when
			// no default branch can be resolved (e.g. a clone without
			// origin/HEAD set yet).
		}
		count, err := releases.EnrichGraphForBranch(c.graph, t.root, t.prefix, branch)
		if err != nil {
			return daemon.EnrichReleasesResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Files += count
		combined.Branch = branch
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// enrichTarget is one (prefix, root) pair the enrichers run against.
type enrichTarget struct {
	prefix string
	root   string
}

// resolveEnrichTargets maps the caller-supplied path scope onto the set
// of tracked repos to enrich. An empty path means "every tracked repo";
// a non-empty path narrows to the one repo whose prefix or root matches.
// Returns an error when nothing matches so the control caller gets a
// clear "no tracked repo" message rather than a silent zero-count
// success. Caller must hold c.mu.
func (c *realController) resolveEnrichTargets(path string) ([]enrichTarget, error) {
	if c.graph == nil {
		return nil, fmt.Errorf("graph not initialized")
	}
	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}
	var targets []enrichTarget
	want := strings.TrimSpace(path)
	for prefix, meta := range c.multiIndexer.AllMetadata() {
		if meta == nil || meta.RootPath == "" {
			continue
		}
		if want != "" && want != prefix && want != meta.RootPath {
			continue
		}
		targets = append(targets, enrichTarget{prefix: prefix, root: meta.RootPath})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no tracked repo matches %q", path)
	}
	return targets, nil
}

// EnrichBlame runs the git-blame authorship enricher against the
// daemon's graph. Mirrors EnrichChurn — c.mu is held for the duration
// and targets resolve via the multi-indexer.
func (c *realController) EnrichBlame(_ context.Context, p daemon.EnrichBlameParams) (daemon.EnrichBlameResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targets, err := c.resolveEnrichTargets(p.Path)
	if err != nil {
		return daemon.EnrichBlameResult{}, err
	}

	started := time.Now()
	var combined daemon.EnrichBlameResult
	for _, t := range targets {
		count, err := blame.EnrichGraph(c.graph, t.root)
		if err != nil {
			return daemon.EnrichBlameResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Nodes += count
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// EnrichCoverage projects the caller-parsed cover-profile segments onto
// the daemon's graph. The CLI parses the profile (the path is relative
// to the caller's cwd, not the daemon's), so the daemon only needs the
// segments and resolves each repo's module path from its working tree.
func (c *realController) EnrichCoverage(_ context.Context, p daemon.EnrichCoverageParams) (daemon.EnrichCoverageResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targets, err := c.resolveEnrichTargets(p.Path)
	if err != nil {
		return daemon.EnrichCoverageResult{}, err
	}

	segments := make([]coverage.Segment, len(p.Segments))
	for i, s := range p.Segments {
		segments[i] = coverage.Segment{
			File:      s.File,
			StartLine: s.StartLine,
			EndLine:   s.EndLine,
			NumStmt:   s.NumStmt,
			Count:     s.Count,
		}
	}

	started := time.Now()
	var combined daemon.EnrichCoverageResult
	combined.Segments = len(segments)
	for _, t := range targets {
		modulePath := coverage.ReadModulePath(t.root)
		combined.Symbols += coverage.EnrichGraph(c.graph, segments, modulePath)
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// EnrichCochange mines co-change edges against the daemon's graph.
// Mirrors EnrichChurn — c.mu is held for the duration and targets
// resolve via the multi-indexer. The repo prefix scopes the file-node
// match in multi-repo graphs.
func (c *realController) EnrichCochange(ctx context.Context, p daemon.EnrichCochangeParams) (daemon.EnrichCochangeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targets, err := c.resolveEnrichTargets(p.Path)
	if err != nil {
		return daemon.EnrichCochangeResult{}, err
	}
	_ = ctx // mining is synchronous; no cancellation surface today

	started := time.Now()
	var combined daemon.EnrichCochangeResult
	for _, t := range targets {
		count, err := cochange.EnrichGraph(c.graph, t.root, t.prefix)
		if err != nil {
			return daemon.EnrichCochangeResult{}, fmt.Errorf("enrich %s: %w", t.prefix, err)
		}
		combined.Edges += count
	}
	combined.DurationMS = time.Since(started).Milliseconds()
	return combined, nil
}

// Untrack evicts a repo from the graph and drops it from config.
// PathOrPrefix accepts either an absolute path or a repo prefix.
func (c *realController) Untrack(_ context.Context, p daemon.UntrackParams) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}

	prefix := p.PathOrPrefix
	// Resolve path → prefix if an absolute path was given. Absolutise (to
	// Clean) and compare with fold-aware EqualPaths so a case-variant
	// spelling of a tracked root on a case-insensitive filesystem still
	// resolves to the right prefix.
	if filepath.IsAbs(p.PathOrPrefix) {
		abs, err := filepath.Abs(p.PathOrPrefix)
		if err != nil {
			abs = p.PathOrPrefix
		}
		for pfx, meta := range c.multiIndexer.AllMetadata() {
			if pathkey.EqualPaths(meta.RootPath, abs) {
				prefix = pfx
				break
			}
		}
	}

	// Detach the watcher before evicting from the graph — otherwise a
	// late fsnotify event could race the eviction and try to re-index
	// files whose nodes are already gone.
	if mw := c.watcher(); mw != nil {
		if err := mw.RemoveRepo(prefix); err != nil {
			c.logger.Debug("untrack: detach watcher",
				zap.String("prefix", prefix), zap.Error(err))
		}
	}

	nodesRemoved, edgesRemoved := c.multiIndexer.UntrackRepo(prefix)

	// Persist the config change.
	if c.configManager != nil {
		_ = c.configManager.Global().RemoveRepo(prefix)
		if err := c.configManager.Global().Save(); err != nil {
			c.logger.Warn("untrack: save config failed", zap.Error(err))
		}
	}

	return json.Marshal(map[string]any{
		"status":        "untracked",
		"prefix":        prefix,
		"nodes_removed": nodesRemoved,
		"edges_removed": edgesRemoved,
	})
}

// Reload re-reads the global config, indexes new repos that were added
// via direct config-file edits, and untracks any that were removed.
// Existing, unchanged tracked repos keep their current state.
// ReloadServers re-reads servers.toml and applies the change to the
// running daemon's Router without a restart: an in-place atomic swap
// when a router already exists, a fresh build-and-publish when the first
// remote is added after a router-less startup, or a teardown when the
// last remote is removed.
func (c *realController) ReloadServers(_ context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	scfg, err := daemon.LoadServersConfig("")
	if err != nil {
		return nil, fmt.Errorf("reload servers.toml: %w", err)
	}
	count := 0
	if scfg != nil {
		count = len(scfg.Server)
	}

	wired := false
	switch {
	case count == 0 && c.liveRouter != nil:
		// Last remote removed — tear the router down so local dispatch
		// returns to the direct in-process path.
		c.liveRouter = nil
		if c.publishRouter != nil {
			c.publishRouter(nil)
		}
	case count == 0:
		// No router and no remotes — nothing to wire.
	case c.liveRouter != nil:
		// In-place atomic swap; the stable *Router pointer keeps every
		// dispatch site (and any in-flight call) consistent.
		c.liveRouter.ReloadConfig(scfg, daemon.NewWorkspaceRosterCache(60*time.Second))
		wired = true
	default:
		// First remote added after a router-less startup — build and
		// publish a fresh router into the dispatch path.
		c.liveRouter = daemon.NewRouter(daemon.RouterConfig{
			Servers:      scfg,
			Rosters:      daemon.NewWorkspaceRosterCache(60 * time.Second),
			LocalSlug:    daemon.LocalServerSentinel,
			LocalExecute: c.localExecute,
			Logger:       c.logger,
			Federation:   resolveFederationConfig(),
		})
		if c.publishRouter != nil {
			c.publishRouter(c.liveRouter)
		}
		wired = true
	}
	return json.Marshal(map[string]any{"servers": count, "router_wired": wired})
}

func (c *realController) Reload(ctx context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.configManager == nil {
		return nil, fmt.Errorf("config manager not initialized")
	}
	if err := c.configManager.Reload(); err != nil {
		return nil, fmt.Errorf("reload config: %w", err)
	}

	// Re-read every already-tracked repo's `.gortex.yaml` and push the
	// refreshed excludes into its live indexer. The tracked-repo diff
	// below keeps such repos by root path and never re-tracks them, so
	// without this an edit to a per-repo config could not reach a running
	// daemon at all: reload was global-config-only in practice.
	refreshed := c.multiIndexer.RefreshRepoConfigs()

	var added, removed int

	// Match configured entries to currently-tracked instances by ROOT
	// PATH, not by a recomputed prefix. A worktree tracked as an
	// independent instance registers under a derived `<base>@<workspace>`
	// prefix, so keying the diff on config.ResolvePrefix(entry) (the bare
	// basename) would fail to recognise it as wanted and untrack it on
	// every reload. The root path is the stable identity of a checkout.
	trackedByRoot := make(map[string]string) // absolute RootPath → prefix
	for prefix, meta := range c.multiIndexer.AllMetadata() {
		if meta != nil {
			trackedByRoot[meta.RootPath] = prefix
		}
	}

	wantedPrefixes := make(map[string]bool)
	for _, entry := range c.configManager.Global().Repos {
		abs, err := filepath.Abs(entry.Path)
		if err != nil {
			abs = entry.Path
		}
		if prefix, ok := trackedByRoot[abs]; ok {
			// Already tracked (under whatever prefix it registered) — keep it.
			wantedPrefixes[prefix] = true
			continue
		}
		res, trackErr := c.multiIndexer.TrackRepoCtx(ctx, entry)
		if trackErr != nil {
			c.logger.Warn("reload: track failed",
				zap.String("path", entry.Path), zap.Error(trackErr))
			continue
		}
		added++
		if res != nil && res.RepoPrefix != "" {
			wantedPrefixes[res.RepoPrefix] = true
		}
	}

	for prefix := range c.multiIndexer.AllMetadata() {
		if wantedPrefixes[prefix] {
			continue
		}
		c.multiIndexer.UntrackRepo(prefix)
		removed++
	}

	return json.Marshal(map[string]any{
		"added":     added,
		"removed":   removed,
		"refreshed": refreshed,
	})
}

// searchBackendInfo bundles the daemon.SearchBackendStats payload with
// the separate text/vector byte counts we need to split per-repo.
type searchBackendInfo struct {
	daemon.SearchBackendStats
	vectorBytes uint64
}

// resolveSearchBackend inspects the live search backend and produces
// the stats needed by status rendering: which backend is active, total
// document count, its heap footprint, and (for disk-backed Bleve) the
// on-disk size.
//
// Real-world unwrap order: Swappable → HybridBackend → (text, vector).
// The text side is itself a concrete BM25/Bleve/SymbolSearcherBackend.
// Both layers have to be peeled; if we stop early we fall into the
// default branch and the status reports "unknown" — which was the bug
// users saw. When the store implements graph.SymbolSearcher, the
// indexer wires up a *search.SymbolSearcherBackend instead of building
// an in-process BM25/Bleve index at all (see initialSearchBackend in
// internal/indexer/indexer.go) — that case has to be matched
// explicitly too, or it falls into the same "unknown" default.
func resolveSearchBackend(b search.Backend) searchBackendInfo {
	out := searchBackendInfo{}
	if b == nil {
		return out
	}

	// 1) Unwrap Swappable so we see the currently-active inner.
	inner := b
	if sw, ok := inner.(*search.Swappable); ok {
		inner = sw.Inner()
	}
	// 2) If Hybrid is in play, split its text/vector sizes and keep
	//    drilling into the text side for name/doc-count identification.
	if hyb, ok := inner.(*search.HybridBackend); ok {
		out.vectorBytes = hyb.VectorSizeBytes()
		inner = hyb.TextBackend()
		// TextBackend() itself could be a Swappable in some setups —
		// unlikely today but cheap to guard.
		if sw, ok := inner.(*search.Swappable); ok {
			inner = sw.Inner()
		}
	}

	switch back := inner.(type) {
	case *search.BleveBackend:
		if path := back.DiskPath(); path != "" {
			out.Name = "bleve-disk"
			out.DiskPath = path
			out.DiskBytes = back.DiskBytes()
		} else {
			out.Name = "bleve-memory"
		}
		out.DocCount = back.Count()
		out.DocCountKnown = true
		out.Bytes = back.SizeBytes()
	case *search.BM25Backend:
		out.Name = "bm25"
		out.DocCount = back.Count()
		out.DocCountKnown = true
		out.Bytes = back.SizeBytes()
	case *search.SymbolSearcherBackend:
		// The FTS5 index lives inside the graph store's own file, not a
		// separate in-memory structure — there is no honest byte count
		// to report here. Report the backend truthfully as disk-resident
		// instead of printing a fabricated "heap=0 B".
		//
		// The document count comes from the index itself, never from
		// Count(): that is a since-construction Add/Remove delta, which
		// is not a corpus size and goes negative as soon as an eviction
		// path drops more than the admit predicate ever added. When the
		// index cannot answer, the figure is omitted rather than filled
		// in with the delta.
		out.Name = "sqlite-fts5"
		out.DiskResident = true
		out.DocCount, out.DocCountKnown = back.DocCount()
	default:
		out.Name = "unknown"
		out.DocCount = b.Count()
		out.DocCountKnown = true
		out.Bytes = search.BackendSize(b)
	}
	return out
}

// Status gathers per-repo stats and basic process metrics. Daemon-level
// fields (PID, uptime, socket, session count) are filled in by the
// daemon itself before the response goes out.
func (c *realController) Status(ctx context.Context) (daemon.StatusResponse, error) {
	// Bail before doing any work if the caller is already gone. Status sits
	// on the critical path of `daemon stop`, `gortex call`, and the agent
	// hooks, all of which now bound the round trip — once their budget has
	// expired, finishing the aggregate only burns store reads that nobody
	// will read.
	if err := ctx.Err(); err != nil {
		return daemon.StatusResponse{}, err
	}

	// Compute the per-repo memory estimate BEFORE taking the coarse
	// controller mutex. On the SQLite backend AllRepoMemoryEstimates is a
	// COUNT … GROUP BY scan that turns pathologically slow under
	// enrichment write load; holding c.mu across it stalls every other
	// control request (status / track / reload) queued on the mutex — the
	// daemon-looks-crashed symptom. Snapshot the graph handle under a
	// brief lock, then run the (store-memoised) estimate lock-free.
	c.mu.Lock()
	g := c.graph
	c.mu.Unlock()
	var memEstimates map[string]graph.RepoMemoryEstimate
	var wholeStoreNodes, wholeStoreEdges int
	if g != nil {
		memEstimates = g.AllRepoMemoryEstimates()
		// NodeCount/EdgeCount share that profile: COUNT(*) on the SQLite
		// backend, a walk of every shard in memory. They were computed
		// under c.mu until the same stall showed up on the whole-store
		// path, so they are hoisted out alongside the estimate.
		wholeStoreNodes = g.NodeCount()
		wholeStoreEdges = g.EdgeCount()
	}

	// runtime.ReadMemStats stops the world. Under reindex allocation churn
	// that pause is long enough to matter, and holding c.mu across it makes
	// every queued control request wait out the pause as well.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Everything above is the slow half and c.mu below is the contended half.
	// Re-check between them: a caller whose budget expired during the scans
	// must not go on to queue behind a mutex held by a minutes-long track.
	if err := ctx.Err(); err != nil {
		return daemon.StatusResponse{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		tracked                  []daemon.TrackedRepoStatus
		searchBackendForResponse daemon.SearchBackendStats
		totalNodes               int
	)
	if c.multiIndexer != nil {
		// memEstimates (per-repo node/edge counts + byte estimates) was
		// computed above, before the controller mutex was taken — see the
		// note at the top of Status. The SQLite store memoises it so a
		// burst of status polls collapses onto one COUNT … GROUP BY scan;
		// the in-memory store serves maintained shard counters directly.

		// Diagnostic: when AllMetadata has tracked repos but
		// AllRepoMemoryEstimates returns nothing (or a much smaller
		// set), some path has cleared the per-repo counters without
		// clearing the underlying nodes. The meta fallback below keeps
		// the table usable in the meantime. A workspace with exactly
		// one Unprefixed repo legitimately runs one bucket short —
		// its nodes carry repo_prefix="" and AllRepoMemoryEstimates'
		// GROUP BY excludes that key by design (handled separately
		// below) — so that expected gap must not trip this warning.
		// A workspace with a single tracked repo owns the entire store:
		// every node and edge belongs to it, whether or not those nodes
		// carry a repo prefix. g.NodeCount()/g.EdgeCount() is therefore the
		// exact per-repo count, and reporting it keeps `daemon status` in
		// agreement with `gortex query stats` (which reports the same
		// whole-store totals — the inconsistency users report in #261/#270).
		//
		// This covers both ways a lone repo's nodes land under
		// repo_prefix="", neither of which produces a usable per-prefix
		// bucket (the in-memory shard counters skip empty-prefix nodes —
		// shard.repoNodeAdd is a deliberate no-op — and the SQLite GROUP BY
		// excludes repo_prefix="" rows via `WHERE repo_prefix <> ''`):
		//   1. Indexed unprefixed (RepoMetadata.Unprefixed) while it was the
		//      workspace's sole tracked repo — the willBeMultiRepo gate in
		//      TrackRepoCtx/ReconcileRepoCtx.
		//   2. Desynced to a prefixed metadata: a second config entry (e.g. a
		//      macOS path-case duplicate) flips willBeMultiRepo true at the
		//      next warm restart, so the metadata is stamped prefixed
		//      (Unprefixed=false) but the existing repo_prefix="" nodes are
		//      never restamped (migrateLoneUnprefixedRepoCtx's guard needs
		//      len(repos)==1, which fails mid-warmup-loop) — leaving an empty
		//      per-prefix bucket.
		// Both used to fall back to the frozen RepoMetadata.NodeCount (stale,
		// often ~1) and render the near-empty row of #261/#270. Byte
		// estimates stay at their zero value here — advisory display detail,
		// not the reported bug.
		allMeta := c.multiIndexer.AllMetadata()
		soleRepo := len(allMeta) == 1
		// wholeStoreNodes / wholeStoreEdges were computed above, before the
		// controller mutex was taken — see the note at the top of Status.

		// Diagnostic: when AllMetadata has tracked repos but
		// AllRepoMemoryEstimates returns nothing (or a much smaller
		// set), some path has cleared the per-repo counters without
		// clearing the underlying nodes. The meta fallback below keeps
		// the table usable in the meantime.
		if c.logger != nil {
			tracked := len(allMeta)
			counted := len(memEstimates)
			// A sole tracked repo (or one indexed unprefixed) legitimately
			// contributes no per-prefix bucket — its nodes carry
			// repo_prefix="" — so the expected shortfall must not trip this
			// warning; the whole-store attribution below covers it.
			expectedGap := 0
			if soleRepo {
				expectedGap = 1
			} else {
				for _, meta := range allMeta {
					if meta != nil && meta.Unprefixed {
						expectedGap = 1
						break
					}
				}
			}
			if tracked > 0 && counted < tracked-expectedGap {
				c.logger.Warn("daemon: per-repo counters below tracked-repo count — graph mutation cleared per-repo index?",
					zap.Int("tracked_repos", tracked),
					zap.Int("counter_buckets", counted),
					zap.Int("graph_total_nodes", wholeStoreNodes))
			}
		}

		// One repo in a multi-repo workspace can still hold all its nodes
		// under repo_prefix="" while its metadata says prefixed — the same
		// desync as the sole-repo case but with a second repo present (it was
		// indexed solo, then another repo joined while the daemon was down, so
		// warmup stamped it prefixed without restamping its nodes; the
		// migrateLoneUnprefixedRepoCtx guard needs len(repos)==1, which is
		// false mid-warmup-loop). Its per-prefix bucket is then empty and it
		// would under-count. Attribute the unaccounted ("") node pool to it —
		// but only when exactly one tracked repo lacks a live bucket, because
		// the "" pool is a single undifferentiated pool and more than one
		// empty-bucket repo is ambiguous. The pool also carries a small number
		// of synthetic cross-repo/global nodes (<1% of a real graph), so the
		// attribution is approximate, not exact.
		var orphanOwner string
		orphanOwnerCount := 0
		if !soleRepo {
			for prefix, meta := range allMeta {
				if meta == nil {
					continue
				}
				if est, ok := memEstimates[prefix]; !ok || est.NodeCount == 0 {
					orphanOwner = prefix
					orphanOwnerCount++
				}
			}
		}
		orphanNodes, orphanEdges := 0, 0
		if orphanOwnerCount == 1 && g != nil {
			orphanNodes, orphanEdges = wholeStoreNodes, wholeStoreEdges
			for _, est := range memEstimates {
				orphanNodes -= est.NodeCount
				orphanEdges -= est.EdgeCount
			}
			if orphanNodes < 0 {
				orphanNodes = 0
			}
			if orphanEdges < 0 {
				orphanEdges = 0
			}
		}

		// Search and vector backends are process-wide (one shared index
		// across all repos), so we compute the global size once and
		// split it proportionally to each repo's node share. Not exact,
		// but it's the best attribution we can make without indexing
		// per-repo which would double storage for the sake of a status
		// breakdown.
		backendStats := resolveSearchBackend(c.multiIndexer.Search())
		// totalNodes drives the SearchBytes share split below. A sole repo
		// owns the whole store; otherwise sum the per-prefix buckets and, if
		// every counter is empty (the post-warmup-wipe case described above),
		// fall back to per-repo meta so the share denominator stays nonzero
		// and the search budget gets attributed instead of falling on the floor.
		if soleRepo {
			totalNodes = wholeStoreNodes
		} else {
			for _, est := range memEstimates {
				totalNodes += est.NodeCount
			}
			totalNodes += orphanNodes
			if totalNodes == 0 {
				for _, meta := range allMeta {
					if meta != nil {
						totalNodes += meta.NodeCount
					}
				}
			}
		}

		for prefix, meta := range allMeta {
			nodes := meta.NodeCount
			edges := meta.EdgeCount
			var mem daemon.MemoryBreakdown
			switch {
			case soleRepo || meta.Unprefixed:
				// The whole store is this repo's graph — see the note
				// above. This keeps `daemon status` in agreement with
				// `gortex query stats` for a single-repo workspace, and
				// corrects the near-empty row when a lone repo's nodes
				// carry repo_prefix="" (indexed unprefixed, or desynced to
				// a prefixed metadata whose per-prefix bucket is empty).
				// Byte estimates stay zero — advisory detail, not the bug.
				nodes = wholeStoreNodes
				edges = wholeStoreEdges
			default:
				est, ok := memEstimates[prefix]
				switch {
				case orphanOwnerCount == 1 && prefix == orphanOwner:
					// This repo's nodes are the unaccounted "" pool — see the
					// note above. Approximate (includes a little synthetic
					// cross-repo overhead), but far closer than the frozen
					// near-empty RepoMetadata fallback.
					nodes = orphanNodes
					edges = orphanEdges
				case ok:
					nodes = est.NodeCount
					edges = est.EdgeCount
					mem.NodesBytes = est.NodeBytes
					mem.EdgesBytes = est.EdgeBytes
				}
			}
			if totalNodes > 0 && nodes > 0 {
				share := float64(nodes) / float64(totalNodes)
				mem.SearchBytes = uint64(float64(backendStats.Bytes) * share)
				mem.VectorsBytes = uint64(float64(backendStats.vectorBytes) * share)
				mem.DiskBytes = uint64(float64(backendStats.DiskBytes) * share)
			}
			mem.TotalBytes = mem.NodesBytes + mem.EdgesBytes + mem.SearchBytes + mem.VectorsBytes

			// Pull the workspace/project slugs straight off the
			// per-repo Indexer — that's the source of truth that
			// stamps every node emitted by this repo. Falls back to
			// the prefix on legacy setups where no .gortex.yaml
			// declares them (the resolveWorkspaceID default).
			var ws, wsProj string
			if idx := c.multiIndexer.GetIndexer(prefix); idx != nil {
				ws = idx.WorkspaceID()
				wsProj = idx.ProjectID()
			}
			if ws == "" {
				ws = prefix
			}
			if wsProj == "" {
				wsProj = prefix
			}

			tracked = append(tracked, daemon.TrackedRepoStatus{
				Prefix:           prefix,
				Path:             meta.RootPath,
				Workspace:        ws,
				WorkspaceProject: wsProj,
				Files:            meta.FileCount,
				Nodes:            nodes,
				Edges:            edges,
				LastIndex:        meta.LastIndexTime.Unix(),
				Memory:           mem,
			})
		}
		searchBackendForResponse = backendStats.SearchBackendStats
	}

	// Aggregate per-workspace stats so the renderer can emit a
	// "workspaces" block. Hidden when every repo defaults to its own
	// slug (the legacy single-workspace-per-repo case where the
	// summary just duplicates the table).
	wsAgg := make(map[string]*daemon.WorkspaceSummary)
	wsKeys := make([]string, 0)
	for _, r := range tracked {
		s, ok := wsAgg[r.Workspace]
		if !ok {
			s = &daemon.WorkspaceSummary{Slug: r.Workspace}
			wsAgg[r.Workspace] = s
			wsKeys = append(wsKeys, r.Workspace)
		}
		s.Repos = append(s.Repos, r.Prefix)
		seenProj := false
		for _, p := range s.Projects {
			if p == r.WorkspaceProject {
				seenProj = true
				break
			}
		}
		if !seenProj {
			s.Projects = append(s.Projects, r.WorkspaceProject)
		}
		s.Files += r.Files
		s.Nodes += r.Nodes
		s.Edges += r.Edges
	}
	// Always populate the per-workspace rollup — even when every
	// workspace is a default singleton. Hiding it on legacy setups
	// makes the boundary feature invisible, which is the opposite
	// of what users want when they're trying to migrate. Renderer-
	// side compaction (single-line hint vs full table) keeps the
	// output tidy when there's nothing meaningful to summarise.
	sort.Strings(wsKeys)
	workspaces := make([]daemon.WorkspaceSummary, 0, len(wsKeys))
	for _, k := range wsKeys {
		workspaces = append(workspaces, *wsAgg[k])
	}

	// mem was sampled before the mutex was taken — see the note at the top
	// of Status.

	resp := daemon.StatusResponse{
		TrackedRepos:  tracked,
		MemoryBytes:   mem.Alloc,
		SearchBackend: searchBackendForResponse,
		Runtime: daemon.RuntimeStats{
			Alloc:        mem.Alloc,
			Sys:          mem.Sys,
			HeapInuse:    mem.HeapInuse,
			HeapIdle:     mem.HeapIdle,
			HeapReleased: mem.HeapReleased,
			StackInuse:   mem.StackInuse,
			NumGC:        mem.NumGC,
			NumGoroutine: runtime.NumGoroutine(),
		},
		PProfAddr:          daemonPProfAddr(),
		Ready:              c.ready.Load(),
		WarmupSeconds:      c.warmupSeconds.Load(),
		EnrichmentComplete: c.enriched.Load(),
		EnrichSeconds:      c.enrichSeconds.Load(),
		Workspaces:         workspaces,
		ConfiguredServers:  c.collectConfiguredServers(),
		LocalServerSlug:    c.localServerSlug(),
		LSPRouter:          c.collectLSPRouterStatus(),
		Enrichment:         c.collectEnrichmentProgress(),
	}
	if c.toolSurface != nil {
		resp.ToolPreset, resp.ToolPresetMode, resp.LearnedTools = c.toolSurface()
	}
	return resp, nil
	// MCPSessions is populated by the daemon Server (it owns the
	// SessionRegistry — the controller doesn't have a back-pointer).
	// See internal/daemon/server.go around the ControlStatus handler.
}

// collectConfiguredServers reads `~/.gortex/servers.toml` (best
// effort — a missing or malformed file just returns nil) and
// projects it onto the status response. Auth tokens are NOT
// included; the HasAuth flag is enough for the human-facing
// "yes/no" decision.
func (c *realController) collectConfiguredServers() []daemon.ConfiguredServerStatus {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil || cfg == nil || len(cfg.Server) == 0 {
		return nil
	}
	local := c.localServerSlug()
	out := make([]daemon.ConfiguredServerStatus, 0, len(cfg.Server))
	for _, s := range cfg.Server {
		out = append(out, daemon.ConfiguredServerStatus{
			Slug:       s.Slug,
			URL:        s.URL,
			Default:    s.Default,
			Local:      s.Slug == local,
			Workspaces: s.Workspaces,
			HasAuth:    s.AuthToken != "" || s.AuthTokenEnv != "",
		})
	}
	return out
}

// localServerSlug returns the reserved sentinel identifying the
// daemon's own in-process graph. It is intentionally NOT derived from
// DefaultServer().Slug: a roster row is always a remote now, so no
// roster entry is ever "local" (the status Local flag is false for
// every row), and a remote marked default=true is still proxied.
func (c *realController) localServerSlug() string {
	return daemon.LocalServerSentinel
}

// collectLSPRouterStatus reflects the daemon's LSP router (when
// wired) into a status payload. Returns nil when no router is wired
// (semantic enrichment disabled in `.gortex.yaml`).
func (c *realController) collectLSPRouterStatus() *daemon.LSPRouterStatus {
	if c.indexer == nil {
		return nil
	}
	semMgr := c.indexer.SemanticManager()
	if semMgr == nil {
		return nil
	}
	router, ok := semMgr.LSPRouter().(*lsp.Router)
	if !ok || router == nil {
		return nil
	}
	out := &daemon.LSPRouterStatus{
		DefaultWorkspace: router.DefaultWorkspace(),
	}
	for _, name := range router.EnabledSpecNames() {
		out.EnabledSpecs = append(out.EnabledSpecs, daemon.LSPSpecStatus{
			Name:      name,
			Available: router.SpecAvailable(name),
			Languages: strings.Join(router.SpecLanguages(name), ","),
		})
	}
	for _, s := range router.Stats() {
		out.ActiveProviders = append(out.ActiveProviders, daemon.LSPActiveProvider{
			Spec:      s.Spec,
			Workspace: s.Workspace,
			LastUsed:  s.LastUsed.Format(time.RFC3339),
		})
	}
	return out
}

// collectEnrichmentProgress reflects the semantic manager's per-(repo,
// provider) enrichment statuses into the compact summary the daemon
// status line needs. Returns nil when no semantic manager is wired, or
// it has never recorded a pass — the "enrichment in progress" state
// with nothing behind it is exactly the bug this closes.
func (c *realController) collectEnrichmentProgress() *daemon.EnrichmentProgress {
	if c.indexer == nil {
		return nil
	}
	semMgr := c.indexer.SemanticManager()
	if semMgr == nil {
		return nil
	}
	return enrichmentProgressFromStatuses(semMgr.EnrichmentStatuses())
}

// enrichmentProgressFromStatuses is the pure reduction behind
// collectEnrichmentProgress, split out so it can be unit tested
// against literal semantic.EnrichmentStatus rows without wiring a
// live indexer + semantic manager. A repo counts as done once every
// provider recorded for it has reached a terminal state; the first
// running row (in the manager's stable repo/provider order) becomes
// Current.
func enrichmentProgressFromStatuses(statuses []semantic.EnrichmentStatus) *daemon.EnrichmentProgress {
	if len(statuses) == 0 {
		return nil
	}

	out := &daemon.EnrichmentProgress{}
	repoDone := make(map[string]bool)
	repoSeen := make(map[string]bool)
	for _, st := range statuses {
		repoSeen[st.Repo] = true
		if _, ok := repoDone[st.Repo]; !ok {
			repoDone[st.Repo] = true // assume done until a non-terminal provider says otherwise
		}
		switch st.State {
		case semantic.EnrichStateRunning:
			out.Running = true
			repoDone[st.Repo] = false
			if out.Current == nil {
				cur := &daemon.EnrichmentCurrent{
					Repo:            st.Repo,
					Provider:        st.Provider,
					DeadlineSeconds: st.DeadlineSeconds,
				}
				if !st.StartedAt.IsZero() {
					cur.ElapsedSeconds = time.Since(st.StartedAt).Seconds()
				}
				out.Current = cur
			}
		}
	}
	out.ReposTotal = len(repoSeen)
	for _, done := range repoDone {
		if done {
			out.ReposDone++
		}
	}
	return out
}

// searchSymbolsFetchFactor bounds how many name-index candidates
// SearchSymbols pulls before the File/Import and repo filters run. The name
// index cannot express those filters, so pushing the caller's limit down
// verbatim can return a page saturated with rows the filter then drops.
// Over-fetching absorbs that without giving up the bound; a repo-scoped
// probe over-fetches harder because one repo can be a thin slice of a
// multi-repo workspace.
const (
	searchSymbolsFetchFactor     = 8
	searchSymbolsRepoFetchFactor = 32
	searchSymbolsMaxFetch        = 2000
)

// SearchSymbols runs a substring match over node names and returns the
// matching symbols. It's the cheap probe path for clients (notably the
// Grep-redirect hook) that need a fast yes/no without setting up a full
// MCP session. File and Import nodes are excluded — the hook only cares
// about real symbol matches.
//
// Candidates come from the sharded name index — one shard read-locked at a
// time, with the fetch bound pushed down — not from AllNodes. AllNodes
// read-locks every shard at once and materialises a slice of the entire
// graph, so on a multi-repo daemon this probe queued behind the indexer's
// shard writers and blew past the hook's 200ms budget. The hook then logged
// probed_miss / timed_out and never once produced a hit.
func (c *realController) SearchSymbols(_ context.Context, p daemon.SearchSymbolsParams) (daemon.SearchSymbolsResult, error) {
	c.mu.Lock()
	g := c.graph
	c.mu.Unlock()

	if g == nil || p.Query == "" {
		return daemon.SearchSymbolsResult{}, nil
	}

	limit := p.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	hits := make([]daemon.SymbolHit, 0, limit)

	// Fast path. Everything that reaches this probe is a grep pattern the
	// hook already classified as identifier-shaped, and the overwhelmingly
	// common answer is a symbol whose short name matches exactly. byName is
	// a hash bucket per shard, so this is a handful of map lookups rather
	// than a walk over every name in the graph — the difference between
	// microseconds and blowing the hook's probe budget on a large graph.
	for _, n := range g.FindNodesByName(p.Query) {
		if !probeSymbolCandidate(n, p.Repo) {
			continue
		}
		hits = append(hits, probeSymbolHit(n))
		if len(hits) >= limit {
			break
		}
	}
	if len(hits) > 0 {
		return daemon.SearchSymbolsResult{Hits: hits}, nil
	}

	// Substring fallback, for patterns that name part of a symbol. The name
	// index cannot express the File/Import or repo filters, so a saturated
	// page can come back with every row filtered away — a query matching
	// many file paths crowds real symbols off it. Widen and retry rather
	// than under-report, but stay bounded: a few rounds capped at
	// searchSymbolsMaxFetch, still orders of magnitude below a full scan.
	//
	// The explicit Contains re-check preserves the pre-index semantics
	// exactly (full Unicode case folding, where SQLite's LIKE folds ASCII
	// only).
	needle := strings.ToLower(p.Query)
	fetch := limit * searchSymbolsFetchFactor
	if p.Repo != "" {
		fetch = limit * searchSymbolsRepoFetchFactor
	}
	for {
		if fetch > searchSymbolsMaxFetch {
			fetch = searchSymbolsMaxFetch
		}
		candidates := g.FindNodesByNameContaining(p.Query, fetch)
		hits = hits[:0]
		for _, n := range candidates {
			if !probeSymbolCandidate(n, p.Repo) {
				continue
			}
			if !strings.Contains(strings.ToLower(n.Name), needle) {
				continue
			}
			hits = append(hits, probeSymbolHit(n))
			if len(hits) >= limit {
				break
			}
		}
		// Enough hits, the index is exhausted, or the bound is reached.
		if len(hits) >= limit || len(candidates) < fetch || fetch >= searchSymbolsMaxFetch {
			break
		}
		fetch *= 4
	}
	return daemon.SearchSymbolsResult{Hits: hits}, nil
}

// probeSymbolCandidate reports whether n can answer a symbol probe. File and
// Import nodes are named for paths rather than code, so they must never make
// a grep pattern look like a real symbol; a repo-scoped probe additionally
// keeps only that repo's nodes.
func probeSymbolCandidate(n *graph.Node, repo string) bool {
	if n == nil {
		return false
	}
	if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
		return false
	}
	if repo != "" && n.RepoPrefix != repo {
		return false
	}
	return true
}

func probeSymbolHit(n *graph.Node) daemon.SymbolHit {
	return daemon.SymbolHit{
		Name:     n.Name,
		Kind:     string(n.Kind),
		FilePath: n.FilePath,
		Line:     n.StartLine,
	}
}

// AttachWatcher is called by warmup to hand over the MultiWatcher once
// it has been initialized. Until this is called, realController.Track
// skips the per-repo watcher attach — a newly-tracked repo gets its
// watcher when the warmup-constructed MultiWatcher iterates
// mi.AllMetadata() at startup.
func (c *realController) AttachWatcher(mw *indexer.MultiWatcher) {
	c.multiWatcher.Store(mw)
}

// watcher reads the attached MultiWatcher without touching the coarse mutex.
// The teardown path needs it, and taking mu there would put the stop request
// back behind the minutes-long track / reload / enrichment it is trying to end.
func (c *realController) watcher() *indexer.MultiWatcher {
	return c.multiWatcher.Load()
}

// StopWatcher halts filesystem watching. Called first during teardown so no
// late event races the snapshot write — we want the snapshot to reflect a
// quiescent graph, not one being mutated by an in-flight re-index.
//
// Deliberately lock-free: this is the first statement of the daemon's shutdown
// hook, and it used to take the coarse controller mutex just to read the
// watcher pointer, which meant `gortex daemon stop` queued behind whatever
// long operation was holding it.
func (c *realController) StopWatcher() {
	if mw := c.watcher(); mw != nil {
		_ = mw.Stop()
	}
}

// MarkReady flips the ready flag once references are resolved and the graph
// is queryable, recording how long the parse + resolve stage took. Safe to
// call concurrently with Status (atomic loads on the read side).
func (c *realController) MarkReady(d time.Duration) {
	c.warmupSeconds.Store(int64(d.Seconds()))
	c.ready.Store(true)
}

// IsReady reports whether the graph is resolved and queryable. The socket
// accepts connections before this; callers waiting to issue queries should
// wait for IsReady.
func (c *realController) IsReady() bool {
	return c.ready.Load()
}

// MarkEnriched flips the enrichment-complete flag once semantic enrichment
// and the graph-wide derivation passes finish in the background, recording
// the full warmup duration. It also sets ready, so the degenerate path where
// MarkReady was skipped still reports a usable daemon.
func (c *realController) MarkEnriched(d time.Duration) {
	c.enrichSeconds.Store(int64(d.Seconds()))
	c.enriched.Store(true)
	c.ready.Store(true)
}

// IsEnriched reports whether the background enrichment + derivation passes
// have finished. Background timers (the periodic snapshotter) gate on this
// rather than IsReady so they don't fight the enrichment pipeline for shard
// locks and GC budget.
func (c *realController) IsEnriched() bool {
	return c.enriched.Load()
}

// Shutdown gives the caller (the daemon main) a chance to flush any
// per-instance stores. The actual socket teardown is the Server's job.
//
// Takes shutdownMu, never mu: a stop request must not queue behind the
// long-running work it is trying to end.
func (c *realController) Shutdown(_ context.Context) error {
	c.shutdownMu.Lock()
	hook := c.onShutdown
	c.shutdownMu.Unlock()
	if hook != nil {
		return hook()
	}
	return nil
}

// setShutdownHook installs the flush-and-close hook Shutdown invokes.
func (c *realController) setShutdownHook(hook func() error) {
	c.shutdownMu.Lock()
	c.onShutdown = hook
	c.shutdownMu.Unlock()
}
