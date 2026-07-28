package indexer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/entrypoints"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/trigram"
	"github.com/zzet/gortex/internal/semantic"
)

// RepoMetadata holds per-repo indexing state.
type RepoMetadata struct {
	RepoPrefix    string
	RootPath      string
	Identity      *RepoIdentity
	LastIndexTime time.Time
	FileCount     int
	NodeCount     int
	EdgeCount     int
	ParseErrors   []IndexError
	FileMtimes    map[string]int64
	// IsWorktree records whether RootPath was a linked git worktree
	// (as opposed to a main checkout) at the time the repo was
	// tracked. Captured here because once the worktree directory is
	// removed from disk it can no longer be classified — the janitor
	// needs this remembered flag to know a vanished root was a
	// worktree and may be garbage-collected.
	IsWorktree bool
	// Unprefixed records that this repo was indexed in single-repo
	// mode: its nodes carry RepoPrefix="" and raw relative file paths.
	// The empty-prefix resolution fallback (RepoRoot, ResolveFilePath)
	// honours only a lone repo that actually minted unprefixed nodes —
	// without the provenance check, stale unprefixed nodes surviving a
	// track/untrack transition would resolve against whatever repo
	// happens to be the lone one. Eviction also branches on it:
	// unprefixed nodes are invisible to the byRepo bucket EvictRepo
	// walks, so UntrackRepo must evict them file-by-file.
	Unprefixed bool
}

// MultiIndexer orchestrates indexing across multiple repositories.
type MultiIndexer struct {
	graph     graph.Store
	registry  *parser.Registry
	search    search.Backend
	embedder  embedding.Provider
	repos     map[string]*RepoMetadata // repoPrefix → metadata
	indexers  map[string]*Indexer      // repoPrefix → per-repo indexer
	configMgr *config.ConfigManager
	logger    *zap.Logger
	mu        sync.RWMutex

	// shadowAdmission is process-wide, not per MultiIndexer. Every cold repo
	// competes for the same weighted in-memory budget; repos that do not fit
	// immediately stream to SQLite instead of waiting or overcommitting RAM.
	shadowAdmission *shadowAdmissionBudget

	// reconcileMu serialises ReconcileContractEdges end-to-end. The pass
	// evicts the prior EdgeMatches / topic / bridge generation and mints
	// a fresh one across many independent graph-store writes — it is NOT
	// atomic. Several goroutines drive it concurrently (the periodic
	// janitor's ReconcileAll, the file-watcher's IncrementalReindex,
	// MCP-triggered track / untrack / index), and mi.mu is only taken in
	// fine-grained spots inside, not across the whole pass. Without this
	// lock two overlapping reconciles can interleave evict and mint and
	// persist a stale generation (a bridge wiped by the other run's
	// EvictFile after it was minted). A dedicated outer mutex keeps the
	// pass self-consistent without widening mi.mu's scope.
	reconcileMu sync.Mutex

	// stitchProber / proxyBudget wire the cross-daemon proxy-edge feature:
	// when set by the daemon entry point (flag on), every CrossRepoResolver
	// this MultiIndexer builds mints proxy edges to remote-owned symbols
	// on positive evidence. A nil prober keeps the resolvers on read-only
	// federation (the default).
	stitchProber resolver.RemoteDeclarationProber
	proxyBudget  int

	// deferGlobalPasses, when set, propagates SetDeferGlobalPasses(true)
	// to every per-repo Indexer constructed by this MultiIndexer. Batch
	// callers (warmup, ReconcileAll) flip it on around their loop and
	// invoke RunGlobalGraphPasses once at the end so the O(global) walks
	// (InferImplements / InferOverrides / markTestSymbolsAndEmitEdges)
	// don't run R times against an R-repo graph.
	deferGlobalPasses bool

	// deferResolve, when set, propagates SetDeferResolve(true) to every
	// per-repo Indexer constructed by this MultiIndexer. Used by the
	// parallel warmup path: per-repo ResolveAll / contract extract /
	// semantic enrich mutate the shared graph, so running them in
	// parallel across repos races. With this flag the parallel loop
	// just parses; RunDeferredPassesAll runs the per-repo passes
	// serially after the loop. Independent of deferGlobalPasses — that
	// flag covers a separate (cheaper) set of O(global) walks.
	deferResolve bool

	// batchChangedPrefixes scopes the per-repo clone-detection and
	// clone-index Rebuild passes in RunGlobalGraphPasses to the repos that
	// actually re-indexed in the current batch. nil — the default, and what
	// every one-off EndBatch caller leaves it as — means "run the clone
	// passes for every tracked repo", the prior whole-workspace behaviour.
	// The daemon warmup arms it (ArmBatchScope) before EndBatch so an
	// N-repo warm restart where only one repo changed stops paying ~N
	// full-graph clone walks for repos whose clone edges are already on
	// disk. Clone edges are per-repo (no cross-repo pair is ever formed),
	// so an unchanged repo's persisted edges stay valid; its in-memory
	// incremental clone index is reseeded lazily on its first later edit.
	// Consumed and cleared by RunGlobalGraphPasses. Guarded by mi.mu.
	batchChangedPrefixes map[string]struct{}
	// batchCensusEligible is the daemon's one-shot full-coverage attestation
	// for the armed batch scope — see ArmBatchCensusEligible.
	batchCensusEligible bool

	// resolverLSPHelper is the resolve-time LSP helper propagated
	// onto every per-repo Indexer and onto the global post-pass
	// resolver in RunDeferredPassesAll. nil means no LSP hot-path
	// (heuristic-only resolution, the pre-N5 behaviour). The
	// daemon installs the helper via SetResolverLSPHelper after
	// constructing the LSP router; a multi-repo composite helper
	// dispatches per-file by repo prefix.
	resolverLSPHelper resolver.LSPHelper

	// onRepoTracked, when non-nil, is invoked after a fresh
	// TrackRepoCtx call has resolved the repo's prefix and
	// absolute path but before indexing starts. The daemon uses
	// this hook to register a per-repo resolver-time LSP helper
	// against the LSPHelper registry so dynamically-tracked repos
	// participate in the N5 hot path without daemon restart.
	onRepoTracked func(prefix, absPath string)

	// skipVectorBuild, when set, propagates SetSkipVectorBuild(true) to
	// every per-repo Indexer this MultiIndexer constructs, so their
	// buildSearchIndex passes populate only the text index and never
	// embed. The daemon flips it on for the warmup loop when a snapshot
	// already carries the workspace vector index — re-embedding 30k+
	// symbols only to overwrite them with the cached index is the
	// dominant restart cost. After warmup it restores the cached index
	// once via ImportVectorIndex and clears the flag.
	skipVectorBuild bool

	// embedChunkOpts is the AST sub-chunking configuration propagated
	// to every per-repo Indexer this MultiIndexer constructs. The zero
	// value leaves the chunker on its built-in defaults.
	embedChunkOpts embedding.ChunkOptions

	// embedMaxSymbols overrides the vector-index size cap propagated to
	// every per-repo Indexer. Zero keeps the built-in default.
	embedMaxSymbols int

	// embedAPIConcurrency bounds parallel embedding requests against an
	// API-backed embedder, propagated to every per-repo Indexer. Zero
	// keeps the built-in default.
	embedAPIConcurrency int

	// semanticMgr is the semantic enrichment manager propagated to
	// every per-repo Indexer. When nil (the default), per-repo
	// deferred passes skip semantic enrichment — this is the root
	// cause of "enrich:0" in daemon mode. Set by the daemon via
	// SetSemanticManager before IndexAll / TrackRepo.
	semanticMgr *semantic.Manager
}

// SetEmbedder installs the embedding provider every per-repo indexer
// should use. Must be called before IndexAll / TrackRepo for vectors
// to land in the graph — without this the fresh Indexer created per
// repo has embedder=nil and buildSearchIndex skips the vector pass.
// Safe to call zero or one times; subsequent calls silently replace.
func (mi *MultiIndexer) SetEmbedder(e embedding.Provider) {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.embedder = e
}

// npmAliasResolver builds a resolver.NpmAliasResolver covering every
// tracked repo's on-disk root. Installed on the global post-pass
// resolver and the cross-repo resolver so a JS/TS import declared
// through an npm alias resolves to a locally-vendored real package
// anywhere in the workspace. Returns nil when no repo has a usable
// root — callers treat that as "no alias rewriting".
func (mi *MultiIndexer) npmAliasResolver() resolver.NpmAliasResolver {
	roots := map[string]string{}
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots[prefix] = meta.RootPath
		}
	}
	idx := newNpmAliasIndex(roots)
	if idx == nil {
		return nil
	}
	return idx.Resolve
}

// workspaceMembershipResolver builds a resolver.WorkspaceMembership
// covering every tracked repo's on-disk root. Installed on the global
// post-pass resolver and the cross-repo resolver so a same-named import
// collision is broken in favour of the importer's own package-manager
// workspace member. Returns nil when no repo is a workspace root —
// callers treat that as "no workspace signal".
func (mi *MultiIndexer) workspaceMembershipResolver() resolver.WorkspaceMembership {
	roots := map[string]string{}
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots[prefix] = meta.RootPath
		}
	}
	idx := newWorkspaceMembershipIndex(roots)
	if idx == nil {
		return nil
	}
	return idx.Lookup
}

// newPerRepoIndexer constructs a per-repo Indexer with the standard
// MultiIndexer wiring (shared search backend, embedder if configured,
// deferred-global-passes flag propagated). Centralised so the flag
// plumbing stays in one place.
func (mi *MultiIndexer) newPerRepoIndexer(cfg config.IndexConfig) *Indexer {
	idx := New(mi.graph, mi.registry, cfg, mi.logger)
	idx.shadowAdmission = mi.shadowAdmission
	idx.search = mi.search
	if mi.embedder != nil {
		idx.SetEmbedder(mi.embedder)
	}
	idx.SetDeferGlobalPasses(mi.deferGlobalPasses)
	idx.SetDeferResolve(mi.deferResolve)
	idx.SetSkipVectorBuild(mi.skipVectorBuild)
	idx.SetEmbeddingChunkOptions(mi.embedChunkOpts)
	idx.SetEmbeddingMaxSymbols(mi.embedMaxSymbols)
	idx.SetEmbeddingAPIConcurrency(mi.embedAPIConcurrency)
	if mi.resolverLSPHelper != nil {
		idx.SetResolverLSPHelper(mi.resolverLSPHelper)
	}
	if mi.semanticMgr != nil {
		idx.SetSemanticManager(mi.semanticMgr)
	}
	return idx
}

// SetEmbeddingChunkOptions installs the AST sub-chunking configuration
// every per-repo Indexer this MultiIndexer constructs should use, and
// re-applies it to every per-repo Indexer already built. Call before
// IndexAll / TrackRepo so the warmup indexers pick it up.
func (mi *MultiIndexer) SetEmbeddingChunkOptions(opts embedding.ChunkOptions) {
	mi.mu.Lock()
	mi.embedChunkOpts = opts
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetEmbeddingChunkOptions(opts)
	}
}

// SetEmbeddingMaxSymbols installs the vector-index size cap every
// per-repo Indexer this MultiIndexer constructs should use, and
// re-applies it to every per-repo Indexer already built. Zero keeps
// the built-in default.
func (mi *MultiIndexer) SetEmbeddingMaxSymbols(n int) {
	mi.mu.Lock()
	mi.embedMaxSymbols = n
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetEmbeddingMaxSymbols(n)
	}
}

// SetEmbeddingAPIConcurrency installs the parallel-embedding-request
// cap every per-repo Indexer this MultiIndexer constructs should use,
// and re-applies it to every per-repo Indexer already built. Zero
// keeps the built-in default; the cap only affects API-backed
// embedders.
func (mi *MultiIndexer) SetEmbeddingAPIConcurrency(n int) {
	mi.mu.Lock()
	mi.embedAPIConcurrency = n
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetEmbeddingAPIConcurrency(n)
	}
}

// SetSemanticManager installs the semantic enrichment manager every
// per-repo Indexer this MultiIndexer constructs should use, and
// re-applies it to every per-repo Indexer already built. Without
// this call, daemon-mode enrichment produces zero results because
// the per-repo Indexers never receive the semantic manager.
func (mi *MultiIndexer) SetSemanticManager(m *semantic.Manager) {
	mi.mu.Lock()
	mi.semanticMgr = m
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetSemanticManager(m)
	}
}

// SetSkipVectorBuild controls whether per-repo Indexers constructed
// from now on skip the embedding pass in buildSearchIndex (text index
// only). The daemon enables it for the warmup loop when a snapshot
// already carries the workspace vector index, then disables it and
// restores the cached index once warmup finishes. It also re-applies
// the flag to every per-repo Indexer already constructed so a flag
// flip mid-lifecycle takes effect everywhere.
func (mi *MultiIndexer) SetSkipVectorBuild(skip bool) {
	mi.mu.Lock()
	mi.skipVectorBuild = skip
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetSkipVectorBuild(skip)
	}
}

// SetResolverLSPHelper installs the resolve-time LSP helper used by
// every per-repo Indexer this MultiIndexer constructs from now on,
// and by the global post-pass resolver in RunDeferredPassesAll. Pass
// nil to detach. Safe to call zero or one times; subsequent calls
// silently replace and propagate to every existing per-repo indexer.
func (mi *MultiIndexer) SetResolverLSPHelper(h resolver.LSPHelper) {
	mi.mu.Lock()
	mi.resolverLSPHelper = h
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetResolverLSPHelper(h)
	}
}

// SetOnRepoTracked installs a callback fired once per TrackRepoCtx
// after the repo's prefix + absolute path have been resolved but
// before indexing starts. The daemon registers per-repo resolver-
// time LSP helpers from this hook so runtime-added repos
// participate in the N5 hot path. Pass nil to detach.
func (mi *MultiIndexer) SetOnRepoTracked(fn func(prefix, absPath string)) {
	mi.mu.Lock()
	mi.onRepoTracked = fn
	mi.mu.Unlock()
}

// BeginBatch enables deferred-global-passes mode for every per-repo
// Indexer that this MultiIndexer constructs after the call AND for
// every Indexer already in mi.indexers (so ReconcileAll's per-repo
// IncrementalReindex calls also skip the O(global) walks). Pair with
// EndBatch.
func (mi *MultiIndexer) BeginBatch() {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.deferGlobalPasses = true
	for _, idx := range mi.indexers {
		idx.SetDeferGlobalPasses(true)
	}
}

// BeginParallelBatch is BeginBatch plus parallel-safety: it also
// propagates SetDeferResolve(true) to every per-repo Indexer
// constructed during the batch. Use this when running the per-repo
// indexing loop across goroutines (warmup) — the parallel parsers
// must not race each other inside ResolveAll / contract extract /
// semantic enrich, which all mutate the shared graph. Pair with
// EndBatch; call RunDeferredPassesAll between the parallel parse and
// EndBatch to run the deferred per-repo passes serially.
func (mi *MultiIndexer) BeginParallelBatch() {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.deferGlobalPasses = true
	mi.deferResolve = true
	for _, idx := range mi.indexers {
		idx.SetDeferGlobalPasses(true)
	}
}

// RunDeferredPassesAll drains the deferred per-repo passes (semantic
// enrich / contract extract+commit) serially across the indexers the
// parallel parse populated. Pairs with BeginParallelBatch: the parallel
// loop parses with deferResolve on; this serial loop runs the passes that
// would otherwise race on the shared graph. The references-completeness
// resolve runs ahead of this in RunPreEnrichResolve (so the daemon can mark
// itself queryable before enrichment); the per-repo resolver pass is
// suppressed here because resolver.ResolveAll walks the entire shared graph
// — paying it R times is O(R · E). One master resolver.New(graph).ResolveAll
// runs at the end to lift the placeholder edges enrichment + contracts added.
//
// Returns the number of repos whose deferred semantic enrichment was
// actually dispatched (pendingEnrich set, or forced via
// GORTEX_WARMUP_FORCE_ENRICH) rather than skipped as unchanged. Sampled
// before runDeferredEnrichParallel runs, since a successful non-partial
// pass clears the flag it reads.
// SeedPendingEnrichAll re-arms the deferred-enrichment gate for every tracked
// repo whose persisted enrichment is known-incomplete at its current clean HEAD
// (see Indexer.MaybeSeedPendingEnrich). The daemon warmup calls it after the
// parallel parse and before RunDeferredPassesAll so a repo left partial or
// abandoned by a prior process resumes even when no file changed this run.
// Returns the number of repos that will enrich (already pending plus newly
// seeded) — the caller uses a non-zero count to run the deferred passes on a
// warm restart that changed nothing on disk.
func (mi *MultiIndexer) SeedPendingEnrichAll() int {
	mi.mu.RLock()
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.RUnlock()
	pending := 0
	for _, idx := range live {
		if idx.MaybeSeedPendingEnrich() {
			pending++
		}
	}
	return pending
}

func (mi *MultiIndexer) RunDeferredPassesAll(ctx context.Context) int {
	return mi.BeginDeferredPasses(ctx, nil).FinishTail()
}

// DeferredPassesRun is one in-flight execution of the deferred pass pipeline,
// split so the warmup can OVERLAP the enrichment pool with the pre-enrich
// resolve phase: the pool's expensive compute (go/packages loads, tree-sitter
// parses) touches no graph state, and its graph applies serialize with the
// resolver through the shared ResolveMutex plus the resolver's own
// mutation-revision interleave handling. The tail (contracts + catch-up
// resolve) stays strictly after both the pool and the caller's resolve phase.
type DeferredPassesRun struct {
	mi              *MultiIndexer
	workIndexers    []*Indexer
	enrichScheduled int
	catchupNeeded   bool
	catchupKnown    bool
	catchupScope    map[string]struct{}
	indexerCount    int
	receiptStore    graph.MutationReceiptStore
	receiptToken    graph.MutationReceiptToken
	poolDone        chan struct{}
	restoreGCTune   func()
	// unresolvedCounter/-Base bound the deferred window's unresolved-target
	// writes. When the counter is unchanged at FinishTail, the incomplete-
	// receipt whole-graph fallback resolve is provably a no-op and is
	// skipped. The base is re-snapshotted at apply-gate open (before any
	// parked apply can run) so the master resolver's own in-window writes —
	// e.g. guard reverts, already handled by that same pass — don't defeat
	// the skip.
	unresolvedCounter graph.UnresolvedInsertionCounter
	unresolvedBase    uint64
}

// SnapshotUnresolvedBase re-bases the deferred window's unresolved-write
// counter. The daemon calls it inside the apply-gate opener, strictly before
// the gate channel closes, so every parked enrichment apply happens-after
// this read.
func (r *DeferredPassesRun) SnapshotUnresolvedBase() {
	if r != nil && r.unresolvedCounter != nil {
		r.unresolvedBase = r.unresolvedCounter.UnresolvedEdgeInsertions()
	}
}

// BeginDeferredPasses selects the repos with deferred work, opens the
// mutation-receipt window, materialises go.mod dependencies, and launches the
// enrichment pool on its own goroutine. The caller must call FinishTail (which
// joins the pool) exactly once. applyGate, when non-nil, parks every
// provider's graph-apply phase until the caller closes it — the caller MUST
// close it (typically when its resolve phase completes) or FinishTail
// deadlocks.
//
// When the pool is overlapped with a resolve phase, the resolver's writes land
// inside the receipt window and void it, so the tail's catch-up resolve takes
// the whole-graph fallback — the fail-closed path it already had — instead of
// the exact-files fast path. That trade is the overlap's cost and is bounded
// by terminal-stamping (the fallback scans only the surviving pending set).
func (mi *MultiIndexer) BeginDeferredPasses(_ context.Context, applyGate <-chan struct{}) *DeferredPassesRun {
	mi.mu.RLock()
	indexers := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		indexers = append(indexers, idx)
	}
	mi.mu.RUnlock()
	sort.Slice(indexers, func(i, j int) bool {
		return indexers[i].repoPrefix < indexers[j].repoPrefix
	})
	forced := os.Getenv("GORTEX_WARMUP_FORCE_ENRICH") == "1"
	run := &DeferredPassesRun{
		mi:           mi,
		catchupKnown: true,
		catchupScope: make(map[string]struct{}),
		indexerCount: len(indexers),
		poolDone:     make(chan struct{}),
		// The deferred phase is the second half of the same allocation burst
		// IndexCtx tunes for — go/packages closures, tree-sitter parses, and
		// the catch-up resolve — but it runs OUTSIDE any IndexCtx window, so
		// on a daemon with a default standing limit it was paced against the
		// lean steady-state ceiling. Hold one ref-counted tuning window
		// across the whole span (pool + contracts + catch-up resolve);
		// FinishTail restores the standing knobs exactly.
		restoreGCTune: applyIndexGCTuning(mi.logger),
	}
	for _, idx := range indexers {
		enrich := idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders() &&
			(idx.pendingEnrich.Load() || forced)
		if enrich {
			run.enrichScheduled++
		}
		// Only repositories with actual deferred work enter the language-stats,
		// enrichment, contract, and retained-state pipeline. This keeps a warm or
		// partial restart proportional to its changed repositories.
		if !enrich && idx.pendingContractReg == nil {
			continue
		}
		run.workIndexers = append(run.workIndexers, idx)
		run.catchupNeeded = true
		if idx.repoPrefix == "" {
			run.catchupKnown = false
			continue
		}
		run.catchupScope[idx.repoPrefix] = struct{}{}
	}
	for _, idx := range run.workIndexers {
		idx.SetSkipResolveInDeferred(true)
		idx.deferredApplyGate = applyGate
	}

	// Keep the receipt window exact: only go.mod materialisation, semantic
	// enrichment, and contract commits are observed (plus, under overlap, the
	// resolver — which voids it, see above). The capability is optional;
	// unsupported stores retain the conservative scheduled-work fallback.
	// Under overlap (applyGate != nil) the resolver runs inside the receipt
	// window and ALWAYS voids it — the receipt is discarded as incomplete on
	// every such run, yet its per-write identity bookkeeping
	// (mutationNodeIdentitiesTx: one identity SELECT per batched ID) taxes
	// every enrichment apply in the window. Don't open a doomed receipt;
	// the unresolved-insertion counter below carries the skip/fallback
	// decision instead.
	if applyGate == nil {
		run.receiptStore, _ = mi.graph.(graph.MutationReceiptStore)
		if run.receiptStore != nil {
			run.receiptToken = run.receiptStore.BeginMutationReceipt()
		}
	}
	run.unresolvedCounter, _ = mi.graph.(graph.UnresolvedInsertionCounter)
	run.SnapshotUnresolvedBase()

	// Per-repo deferred work starts with serial go.mod materialisation.
	// Semantic enrichment then runs in bounded parallel lanes on its own
	// goroutine so the caller may overlap it with the resolve phase.
	for _, idx := range run.workIndexers {
		idx.runDeferredGoMod()
	}
	go func() {
		defer close(run.poolDone)
		mi.runDeferredEnrichPool(run.workIndexers)
	}()
	return run
}

// Wait blocks until every enrichment lane has drained.
func (r *DeferredPassesRun) Wait() { <-r.poolDone }

// FinishTail joins the enrichment pool, runs the contract passes, closes the
// receipt window, and performs the deferred-mutation catch-up resolve. It
// returns the number of repos that had enrichment scheduled.
func (r *DeferredPassesRun) FinishTail() int {
	r.Wait()
	// Contract passes run serially only after every enrichment lane has
	// drained: the "no contract mutation overlaps enrichment" invariant
	// holds globally instead of per batch, and each repo's retained
	// compiler state is the compact binding projection, which stays
	// cheap to hold until its pass releases it here.
	for _, idx := range r.workIndexers {
		idx.runDeferredContractsAndReleaseSemanticState()
	}
	var mutationReceipt *graph.MutationReceipt
	if r.receiptStore != nil {
		receipt := r.receiptStore.EndMutationReceipt(r.receiptToken)
		mutationReceipt = &receipt
	}
	for _, idx := range r.workIndexers {
		idx.SetSkipResolveInDeferred(false)
		idx.deferredApplyGate = nil
	}
	scope := normalizeDeferredCatchupScope(r.catchupScope, r.catchupKnown, r.indexerCount)
	noNewUnresolved := r.unresolvedCounter != nil &&
		r.unresolvedCounter.UnresolvedEdgeInsertions() == r.unresolvedBase
	r.mi.resolveDeferredMutations(mutationReceipt, r.catchupNeeded, scope, noNewUnresolved)
	if r.restoreGCTune != nil {
		r.restoreGCTune()
	}
	return r.enrichScheduled
}

// normalizeDeferredCatchupScope preserves the resolver's full-pass semantics
// when deferred work covered every registered repo. A non-empty scope disables
// terminal-unresolved stamping inside ResolveAll; treating an all-repo map as
// scoped would therefore leave permanently external edges hot on every future
// warmup even though the pass had complete workspace evidence. Unknown/single-
// repo prefixes are likewise conservative full passes. Only a strict subset is
// safe to retain as a scoped catch-up.
func normalizeDeferredCatchupScope(scope map[string]struct{}, known bool, repoCount int) map[string]struct{} {
	if !known || (repoCount > 0 && len(scope) >= repoCount) {
		return nil
	}
	return scope
}

type deferredResolveMode string

const (
	deferredResolveSkipped  deferredResolveMode = "skipped"
	deferredResolveExact    deferredResolveMode = "exact_files"
	deferredResolveFallback deferredResolveMode = "fallback_all"
)

// resolveDeferredMutations chooses the narrowest safe catch-up resolution.
// A complete receipt is authoritative even when the old scheduled-work
// heuristic predicted mutations; an incomplete receipt always fails closed to
// a whole-graph pass. nil means the store does not support receipts yet.
func (mi *MultiIndexer) resolveDeferredMutations(receipt *graph.MutationReceipt, fallbackNeeded bool, fallbackScope map[string]struct{}, noNewUnresolved bool) deferredResolveMode {
	// The counter's verdict holds regardless of receipt availability: zero
	// unresolved-target writes in the window means the catch-up resolve —
	// receipt-scoped, heuristic-scoped, or whole-graph — has provably
	// nothing to bind.
	if noNewUnresolved {
		mi.logger.Info("DEFERRED-TIMING no unresolved-target writes in deferred window; skipping catch-up resolve")
		return deferredResolveSkipped
	}
	if receipt != nil {
		if !receipt.Complete {
			if noNewUnresolved {
				// The receipt was voided (overlap does this by design), but
				// the store counted zero unresolved-target edge writes since
				// the apply gate opened — nothing exists for a whole-graph
				// fallback to resolve that the master pass hasn't already
				// seen. Measured cost of the blind fallback: 68.8s to
				// resolve 0 of 282,669 pending.
				mi.logger.Info("DEFERRED-TIMING mutation receipt incomplete but no unresolved-target writes in window; skipping fallback resolve",
					zap.String("incomplete_reason", receipt.IncompleteReason))
				return deferredResolveSkipped
			}
			mi.logger.Info("DEFERRED-TIMING mutation receipt incomplete; resolving all",
				zap.String("incomplete_reason", receipt.IncompleteReason))
			mi.runMasterResolve(nil, false)
			return deferredResolveFallback
		}
		if !receipt.ResolutionRelevant {
			mi.logger.Info("DEFERRED-TIMING mutation receipt has no resolution delta",
				zap.Int("changed_files", len(receipt.ChangedFiles)),
				zap.Int("target_ids", len(receipt.TargetIDs)))
			return deferredResolveSkipped
		}
		files := receipt.ResolutionFiles()
		if len(files) == 0 {
			// Completeness implementations should already reject this shape, but
			// keep the consumer fail-closed if a future backend gets it wrong.
			mi.logger.Warn("DEFERRED-TIMING resolution delta lacks exact files; resolving all")
			mi.runMasterResolve(nil, false)
			return deferredResolveFallback
		}
		mi.runMasterResolveFiles(files, false)
		return deferredResolveExact
	}
	if !fallbackNeeded {
		return deferredResolveSkipped
	}
	mi.runMasterResolve(fallbackScope, false)
	return deferredResolveFallback
}

// runMasterResolve runs one same-repo resolver over the whole shared graph,
// lifting every placeholder edge to its canonical target. Split out so the
// pre-enrichment resolve stage (RunPreEnrichResolve) and the post-enrichment
// catch-up (RunDeferredPassesAll) share one implementation.
// scope, when non-empty, restricts the pass to the edges that could resolve
// into one of the named changed repos (see resolver.SetScope). It is honoured
// only when scoped global passes are enabled; a nil / empty scope or a
// disabled switch runs the whole-graph resolve, exactly the prior behaviour.
func (mi *MultiIndexer) newMasterResolver(useLSP bool) *resolver.Resolver {
	if mi.graph == nil {
		return nil
	}
	master := resolver.New(mi.graph)
	master.SetLogger(mi.logger)
	// The master resolve is the only pass with whole-graph evidence, so it is
	// the one allowed to durably flag terminally-unresolved edges (permanently
	// external / stdlib / definition-less) that a later scoped warm resolve can
	// skip. A scoped/file pass leaves the flag alone; only a full ResolveAll
	// stamps and self-heals terminal state.
	master.SetStampTerminal(true)
	// The pre-enrichment queryability pass uses resolver-time LSP precision.
	// The post-enrichment catch-up disables it because semantic providers just
	// ran and replaying synchronous definition lookups dominates cold startup.
	if useLSP && mi.resolverLSPHelper != nil {
		master.SetLSPHelper(mi.resolverLSPHelper)
	}
	master.SetNpmAliasResolver(mi.npmAliasResolver())
	master.SetPathAliasResolver(mi.pathAliasResolver())
	master.SetWorkspaceMembership(mi.workspaceMembershipResolver())
	return master
}

func (mi *MultiIndexer) runMasterResolve(scope map[string]struct{}, useLSP bool) {
	mi.runMasterResolveHooked(scope, useLSP, nil)
}

// runMasterResolveHooked is runMasterResolve with an optional compute-done
// hook threaded into the resolver (see Resolver.OnComputeDone).
func (mi *MultiIndexer) runMasterResolveHooked(scope map[string]struct{}, useLSP bool, onComputeDone func()) {
	master := mi.newMasterResolver(useLSP)
	if master == nil {
		if onComputeDone != nil {
			onComputeDone()
		}
		return
	}
	master.OnComputeDone = onComputeDone
	scoped := len(scope) > 0 && mi.scopedGlobalPassesEnabled()
	if scoped {
		master.SetScope(scope)
	}
	mt := time.Now()
	stats := master.ResolveAll()
	mi.logger.Info("DEFERRED-TIMING master.ResolveAll",
		zap.Duration("elapsed", time.Since(mt)),
		zap.Bool("scoped", scoped),
		zap.Bool("lsp_enabled", useLSP && mi.resolverLSPHelper != nil),
		zap.Int("scope_repos", len(scope)),
		// Scanned vs admitted, NOT residual-after-resolution: PendingAfter
		// counts the rows that survived scope/terminal filtering into the
		// pass (see ResolveStats). A 666k-conversion pass with these two
		// nearly equal is a pass that admitted nearly everything it scanned.
		zap.Int("pending_scanned", stats.PendingBefore),
		zap.Int("pending_admitted", stats.PendingAfter))
}

func (mi *MultiIndexer) runMasterResolveFiles(files []string, useLSP bool) {
	master := mi.newMasterResolver(useLSP)
	if master == nil {
		return
	}
	mt := time.Now()
	stats := master.ResolveFilesAndIncoming(files)
	mi.logger.Info("DEFERRED-TIMING master.ResolveFilesAndIncoming",
		zap.Duration("elapsed", time.Since(mt)),
		zap.Bool("lsp_enabled", useLSP && mi.resolverLSPHelper != nil),
		zap.Int("files", len(files)),
		zap.Int("pending_scanned", stats.PendingBefore),
		zap.Int("pending_admitted", stats.PendingAfter))
}

// RunPreEnrichResolve runs the resolution stage that makes references queryable
// ahead of the slow semantic-enrichment pass. It materialises go.mod dep
// contract nodes (so the resolver's import bridge can re-target Go imports of
// declared modules), runs the same-repo master resolver to lift every
// parse-time placeholder edge to its canonical target, then runs the cross-repo
// resolver so references that span repo boundaries resolve too. Contract-bridge
// reconciliation is intentionally left to RunGlobalResolve, which runs after the
// contract pass has committed its nodes.
//
// The daemon warmup calls this between the parallel parse and the enrichment
// phase. scope, when non-empty, restricts the same-repo master resolve to the
// changed repos (see runMasterResolve / resolver.SetScope). The daemon warmup
// passes the set of repos that re-indexed so a warm restart of one repo out of
// many skips a whole-graph same-repo resolve; a nil / empty scope keeps the
// whole-graph behaviour. The cross-repo resolve below stays whole-graph
// regardless — it is the only pass that binds an unchanged repo's inbound
// reference into a symbol a changed repo just added.
//
// onComputeDone (may be nil) carves a readiness boundary out of the stage: it
// fires inside the master resolver, right after the parallel compute loop
// commits and before the serial refinement tail — the earliest point at which
// same-repo references are queryable; the daemon marks itself ready there
// instead of waiting out tail + cross-repo (minutes on a large workspace,
// with only confidence refinement and cross-repo binding left). Enrichment
// applies get no earlier admission point: they stay parked for the WHOLE
// stage, cross-repo included, because an apply holds the shared ResolveMutex
// in multi-minute stretches and admitting applies between the master and
// cross-repo passes starved cross-repo to a standstill (measured: 1,049s for
// a pass that runs in ~38s uncontended on the same workspace).
func (mi *MultiIndexer) RunPreEnrichResolve(ctx context.Context, scope map[string]struct{}, onComputeDone func()) {
	mi.mu.RLock()
	indexers := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		indexers = append(indexers, idx)
	}
	mi.mu.RUnlock()
	sort.Slice(indexers, func(i, j int) bool {
		return indexers[i].repoPrefix < indexers[j].repoPrefix
	})
	for _, idx := range indexers {
		idx.runDeferredGoMod()
	}
	mi.runMasterResolveHooked(scope, true, onComputeDone)
	// Cross-repo references resolve here so a multi-repo workspace is fully
	// resolved, not just within each repo. Whole-graph so inbound references
	// from unchanged repos into the changed repos bind too.
	mi.runCrossRepoResolve(false)
}

// runDeferredEnrichParallel runs each indexer's semantic enrichment in a
// bounded worker pool. Concurrency is capped so at most a few LSP servers
// background-index at once (the memory-sensitive part). The manager pins each
// repo's LSP provider in-use for the duration of its pass, so the router's
// LRU evictor never closes a provider another repo is still enriching against.
func (mi *MultiIndexer) runDeferredEnrichParallel(indexers []*Indexer) {
	mi.runDeferredEnrichPool(indexers)
}

// runDeferredEnrichPool drains per-repo semantic enrichment through a
// bounded worker pool with no inter-repo barriers. The old fixed batches
// made every batch wait for its slowest member and gave each heavy-Go repo
// an exclusive batch, which serialized that repo's ENTIRE provider chain —
// a whale's LSP sweep rode the critical path behind its own type-check.
// Exclusivity for the genuinely heavyweight resource is enforced where the
// resource lives: the go/types provider's heavy gate admits one full
// go/packages program at a time (GORTEX_GOTYPES_CONCURRENCY overrides), and
// that provider runs under the manager's outer window, so a queued heavy
// repo waits on the gate without burning a lane or a per-repo deadline.
// Everything else — LSP sweeps, tree-sitter type providers, the other
// languages of the same repo — flows through the pool lanes.
func (mi *MultiIndexer) runDeferredEnrichPool(indexers []*Indexer) {
	if len(indexers) == 0 {
		return
	}
	// Per-repo language sets computed once from a single graph-stats scan,
	// shared by the spec-grouped ordering and the pool-raise sizing so the
	// Manager's per-repo enrichment scan is not duplicated here.
	langSets, goNodeCounts := mi.batchLanguageSets(indexers)
	// Deterministic, spec-grouped order: repos needing the same language
	// servers run contiguously so the router's capped provider pool cycles
	// through far fewer distinct (spec, workspace) keys — a warmed server
	// stays alive across the runs that need it instead of being evicted and
	// respawned per repo.
	indexers = orderIndexersBySpecGroup(indexers, langSets)

	conc := enrichConcurrency(len(indexers))

	// Temporarily raise the router's live-provider cap for the pool so the
	// concurrent passes don't evict each other's warmed servers, restoring
	// it (and logging the churn observed) when the pool drains.
	restore := mi.scopeRouterPoolForBatch(langSets, conc)
	defer restore()

	queue := deferredEnrichQueue(indexers, langSets, goNodeCounts)

	// Heartbeat: the pool runs for many minutes and individual passes log
	// only at their own start/complete, so a watcher cannot tell which repos
	// the lanes are on or how deep the queue still is during a quiet stretch.
	// A 30s pulse makes every silent window attributable at a glance.
	type laneWork struct {
		repo  string
		since time.Time
	}
	var laneMu sync.Mutex
	lanes := make(map[int]laneWork, conc)
	var completed atomic.Int32
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ticker.C:
				laneMu.Lock()
				active := make([]string, 0, len(lanes))
				for _, work := range lanes {
					active = append(active, fmt.Sprintf("%s(%s)", work.repo, time.Since(work.since).Round(time.Second)))
				}
				laneMu.Unlock()
				sort.Strings(active)
				mi.logger.Info("deferred enrichment pool heartbeat",
					zap.Strings("active", active),
					zap.Int32("repos_done", completed.Load()),
					zap.Int("repos_total", len(queue)))
			}
		}
	}()
	runOne := func(lane int, idx *Indexer) {
		laneMu.Lock()
		lanes[lane] = laneWork{repo: idx.repoPrefix, since: time.Now()}
		laneMu.Unlock()
		idx.runDeferredEnrich()
		laneMu.Lock()
		delete(lanes, lane)
		laneMu.Unlock()
		completed.Add(1)
	}

	if conc <= 1 {
		for _, idx := range queue {
			runOne(0, idx)
		}
		return
	}

	jobs := make(chan *Indexer)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			for idx := range jobs {
				runOne(lane, idx)
			}
		}(w)
	}
	for _, idx := range queue {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()
}

// goHeavyEnrichNodeThreshold splits Go repositories into two scheduling
// classes. At or above it, the repo's go/packages program is a heavyweight,
// minutes-scale run serialized by the go/types provider's heavy gate — such
// repos are queued first so the gate chain forms the pool's backbone. Below
// it the type-check is seconds-scale (a grammar repo's Go bindings, a tool
// repo's helper package) and the repo schedules like any other.
const goHeavyEnrichNodeThreshold = 8192

// deferredEnrichQueue orders the pool's work: heavy-Go repositories first, so
// the serialized go/packages chain — the schedule's backbone — starts on the
// first lane immediately and light repos fill the remaining lanes around it.
// Within each class the spec-grouped order is preserved for warm-server
// reuse. Pure so the schedule shape is directly testable.
func deferredEnrichQueue(indexers []*Indexer, langSets map[*Indexer][]string, goNodeCounts map[*Indexer]int) []*Indexer {
	heavy := make([]*Indexer, 0, len(indexers))
	light := make([]*Indexer, 0, len(indexers))
	for _, idx := range indexers {
		hasGo := false
		for _, language := range langSets[idx] {
			if language == "go" {
				hasGo = true
				break
			}
		}
		if hasGo && goNodeCounts[idx] >= goHeavyEnrichNodeThreshold {
			heavy = append(heavy, idx)
			continue
		}
		light = append(light, idx)
	}
	return append(heavy, light...)
}

// maxBatchProviders caps the temporary live-provider pool raise during
// batch enrichment. Even a batch touching many distinct language servers
// at high concurrency is held to this ceiling so warmup cannot spawn an
// unbounded number of LSP subprocesses at once.
const maxBatchProviders = 12

// batchLanguageSets returns each indexer's sorted set of present languages
// from one node-only repository projection. SQLite never joins or counts edges
// here; the sets drive both spec-grouped ordering and batch pool-raise sizing.
func (mi *MultiIndexer) batchLanguageSets(indexers []*Indexer) (map[*Indexer][]string, map[*Indexer]int) {
	out := make(map[*Indexer][]string, len(indexers))
	goNodes := make(map[*Indexer]int, len(indexers))
	prefixes := make([]string, 0, len(indexers))
	for _, idx := range indexers {
		prefixes = append(prefixes, idx.repoPrefix)
	}
	var counts map[string]map[string]int
	if mi.graph != nil {
		counts = graph.ReadRepoLanguageCounts(mi.graph, prefixes)
	}
	for _, idx := range indexers {
		langs := make([]string, 0, len(counts[idx.repoPrefix]))
		for language := range counts[idx.repoPrefix] {
			langs = append(langs, language)
		}
		sort.Strings(langs)
		out[idx] = langs
		goNodes[idx] = counts[idx.repoPrefix]["go"]
	}
	return out, goNodes
}

// orderIndexersBySpecGroup returns a stable, deterministic ordering of
// indexers grouped by their language set (so repos needing the same LSP
// servers run contiguously), breaking ties by repo prefix. It does not
// mutate the input slice.
func orderIndexersBySpecGroup(indexers []*Indexer, langSets map[*Indexer][]string) []*Indexer {
	out := make([]*Indexer, len(indexers))
	copy(out, indexers)
	sort.SliceStable(out, func(i, j int) bool {
		ki := strings.Join(langSets[out[i]], ",")
		kj := strings.Join(langSets[out[j]], ",")
		if ki != kj {
			return ki < kj
		}
		return out[i].repoPrefix < out[j].repoPrefix
	})
	return out
}

// distinctBatchSpecs counts the enabled, available LSP specs whose
// languages intersect any language present in the batch — i.e. how many
// distinct language servers the batch will actually drive.
func distinctBatchSpecs(langSets map[*Indexer][]string, router semantic.LSPRouter) int {
	langs := make(map[string]bool)
	for _, ls := range langSets {
		for _, l := range ls {
			langs[l] = true
		}
	}
	if len(langs) == 0 {
		return 0
	}
	seen := make(map[string]bool)
	for _, name := range router.EnabledSpecNames() {
		if !router.SpecAvailable(name) {
			continue
		}
		for _, l := range router.SpecLanguages(name) {
			if langs[l] {
				seen[name] = true
				break
			}
		}
	}
	return len(seen)
}

// scopeRouterPoolForBatch raises the LSP router's live-provider cap to
// enrichConcurrency × distinct-provider-specs (ceiling maxBatchProviders)
// for the duration of a batch enrichment pass, so the concurrent passes
// don't evict each other's warmed servers. It returns a restore closure
// that puts the cap back and logs the eviction churn observed during the
// batch. Safe no-op when no router is installed.
func (mi *MultiIndexer) scopeRouterPoolForBatch(langSets map[*Indexer][]string, conc int) func() {
	if mi.semanticMgr == nil {
		return func() {}
	}
	router := mi.semanticMgr.LSPRouter()
	if router == nil {
		return func() {}
	}
	before := router.EvictionCount()
	old := router.MaxAlive()
	raised := false
	if specs := distinctBatchSpecs(langSets, router); specs > 0 {
		needed := conc * specs
		if needed > maxBatchProviders {
			needed = maxBatchProviders
		}
		if needed > old {
			router.SetMaxAlive(needed)
			raised = true
			mi.logger.Info("LSP router pool temporarily raised for batch enrichment",
				zap.Int("from", old),
				zap.Int("to", needed),
				zap.Int("distinct_specs", specs),
				zap.Int("concurrency", conc),
			)
		}
	}
	return func() {
		if raised {
			router.SetMaxAlive(old)
		}
		mi.logger.Info("batch enrichment LSP provider churn",
			zap.Uint64("evictions", router.EvictionCount()-before),
			zap.Bool("pool_raised", raised),
		)
	}
}

// enrichConcurrency caps how many repos enrich at once during batch warmup.
// Half the CPUs, clamped to [1,4] and to the repo count — a few concurrent
// LSP servers background-index in parallel without a memory blow-up.
func enrichConcurrency(repos int) int {
	c := runtime.NumCPU() / 2
	if c > 4 {
		c = 4
	}
	if c < 1 {
		c = 1
	}
	if c > repos {
		c = repos
	}
	return c
}

// EndBatch turns off deferred-global-passes mode and runs the graph-
// wide derivation passes (InferImplements, InferOverrides,
// markTestSymbolsAndEmitEdges) once against the shared graph. Restores
// the per-Indexer flag too so a subsequent one-off TrackRepoCtx call
// runs the passes inline as expected.
// ArmBatchScope records the prefixes of the repos that re-indexed in the
// batch about to be ended, so the next RunGlobalGraphPasses runs the
// per-repo clone-detection + clone-index Rebuild passes only for those
// repos instead of for every tracked repo. An empty set, or scoped global
// passes being disabled, leaves the scope nil (run all). Only the daemon
// warmup arms it; every other EndBatch caller keeps the whole-workspace
// behaviour.
func (mi *MultiIndexer) ArmBatchScope(changedPrefixes map[string]struct{}) {
	if mi == nil || len(changedPrefixes) == 0 || !mi.scopedGlobalPassesEnabled() {
		return
	}
	mi.mu.Lock()
	mi.batchChangedPrefixes = changedPrefixes
	mi.mu.Unlock()
}

// ArmBatchCensusEligible records the daemon's attestation that the armed
// batch scope covers EVERY tracked repository (a cold index or a full warm
// reconciliation). The framework-synthesis pass then builds its admission
// census from the raw whole store while execution stays scoped. One-shot:
// consumed by the next RunGlobalGraphPasses and reset, so it can never leak
// into a later incremental batch. The attestation is the caller's — it is
// deliberately not inferred here from scope size.
func (mi *MultiIndexer) ArmBatchCensusEligible() {
	if mi == nil {
		return
	}
	mi.mu.Lock()
	mi.batchCensusEligible = true
	mi.mu.Unlock()
}

func (mi *MultiIndexer) takeBatchCensusEligible() bool {
	if mi == nil {
		return false
	}
	mi.mu.Lock()
	eligible := mi.batchCensusEligible
	mi.batchCensusEligible = false
	mi.mu.Unlock()
	return eligible
}

// takeBatchScope returns the armed clone-pass scope and clears it, so the
// scope governs exactly one RunGlobalGraphPasses run. A nil result means
// "no scope — run the clone passes for every repo".
func (mi *MultiIndexer) takeBatchScope() map[string]struct{} {
	mi.mu.Lock()
	scope := mi.batchChangedPrefixes
	mi.batchChangedPrefixes = nil
	mi.mu.Unlock()
	return scope
}

// scopedGlobalPassesEnabled reports whether per-repo global passes may be
// scoped to the changed-repo set. GORTEX_INDEX_SCOPED_GLOBAL_PASSES
// overrides the config key; default ON, mirroring Indexer.scopedGlobalPassesEnabled.
func (mi *MultiIndexer) scopedGlobalPassesEnabled() bool {
	if v := os.Getenv("GORTEX_INDEX_SCOPED_GLOBAL_PASSES"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	for _, idx := range mi.indexers {
		return idx.config.ScopedGlobalPassesEnabledOrDefault()
	}
	return true
}

func (mi *MultiIndexer) EndBatch() {
	mi.mu.Lock()
	mi.deferGlobalPasses = false
	mi.deferResolve = false
	for _, idx := range mi.indexers {
		idx.SetDeferGlobalPasses(false)
	}
	mi.mu.Unlock()
	mi.RunGlobalGraphPasses(context.Background())
}

// ResetBatch clears deferred-batch mode WITHOUT running the graph-wide
// derivation passes. It is the warm-restart fast-path counterpart to
// EndBatch: when the warmup reconcile loop observed zero changed files
// across every repo, the persistent backend already holds every resolved
// and derived edge from the prior run, so RunGlobalGraphPasses (plus the
// RunDeferredPassesAll / RunGlobalResolve the caller also skips) would
// only recompute what's already on disk — the work that turns a warm
// restart into a 30s–500s stall. The per-Indexer SetDeferGlobalPasses
// flag is still restored so a later watch-triggered TrackRepoCtx /
// IncrementalReindex runs its passes inline as normal.
func (mi *MultiIndexer) ResetBatch() {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.deferGlobalPasses = false
	mi.deferResolve = false
	for _, idx := range mi.indexers {
		idx.SetDeferGlobalPasses(false)
	}
}

// RunGlobalGraphPasses runs the graph-wide derivation passes once
// against the shared graph: InferImplements (structural interface
// satisfaction), InferOverrides (method-level overrides on
// extends/implements/composes parents), and markTestSymbolsAndEmitEdges
// (test→subject EdgeTests). Idempotent — graph.AddEdge dedupes by
// edgeKey and the resolver passes skip already-present parents.
func (mi *MultiIndexer) RunGlobalGraphPasses(ctx context.Context) {
	if mi.graph == nil {
		return
	}
	reporter := progress.FromContext(ctx)
	r := resolver.New(mi.graph)
	// Unconditional per-sub-pass timing. These global passes run serially and
	// were previously logged only when a pass emitted edges, so a slow but
	// low-yield pass left no breadcrumb — the source of the multi-minute
	// "silent" span after resolve on a cold index.
	globalStart := time.Now()

	// Acquire the changed-repo scope once for the whole run and derive the two
	// shapes the passes below consume. A nil scope means whole-graph — the
	// fresh-index / one-off behaviour every pass keeps as its fallback — so an
	// unscoped run is byte-identical to before. A non-nil scope (armed by the
	// daemon warmup / hourly janitor via ArmBatchScope) narrows each pass to the
	// repos that re-indexed this batch: an unchanged repo's derived edges are
	// already on disk and were never dropped, so re-deriving them is skipped.
	//   - changedPrefixes: the changed-repo prefix set, for the edge-driven passes
	//     (capability, test edges, framework synthesis, external calls), each of
	//     which owns an edge iff its FROM node is in a changed repo.
	//   - scopedTypeIfaceIDs: the changed repos' type/interface node IDs, for the
	//     implements/overrides inference. The scoped inference keeps a pair when
	//     EITHER endpoint is in this set, so a cross-repo override whose child is
	//     in a changed repo and whose parent is in an unchanged one (or vice
	//     versa) is still re-derived; structural implements never crosses repos
	//     (its same-repo gate), so scoping both its sides here is complete.
	scope := mi.takeBatchScope()
	var changedPrefixes map[string]bool
	var scopedTypeIfaceIDs map[string]bool
	var scopedRepoPrefixes []string
	if scope != nil {
		changedPrefixes = make(map[string]bool, len(scope))
		scopedRepoPrefixes = make([]string, 0, len(scope))
		for prefix := range scope {
			changedPrefixes[prefix] = true
			if prefix != "" {
				scopedRepoPrefixes = append(scopedRepoPrefixes, prefix)
			}
		}
		sort.Strings(scopedRepoPrefixes)
		scopedTypeIfaceIDs = make(map[string]bool)
		for _, id := range graph.ReadRepoNodeIDsByKinds(
			mi.graph,
			scopedRepoPrefixes,
			[]graph.NodeKind{graph.KindType, graph.KindInterface},
		) {
			scopedTypeIfaceIDs[id] = true
		}
	}

	// Start breadcrumb per pass: completion-only logging left every slow pass
	// silent until it finished, which is exactly when a breadcrumb is useless.
	passStart := func(pass string) {
		mi.logger.Info("global pass starting",
			zap.String("pass", pass),
			zap.Bool("scoped", scope != nil))
	}

	// The global passes below are the first big read sweeps after the
	// resolve/enrich write storm (a cold pass rewrites 600k+ edge rows).
	// Reading through a multi-GB WAL pays a wal-index lookup per page — a
	// census that takes ~11s against a checkpointed store was measured at
	// ~533s in-run. Drain the log once at this boundary so every pass below
	// reads the main file.
	if cp, ok := mi.graph.(interface{ CheckpointWAL() error }); ok {
		cpStart := time.Now()
		err := cp.CheckpointWAL()
		mi.logger.Info("global passes: WAL checkpoint at read boundary",
			zap.Duration("elapsed", time.Since(cpStart)),
			zap.Error(err))
	}

	passStart("infer_implements")
	implStart := time.Now()
	implAdded := 0
	switch {
	case scope == nil:
		implAdded = r.InferImplements()
	case len(scopedTypeIfaceIDs) > 0:
		// Empty set => no type/interface changed in the batch => no inferred
		// implements edge could have been dropped, so the pass is skipped.
		implAdded = r.InferImplementsScoped(scopedTypeIfaceIDs, scopedTypeIfaceIDs)
	}
	mi.logger.Info("global pass: infer implements",
		zap.Int("added", implAdded),
		zap.Bool("scoped", scope != nil),
		zap.Duration("elapsed", time.Since(implStart)))
	passStart("infer_overrides")
	overStart := time.Now()
	overAdded := 0
	switch {
	case scope == nil:
		overAdded = r.InferOverrides()
	case len(scopedTypeIfaceIDs) > 0:
		overAdded = r.InferOverridesScoped(scopedTypeIfaceIDs)
	}
	mi.logger.Info("global pass: infer overrides",
		zap.Int("added", overAdded),
		zap.Bool("scoped", scope != nil),
		zap.Duration("elapsed", time.Since(overStart)))
	passStart("test_edges")
	testStart := time.Now()
	marked, emitted := markTestSymbolsAndEmitEdgesScoped(mi.graph, changedPrefixes)
	mi.logger.Info("global pass: test edges",
		zap.Int("test_symbols", marked),
		zap.Int("edges", emitted),
		zap.Duration("elapsed", time.Since(testStart)))
	passStart("entrypoint_hierarchy")
	ctrlStart := time.Now()
	// Seeds from already-stamped entry points, so cost is O(seed
	// subtree) and language-gated — safe to run unscoped here like the
	// other global passes; the watcher path uses the Scoped variant.
	if ctrl := entrypoints.PropagateEntryPointsDownHierarchy(mi.graph); ctrl > 0 {
		mi.logger.Info("global pass: entry-point hierarchy",
			zap.Int("stamped", ctrl),
			zap.Duration("elapsed", time.Since(ctrlStart)))
	}
	passStart("capability_edges")
	capStart := time.Now()
	capRe, capEp, capFa := synthesizeCapabilityEdgesScoped(mi.graph, changedPrefixes)
	mi.logger.Info("global pass: capability edges",
		zap.Int("reads_env", capRe),
		zap.Int("executes_process", capEp),
		zap.Int("accesses_field", capFa),
		zap.Duration("elapsed", time.Since(capStart)))
	// Clone detection is PER-REPOSITORY: each tracked repo gets its own
	// finalise + detect over its own nodes (scoped by RepoPrefix), so no
	// cross-repo candidate pair is ever formed and each repo's boilerplate
	// CMS / threshold is computed from that repo's bodies alone. This
	// matches the per-repo incremental maintainer (cloneIndex.Rebuild /
	// UpdateFuncs) so the batch and incremental edge sets agree.
	reporter.Report("clone detection pass (global)", 0, 0)
	mi.mu.RLock()
	cloneIdx := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		cloneIdx = append(cloneIdx, idx)
	}
	mi.mu.RUnlock()
	// Scope to the repos that re-indexed this batch when the warmup armed a
	// batch scope. An unchanged repo's clone edges are already on disk and
	// its incremental clone index is reseeded lazily on its first later edit
	// (indexFile: if !built → Rebuild), so skipping its full-graph detect +
	// Rebuild here is sound and cuts an N-repo warm restart from N full-graph
	// clone walks to just the changed repos'. A nil scope runs every repo. The
	// scope is taken once at the top of RunGlobalGraphPasses and shared by every
	// pass; the clone passes reuse it here (takeBatchScope must not be called
	// twice — it clears the armed scope on the first read).
	inCloneScope := func(prefix string) bool {
		if scope == nil {
			return true
		}
		_, ok := scope[prefix]
		return ok
	}
	passStart("clone_detect")
	cloneDetectStart := time.Now()
	clonesDetected := 0
	for _, idx := range cloneIdx {
		if !inCloneScope(idx.repoPrefix) {
			continue
		}
		clonesDetected++
		// Per-repo threshold, NOT a max-over-repos value: the batch must use
		// the same cutoff the per-repo incremental maintainer uses
		// (UpdateFuncs/Rebuild → idx.cloneThreshold()), or the batch and
		// incremental edge sets diverge for any repo whose configured
		// threshold differs from the workspace maximum.
		if cs := detectClonesAndEmitEdgesCtx(ctx, mi.graph, idx.repoPrefix, idx.cloneThreshold()); cs.Items > 0 {
			mi.logger.Info("clone edges emitted (global)",
				zap.String("repo", idx.repoPrefix),
				zap.Int("items", cs.Items),
				zap.Int("clone_pairs", cs.Pairs),
				zap.Int("edges", cs.Edges),
				zap.Int("skipped_buckets", cs.SkippedBuckets),
				zap.Int("skipped_bucket_items", cs.SkippedBucketItems),
				zap.Int("diffused_pairs", cs.DiffusedPairs),
				zap.Int("diffused_edges", cs.DiffusedEdges),
			)
		}
	}
	// Seed each per-repo indexer's incremental clone index from the
	// freshly-baselined signatures + sidecar (scoped to that repo's
	// prefix) so steady-state single-file edits after this batch go
	// incremental instead of re-running the whole-graph pass per file.
	cloneRebuildStart := time.Now()
	clonesRebuilt := 0
	for _, idx := range cloneIdx {
		if !inCloneScope(idx.repoPrefix) {
			continue
		}
		if idx.cloneIndex != nil {
			idx.cloneIndex.Rebuild(mi.graph, idx.repoPrefix)
			clonesRebuilt++
		}
	}
	// Aggregate timing for the clone passes — previously the most expensive
	// and least observable part of a warm restart (the per-repo logs above
	// only fire when a repo actually has clone pairs, so a long run could
	// pass with no breadcrumbs).
	mi.logger.Info("clone passes done (global)",
		zap.Bool("scoped", scope != nil),
		zap.Int("repos_total", len(cloneIdx)),
		zap.Int("detected", clonesDetected),
		zap.Int("rebuilt", clonesRebuilt),
		zap.Duration("detect_elapsed", cloneRebuildStart.Sub(cloneDetectStart)),
		zap.Duration("rebuild_elapsed", time.Since(cloneRebuildStart)),
	)
	// Framework dynamic-dispatch synthesis (gRPC stubs, Temporal
	// workflow→activity, in-process / native event channels, native
	// bridges). After InferImplements/InferOverrides (the
	// interface-satisfaction signals) and before DetectCrossRepoEdges so
	// a cross-repo synthesized call gets its parallel cross_repo_calls
	// edge.
	reporter.Report("framework dispatch synthesis (global)", 0, 0)
	passStart("framework_synthesis")
	// A full-coverage batch (cold index / full-workspace reconciliation)
	// carries the daemon's one-shot census attestation: admission censuses
	// read the raw store while synthesizer execution keeps the scoped view.
	// Taken OUTSIDE the pass timer: it contends on mi.mu, and a stall there
	// must read as its own number, not as unattributable synthesis wall.
	censusTakeStart := time.Now()
	batchCensusEligible := mi.takeBatchCensusEligible()
	censusTakeWait := time.Since(censusTakeStart)
	fwStart := time.Now()
	fwRep := resolver.RunFrameworkSynthesizersScopedWithCensus(mi.graph, changedPrefixes, batchCensusEligible)
	mi.logger.Info("global pass: framework dispatch synthesis",
		zap.Int("edges", fwRep.Total),
		zap.Any("per_synthesizer", fwRep.Per),
		zap.Int64("census_ms", fwRep.CensusMillis),
		zap.Int64("scope_ms", fwRep.ScopeMillis),
		zap.Int64("gate_ms", fwRep.GateMillis),
		zap.Int64("claim_ms", fwRep.ClaimMillis),
		zap.Int64("demote_ms", fwRep.DemoteMillis),
		zap.Duration("census_take_wait", censusTakeWait),
		zap.Duration("elapsed", time.Since(fwStart)))
	// External-call placeholder synthesis (opt-in). Runs after the
	// stub passes so only genuinely un-indexed external targets are
	// left to materialise into call-chain terminals.
	reporter.Report("external-call synthesis (global)", 0, 0)
	passStart("external_call_synthesis")
	extStart := time.Now()
	extEnabled := mi.externalCallSynthesisEnabled()
	extCalls := 0
	if scope != nil {
		extCalls = resolver.SynthesizeExternalCallsForRepos(mi.graph, extEnabled, changedPrefixes)
	} else {
		extCalls = resolver.SynthesizeExternalCalls(mi.graph, extEnabled)
	}
	mi.logger.Info("global pass: external-call synthesis",
		zap.Int("edges", extCalls),
		zap.Bool("scoped", scope != nil),
		zap.Duration("elapsed", time.Since(extStart)))
	// Cross-repo edge layer. Runs after InferImplements / InferOverrides
	// so the implements / extends edges they materialise across repo
	// boundaries pick up their parallel cross_repo_* edges.
	reporter.Report("cross-repo edges (global)", 0, 0)
	passStart("cross_repo_edges")
	crStart := time.Now()
	crossRepoEdges := 0
	if scope != nil {
		crossRepoEdges = resolver.DetectCrossRepoEdgesForRepos(mi.graph, scopedRepoPrefixes)
	} else {
		crossRepoEdges = resolver.DetectCrossRepoEdges(mi.graph)
	}
	mi.logger.Info("global pass: cross-repo edges",
		zap.Int("edges", crossRepoEdges),
		zap.Duration("elapsed", time.Since(crStart)))
	mi.logger.Info("global passes complete",
		zap.Duration("total", time.Since(globalStart)))
}

// externalCallSynthesisEnabled resolves whether external-call placeholder
// synthesis should run over the shared graph. The pass is graph-wide, so
// it is enabled when any tracked repo opted in — a repo that wants the
// external hops in its call chains gets them even when a sibling repo
// left the option off.
func (mi *MultiIndexer) externalCallSynthesisEnabled() bool {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	for _, idx := range mi.indexers {
		if idx.externalCallSynthesisEnabled() {
			return true
		}
	}
	return false
}

// NewMultiIndexer creates a MultiIndexer.
func NewMultiIndexer(
	g graph.Store,
	reg *parser.Registry,
	s search.Backend,
	cm *config.ConfigManager,
	logger *zap.Logger,
) *MultiIndexer {
	return &MultiIndexer{
		graph:           g,
		registry:        reg,
		search:          s,
		repos:           make(map[string]*RepoMetadata),
		indexers:        make(map[string]*Indexer),
		configMgr:       cm,
		logger:          logger,
		shadowAdmission: processShadowAdmission,
	}
}

// IndexAll indexes all active repos concurrently. Each repo gets its own
// Indexer instance with repo-specific config. Returns per-repo IndexResults.
func (mi *MultiIndexer) IndexAll() (map[string]*IndexResult, error) {
	return mi.IndexScoped("", "")
}

// IndexScoped is IndexAll restricted to repos whose workspace and
// project slugs match. Empty filters disable that axis (so empty/empty
// is equivalent to IndexAll). Resolution honours the same precedence
// as resolveWorkspaceID/resolveProjectID — RepoEntry override →
// `.gortex.yaml::workspace` → repo prefix — so a `gortex server
// --workspace foo` invocation matches both repos that declare
// `workspace: foo` in their own `.gortex.yaml` and repos pinned to
// `foo` from the user's global config.
//
// Returns an error when the filters exclude every active repo, so a
// `--workspace typo` surfaces as a startup failure rather than a
// silently empty graph.
func (mi *MultiIndexer) IndexScoped(workspaceSlug, projectSlug string) (map[string]*IndexResult, error) {
	repos := mi.configMgr.ActiveRepos()
	if len(repos) == 0 {
		return nil, nil
	}
	if workspaceSlug != "" || projectSlug != "" {
		filtered := mi.filterReposByScope(repos, workspaceSlug, projectSlug)
		if len(filtered) == 0 {
			return nil, fmt.Errorf("scope filter matched zero of %d active repos (workspace=%q project=%q)", len(repos), workspaceSlug, projectSlug)
		}
		repos = filtered
	}

	// indexMultiRepo owns the complete coordinated pipeline, including the
	// one contract reconciliation required before graph-wide derivation. Do
	// not repeat it here: reconciliation evicts and rebuilds a generation and
	// a second identical pass is pure global work.
	//
	// A lone repo takes the same path as any other — it is simply the first
	// tracked repo, and its nodes carry its prefix like everyone else's.
	return mi.indexMultiRepo(repos)
}

// filterReposByScope returns the subset of repos whose resolved
// workspace and project slugs match the supplied filters. Empty
// filters disable that axis. Loads each repo's `.gortex.yaml` first so
// resolution sees the workspace/project declared there — matching only
// against `RepoEntry.Workspace` would miss repos that declare their
// slug in their own config file (the typical case for first-party
// repos).
func (mi *MultiIndexer) filterReposByScope(repos []config.RepoEntry, workspaceSlug, projectSlug string) []config.RepoEntry {
	if workspaceSlug == "" && projectSlug == "" {
		return repos
	}
	out := make([]config.RepoEntry, 0, len(repos))
	for _, e := range repos {
		absPath, err := filepath.Abs(e.Path)
		if err != nil {
			continue
		}
		prefix := config.ResolvePrefix(e)
		mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
		cfg := mi.configMgr.GetRepoConfig(prefix)
		entryCopy := e
		if workspaceSlug != "" && resolveWorkspaceID(&entryCopy, cfg, prefix) != workspaceSlug {
			continue
		}
		if projectSlug != "" && resolveProjectID(&entryCopy, cfg, prefix) != projectSlug {
			continue
		}
		out = append(out, e)
	}
	return out
}

// readGoModModule reads the module path from a go.mod file.
func readGoModModule(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module"))
		}
	}
	return ""
}

// rebuildColdRefFacts seeds the durable resolved-reference sidecar once for
// the complete successful repository set. The backend implementation is a
// single set-oriented transaction; keeping the capability probe here avoids a
// per-repository persistence loop and leaves in-memory stores as a no-op.
func (mi *MultiIndexer) rebuildColdRefFacts(ctx context.Context, repoPrefixes []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rebuilder, ok := mi.graph.(graph.RefFactsRebuilder)
	if !ok {
		return nil
	}
	if err := rebuilder.RebuildRefFactsForRepos(repoPrefixes); err != nil {
		return err
	}
	// The current backend contract is atomic but not interruptible in flight.
	// Observing cancellation here prevents completion/global consumers from
	// publishing a generation whose ref-fact boundary the caller abandoned.
	return ctx.Err()
}

// indexMultiRepo indexes multiple repos concurrently with repo prefixes.
func (mi *MultiIndexer) indexMultiRepo(repos []config.RepoEntry) (map[string]*IndexResult, error) {
	type repoResult struct {
		prefix string
		result *IndexResult
		idx    *Indexer
		meta   *RepoMetadata
		err    error
	}

	// Resolve each repo's prefix serially first: git-worktree instancing,
	// the derived-name persistence, and the cross-batch collision guard
	// must be deterministic, not raced across the parallel index
	// goroutines below. The same loop builds the tracked-module map used
	// for cross-repo dependency detection.
	type resolvedRepo struct {
		entry    config.RepoEntry
		prefix   string
		cfg      *config.Config
		absPath  string
		identity *RepoIdentity
	}
	trackedModules := make(map[string]string)
	resolved := make([]resolvedRepo, 0, len(repos))
	seenPrefix := make(map[string]string, len(repos)) // prefix → absPath
	var resolveErrors []string
	for _, entry := range repos {
		absPath, err := filepath.Abs(entry.Path)
		if err != nil {
			resolveErrors = append(resolveErrors, fmt.Sprintf("resolving path %s: %v", entry.Path, err))
			continue
		}
		info, err := os.Stat(absPath)
		if err != nil {
			resolveErrors = append(resolveErrors, fmt.Sprintf("opening repository %s: %v", entry.Path, err))
			continue
		}
		if !info.IsDir() {
			resolveErrors = append(resolveErrors, fmt.Sprintf("opening repository %s: not a directory", entry.Path))
			continue
		}
		identity, _ := DetectIdentity(absPath)
		e := entry
		prefix, cfg := mi.resolveTrackPrefix(&e, absPath, identity)
		// Two distinct checkouts can still land on the same derived prefix
		// (e.g. two worktrees declaring the same workspace). resolveTrackPrefix
		// only sees already-tracked repos, so guard against collisions within
		// this batch too.
		if prev, ok := seenPrefix[prefix]; ok && prev != absPath {
			prefix += "-" + shortPathHash(absPath)
			e.Name = prefix
		}
		seenPrefix[prefix] = absPath
		resolved = append(resolved, resolvedRepo{entry: e, prefix: prefix, cfg: cfg, absPath: absPath, identity: identity})
		if mod := readGoModModule(absPath); mod != "" {
			trackedModules[prefix] = mod
			mi.logger.Debug("tracked repo module", zap.String("repo", prefix), zap.String("module", mod))
		}
	}

	resultCh := make(chan repoResult, len(resolved))
	var wg sync.WaitGroup
	coordinatedBulk, _ := mi.graph.(graph.CoordinatedBulkLoader)
	coordinatedBulkActive := coordinatedBulk != nil && coordinatedBulk.BeginCoordinatedBulkLoad()
	defer func() {
		if coordinatedBulkActive {
			if err := coordinatedBulk.EndCoordinatedBulkLoad(); err != nil {
				mi.logger.Error("multi-repo bulk-load cleanup failed", zap.Error(err))
			}
		}
	}()

	for _, rr := range resolved {
		wg.Add(1)
		go func(r resolvedRepo) {
			defer wg.Done()

			idx := mi.newPerRepoIndexer(r.cfg.Index)
			idx.SetRepoPrefix(r.prefix)
			entryCopy := r.entry
			idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, r.cfg, r.prefix))
			idx.SetProjectID(resolveProjectID(&entryCopy, r.cfg, r.prefix))
			idx.SetTrackedRepoModules(trackedModules)
			// Defer the per-repo cross-cutting passes (ResolveAll,
			// semantic enrich, contract extract+commit) so they don't
			// race against each other across goroutines on the shared
			// graph. They run serially below via RunDeferredPasses after
			// wg.Wait(). The graph-wide derivation passes run once after
			// the loop via mi.RunGlobalGraphPasses().
			idx.SetDeferResolve(true)

			result, err := idx.Index(r.absPath)
			if err != nil {
				resultCh <- repoResult{prefix: r.prefix, err: fmt.Errorf("indexing %s: %w", r.absPath, err)}
				return
			}
			result.RepoPrefix = r.prefix

			meta := &RepoMetadata{
				RepoPrefix:    r.prefix,
				RootPath:      r.absPath,
				Identity:      r.identity,
				LastIndexTime: time.Now(),
				FileCount:     result.FileCount,
				NodeCount:     result.NodeCount,
				EdgeCount:     result.EdgeCount,
				ParseErrors:   result.Errors,
				FileMtimes:    idx.FileMtimes(),
				IsWorktree:    ResolveWorktree(r.absPath).IsWorktree,
			}

			resultCh <- repoResult{prefix: r.prefix, result: result, idx: idx, meta: meta}
		}(rr)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	results := make(map[string]*IndexResult)
	indexErrors := resolveErrors

	mi.mu.Lock()
	for rr := range resultCh {
		if rr.err != nil {
			mi.logger.Error("failed to index repo", zap.String("prefix", rr.prefix), zap.Error(rr.err))
			indexErrors = append(indexErrors, rr.err.Error())
			continue
		}
		mi.repos[rr.prefix] = rr.meta
		mi.indexers[rr.prefix] = rr.idx
		results[rr.prefix] = rr.result
	}
	mi.mu.Unlock()
	if coordinatedBulkActive {
		if err := coordinatedBulk.EndCoordinatedBulkLoad(); err != nil {
			return nil, fmt.Errorf("multi-repo bulk-load finalize: %w", err)
		}
		coordinatedBulkActive = false
	}

	// Do not publish a completed pipeline when no repository reached the
	// deferred stages. Besides making the failure deterministic, this prevents
	// global passes from deriving edges from a partially-drained failed batch.
	if len(indexErrors) > 0 && len(results) == 0 {
		sort.Strings(indexErrors)
		return nil, fmt.Errorf("all repos failed to index: %s", strings.Join(indexErrors, "; "))
	}

	// Complete cold multi-repo indexing through the same coordinated pipeline
	// used by daemon warmup:
	//   1. materialise go.mod contracts once, then run one shared base resolve;
	//   2. enrich repositories in bounded language-aware batches (large Go
	//      repositories remain exclusive), committing contracts only after each
	//      batch drains;
	//   3. use the mutation receipt to perform only the exact catch-up needed for
	//      semantic/contract mutations.
	//
	// The old loop called idx.RunDeferredPasses with
	// skipResolveInDeferred=false, so every repository performed ResolveAll over
	// the entire shared graph. At R repositories and E edges that was O(R*E) and
	// was the dominant cold-index regression. runDeferredGoMod is generation-
	// idempotent, so RunDeferredPassesAll does not repeat the pre-resolve work.
	deferCtx := context.Background()
	mi.RunPreEnrichResolve(deferCtx, nil, nil)
	enrichScheduled := mi.RunDeferredPassesAll(deferCtx)
	mi.logger.Info("multi-repo coordinated deferred passes complete",
		zap.Int("repos_indexed", len(results)),
		zap.Int("repos_failed", len(indexErrors)),
		zap.Int("enrich_scheduled", enrichScheduled))

	// Semantic and contract passes can add new cross-repository candidates.
	// Refresh them once after the receipt-scoped same-repo catch-up and reconcile
	// contract bridges before graph-wide derivation consumes the final graph.
	mi.runCrossRepoResolve(true)

	// ResolveAll normally seeds ref_facts after a full resolve. The coordinated
	// cold path intentionally bypasses per-repository ResolveAll, so seed the
	// successful repository set once after every base, semantic catch-up, and
	// cross-repository mutation has settled. Sorting makes the boundary stable
	// for tracing/tests; SQLite consumes the whole slice in one transaction.
	successfulPrefixes := make([]string, 0, len(results))
	for prefix := range results {
		successfulPrefixes = append(successfulPrefixes, prefix)
	}
	sort.Strings(successfulPrefixes)
	if err := mi.rebuildColdRefFacts(deferCtx, successfulPrefixes); err != nil {
		return results, fmt.Errorf("multi-repo reference-fact rebuild: %w", err)
	}

	// Graph-wide derivation passes run exactly once after every repo
	// has been parsed, every per-repo and cross-repo resolver has lifted
	// placeholder edges, and contract bridges are in place. RunDeferredPasses
	// intentionally skips these so we don't pay an O(global) walk per
	// repo (was the dominant cost at R≈100+).
	mi.RunGlobalGraphPasses(context.Background())

	return results, nil
}

// IndexRepo re-indexes a single repo by prefix. Evicts existing data first.
func (mi *MultiIndexer) IndexRepo(repoPrefix string) (*IndexResult, error) {
	mi.mu.RLock()
	meta, ok := mi.repos[repoPrefix]
	mi.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("repository not found: %s", repoPrefix)
	}

	// Evict existing data for this repo before re-indexing. Always — a lone
	// repo is now stored prefixed (see SetRepoPrefix below), so the eviction
	// must clear the prefixed slice regardless of repo count.
	mi.graph.EvictRepo(repoPrefix)

	mi.configMgr.LoadWorkspaceConfig(repoPrefix, meta.RootPath)
	cfg := mi.configMgr.GetRepoConfig(repoPrefix)
	idx := mi.newPerRepoIndexer(cfg.Index)
	// Always stamp the repo prefix, even when this is the only tracked repo.
	// The multi-repo cold path (indexMultiRepo) already prefixes
	// unconditionally; gating the single-repo re-index on repo count left the
	// two paths producing different id / path shapes for the same repo, so
	// adding a second repo turned the first from bare into prefixed — a lossy
	// whole-graph rewrite. Uniform prefixing removes the single/multi data-shape
	// split: a repo is prefixed from its first index, and adding another is
	// purely additive.
	idx.SetRepoPrefix(repoPrefix)
	entry := mi.configMgr.Global().FindRepoByPrefix(repoPrefix)
	idx.SetWorkspaceID(resolveWorkspaceID(entry, cfg, repoPrefix))
	idx.SetProjectID(resolveProjectID(entry, cfg, repoPrefix))

	result, err := idx.Index(meta.RootPath)
	if err != nil {
		return nil, fmt.Errorf("indexing %s: %w", meta.RootPath, err)
	}

	mi.mu.Lock()
	mi.repos[repoPrefix] = &RepoMetadata{
		RepoPrefix:    repoPrefix,
		RootPath:      meta.RootPath,
		Identity:      meta.Identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
	}
	mi.indexers[repoPrefix] = idx
	mi.mu.Unlock()

	// TODO: After re-indexing, run CrossRepoResolver.ResolveForRepo(repoPrefix)
	// to update cross-repo edges. This will be implemented in Task 7.1.

	mi.ReconcileContractEdges()

	return result, nil
}

// RefreshRepoConfigs re-reads every tracked repo's `.gortex.yaml` and
// pushes the refreshed effective exclude list into that repo's live
// Indexer. Returns the number of repos whose config was re-read.
//
// This is the deterministic re-read path for an already-tracked repo.
// Track and index both load the workspace config on their way in, but a
// repo that is already tracked short-circuits before that — so an edit to
// `.gortex.yaml` had no way to reach a running daemon. Pushing the
// exclude list into the live Indexer matters just as much: it is reused
// by every incremental and scoped re-index, and it froze its ignore list
// at construction.
func (mi *MultiIndexer) RefreshRepoConfigs() int {
	if mi == nil || mi.configMgr == nil {
		return 0
	}

	mi.mu.RLock()
	roots := make(map[string]string, len(mi.repos))
	for prefix, meta := range mi.repos {
		if meta != nil && meta.RootPath != "" {
			roots[prefix] = meta.RootPath
		}
	}
	mi.mu.RUnlock()

	for prefix, root := range roots {
		mi.configMgr.LoadWorkspaceConfig(prefix, root)

		cfg := mi.configMgr.GetRepoConfig(prefix)
		if cfg == nil {
			continue
		}
		mi.mu.RLock()
		idx := mi.indexers[prefix]
		mi.mu.RUnlock()
		if idx != nil {
			idx.SetExcludePatterns(cfg.Index.Exclude)
		}
	}
	return len(roots)
}

// IncrementalReindexRepo incrementally re-indexes a single tracked repo
// by prefix: only the files that changed since the last pass are
// re-parsed and deleted files are evicted, against the repo's existing
// per-repo Indexer (so its mtime snapshot is preserved). Unlike
// IndexRepo it does NOT evict the whole repo first.
//
// When paths is non-empty the pass is scoped to those files /
// directories; otherwise the whole repo root is scanned. Returns an
// error when the prefix is not a tracked repo.
func (mi *MultiIndexer) IncrementalReindexRepo(repoPrefix string, paths []string) (*IndexResult, error) {
	mi.mu.RLock()
	meta, ok := mi.repos[repoPrefix]
	idx := mi.indexers[repoPrefix]
	mi.mu.RUnlock()
	if !ok || meta == nil {
		return nil, fmt.Errorf("repository not found: %s", repoPrefix)
	}
	if idx == nil {
		// Tracked but no live indexer (e.g. restored from snapshot
		// without one) — fall back to a full re-index, which rebuilds
		// the per-repo indexer from scratch.
		return mi.IndexRepo(repoPrefix)
	}

	result, receipt, batch, err := idx.incrementalReindexPathsWithReceipt(meta.RootPath, paths)
	if err != nil {
		return nil, fmt.Errorf("reindexing %s: %w", meta.RootPath, err)
	}

	// Resolve exactly once after the bounded parse/evict batch. Complete
	// mutation receipts provide the precise changed/definition file frontier;
	// only an incomplete receipt falls back to the conservative scoped-global
	// resolver. Derived invalidations run once below after bindings are current.
	mi.resolveIncrementalRepoMutation(repoPrefix, result, receipt, batch)

	mi.mu.Lock()
	mi.repos[repoPrefix] = &RepoMetadata{
		RepoPrefix:    repoPrefix,
		RootPath:      meta.RootPath,
		Identity:      meta.Identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
		// Carried over from the prior metadata: this pass doesn't
		// change either property, and dropping them here (both zero
		// value on a fresh struct literal) used to silently flip a
		// worktree or an Unprefixed solo repo back to their false
		// defaults on the very first watcher-triggered incremental
		// update, defeating callers that key behaviour off them (see
		// the Unprefixed branch in cmd/gortex daemon status).
		IsWorktree: meta.IsWorktree,
		Unprefixed: meta.Unprefixed,
	}
	mi.mu.Unlock()

	idx.observeIncrementalCatchup("derived", result.DerivedInvalidation.Files)
	mi.RunIncrementalDerivedPasses(context.Background(), map[string]DerivedInvalidationPlan{
		repoPrefix: result.DerivedInvalidation,
	})

	return result, nil
}

// TrackRepo validates the path, detects identity, indexes, and adds to config.
func (mi *MultiIndexer) TrackRepo(entry config.RepoEntry) (*IndexResult, error) {
	return mi.TrackRepoCtx(context.Background(), entry)
}

// resolveTrackPrefix determines the repo prefix that absPath should be
// registered under and loads the per-repo `.gortex.yaml` config keyed to
// that prefix. It is the single place that decides whether a git
// worktree of an already-tracked repo becomes an INDEPENDENT instance
// (see WorktreeInstanceName); the track and reconcile paths both call it
// before their "already tracked?" check so a worktree that joins a
// different workspace than the canonical no longer silently coalesces
// into it.
//
// When a separate instance is created the derived `<base>@<tag>` prefix
// is written back into entry.Name so it is persisted to config and
// reproduced deterministically on the next daemon warmup. entry is
// mutated in place; callers pass a pointer to the value they will add to
// the global config.
func (mi *MultiIndexer) resolveTrackPrefix(entry *config.RepoEntry, absPath string, identity *RepoIdentity) (string, *config.Config) {
	base := config.ResolvePrefix(*entry)
	if base == "" || base == "." {
		if identity != nil {
			base = identity.RepoPrefix
		}
	}
	if mi.configMgr == nil {
		return base, config.Default()
	}

	// Load the repo's `.gortex.yaml` under the base prefix first so we can
	// read its declared workspace before deciding the final prefix.
	mi.configMgr.LoadWorkspaceConfig(base, absPath)
	cfg := mi.configMgr.GetRepoConfig(base)

	// An explicit Name already pins the prefix — honour it verbatim. This
	// is also the warm-restart fast path: once a worktree instance has
	// been persisted with its derived Name, every later load short-circuits
	// here without re-deriving.
	if entry.Name != "" {
		return base, cfg
	}

	declaredWS := entry.Workspace
	if declaredWS == "" && cfg != nil {
		declaredWS = cfg.Workspace
	}

	name, separate := WorktreeInstanceName(absPath, base, declaredWS, entry.AsWorktree)
	if !separate {
		return base, cfg
	}

	// Guard against two different checkouts colliding on the same derived
	// name (e.g. two worktrees that both declare workspace `x`): keep the
	// first, suffix the rest with a path hash.
	prefix := name
	mi.mu.RLock()
	existing, ok := mi.repos[prefix]
	mi.mu.RUnlock()
	if ok && existing != nil && !pathkey.SamePathIdentity(existing.RootPath, absPath) {
		prefix = name + "-" + shortPathHash(absPath)
	}

	entry.Name = prefix // persist the decision so warmup reproduces it
	mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
	return prefix, mi.configMgr.GetRepoConfig(prefix)
}

// EffectiveRepoPrefix returns the prefix a repo entry is tracked under,
// accounting for git-worktree instancing — the same value
// resolveTrackPrefix registers, minus the (rare) collision-guard suffix
// it cannot reproduce without the live registry. Warm-restart keying
// (the snapshot-store mtime lookup, the resolve-time LSP helper
// registry) uses this instead of config.ResolvePrefix so a disk-backed
// reconcile finds the worktree instance's own persisted state rather
// than the canonical checkout's. For a plain repo it equals
// config.ResolvePrefix(entry). cm may be nil (then only an explicit
// RepoEntry.Workspace override can trigger instancing).
func EffectiveRepoPrefix(cm *config.ConfigManager, entry config.RepoEntry) string {
	base := config.ResolvePrefix(entry)
	if base == "" || base == "." || entry.Name != "" {
		return base
	}
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return base
	}
	declaredWS := entry.Workspace
	if declaredWS == "" && cm != nil {
		cm.LoadWorkspaceConfig(base, absPath)
		if cfg := cm.GetRepoConfig(base); cfg != nil {
			declaredWS = cfg.Workspace
		}
	}
	name, _ := WorktreeInstanceName(absPath, base, declaredWS, entry.AsWorktree)
	return name
}

// TrackRepoCtx is TrackRepo with a context, allowing callers to pipe progress
// reporters (via progress.WithReporter) through to the underlying Index call.
func (mi *MultiIndexer) TrackRepoCtx(ctx context.Context, entry config.RepoEntry) (*IndexResult, error) {
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", entry.Path, err)
	}
	// Normalise the volume (upper-case a Windows drive letter) so a newly
	// tracked path converges with os.Getwd's convention. The volume is
	// never part of a repo basename, so this cannot rotate a repo prefix;
	// no-op on POSIX.
	absPath = pathkey.NormalizeVolume(absPath)

	// Validate path exists and is a directory.
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", absPath)
	}

	if reason, blocked := unsafeRootBlocked(absPath, entry.Force); blocked {
		return nil, fmt.Errorf("%s; pass force to track it anyway", reason)
	}

	identity, err := DetectIdentity(absPath)
	if err != nil {
		return nil, fmt.Errorf("detecting identity for %s: %w", absPath, err)
	}

	// Resolve the prefix (honouring git-worktree instancing) and load the
	// per-repo `.gortex.yaml` BEFORE the already-tracked check, so a
	// worktree that joins a different workspace than the canonical gets
	// its own `<base>@<tag>` prefix instead of coalescing into it. cfg is
	// keyed to the final prefix; entry.Name is set when a separate
	// instance is created so the decision persists to config. Loading the
	// `.gortex.yaml` here also gives GetRepoConfig the workspace / project
	// slugs declared inside the repo (without it every repo would fall
	// back to `workspace = repoPrefix`, making shared-workspace cross-repo
	// matching impossible to express).
	prefix, cfg := mi.resolveTrackPrefix(&entry, absPath, identity)

	// Check if already tracked. The prefix-keyed check catches the common
	// case; the path scan additionally catches a case-only or Unicode
	// spelling variant of an already-tracked directory on a
	// case-insensitive filesystem (which resolves to a different derived
	// prefix and would otherwise be minted as a bogus second instance —
	// the very thing that flips the daemon into multi-repo mode, #270).
	mi.mu.RLock()
	if _, exists := mi.repos[prefix]; exists {
		mi.mu.RUnlock()
		return nil, nil // already tracked
	}
	for _, meta := range mi.repos {
		if meta != nil && pathkey.SamePathIdentity(meta.RootPath, absPath) {
			mi.mu.RUnlock()
			return nil, nil // same directory, different path spelling
		}
	}
	hook := mi.onRepoTracked
	mi.mu.RUnlock()

	if hook != nil {
		hook(prefix, absPath)
	}

	// Every repo is prefixed, including the first one. There is no
	// repo-count gate here any more: a lone repo is just the first tracked
	// repo, so one ID scheme covers the whole graph whether the workspace
	// holds one repo or twenty. Gating on the count produced two schemes for
	// one graph, and every consumer that keyed on ownership — per-repo
	// counters, scope filters, the nodes_by_repo partial index, warm-restart
	// mtime lookups — had to carry a branch for the unprefixed shape.
	idx := mi.newPerRepoIndexer(cfg.Index)
	idx.SetRepoPrefix(prefix)
	// Workspace / project slugs stamped on every node. Resolution
	// order (highest priority first): RepoEntry.Workspace from the
	// global config (lets users pin OSS repos without committing a
	// `.gortex.yaml`) → `.gortex.yaml::workspace` → repoPrefix
	// (default). resolveWorkspaceID encodes the precedence; the
	// WorkspaceID-keyed contract registry and the boundary-enforced
	// matcher both consume the result.
	entryCopy := entry
	idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, cfg, prefix))
	idx.SetProjectID(resolveProjectID(&entryCopy, cfg, prefix))

	result, err := idx.IndexCtx(ctx, absPath)
	if err != nil {
		return nil, fmt.Errorf("indexing %s: %w", absPath, err)
	}
	result.RepoPrefix = prefix

	mi.mu.Lock()
	mi.repos[prefix] = &RepoMetadata{
		RepoPrefix:    prefix,
		RootPath:      absPath,
		Identity:      identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
		IsWorktree:    ResolveWorktree(absPath).IsWorktree,
	}
	mi.indexers[prefix] = idx
	mi.mu.Unlock()

	// Add to global config.
	entry.Path = absPath
	if err := mi.configMgr.Global().AddRepo(entry); err != nil {
		mi.logger.Warn("failed to add repo to config", zap.Error(err))
	}

	// Skip the per-repo contract reconcile when batching: it walks every
	// edge in the shared graph to evict stale EdgeMatches and rebuilds
	// the matcher across every indexer, so paying it once per repo on a
	// warmup over 100+ repos is O(R · E). The batch caller runs it once
	// after the loop (RunGlobalResolve does, and the janitor's ReconcileAll
	// fires it post-loop too).
	if !mi.deferGlobalPasses {
		mi.ReconcileContractEdges()
	}

	return result, nil
}

// ReconcileRepoCtx registers a repo that already has nodes in the graph
// (typically restored from a daemon snapshot) and brings it back into
// agreement with the filesystem without a full re-index. priorMtimes
// carries the mtimes recorded at the time the snapshot was taken;
// IncrementalReindex uses them to detect files that changed offline
// (re-indexes) and files that were deleted offline (evicts).
//
// Falls back to TrackRepoCtx when the repo is not yet tracked AND no
// prior mtimes are available — in that case there's nothing to
// reconcile against and a full index is the correct path.
func (mi *MultiIndexer) ReconcileRepoCtx(ctx context.Context, entry config.RepoEntry, priorMtimes map[string]int64) (*IndexResult, error) {
	start := time.Now()

	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", entry.Path, err)
	}
	// Normalise the volume (upper-case a Windows drive letter) to converge
	// with os.Getwd's convention. Cosmetic; never touches the basename.
	absPath = pathkey.NormalizeVolume(absPath)
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", absPath)
	}
	identity, err := DetectIdentity(absPath)
	if err != nil {
		return nil, fmt.Errorf("detecting identity for %s: %w", absPath, err)
	}

	// Resolve the prefix (honouring git-worktree instancing) and load the
	// per-repo `.gortex.yaml` before the already-tracked check. Mirrors
	// TrackRepoCtx so a worktree instance keeps its derived prefix on the
	// reconcile path too; config can change between sessions and warmup-
	// time reconcile runs after a daemon restart.
	prefix, cfg := mi.resolveTrackPrefix(&entry, absPath, identity)

	// Already tracked — nothing to do.
	mi.mu.RLock()
	_, exists := mi.repos[prefix]

	mi.mu.RUnlock()
	if exists {
		return nil, nil
	}

	// Fall back to full TrackRepoCtx when we have no prior mtimes:
	// there's nothing meaningful to reconcile against, and
	// IncrementalReindex would treat every file as stale, producing
	// the same duplicate-writes problem we're fixing. entry.Name is
	// already pinned by resolveTrackPrefix, so the fallback reproduces
	// the same prefix.
	if len(priorMtimes) == 0 {
		return mi.TrackRepoCtx(ctx, entry)
	}

	// Prefix unconditionally, as TrackRepoCtx does — see the note there.
	idx := mi.newPerRepoIndexer(cfg.Index)
	idx.SetRepoPrefix(prefix)
	entryCopy := entry
	idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, cfg, prefix))
	idx.SetProjectID(resolveProjectID(&entryCopy, cfg, prefix))
	idx.SetRootPath(absPath)
	idx.SetFileMtimes(priorMtimes)

	// Choose the reconcile strategy from a census of what changed on disk
	// while the daemon was down. Scoped incremental is the default: re-index
	// only the changed files and evict only the deleted ones, leaving the
	// rest of the already-persisted graph untouched. A whole-repo re-track
	// (IndexCtx — which evicts and re-parses every file, then bulk-drains)
	// is reserved for the cases where scoping is unsafe or not worth it: the
	// census could not be taken, the churn is a large fraction of the repo,
	// or an operator forced it via GORTEX_WARMUP_FULL_RETRACK. A repo with
	// zero changes keeps the fast IncrementalReindex no-op (walk + 0 stale →
	// return), which is what makes an unchanged warm restart near-instant.
	//
	// The in-memory backend (*graph.Graph) keeps its exact prior behaviour:
	// IncrementalReindex is the authoritative path there — it evicts
	// offline-deleted files in place, has no reopened disk store, and so no
	// per-edge write to route around. Gate on the store type.
	_, memoryBacked := mi.graph.(*graph.Graph)
	var (
		result           *IndexResult
		changed, deleted []string
		route            = "incremental"
	)
	// fullRetrack is the whole-repo re-track — the ONE place FullRetrack is
	// stamped. StaleFileCount keeps its honest incremental-work meaning (0
	// here) because the changed-file set is not enumerated on this path, so
	// callers must key "did this repo change" off FullRetrack instead.
	fullRetrack := func() (*IndexResult, error) {
		r, e := idx.IndexCtx(ctx, absPath)
		if e == nil && r != nil {
			r.FullRetrack = true
		}
		return r, e
	}
	switch {
	case memoryBacked:
		result, err = idx.IncrementalReindex(absPath)
	default:
		var censusErr error
		changed, deleted, censusErr = idx.ChangedSinceMtimes(absPath)
		churn := len(changed) + len(deleted)
		priorCount := len(priorMtimes)
		forceFull := os.Getenv("GORTEX_WARMUP_FULL_RETRACK") == "1"
		switch {
		case censusErr != nil || forceFull:
			route = "full_retrack"
			result, err = fullRetrack()
		case churn == 0:
			route = "incremental"
			result, err = idx.IncrementalReindex(absPath)
		case priorCount > 0 && churn*100 > priorCount*40:
			route = "full_retrack"
			result, err = fullRetrack()
		default:
			route = "scoped"
			result, err = idx.IncrementalReindexPaths(absPath, append(changed, deleted...))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("reconciling %s: %w", absPath, err)
	}
	result.RepoPrefix = prefix

	mi.mu.Lock()
	mi.repos[prefix] = &RepoMetadata{
		RepoPrefix:    prefix,
		RootPath:      absPath,
		Identity:      identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
		IsWorktree:    ResolveWorktree(absPath).IsWorktree,
	}
	mi.indexers[prefix] = idx
	mi.mu.Unlock()

	entry.Path = absPath
	if err := mi.configMgr.Global().AddRepo(entry); err != nil {
		mi.logger.Warn("failed to add repo to config", zap.Error(err))
	}

	// See TrackRepoCtx for why this is skipped under deferGlobalPasses.
	if !mi.deferGlobalPasses {
		mi.ReconcileContractEdges()
	}

	mi.logger.Info("daemon: reconciled repo from snapshot",
		zap.String("prefix", prefix),
		zap.String("route", route),
		zap.Int("changed", len(changed)),
		zap.Int("deleted", len(deleted)),
		zap.Bool("full_retrack", result.FullRetrack),
		zap.Int("stale_files_reindexed", result.StaleFileCount),
		zap.Duration("elapsed", time.Since(start)))
	if len(changed) > 0 || len(deleted) > 0 {
		mi.logger.Debug("daemon: reconcile changed-file census",
			zap.String("prefix", prefix),
			zap.Strings("changed", firstNStrings(changed, 5)),
			zap.Strings("deleted", firstNStrings(deleted, 5)))
	}

	return result, nil
}

// firstNStrings returns at most the first n elements of s — used to cap the
// changed/deleted samples in the reconcile debug log so a large-churn repo
// does not dump thousands of paths into the log line.
func firstNStrings(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ReconcileAll runs IncrementalReindex on every tracked repo. Used by
// the daemon's periodic janitor to catch files whose mutations slipped
// past fsnotify — inotify watch limits, NFS / SMB mounts, kernel event
// queue overflow, or daemon downtime where edits happened while nobody
// was listening. Cheap on steady-state repos (one filepath.WalkDir +
// per-file os.Stat per repo), and correctness is self-healing: whatever
// was missed gets picked up on the next tick.
//
// Returns a map of prefix → IndexResult for logging / metrics. Errors
// per repo are logged and do not abort the rest of the sweep — a broken
// permission bit on one repo should not starve reconciliation on the
// others.
func (mi *MultiIndexer) ReconcileAll() map[string]*IndexResult {
	return mi.ReconcileAllCtx(context.Background())
}

// ReconcileAllCtx is ReconcileAll with cooperative cancellation between
// repositories and before the derived pass coordinator.
func (mi *MultiIndexer) ReconcileAllCtx(ctx context.Context) map[string]*IndexResult {
	mi.mu.RLock()
	prefixes := make([]string, 0, len(mi.indexers))
	for p := range mi.indexers {
		prefixes = append(prefixes, p)
	}
	mi.mu.RUnlock()

	// Same batch trick as warmup: each per-repo IncrementalReindex
	// triggers an O(global) InferImplements/InferOverrides walk if we
	// don't suppress it. With ~100 repos that's ~100× the work for the
	// hourly janitor.
	mi.BeginBatch()
	// Always restore batch flags on exit (incl. panic) WITHOUT running the
	// graph-wide derivation passes — those are run explicitly below, and
	// only when a repo actually reindexed. The hourly janitor used to run
	// EndBatch unconditionally, walking the full graph (InferImplements /
	// InferOverrides / clone detection over hundreds of thousands of
	// edges) every cycle even when nothing changed — wasted CPU and, on a
	// small resident buffer pool, needless memory churn.
	defer mi.ResetBatch()

	results := make(map[string]*IndexResult, len(prefixes))
	plans := make(map[string]DerivedInvalidationPlan)
	for _, prefix := range prefixes {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return results
			default:
			}
		}
		mi.mu.RLock()
		idx, ok := mi.indexers[prefix]
		meta, metaOK := mi.repos[prefix]
		mi.mu.RUnlock()
		if !ok || !metaOK || meta == nil || meta.RootPath == "" {
			continue
		}
		result, err := idx.IncrementalReindex(meta.RootPath)
		if err != nil {
			mi.logger.Warn("janitor: reconcile failed",
				zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		if result != nil && (result.StaleFileCount > 0 || result.DeletedFileCount > 0) {
			mi.logger.Info("janitor: reconciled repo",
				zap.String("prefix", prefix),
				zap.Int("stale_files_reindexed", result.StaleFileCount))
			plan := plans[prefix]
			plan.Merge(result.DerivedInvalidation)
			plans[prefix] = plan
		}
		results[prefix] = result

		// Keep RepoMetadata.FileMtimes in sync so the next snapshot
		// picks up the reconciled mtimes.
		mi.mu.Lock()
		if m, ok := mi.repos[prefix]; ok && m != nil {
			m.FileMtimes = idx.FileMtimes()
			m.LastIndexTime = time.Now()
			// This pass was whole-repo, so its counts describe the repo
			// and are safe to stamp. Doing so also heals metadata that a
			// scoped pass left describing only its own scope, so a daemon
			// already reporting a wrong file count converges on its own
			// instead of needing a restart.
			if result != nil {
				m.FileCount = result.FileCount
				m.NodeCount = result.NodeCount
				m.EdgeCount = result.EdgeCount
			}
		}
		mi.mu.Unlock()
	}

	if len(plans) > 0 {
		// Run only the derived families selected by each repo's exact file
		// deltas. Body-only and metadata-only changes therefore avoid global
		// derivation, while structural changes retain conservative fallbacks.
		mi.RunIncrementalDerivedPasses(ctx, plans)
	}
	return results
}

// UntrackRepo evicts a repo from the graph and removes it from config.
func (mi *MultiIndexer) UntrackRepo(repoPrefix string) (int, int) {
	mi.mu.Lock()
	meta, ok := mi.repos[repoPrefix]
	if !ok {
		mi.mu.Unlock()
		return 0, 0
	}
	delete(mi.repos, repoPrefix)
	delete(mi.indexers, repoPrefix)
	mi.mu.Unlock()

	var nodesRemoved, edgesRemoved int
	if meta.Unprefixed {
		// Single-repo-mode nodes carry RepoPrefix="" and never enter the
		// byRepo bucket EvictRepo walks — evict them file-by-file off the
		// recorded file set instead, or they linger in the graph and a
		// later lone repo would mis-resolve them.
		for path := range meta.FileMtimes {
			n, e := mi.graph.EvictFile(path)
			nodesRemoved += n
			edgesRemoved += e
		}
	} else if purger, ok := mi.graph.(interface{ PurgeRepo(string) error }); ok {
		// Prefer the full sidecar-aware purge. EvictRepo drops only
		// nodes+edges and leaves fifteen repo_prefix-keyed sidecar tables
		// (file_mtimes, *_enrichment, symbol_fts, content_fts, ...) behind,
		// which accumulate across untrack/retrack cycles until they dominate
		// a long-lived store. PurgeRepo clears them in one transaction. It
		// returns no counts, so report the repo's last-index metadata as the
		// removed estimate; fall back to EvictRepo (real counts) on error.
		if err := purger.PurgeRepo(repoPrefix); err != nil {
			mi.logger.Warn("purge repo failed; falling back to node/edge eviction",
				zap.String("prefix", repoPrefix), zap.Error(err))
			nodesRemoved, edgesRemoved = mi.graph.EvictRepo(repoPrefix)
		} else {
			nodesRemoved, edgesRemoved = meta.NodeCount, meta.EdgeCount
		}
	} else {
		// Backends without the purge capability (the in-memory store has no
		// sidecars, so EvictRepo is already complete there).
		nodesRemoved, edgesRemoved = mi.graph.EvictRepo(repoPrefix)
	}

	// Remove from global config.
	if meta.RootPath != "" {
		if err := mi.configMgr.Global().RemoveRepo(meta.RootPath); err != nil {
			mi.logger.Warn("failed to remove repo from config",
				zap.String("prefix", repoPrefix), zap.Error(err))
		}
	}

	mi.ReconcileContractEdges()

	return nodesRemoved, edgesRemoved
}

// WorktreeGC is the per-repo outcome of GCVanishedWorktrees.
type WorktreeGC struct {
	RepoPrefix   string
	RootPath     string
	NodesRemoved int
	EdgesRemoved int
}

// GCVanishedWorktrees garbage-collects the index of any tracked linked
// git worktree whose root directory has disappeared from disk — the
// `git worktree remove` (or manual deletion) case. Each vanished
// worktree's branch-keyed snapshot slot and graph nodes would otherwise
// leak forever: a removed worktree never fires a per-file fsnotify
// delete for its whole tree, and the janitor's IncrementalReindex just
// errors out on the missing root without evicting anything.
//
// Only repos recorded as worktrees (RepoMetadata.IsWorktree) are
// eligible — a vanished *main* checkout is left alone, since that is
// far more likely a transient mount problem than an intentional
// removal, and untracking it would also orphan every linked worktree
// that shares its .git. The directory-existence test uses the same
// not-exist-only rule as the per-file deletion detector, so a flaky
// filesystem cannot trigger a destructive eviction.
//
// Returns one WorktreeGC record per repo evicted; an empty slice when
// every tracked worktree is still present.
func (mi *MultiIndexer) GCVanishedWorktrees() []WorktreeGC {
	// Snapshot the candidate set under the read lock, then evict
	// outside it — UntrackRepo takes the write lock itself.
	type candidate struct {
		prefix string
		root   string
	}
	var candidates []candidate
	mi.mu.RLock()
	for prefix, meta := range mi.repos {
		if meta == nil || !meta.IsWorktree || meta.RootPath == "" {
			continue
		}
		if WorktreeRootGone(meta.RootPath) {
			candidates = append(candidates, candidate{prefix: prefix, root: meta.RootPath})
		}
	}
	mi.mu.RUnlock()

	if len(candidates) == 0 {
		return nil
	}

	out := make([]WorktreeGC, 0, len(candidates))
	for _, c := range candidates {
		nodes, edges := mi.UntrackRepo(c.prefix)
		mi.logger.Info("janitor: garbage-collected vanished worktree",
			zap.String("prefix", c.prefix),
			zap.String("root", c.root),
			zap.Int("nodes_removed", nodes),
			zap.Int("edges_removed", edges))
		out = append(out, WorktreeGC{
			RepoPrefix:   c.prefix,
			RootPath:     c.root,
			NodesRemoved: nodes,
			EdgesRemoved: edges,
		})
	}
	return out
}

// GetMetadata returns the metadata for a specific repo, or nil if not found.
func (mi *MultiIndexer) GetMetadata(repoPrefix string) *RepoMetadata {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return mi.repos[repoPrefix]
}

// AllMetadata returns a copy of all repo metadata.
func (mi *MultiIndexer) AllMetadata() map[string]*RepoMetadata {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	out := make(map[string]*RepoMetadata, len(mi.repos))
	for k, v := range mi.repos {
		out[k] = v
	}
	return out
}

// IsMultiRepo returns true when more than one repo is tracked.
func (mi *MultiIndexer) IsMultiRepo() bool {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return len(mi.repos) > 1
}

// RepoForFile returns the repo prefix for a given file path by checking
// which repo root contains it. Returns empty string if no match.
func (mi *MultiIndexer) RepoForFile(filePath string) string {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return ""
	}

	mi.mu.RLock()
	defer mi.mu.RUnlock()

	var bestPrefix string
	var bestLen int

	for prefix, meta := range mi.repos {
		// Fold-aware containment so a case-variant file path still maps
		// to its repo on a case-insensitive filesystem. Longest-root-wins
		// breaks ties by nesting depth; nested roots share a prefix, so
		// the raw RootPath length orders them the same as their folded
		// forms would.
		if pathkey.HasPathPrefix(absPath, meta.RootPath) {
			if len(meta.RootPath) > bestLen {
				bestLen = len(meta.RootPath)
				bestPrefix = prefix
			}
		}
	}

	return bestPrefix
}

// GetIndexer returns the Indexer for a specific repo prefix, or nil.
func (mi *MultiIndexer) GetIndexer(repoPrefix string) *Indexer {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return mi.indexers[repoPrefix]
}

type grepRepoJob struct {
	prefix string
	idx    *Indexer
}

func (mi *MultiIndexer) grepRepoJobs(repoAllow map[string]bool) []grepRepoJob {
	if mi == nil {
		return nil
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	capHint := len(mi.indexers)
	if repoAllow != nil {
		capHint = len(repoAllow)
	}
	jobs := make([]grepRepoJob, 0, capHint)
	for prefix, idx := range mi.indexers {
		if idx == nil {
			continue
		}
		if repoAllow != nil && !repoAllow[prefix] {
			continue
		}
		jobs = append(jobs, grepRepoJob{prefix: prefix, idx: idx})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].prefix < jobs[j].prefix })
	return jobs
}

func singleAllowedRepo(repoAllow map[string]bool) (string, bool) {
	if repoAllow == nil {
		return "", false
	}
	var only string
	count := 0
	for prefix, allowed := range repoAllow {
		if !allowed {
			continue
		}
		only = prefix
		count++
	}
	return only, count == 1
}

func stampGrepMatchPaths(prefix string, hits []trigram.Match) []trigram.Match {
	if prefix == "" {
		return hits
	}
	for i := range hits {
		hits[i].Path = prefix + "/" + hits[i].Path
	}
	return hits
}

func capGrepMatches(matches []trigram.Match, limit int) []trigram.Match {
	if limit > 0 && len(matches) > limit {
		return matches[:limit]
	}
	return matches
}

// GrepText fans out a trigram-accelerated literal search across every
// tracked per-repo Indexer and returns the union, capped at limit.
// Match paths are re-stamped from repo-root-relative to repo-prefixed
// (e.g. "internal/foo.go" → "gortex/internal/foo.go") so callers can
// route hits back to the right working tree without consulting the
// MultiIndexer afterwards. A non-positive limit returns every match.
// Returns nil when no per-repo indexer can serve the query — the
// single-Indexer path (Indexer.GrepText) is used by callers without a
// MultiIndexer.
func (mi *MultiIndexer) GrepText(query string, limit int) []trigram.Match {
	return capGrepMatches(mi.GrepTextForRepos(query, nil, limit), limit)
}

// GrepTextForRepos is the scoped variant of GrepText. When repoAllow is
// non-nil, only those repo prefixes are searched. perRepoLimit caps each
// searched repo independently; the returned union is intentionally not
// globally capped so callers can apply path / graph-scope filters first.
func (mi *MultiIndexer) GrepTextForRepos(query string, repoAllow map[string]bool, perRepoLimit int) []trigram.Match {
	if mi == nil || query == "" {
		return nil
	}
	if prefix, ok := singleAllowedRepo(repoAllow); ok {
		idx := mi.GetIndexer(prefix)
		if idx == nil {
			return nil
		}
		return stampGrepMatchPaths(prefix, idx.GrepText(query, perRepoLimit))
	}

	// Per-repo cap mirrors the caller's page size when set. The caller
	// applies the final cap after any path / graph-scope filters, so a
	// repo outside those filters cannot consume the page first. Zero /
	// negative means no per-repo cap (let each searcher return everything).
	jobs := mi.grepRepoJobs(repoAllow)
	out := make([]trigram.Match, 0, len(jobs)*8)
	for _, j := range jobs {
		hits := j.idx.GrepText(query, perRepoLimit)
		if len(hits) == 0 {
			continue
		}
		// Trigram emits forward-slash repo-relative paths. Stamp the repo
		// prefix so downstream tools (resolveGraphPath, path-prefix filters)
		// see the same shape they get from the graph nodes.
		out = append(out, stampGrepMatchPaths(j.prefix, hits)...)
	}
	return out
}

// GrepRegexp fans out a trigram-accelerated regex search across every
// tracked per-repo Indexer and returns the union, capped at limit.
// Mirrors GrepText: match paths are re-stamped from repo-root-relative
// to repo-prefixed so downstream tools route hits back to the right
// working tree. pathPrefix, when non-empty, restricts the scan to
// files under that prefix on each indexer. A pattern that does not
// compile in any indexer is reported once; per-indexer errors after
// the first compile are otherwise treated as no-match.
func (mi *MultiIndexer) GrepRegexp(pattern, pathPrefix string, limit int) ([]trigram.Match, error) {
	hits, err := mi.GrepRegexpForRepos(pattern, pathPrefix, nil, limit)
	if err != nil {
		return nil, err
	}
	return capGrepMatches(hits, limit), nil
}

// GrepRegexpForRepos is the scoped variant of GrepRegexp. repoAllow and
// perRepoLimit have the same semantics as GrepTextForRepos.
func (mi *MultiIndexer) GrepRegexpForRepos(pattern, pathPrefix string, repoAllow map[string]bool, perRepoLimit int) ([]trigram.Match, error) {
	if mi == nil || pattern == "" {
		return nil, nil
	}
	if prefix, ok := singleAllowedRepo(repoAllow); ok {
		idx := mi.GetIndexer(prefix)
		if idx == nil {
			return nil, nil
		}
		hits, err := idx.GrepRegexp(pattern, pathPrefix, perRepoLimit)
		if err != nil {
			return nil, err
		}
		return stampGrepMatchPaths(prefix, hits), nil
	}

	jobs := mi.grepRepoJobs(repoAllow)
	out := make([]trigram.Match, 0, len(jobs)*8)
	for _, j := range jobs {
		hits, err := j.idx.GrepRegexp(pattern, pathPrefix, perRepoLimit)
		if err != nil {
			// First compile error short-circuits — the pattern is the
			// caller's fault and won't compile in any other indexer
			// either (the trigram searcher uses the same regexp engine).
			return nil, err
		}
		if len(hits) == 0 {
			continue
		}
		out = append(out, stampGrepMatchPaths(j.prefix, hits)...)
	}
	return out, nil
}

// IndexerForFile routes an absolute path to the per-repo Indexer that
// owns it. Returns (nil, "") when no tracked repo contains the path.
// Used by the MCP overlay middleware to find the right Indexer for a
// pushed file when constructing the per-request overlay layer.
func (mi *MultiIndexer) IndexerForFile(absPath string) (*Indexer, string) {
	prefix := mi.RepoForFile(absPath)
	if prefix == "" {
		return nil, ""
	}
	return mi.GetIndexer(prefix), prefix
}

// ResolveFilePath takes a repo-prefixed relative path (e.g. "ade/internal/foo.go")
// and returns the absolute filesystem path by looking up the repo's root directory.
// Returns empty string if the repo prefix is not found.
func (mi *MultiIndexer) ResolveFilePath(prefixedPath string) string {
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	// Longest matching prefix wins. With worktree instances two prefixes
	// can share a leading segment (`oas-orm` vs `oas-orm@task-ws`); map
	// iteration order is random, so a plain first-match could resolve a
	// `oas-orm@task-ws/...` path against the shorter `oas-orm` root. The
	// "/"-boundary check already keeps the two disjoint, but ranking by
	// length makes that robust regardless of any future prefix shapes.
	var bestPrefix, bestRoot string
	for prefix, meta := range mi.repos {
		if meta == nil {
			continue
		}
		if strings.HasPrefix(prefixedPath, prefix+"/") && len(prefix) > len(bestPrefix) {
			bestPrefix, bestRoot = prefix, meta.RootPath
		}
	}
	if bestPrefix == "" {
		// No known prefix leads this path. Graph paths are always prefixed,
		// so this is a caller-supplied repo-relative path — an agent naming
		// `internal/foo.go` without the prefix. With exactly one tracked
		// repo that is unambiguous, so anchor it there; with several it is
		// genuinely ambiguous and must fail closed.
		if meta := mi.loneRepoLocked(); meta != nil && meta.RootPath != "" {
			return filepath.Join(meta.RootPath, prefixedPath)
		}
		return ""
	}
	return filepath.Join(bestRoot, strings.TrimPrefix(prefixedPath, bestPrefix+"/"))
}

// RepoPrefixes returns the set of registered repo prefixes. The returned
// slice is a snapshot — safe to retain and iterate concurrently with
// other multi-indexer operations. Order is unspecified; callers that
// need stability should sort.
func (mi *MultiIndexer) RepoPrefixes() []string {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	prefixes := make([]string, 0, len(mi.repos))
	for prefix := range mi.repos {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// RepoRoot returns the local filesystem root for the given repo prefix.
// ok is true only when the prefix is registered AND meta.RootPath is non-empty.
// Caller is responsible for joining repo-relative file paths against the root.
//
// The empty prefix resolves to the lone registered repo when exactly one is
// tracked: single-repo mode indexes nodes without a repo prefix (see
// indexSingleRepo) while registering its metadata under the repo's real
// prefix, so every node the single-repo indexer mints carries RepoPrefix=""
// — refusing the empty prefix would orphan all of them. With two or more
// repos the empty prefix is ambiguous and stays a miss.
func (mi *MultiIndexer) RepoRoot(repoPrefix string) (string, bool) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	if repoPrefix == "" {
		if meta := mi.loneRepoLocked(); meta != nil && meta.RootPath != "" {
			return meta.RootPath, true
		}
		return "", false
	}
	meta, ok := mi.repos[repoPrefix]
	if !ok || meta == nil || meta.RootPath == "" {
		return "", false
	}
	return meta.RootPath, true
}

// loneRepoLocked returns the metadata of the only registered repo when
// exactly one repo is tracked, and nil otherwise. Caller must hold mi.mu.
//
// This is what makes an unqualified path resolve: with one tracked repo
// there is no ambiguity about which repo `internal/foo.go` means, so agents
// (and hooks, and the review pack) can keep naming files without first
// learning the prefix. With two or more repos the same path IS ambiguous and
// callers must fail closed rather than pick one.
//
// It used to additionally require that the repo had been indexed unprefixed,
// because a lone repo's nodes then carried raw relative paths and the
// provenance check kept stale unprefixed nodes from a departed repo from
// resolving against the wrong checkout. Every repo is prefixed now, so there
// is no second ID shape to disambiguate and the repo count is the whole
// question.
func (mi *MultiIndexer) loneRepoLocked() *RepoMetadata {
	if len(mi.repos) != 1 {
		return nil
	}
	for _, meta := range mi.repos {
		if meta != nil {
			return meta
		}
	}
	return nil
}

// LinkedWorktreeRoots returns the on-disk roots of every tracked linked
// git worktree that shares its .git common directory with the checkout
// at mainRepoPath — i.e. the worktree siblings of that main repo. The
// query is keyed on the resolved MainRepoPath so it matches whether the
// caller passes a main checkout or one of its worktrees.
//
// Used by the edit-tool file resolver: because all worktrees of one
// repo reuse a single index identity, a repo-relative path can resolve
// against a sibling checkout. When the resolved file belongs to a
// linked worktree, the resolver re-roots the write there so an edit
// lands in the worktree's copy rather than the main checkout's.
func (mi *MultiIndexer) LinkedWorktreeRoots(mainRepoPath string) []string {
	if mainRepoPath == "" {
		return nil
	}
	wantMain := resolvedMainRepo(mainRepoPath)

	mi.mu.RLock()
	defer mi.mu.RUnlock()
	var out []string
	for _, meta := range mi.repos {
		if meta == nil || meta.RootPath == "" || !meta.IsWorktree {
			continue
		}
		if resolvedMainRepo(meta.RootPath) == wantMain {
			out = append(out, meta.RootPath)
		}
	}
	return out
}

// resolvedMainRepo resolves a checkout path to its repo's main worktree
// root with symlinks evaluated. ResolveWorktree derives the main path
// two different ways — filepath.Abs for a main checkout vs git's
// canonicalized `commondir` for a linked worktree — and on platforms
// where the temp / home tree is a symlink (macOS /var -> /private/var)
// those two forms differ for the same repo. Evaluating symlinks on the
// result gives one stable identity that both inputs agree on.
func resolvedMainRepo(path string) string {
	main := ResolveWorktree(path).MainRepoPath
	if resolved, err := filepath.EvalSymlinks(main); err == nil {
		return resolved
	}
	return main
}

// MergedContractRegistry combines contract registries from all per-repo
// indexers into a single registry. In multi-repo mode each repo's indexer
// runs extractContracts independently; this merges the results.
func (mi *MultiIndexer) MergedContractRegistry() *contracts.Registry {
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	merged := contracts.NewRegistry()
	for repoPrefix, idx := range mi.indexers {
		cr := idx.ContractRegistry()
		if cr == nil {
			continue
		}
		// Re-stamp the workspace/project slugs from the indexer
		// alongside the repo prefix on merge. The contracts already
		// carry these slugs from
		// their source registry, but AddAllScoped is idempotent (skips
		// non-empty existing values) so this stays correct even if a
		// future code path forgets the stamp on first insert.
		merged.AddAllScoped(cr.All(), repoPrefix, idx.WorkspaceID(), idx.ProjectID())
	}
	return merged
}

// attachInlinedShapes folds the field shape of each contract's
// response_type / request_type into the contract's Meta so the
// dashboard can render the expanded field list. Targets contracts
// where the type has been resolved to a graph node ID (contains
// "::") AND the type node has a shape stored in its Meta.
//
// Type-shape extraction normally runs in commitContracts via
// snapshotContractShapes + inlineEnvelopeShapes — but those passes
// run during the initial extract and miss contracts added later by
// InlineWrappers. This is the post-inline equivalent: it doesn't
// re-extract shapes (the type nodes already have them from
// snapshotContractShapes if they were referenced anywhere), it just
// attaches them to the new contract entries.
func (mi *MultiIndexer) attachInlinedShapes(cr *contracts.Registry, g graph.Store) {
	idsToTouch := map[string]bool{}
	typeIDs := map[string]struct{}{}
	bareTypeNames := map[string]struct{}{}
	collectType := func(raw any) {
		value, _ := raw.(string)
		if value == "" {
			return
		}
		if strings.Contains(value, "::") {
			typeIDs[value] = struct{}{}
		} else {
			bareTypeNames[value] = struct{}{}
		}
	}
	for _, c := range cr.All() {
		if c.Meta == nil {
			continue
		}
		for _, key := range []string{"response_type", "request_type"} {
			if v, _ := c.Meta[key].(string); v != "" && strings.Contains(v, "::") {
				idsToTouch[c.ID] = true
			}
			collectType(c.Meta[key])
		}
		if env, ok := c.Meta["response_envelope"].([]map[string]any); ok && len(env) > 0 {
			// Touch any contract that has an envelope, even when
			// the rows still carry bare type names — the loop below
			// upgrades them. Otherwise we skip them and lose the
			// shape attachment for sibling-file types.
			idsToTouch[c.ID] = true
			for _, row := range env {
				collectType(row["type"])
			}
		}
	}
	fullIDs := make([]string, 0, len(typeIDs))
	for id := range typeIDs {
		fullIDs = append(fullIDs, id)
	}
	resolvedNodes := g.GetNodesByIDs(fullIDs)
	bareNames := make([]string, 0, len(bareTypeNames))
	for name := range bareTypeNames {
		bareNames = append(bareNames, name)
	}
	bareCandidates := g.FindNodesByNames(bareNames)
	srcCache := map[string][]byte{}
	shapeUpdates := map[string]*graph.Node{}
	resolveShape := func(typeID string) any {
		if typeID == "" || !strings.Contains(typeID, "::") {
			return nil
		}
		node := resolvedNodes[typeID]
		if node == nil {
			return nil
		}
		if node.Kind != graph.KindType && node.Kind != graph.KindInterface {
			return nil
		}
		if node.Meta == nil {
			node.Meta = map[string]any{}
		}
		if shape, ok := node.Meta["shape"]; ok && shape != nil {
			return shape
		}
		// Lazy-extract: snapshotContractShapes only walks types
		// referenced by the initial bulk extract. Types referenced
		// ONLY by wrapper-inlined contracts need this fallback or
		// their fields stay unread.
		src := srcCache[node.FilePath]
		if src == nil {
			data, ok := mi.readNodeSource(node)
			if !ok {
				srcCache[node.FilePath] = []byte{}
				return nil
			}
			src = data
			srcCache[node.FilePath] = src
		}
		if len(src) == 0 {
			return nil
		}
		extracted := contracts.ExtractShape(node.FilePath, src, node.StartLine, node.EndLine)
		if extracted == nil {
			return nil
		}
		node.Meta["shape"] = extracted
		shapeUpdates[node.ID] = node
		return extracted
	}
	for id := range idsToTouch {
		items := cr.ByID(id)
		changed := false
		for i := range items {
			if items[i].Meta == nil {
				continue
			}
			// Top-level request/response type shapes.
			for _, pair := range []struct{ typeKey, shapeKey string }{
				{"response_type", "response_shape"},
				{"request_type", "request_shape"},
			} {
				if _, has := items[i].Meta[pair.shapeKey]; has {
					continue
				}
				typeID, _ := items[i].Meta[pair.typeKey].(string)
				if shape := resolveShape(typeID); shape != nil {
					items[i].Meta[pair.shapeKey] = shape
					changed = true
				}
			}
			// Envelope rows — upgrade bare type names to graph IDs
			// (so the shape lookup hits) and attach shapes.
			if env, ok := items[i].Meta["response_envelope"].([]map[string]any); ok && len(env) > 0 {
				envChanged := false
				for ri, row := range env {
					typeID, _ := row["type"].(string)
					// Upgrade bare type name → graph ID when the
					// in-file resolveTypeInFile pass left it bare
					// (the type lives in a sibling file).
					if typeID != "" && !strings.Contains(typeID, "::") {
						matches := bareCandidates[typeID]
						var resolved string
						for _, n := range matches {
							if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
								continue
							}
							resolved = n.ID
							resolvedNodes[n.ID] = n
							if items[i].RepoPrefix != "" && strings.HasPrefix(n.ID, items[i].RepoPrefix+"/") {
								break // prefer same-repo
							}
						}
						if resolved != "" {
							env[ri]["type"] = resolved
							typeID = resolved
							envChanged = true
						}
					}
					if _, has := row["shape"]; has {
						continue
					}
					if shape := resolveShape(typeID); shape != nil {
						env[ri]["shape"] = shape
						envChanged = true
					}
				}
				if envChanged {
					items[i].Meta["response_envelope"] = env
					changed = true
				}
			}
		}
		if changed {
			cr.ReplaceByID(id, items)
		}
	}
	if len(shapeUpdates) > 0 {
		nodes := make([]*graph.Node, 0, len(shapeUpdates))
		for _, node := range shapeUpdates {
			nodes = append(nodes, node)
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
		g.AddBatch(nodes, nil)
	}
}

// readNodeSource returns the source bytes of the file the node lives
// in, resolving the repo prefix to a real disk path via tracked-repo
// metadata. Mirrors wrapperSourceReader's path-resolution dance.
func (mi *MultiIndexer) readNodeSource(node *graph.Node) ([]byte, bool) {
	if node == nil || node.FilePath == "" {
		return nil, false
	}
	rel := node.FilePath
	if node.RepoPrefix != "" {
		meta := mi.GetMetadata(node.RepoPrefix)
		if meta == nil || meta.RootPath == "" {
			return nil, false
		}
		rel = strings.TrimPrefix(rel, node.RepoPrefix+"/")
		data, err := os.ReadFile(filepath.Join(meta.RootPath, rel))
		if err != nil {
			return nil, false
		}
		return data, true
	}
	for _, m := range mi.AllMetadata() {
		data, err := os.ReadFile(filepath.Join(m.RootPath, rel))
		if err == nil {
			return data, true
		}
	}
	return nil, false
}

// wrapperSourceReader returns a SourceReader closure that maps a graph
// node back to its on-disk bytes by joining the node's repo-relative
// FilePath with the repo's RootPath from MultiIndexer metadata. In
// single-repo mode (no RepoPrefix on nodes) the indexer's sole root is
// used. Read results are memoized inside the closure so multi-caller
// wrappers don't trigger N disk reads per file.
func (mi *MultiIndexer) wrapperSourceReader() contracts.SourceReader {
	cache := make(map[string][]byte)
	miss := make(map[string]struct{})

	readFile := func(absPath string) ([]byte, bool) {
		if b, ok := cache[absPath]; ok {
			return b, true
		}
		if _, skipped := miss[absPath]; skipped {
			return nil, false
		}
		b, err := os.ReadFile(absPath)
		if err != nil {
			miss[absPath] = struct{}{}
			return nil, false
		}
		cache[absPath] = b
		return b, true
	}

	return func(n *graph.Node) ([]byte, bool) {
		if n == nil || n.FilePath == "" {
			return nil, false
		}
		// Multi-repo case: strip "<repo-prefix>/" from the FilePath and
		// join with the repo's recorded root.
		if n.RepoPrefix != "" {
			meta := mi.GetMetadata(n.RepoPrefix)
			if meta == nil || meta.RootPath == "" {
				return nil, false
			}
			rel := strings.TrimPrefix(n.FilePath, n.RepoPrefix+"/")
			return readFile(filepath.Join(meta.RootPath, rel))
		}
		// Single-repo fallback: a node without a RepoPrefix carries its
		// path relative to the sole indexer root. Try each known repo
		// root since the single-repo path wraps through indexSingleRepo.
		for _, meta := range mi.AllMetadata() {
			if b, ok := readFile(filepath.Join(meta.RootPath, n.FilePath)); ok {
				return b, true
			}
		}
		// Last resort: treat FilePath as already absolute (tests).
		return readFile(n.FilePath)
	}
}

// ReconcileContractEdges walks the merged contract registry, runs the
// consumer↔provider matcher, and writes the results into the graph as
// EdgeMatches edges pointing from consumer-contract nodes to their matched
// provider-contract nodes. Every call evicts the prior set of EdgeMatches
// first so the edges stay in sync with the current contract view; that's
// correct after track / untrack / re-index / watcher re-scan. Returns the
// number of match edges added so callers can log or test the effect.
//
// This is what makes get_call_chain traverse service boundaries: without a
// persisted contract→contract edge, the matcher's result is only visible
// via the `contracts check` tool and traversals stop at each service's
// boundary.
func (mi *MultiIndexer) ReconcileContractEdges() int {
	// Serialise the whole pass: the evict-then-mint of EdgeMatches, topic
	// edges, and the bridge subgraph spans many non-atomic store writes,
	// and several goroutines call this concurrently (see reconcileMu).
	mi.reconcileMu.Lock()
	defer mi.reconcileMu.Unlock()

	g := mi.Graph()
	if g == nil {
		return 0
	}

	// Replace the three derived edge generations with one backend delete. The
	// old collect+RemoveEdge loops opened one SQLite mutation per relationship.
	graph.EvictEdgesByKinds(g, []graph.EdgeKind{
		graph.EdgeMatches,
		graph.EdgeProducesTopic,
		graph.EdgeConsumesTopic,
	})

	merged := mi.MergedContractRegistry()
	if merged == nil {
		return 0
	}

	// Inline HTTP wrapper callers (T2.4). Codebases that route every
	// endpoint through a helper like `request(path, ...)` produce one
	// parametric consumer contract per wrapper at extraction time —
	// useless for matching. InlineWrappers walks incoming call edges
	// of each wrapper, re-reads the caller's source, and emits a
	// specific consumer contract per literal path. The returned
	// contracts are also persisted into their owning repo's per-repo
	// registry so subsequent `contracts list`/`check` calls see them
	// (MergedContractRegistry rebuilds fresh each call and would
	// otherwise lose them).
	inlined := contracts.InlineWrappers(merged, g, mi.wrapperSourceReader())
	if len(inlined) > 0 {
		mi.mu.RLock()
		for _, c := range inlined {
			if c.RepoPrefix == "" {
				continue
			}
			idx, ok := mi.indexers[c.RepoPrefix]
			if !ok {
				continue
			}
			cr := idx.ContractRegistry()
			if cr == nil {
				continue
			}
			// Skip if the same contract is already persisted —
			// ReconcileContractEdges runs on every repo change, and
			// appending the same inlined contract on every pass would
			// blow up the registry with duplicates. Compare on the
			// Registry.All() dedupe key.
			alreadyPersisted := false
			for _, existing := range cr.ByID(c.ID) {
				if existing.SymbolID == c.SymbolID &&
					existing.FilePath == c.FilePath &&
					existing.Role == c.Role {
					alreadyPersisted = true
					break
				}
			}
			if !alreadyPersisted {
				cr.Add(c)
			}
		}
		mi.mu.RUnlock()

		// Wrapper-inlined contracts arrive AFTER commitContracts ran
		// its UpgradeBareTypeRefs pass, so their response_type /
		// request_type still carries bare names like "ToolInfo".
		// Re-run the upgrade against the merged graph so downstream
		// snapshotContractShapes finds the type node and the
		// dashboard sees fields instead of a string.
		mi.mu.RLock()
		registries := make([]*contracts.Registry, 0, len(mi.indexers))
		bareTypeNames := map[string]struct{}{}
		for _, idx := range mi.indexers {
			cr := idx.ContractRegistry()
			if cr == nil {
				continue
			}
			registries = append(registries, cr)
			for _, contract := range cr.All() {
				if contract.Meta == nil {
					continue
				}
				for _, key := range []string{"request_type", "response_type"} {
					name, _ := contract.Meta[key].(string)
					if name != "" && !strings.Contains(name, "::") {
						bareTypeNames[name] = struct{}{}
					}
				}
			}
		}
		names := make([]string, 0, len(bareTypeNames))
		for name := range bareTypeNames {
			names = append(names, name)
		}
		matchesByName := mi.graph.FindNodesByNames(names)
		lookup := func(name, repoHint string) []string {
			matches := matchesByName[name]
			if len(matches) == 0 {
				return nil
			}
			ids := make([]string, 0, len(matches))
			for _, n := range matches {
				if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
					continue
				}
				ids = append(ids, n.ID)
			}
			// Prefer same-repo when multiple match.
			if len(ids) > 1 && repoHint != "" {
				var sameRepo []string
				for _, id := range ids {
					if strings.HasPrefix(id, repoHint+"/") {
						sameRepo = append(sameRepo, id)
					}
				}
				if len(sameRepo) > 0 {
					return sameRepo
				}
			}
			return ids
		}
		for _, cr := range registries {
			cr.UpgradeBareTypeRefs(lookup)
		}
		// UpgradeBareTypeRefs leaves names with ≥2 candidates alone
		// (e.g. a TS app declaring `DashboardSnapshot` in both
		// `lib/schema.ts` and `lib/types.ts`). disambiguateBareTypesViaImports
		// re-reads the consumer's source, parses its `import` lines,
		// and picks the candidate whose graph FilePath matches an
		// imported module. Runs before attachInlinedShapes so the
		// shape attachment sees fully-qualified IDs.
		mi.disambiguateBareTypesViaImportsBatch(registries, mi.graph)
		// Now that response_type / request_type point at real graph
		// nodes, fold each referenced type's shape (struct fields)
		// into the contract's Meta so the dashboard renders the
		// expanded field list instead of just the type name. Mirrors
		// what snapshotContractShapes + inlineEnvelopeShapes do for
		// initially-extracted contracts.
		for _, cr := range registries {
			mi.attachInlinedShapes(cr, mi.graph)
		}
		mi.mu.RUnlock()
	}

	// Bind provider-contract SymbolIDs that came from spec files
	// (.proto for gRPC, OpenAPI YAML/JSON for HTTP). Without this
	// step the matcher finds pairs but the bridge-emission check
	// below skips them because provider SymbolID is empty. Must run
	// before Match so Match sees the updated records.
	contracts.BindProviderSymbols(merged, g)

	result := contracts.Match(merged)
	added := 0
	// Track which topic nodes we've already materialised so the loop
	// emits one node per (broker, topic) bucket even when a topic has
	// fan-out across many consumers. The dedupe key is the topic
	// node's ID — its repo-prefix is already encoded in Contract.ID.
	topicNodes := make(map[string]struct{})
	var reconcileNodes []*graph.Node
	var reconcileEdges []*graph.Edge
	for _, m := range result.Matched {
		// Connect the consumer's enclosing symbol directly to the
		// provider's enclosing symbol. Contract nodes in the graph are
		// deduped by Contract.ID, so a provider and a consumer that share
		// the same ID collapse into one node — a contract→contract edge
		// would be a self-loop. Symbol→symbol bypasses that and gives
		// get_call_chain the traversal it needs: saveTuck (extension) →
		// Handler.CreateTuck (core-api).
		if m.Provider.SymbolID == "" || m.Consumer.SymbolID == "" {
			continue
		}
		if m.Provider.SymbolID == m.Consumer.SymbolID {
			continue
		}
		reconcileEdges = append(reconcileEdges, &graph.Edge{
			From:            m.Consumer.SymbolID,
			To:              m.Provider.SymbolID,
			Kind:            graph.EdgeMatches,
			FilePath:        m.Consumer.FilePath,
			Line:            m.Consumer.Line,
			Confidence:      1.0,
			ConfidenceLabel: "EXTRACTED",
			Origin:          graph.OriginASTResolved,
			CrossRepo:       m.CrossRepo,
		})
		added++

		// Materialise the broker topic node + producer/consumer
		// edges. Only fires for ContractTopic matches; HTTP / gRPC /
		// WS / GraphQL / env contracts fall through with just the
		// EdgeMatches edge above.
		if m.Provider.Type == contracts.ContractTopic {
			appendTopicEdges(m, topicNodes, &reconcileNodes, &reconcileEdges)
		}
	}
	if len(reconcileNodes) > 0 || len(reconcileEdges) > 0 {
		g.AddBatch(reconcileNodes, reconcileEdges)
	}

	// Persist the matched contract groups as the bridge subgraph: one
	// KindContractBridge node per group plus EdgeBridges fan-out to
	// the participating contract nodes. The pass evicts the previous
	// bridge generation internally, so it stays idempotent across
	// reconciles and drops bridges whose contracts disappeared.
	MaterializeContractBridges(g, result.Matched)

	// Topic nodes whose producer and consumer edges all evaporated
	// since the previous reconcile remain in the graph as leaf
	// nodes — Graph has no public RemoveNode and the next reconcile
	// upserts them anyway. A topic that lost every callsite is an
	// invisible-but-harmless cost (a single KindTopic node with no
	// neighbors). Worth revisiting if topic churn ever bloats the
	// graph; in the meantime we lean on EvictRepo / EvictFile to
	// reclaim memory when a whole repo's topic vocabulary changes.
	return added
}

// appendTopicEdges stages one topic match into the reconciliation generation.
// The caller publishes all nodes and edges with one AddBatch after matching,
// avoiding one node write plus two edge transactions per match on SQLite.
func appendTopicEdges(
	m contracts.CrossLink,
	topicNodes map[string]struct{},
	nodes *[]*graph.Node,
	edges *[]*graph.Edge,
) {
	// Trust the matcher to bucket only same-broker contracts together
	// because Contract.ID already includes the broker token; if the
	// broker isn't on the provider Meta, fall through to the contract
	// ID's middle segment so the node carries something useful.
	broker, _ := m.Provider.Meta["broker"].(string)
	topicName, _ := m.Provider.Meta["topic"].(string)
	if broker == "" || topicName == "" {
		// Defensive fallback — extract from the Contract.ID shape
		// `topic::<broker>::<name>`. If the ID isn't structured we
		// skip rather than emit a node with an unidentifiable broker
		// (such an edge would be misleading in cross-broker queries).
		broker2, name2, ok := parseTopicContractID(m.Provider.ID)
		if !ok {
			return
		}
		if broker == "" {
			broker = broker2
		}
		if topicName == "" {
			topicName = name2
		}
	}

	topicID := m.Provider.ID // canonical: topic::<broker>::<name>
	if _, ok := topicNodes[topicID]; !ok {
		topicNodes[topicID] = struct{}{}
		// Preserve any existing node (a prior reconcile may have
		// created it) but always refresh Meta so a broker rename
		// isn't sticky across reconciles. AddNode in this codebase
		// is upsert-style — see graph.Graph.AddNode.
		*nodes = append(*nodes, &graph.Node{
			ID:          topicID,
			Kind:        graph.KindTopic,
			Name:        topicName,
			FilePath:    m.Provider.FilePath,
			Language:    "contract",
			RepoPrefix:  m.Provider.RepoPrefix,
			WorkspaceID: m.Provider.EffectiveWorkspace(),
			ProjectID:   m.Provider.EffectiveProject(),
			Meta: map[string]any{
				"broker": broker,
				"name":   topicName,
			},
		})
	}

	*edges = append(*edges, &graph.Edge{
		From:            m.Provider.SymbolID,
		To:              topicID,
		Kind:            graph.EdgeProducesTopic,
		FilePath:        m.Provider.FilePath,
		Line:            m.Provider.Line,
		Confidence:      1.0,
		ConfidenceLabel: "EXTRACTED",
		Origin:          graph.OriginASTResolved,
		CrossRepo:       false,
		Meta: map[string]any{
			"broker": broker,
		},
	})
	*edges = append(*edges, &graph.Edge{
		From:            m.Consumer.SymbolID,
		To:              topicID,
		Kind:            graph.EdgeConsumesTopic,
		FilePath:        m.Consumer.FilePath,
		Line:            m.Consumer.Line,
		Confidence:      1.0,
		ConfidenceLabel: "EXTRACTED",
		Origin:          graph.OriginASTResolved,
		CrossRepo:       m.CrossRepo,
		Meta: map[string]any{
			"broker": broker,
		},
	})
}

// parseTopicContractID splits a Contract.ID of the form
// `topic::<broker>::<name>` (or its repo-prefixed counterpart
// `<repo>/topic::<broker>::<name>`) into broker + name. Returns
// ok==false for any other shape so callers can skip ill-formed
// topic contracts rather than fabricate a broker label.
func parseTopicContractID(id string) (broker, name string, ok bool) {
	// Strip any leading repo-prefix segment ("repo/topic::..."). The
	// applyRepoPrefix step prepends `<repo>/` to synthetic IDs of
	// the form `topic::...`, so we look for the inner `topic::`
	// marker rather than splitting on the leading slash.
	idx := strings.Index(id, "topic::")
	if idx < 0 {
		return "", "", false
	}
	rest := id[idx+len("topic::"):]
	sep := strings.Index(rest, "::")
	if sep <= 0 || sep == len(rest)-2 {
		return "", "", false
	}
	return rest[:sep], rest[sep+2:], true
}

// Graph returns the underlying shared graph.
func (mi *MultiIndexer) Graph() graph.Store {
	return mi.graph
}

// SetRemoteStitch enables cross-daemon proxy-edge minting on every
// CrossRepoResolver this MultiIndexer constructs. Called once by the
// daemon entry point when federation.edges.enabled; a nil prober leaves
// the resolvers on read-only federation.
func (mi *MultiIndexer) SetRemoteStitch(prober resolver.RemoteDeclarationProber, budget int) {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.stitchProber = prober
	mi.proxyBudget = budget
}

// applyRemoteStitch wires the proxy-edge mint into a freshly built resolver
// when a prober is installed.
func (mi *MultiIndexer) applyRemoteStitch(cr *resolver.CrossRepoResolver) {
	mi.mu.RLock()
	prober, budget := mi.stitchProber, mi.proxyBudget
	mi.mu.RUnlock()
	if prober != nil {
		cr.EnableRemoteStitch(prober, budget)
	}
}

// Search returns the shared search backend.
func (mi *MultiIndexer) Search() search.Backend {
	return mi.search
}

// ExportVectorIndex serializes the workspace-global semantic-search
// vector index — there is one shared HNSW index across every tracked
// repo, not one per repo. Returns nil, 0, 0 when no vector index is
// active (embeddings disabled, or the backend is still text-only).
// Used by the daemon snapshot path so a default-on daemon does not
// re-embed the whole graph on every restart.
func (mi *MultiIndexer) ExportVectorIndex() ([]byte, int, int) {
	sw, ok := mi.search.(*search.Swappable)
	if !ok {
		return nil, 0, 0
	}
	hybrid, ok := sw.Inner().(*search.HybridBackend)
	if !ok {
		return nil, 0, 0
	}
	vec := hybrid.VectorIndex()
	if vec == nil || vec.Count() == 0 {
		return nil, 0, 0
	}
	var buf bytes.Buffer
	if err := vec.Save(&buf); err != nil {
		mi.logger.Warn("failed to export vector index", zap.Error(err))
		return nil, 0, 0
	}
	return buf.Bytes(), vec.Dims(), vec.Count()
}

// ImportVectorIndex restores a previously-exported vector index into
// the shared search backend, wrapping the current text backend in a
// HybridBackend. It is a no-op when embeddings are disabled (no
// configured embedder) or when the cached index's dimensionality does
// not match the active embedder — a provider switch (GloVe 50d → ONNX
// 384d) makes the cached vectors meaningless, so the indexer re-embeds
// instead. Returns an error only on a structurally corrupt index blob.
func (mi *MultiIndexer) ImportVectorIndex(data []byte, dims, count int) error {
	if len(data) == 0 || mi.embedder == nil {
		return nil
	}
	if embedderDims := mi.embedder.Dimensions(); embedderDims > 0 && embedderDims != dims {
		mi.logger.Info("vector index dims mismatch, will re-embed",
			zap.Int("cached_dims", dims), zap.Int("embedder_dims", embedderDims))
		return nil
	}
	sw, ok := mi.search.(*search.Swappable)
	if !ok {
		return nil
	}
	vec := search.NewVector(dims)
	if err := vec.LoadFrom(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("import vector index: %w", err)
	}
	vec.SetCount(count)

	// Unwrap an existing HybridBackend to its text side before
	// re-wrapping so we never nest Hybrids (each retains a stale
	// vector index — see buildSearchIndex for the memory rationale).
	inner := sw.Inner()
	if hyb, ok := inner.(*search.HybridBackend); ok {
		inner = hyb.TextBackend()
	}
	sw.Swap(search.NewHybrid(inner, vec, mi.embedder))
	mi.logger.Info("restored vector index from snapshot",
		zap.Int("vectors", count), zap.Int("dims", dims))
	return nil
}

// AutoDetectRepos walks immediate subdirectories of parentPath looking for
// .git directories. If parentPath itself is a Git repo, it returns a single
// entry (the caller should index it as single-repo). If zero Git repos are
// found, it returns nil so the caller can fall back to single-repo mode.
// This is gated by the workspace.auto_detect config flag.
func (mi *MultiIndexer) AutoDetectRepos(parentPath string) []config.RepoEntry {
	absPath, err := filepath.Abs(parentPath)
	if err != nil {
		mi.logger.Warn("auto-detect: failed to resolve path", zap.String("path", parentPath), zap.Error(err))
		return nil
	}

	// If the path itself is a Git repo, return it as a single repo.
	if isGitRepo(absPath) {
		return []config.RepoEntry{{
			Path: absPath,
			Name: filepath.Base(absPath),
		}}
	}

	// Walk immediate subdirectories (not recursive) for .git dirs.
	entries, err := os.ReadDir(absPath)
	if err != nil {
		mi.logger.Warn("auto-detect: failed to read directory", zap.String("path", absPath), zap.Error(err))
		return nil
	}

	var repos []config.RepoEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subDir := filepath.Join(absPath, entry.Name())
		if isGitRepo(subDir) {
			repos = append(repos, config.RepoEntry{
				Path: subDir,
				Name: entry.Name(), // Derive RepoPrefix from subdirectory name.
			})
		}
	}

	// If zero Git repos found, return nil — caller falls back to single-repo.
	if len(repos) == 0 {
		return nil
	}

	return repos
}

// isGitRepo checks whether the given directory contains a .git subdirectory.
func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}
