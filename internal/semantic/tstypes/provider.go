package tstypes

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider is the semantic.Provider over one LangSpec. Pure in-process
// — Available is unconditionally true, no subprocess is ever spawned,
// Close is a no-op. It is supplemental: it augments whichever provider
// wins the per-language arbitration (LSP / SCIP) instead of competing
// with it, and only ever stamps AST-grade provenance, so a
// compiler-grade pass running before or after never gets downgraded.
type Provider struct {
	spec         *LangSpec
	logger       *zap.Logger
	observePage  func(factPageStats) // synchronous test/diagnostic hook
	observeSpool func(string)        // synchronous cleanup test hook
}

var (
	_ semantic.ContextEnricher = (*Provider)(nil)
	// The batch faces are what keep a file frontier a file frontier. Without
	// them Manager.EnrichFilesContext's dispatch ladder falls through to
	// EnrichRepoContext, which re-collects every file of these languages in the
	// repository and discards the frontier entirely.
	_ semantic.ContextFileBatchEnricher = (*Provider)(nil)
	_ semantic.FileBatchEnricher        = (*Provider)(nil)
)

// NewProvider wraps a LangSpec as a semantic provider.
func NewProvider(spec *LangSpec, logger *zap.Logger) *Provider {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Provider{spec: spec, logger: logger}
}

// DefaultProviders returns the in-process type resolvers for every
// supported language. Registered unconditionally at daemon boot —
// disable one via a `semantic.providers` config entry with
// `enabled: false` under its name.
func DefaultProviders(logger *zap.Logger) []*Provider {
	return []*Provider{
		NewProvider(JavaSpec(), logger),
		NewProvider(PythonSpec(), logger),
		NewProvider(RubySpec(), logger),
		NewProvider(RustSpec(), logger),
		NewProvider(TypeScriptSpec(), logger),
		NewProvider(CSharpSpec(), logger),
		NewProvider(PHPSpec(), logger),
		NewProvider(KotlinSpec(), logger),
		NewProvider(GoSpec(), logger),
	}
}

func (p *Provider) Name() string        { return p.spec.ProviderName }
func (p *Provider) Languages() []string { return p.spec.Languages }
func (p *Provider) Available() bool     { return true }
func (p *Provider) Close() error        { return nil }

// Supplemental marks this provider as augmenting (see
// semantic.SupplementalProvider): the manager runs it for its
// languages in addition to the arbitration winner.
func (p *Provider) Supplemental() bool { return true }

// Enrich runs the full-repo pass for a single-repo (un-prefixed) graph.
// It delegates to EnrichRepo with an empty prefix — the in-memory single
// repo case where every real node carries RepoPrefix "".
func (p *Provider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepoContext(context.Background(), g, "", repoRoot, nil)
}

// EnrichRepo runs the full-repo pass: parse every file of the provider's
// languages that belong to repoPrefix under repoRoot in a bounded worker
// pool, then apply the per-file facts to the graph from a single
// goroutine. repoPrefix scopes file selection so a multi-repo graph with
// a colliding relative path never reads the wrong repo's bytes.
func (p *Provider) EnrichRepo(g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepoContext(context.Background(), g, repoPrefix, repoRoot, nil)
}

func (p *Provider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	start := time.Now()
	res := p.newResult()
	if ctx == nil {
		ctx = context.Background()
	}
	if p.suppressed() {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	return p.runPass(ctx, g, repoPrefix, languageFiles(g, p.spec, repoPrefix, repoRoot),
		deadline, repoWideSymbolTotal, res, start)
}

// EnrichFiles is the un-contexted semantic.FileBatchEnricher face of
// EnrichFilesContext, kept because the Manager's ladder tries
// ContextFileBatchEnricher first and FileBatchEnricher second, and a caller
// holding no context must still reach the frontier path rather than fall
// through to the whole-repo one. With a Background root the pass is unbounded
// — the same meaning EnrichRepo gives a nil EnrichDeadlinePolicy.
func (p *Provider) EnrichFiles(g graph.Store, repoPrefix, repoRoot string, filePaths []string) (*semantic.EnrichResult, error) {
	return p.EnrichFilesContext(context.Background(), g, repoPrefix, repoRoot, filePaths)
}

// EnrichFilesContext runs the pass over an explicit file frontier instead of
// the whole repository — the semantic.ContextFileBatchEnricher entry
// Manager.EnrichFilesContext's dispatch ladder prefers over every other rung.
//
// Without it that ladder fell through to EnrichRepoContext, which re-collects
// languageFiles for the WHOLE repository and never looks at the frontier at
// all. Measured on a 9.8k-file Python workspace: a worktree-copy repair over 82
// files ran the full python-types pass — 488 s alone, 89 min under contention
// — and under the watcher's 120 s save deadline every such pass was cut
// Partial having stamped nothing.
//
// The selection is the intersection of languageFiles (which applies the repo
// membership, language, low-value and on-disk gates the whole-repo pass
// applies) with the caller's normalised frontier. An empty intersection is a
// complete, non-Partial, empty result: nothing of this provider's languages was
// in the frontier, which is an answer rather than a failure. It is never a
// fallback to the whole repository — that fallback is the defect this entry
// point exists to remove.
//
// Coverage is measured against the FRONTIER's candidates, not the repository's:
// a pass reports the fraction of what it was asked to cover. Reusing the
// whole-repo denominator would make a complete 2-file pass on a 9.8k-file repo
// read as 0.02% coverage, which is indistinguishable from a pass that did
// nothing.
//
// The deadline is ctx and only ctx. The Manager already wraps this call
// (EnrichFilesWithDeadline sizes the bound: the configured save timeout, or a
// copy repair's CopyRepairDeadline), so no EnrichDeadlinePolicy is derived
// here and no phase narrows the caller's bound further. A ctx that expires
// mid-pass surfaces as Partial with AbortReason set and BoundReason
// EnrichBoundBudget, exactly as on the whole-repo path — which is what
// Indexer.runScopedDeferredEnrich keys its copy-repair escalation on.
//
// The apply phase mutates only nodes owned by the staged files (every fact page
// is anchored at one file's own index) and performs no repo-wide eviction, so a
// frontier pass cannot drop edges belonging to files outside it.
func (p *Provider) EnrichFilesContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, filePaths []string) (*semantic.EnrichResult, error) {
	start := time.Now()
	res := p.newResult()
	if ctx == nil {
		ctx = context.Background()
	}
	if p.suppressed() {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	files := frontierFiles(g, p.spec, repoPrefix, repoRoot, filePaths)
	return p.runPass(ctx, g, repoPrefix, files, nil,
		p.countSymbolsInFiles(g, repoPrefix, files), res, start)
}

// newResult is the empty result every entry point starts from.
func (p *Provider) newResult() *semantic.EnrichResult {
	return &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.spec.Languages[0],
	}
}

// suppressed is the toolchain-fallback gate: a spec that suppresses itself on
// this host (its authoritative compiler-grade provider is available)
// contributes nothing — no parse, no graph mutation — so it can never alter,
// duplicate or downgrade that provider's resolutions.
func (p *Provider) suppressed() bool {
	return p.spec.Suppressed != nil && p.spec.Suppressed()
}

// repoWideSymbolTotal is the symbolsTotal runPass reads as "count the whole
// repository's symbols as the coverage denominator". A file-scoped pass passes
// its own pre-counted frontier total instead.
const repoWideSymbolTotal = -1

// runPass is the pipeline both entry points share: stage the selected files'
// facts off the graph, then apply them under the graph-wide resolve mutex. The
// ONLY thing that differs between a whole-repo pass and a frontier pass is the
// file selection and the coverage denominator — the deadline discipline, the
// lock discipline, the yield discipline and the partial semantics are one
// implementation on purpose, so a frontier pass cannot drift into a second set
// of rules.
func (p *Provider) runPass(ctx context.Context, g graph.Store, repoPrefix string, files []fileRef, deadline semantic.EnrichDeadlinePolicy, symbolsTotal int, res *semantic.EnrichResult, start time.Time) (*semantic.EnrichResult, error) {
	// The budget is charged phase by phase against a movable deadline instead
	// of arming one context up front: the wait for the graph-wide resolve
	// mutex below is ANOTHER pass's work, and letting it burn this pass's
	// budget turned a queued small repo into a partial with zero coverage
	// (observed: a 641s "pass" that was ~630s of queueing behind a Go
	// compiler apply). A parent context's own deadline still caps every
	// phase — WithDeadline never extends the parent.
	var deadlineAt time.Time
	res.FileCount = len(files)
	if deadline != nil {
		if d := deadline(len(files)); d > 0 {
			deadlineAt = start.Add(d)
			res.BudgetSeconds = d.Seconds()
		}
	}
	phaseCtx := func() (context.Context, context.CancelFunc) {
		if deadlineAt.IsZero() {
			return ctx, func() {}
		}
		return context.WithDeadline(ctx, deadlineAt)
	}
	if err := ctx.Err(); err != nil {
		markContextPartial(res, err)
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	if len(files) > 0 {
		spool, err := newFactSpool()
		if err != nil {
			return nil, err
		}
		defer spool.close()
		if p.observeSpool != nil {
			p.observeSpool(spool.path)
		}
		parseCtx, cancelParse := phaseCtx()
		stageStart := time.Now()
		if err := p.stageRepoFacts(parseCtx, files, spool); err != nil {
			res.StagingMs = time.Since(stageStart).Milliseconds()
			parseErr := parseCtx.Err()
			cancelParse()
			if parseErr != nil {
				markContextPartial(res, parseErr)
				res.DurationMs = time.Since(start).Milliseconds()
				return res, nil
			}
			return nil, err
		}
		res.StagingMs = time.Since(stageStart).Milliseconds()
		cancelParse()
		// Parsing above is pure and fans out across workers; the apply
		// phase mutates the shared graph (retargets edges, reindexes,
		// stamps provenance) and MUST run under the graph-wide resolve
		// mutex so it serialises against concurrent resolver / cross-repo
		// passes — the same lock every other edge-mutating pass holds.
		mu := g.ResolveMutex()
		lockStart := time.Now()
		// Warmup apply barrier first (parks while the pool overlaps the
		// resolve phase), then the mutex. Both waits are other passes' work,
		// so both are excluded from this pass's budget below.
		if err := semantic.ApplyGateWait(ctx); err != nil {
			markContextPartial(res, err)
			res.DurationMs = time.Since(start).Milliseconds()
			return res, nil
		}
		mu.Lock()
		lockWait := time.Since(lockStart)
		res.LockWaitMs = lockWait.Milliseconds()
		if lockWait > 5*time.Second {
			// The completion log's lock_wait_ms explains a long pass only
			// after the fact; announce the wait as it resolves so a watcher
			// can attribute the quiet stretch in real time.
			p.logger.Info("tstypes: apply began after queueing",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Duration("queued", lockWait))
		}
		if !deadlineAt.IsZero() {
			deadlineAt = deadlineAt.Add(lockWait)
		}
		// The apply's bound is a PAUSABLE budget rather than a context
		// deadline. Its page-boundary yield admits another pass on purpose,
		// and charging that pass's work here is the same mistake the lockWait
		// refund above exists to prevent — through a door that opens
		// mid-apply, where a context.WithDeadline can no longer be moved. See
		// applyBudget.
		applyCtx, cancelApply := context.WithCancelCause(ctx)
		budget := newApplyBudget(time.Until(deadlineAt), func() {
			cancelApply(context.DeadlineExceeded)
		})
		if err := applyContextErr(applyCtx); err != nil {
			mu.Unlock()
			budget.stop()
			cancelApply(nil)
			markContextPartial(res, err)
			res.DurationMs = time.Since(start).Milliseconds()
			return res, nil
		}
		applyStart := time.Now()
		applyErr := p.applyStagedFacts(applyCtx, g, repoPrefix, spool, res, symbolsTotal, func() bool {
			budget.pause()
			mutated, _ := yieldResolveMutex(g, mu)
			budget.resume()
			return mutated
		})
		res.ApplyMs = time.Since(applyStart).Milliseconds()
		mu.Unlock()
		budget.stop()
		applyCtxErr := applyContextErr(applyCtx)
		cancelApply(nil)
		if refund := budget.refunded(); refund >= applyRefundLogThreshold {
			// The mirror of "apply began after queueing": that line reports the
			// wait before the pass, this one the waits inside it. Without it a
			// pass that finished only because of the refund reads as one that
			// was simply fast enough.
			p.logger.Info("tstypes: apply budget refunded for time queued behind another pass",
				zap.String("provider", p.Name()),
				zap.String("repo_prefix", repoPrefix),
				zap.Duration("refunded", refund))
		}
		if applyErr != nil {
			if applyCtxErr != nil {
				markContextPartial(res, applyCtxErr)
				res.DurationMs = time.Since(start).Milliseconds()
				return res, nil
			}
			return nil, applyErr
		}
	}
	if err := ctx.Err(); err != nil {
		markContextPartial(res, err)
	}

	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

func markContextPartial(res *semantic.EnrichResult, err error) {
	res.Partial = true
	res.AbortReason = err.Error()
	res.BoundReason = semantic.EnrichBoundBudget
}

// EnrichFile runs the single-file incremental pass. filePath is the graph
// file key (prefixed in multi-repo mode), which is globally unique — the
// file's own node names the repo, so this is inherently scoped to the
// right repo without a separate prefix argument.
func (p *Provider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	start := time.Now()
	res := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.spec.Languages[0],
	}
	// Toolchain-fallback gate (see EnrichRepo): a suppressed spec is a no-op.
	if p.spec.Suppressed != nil && p.spec.Suppressed() {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	// Find the file's own node by its exact graph key. It carries the
	// RepoPrefix that maps the prefixed path back to the on-disk file.
	var fileNode *graph.Node
	for _, n := range g.GetFileNodes(filePath) {
		if n.Kind == graph.KindFile {
			fileNode = n
			break
		}
	}
	if fileNode == nil || !p.spec.handles(fileNode.Language) {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	ref, ok := fileRefFor(fileNode, repoRoot)
	if !ok {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	facts, err := analyzeFile(p.spec, ref)
	if err != nil {
		return nil, err
	}
	if facts != nil {
		// Same contract as the full pass: the apply phase mutates the
		// shared graph and runs under the resolve mutex so it does not
		// race a concurrent watcher / resolver pass on another file.
		mu := g.ResolveMutex()
		mu.Lock()
		ap := newApplier(g, p.spec, p.Name()).withLogger(p.logger)
		ap.applyAll([]*fileFacts{facts}, res)
		ap.flush()
		p.countCoverage(g, fileNode.RepoPrefix, map[string]bool{facts.file: true}, res)
		mu.Unlock()
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// countCoverage fills the symbols-covered counters: total is every
// symbol of the provider's languages, covered is the subset living in
// files the pass analyzed.
func (p *Provider) countCoverage(g graph.Store, repoPrefix string, analyzed map[string]bool, res *semantic.EnrichResult) {
	langs := make(map[string]bool, len(p.spec.Languages))
	for _, l := range p.spec.Languages {
		langs[l] = true
	}
	// Indexed exact-prefix scan; empty prefix is the embedded single-repository
	// namespace rather than a graph-wide wildcard.
	nodes := g.GetRepoNodes(repoPrefix)
	for _, n := range nodes {
		if n.RepoPrefix != repoPrefix || !langs[n.Language] || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		res.SymbolsTotal++
		if analyzed[n.FilePath] {
			res.SymbolsCovered++
		}
	}
	if res.SymbolsTotal > 0 {
		res.CoveragePercent = float64(res.SymbolsCovered) / float64(res.SymbolsTotal) * 100
	}
}
