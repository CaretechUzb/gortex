package indexer

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

// A file-scoped enrichment pass is real work, and until it stamped the content
// counter the system had no way to say so. The whole-repo pass records what it
// covered; the scoped pass did not, so an edited repo read "partial" from the
// first save until something forced a whole-workspace re-enrich -- and the next
// keystroke put it straight back. readiness.BlocksQueries is true for partial,
// so those repos answered graph queries with a subset.
//
// The tests here pin the two halves that make the stamp sound: it may never
// promote a provider that has never run (that row's zero is the only thing
// re-arming the repo), and it must withhold from a provider whose language was
// in the frontier but produced nothing.

// scopedStampFixture is one sqlite-backed indexer armed with a file-scoped
// frontier, which is the only shape that reaches the branch under test.
type scopedStampFixture struct {
	idx      *Indexer
	store    *store_sqlite.Store
	provider *partialScopeBatchProvider
	root     string
	mtimes   map[string]int64
}

// newScopedStampFixture indexes one python file behind an available provider,
// registering any extra providers the caller passes, and leaves the indexer
// disarmed -- the state a warm daemon is in just before the user saves.
func newScopedStampFixture(t *testing.T, extraProviders ...semantic.Provider) *scopedStampFixture {
	t.Helper()
	root := t.TempDir()
	store := openTestSqlite(t)
	idx := New(store, partialScopeRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetRootPath(root)
	// The derived passes are irrelevant here and cost seconds each.
	idx.deferGlobalPasses.Store(true)

	provider := &partialScopeBatchProvider{language: "python"}
	all := append([]semantic.Provider{provider}, extraProviders...)
	configs := make([]semantic.ProviderConfig, 0, len(all))
	for _, p := range all {
		configs = append(configs, semantic.ProviderConfig{
			Name: p.Name(), Languages: p.Languages(), Priority: 1, Enabled: true,
		})
	}
	manager := semantic.NewManager(semantic.Config{Enabled: true, Providers: configs}, zap.NewNop())
	for _, p := range all {
		manager.RegisterProvider(p)
	}
	idx.SetSemanticManager(manager)

	f := &scopedStampFixture{
		idx: idx, store: store, provider: provider,
		root: root, mtimes: map[string]int64{},
	}
	f.addFile(t, "app.py", "def value():\n    return 1\n")
	return f
}

// addFile writes and indexes one file, recording its mtime the way a completed
// index run leaves it.
func (f *scopedStampFixture) addFile(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(f.root, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	require.NoError(t, f.idx.IndexFile(path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	f.mtimes[name] = info.ModTime().UnixNano()
	f.idx.SetFileMtimes(f.mtimes)
}

// declareComplete seeds the applicability rows a repo carries after a real
// whole-repo pass: every named provider current as of the content counter.
func (f *scopedStampFixture) declareComplete(t *testing.T, providers ...string) {
	t.Helper()
	require.NoError(t, f.store.DeclareEnrichmentProviders("r", providers))
	gen := f.contentGen(t)
	for _, provider := range providers {
		require.NoError(t, f.store.CompleteEnrichmentProvider("r", provider, gen))
	}
	// gen is copied from repo_graph_gen.gen, and the whole stamp is gated on
	// gen > 0. A fixture whose rows landed at zero would exercise nothing.
	gens, err := f.store.EnrichmentContentGens("r")
	require.NoError(t, err)
	require.Len(t, gens, len(providers))
}

// declareNeverRan adds a provider that is applicable but has never completed --
// the gen = 0 row whose zero is the only thing that re-arms the repo.
func (f *scopedStampFixture) declareNeverRan(t *testing.T, completed []string, neverRan string) {
	t.Helper()
	require.NoError(t, f.store.DeclareEnrichmentProviders("r", append(append([]string(nil), completed...), neverRan)))
	gen := f.contentGen(t)
	for _, provider := range completed {
		require.NoError(t, f.store.CompleteEnrichmentProvider("r", provider, gen))
	}
}

// disarm clears the pending state that indexing left behind, so the only work
// the pass under test sees is the edit the caller makes next.
func (f *scopedStampFixture) disarm() {
	f.idx.pendingEnrich.Store(false)
	f.idx.deferredEnrichMu.Lock()
	f.idx.deferredEnrichFiles = nil
	f.idx.deferredEnrichFull = false
	f.idx.deferredEnrichMu.Unlock()
	*f.provider = partialScopeBatchProvider{language: f.provider.language}
}

// save rewrites the named files with future mtimes and reindexes them, which is
// what arms the per-file ledger and advances the content counter.
func (f *scopedStampFixture) save(t *testing.T, edits map[string]string) {
	t.Helper()
	future := time.Now().Add(time.Minute)
	paths := make([]string, 0, len(edits))
	for name, body := range edits {
		path := filepath.Join(f.root, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
		require.NoError(t, os.Chtimes(path, future, future))
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result, err := f.idx.IncrementalReindexPaths(f.root, paths)
	require.NoError(t, err)
	require.Equal(t, len(edits), result.StaleFileCount)
	require.Empty(t, result.FailedFiles)

	files, full, _ := f.idx.deferredEnrichScope()
	require.False(t, full, "an edit must stay an exact file frontier, not a repo pass")
	require.Len(t, files, len(edits), "every edited file must be on the frontier")
}

func (f *scopedStampFixture) contentGen(t *testing.T) int64 {
	t.Helper()
	gen, err := f.store.RepoContentGen("r")
	require.NoError(t, err)
	return gen
}

func (f *scopedStampFixture) providerGens(t *testing.T) map[string]int64 {
	t.Helper()
	gens, err := f.store.EnrichmentContentGens("r")
	require.NoError(t, err)
	return gens
}

// minProviderGen mirrors the readiness sub-verdict: the MINIMUM across a repo's
// real provider rows, sentinels excluded. A single lagging row holds the repo
// below ready, which is why the stamp cannot cover only the languages that ran.
func minProviderGen(t *testing.T, gens map[string]int64) int64 {
	t.Helper()
	out := int64(-1)
	for provider, gen := range gens {
		if provider == graph.EnrichProviderNone || provider == graph.EnrichProviderRepoMarker {
			continue
		}
		if out < 0 || gen < out {
			out = gen
		}
	}
	require.GreaterOrEqual(t, out, int64(0), "no real provider rows to take a minimum over")
	return out
}

// unavailableBatchProvider is registered and selected for its language, and
// then screened out by EnrichFilesContext's Available() check -- so the pass
// yields (nil, nil) with no error and no work done. It is the shape that
// separates "no provider covers this language" from "a provider covers it and
// ran nothing", which is the whole point of the withheld set.
type unavailableBatchProvider struct {
	language string
	calls    int
}

func (p *unavailableBatchProvider) Name() string        { return "unavailable-" + p.language }
func (p *unavailableBatchProvider) Languages() []string { return []string{p.language} }
func (p *unavailableBatchProvider) Available() bool     { return false }
func (p *unavailableBatchProvider) Close() error        { return nil }
func (p *unavailableBatchProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	p.calls++
	return nil, nil
}
func (p *unavailableBatchProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	p.calls++
	return nil, nil
}
func (p *unavailableBatchProvider) EnrichFiles(graph.Store, string, string, []string) (*semantic.EnrichResult, error) {
	p.calls++
	return nil, nil
}

// THE REGRESSION. A file-scoped pass did every bit of the work and recorded
// none of it, so the repo stayed behind its own content counter forever: the
// ledger was consumed, the tree is dirty so the marker branch declines, and gen
// is non-zero so the never-ran branch declines too. Nothing re-armed it.
func TestAFileScopedPassRecordsTheContentItCovers(t *testing.T) {
	f := newScopedStampFixture(t)
	f.declareComplete(t, f.provider.Name())
	f.disarm()

	f.save(t, map[string]string{"app.py": "def value(arg):\n    return arg\n"})
	content := f.contentGen(t)
	require.Greater(t, content, minProviderGen(t, f.providerGens(t)),
		"the save must leave enrichment behind, or the test proves nothing")

	f.idx.runDeferredEnrich()

	require.Equal(t, 1, f.provider.batchCalls, "the scoped branch must have actually run")
	assert.Equal(t, content, minProviderGen(t, f.providerGens(t)),
		"a completed file-scoped pass must record the content generation it covered")
}

// Readiness takes the MINIMUM across a repo's provider rows, so stamping only
// the languages that appeared in the frontier leaves every other row behind and
// the repo still reads "partial" -- the fix would appear to work only when the
// user happened to edit every language at once.
//
// Stamping the untouched rows is sound, and it has a precedent: runEnrichOne
// stamps a provider the completion-marker gate SKIPPED, on the reasoning that a
// skip is an assertion rather than an absence of work. A provider with no file
// on the frontier is current at this generation for exactly the same reason.
func TestAFileScopedPassStampsProvidersWhoseLanguageWasNotInTheFrontier(t *testing.T) {
	f := newScopedStampFixture(t)
	// go-types completed a real pass and has no file in the frontier below.
	f.declareComplete(t, f.provider.Name(), "go-types")
	f.disarm()

	f.save(t, map[string]string{"app.py": "def value(arg):\n    return arg\n"})
	content := f.contentGen(t)

	f.idx.runDeferredEnrich()

	require.Equal(t, 1, f.provider.batchCalls, "one language on the frontier, one batch call")
	gens := f.providerGens(t)
	assert.Equal(t, content, gens[f.provider.Name()], "the language that ran is current")
	assert.Equal(t, content, gens["go-types"],
		"a provider with no file on the frontier is current too, or the minimum keeps the repo partial")
	assert.Equal(t, content, minProviderGen(t, gens))
}

// THE hard correctness hole, at the layer that decides it. gen = 0 is the only
// signal that re-arms a repo whose enrichment has never run, and it is monotone
// -- once a pass completes the row can never return to zero, so acting on it
// discharges it for good.
//
// The obvious implementation of this stamp (loop the rows through
// CompleteEnrichmentProvider) would set gen = excluded.gen unconditionally and
// destroy that signal: a repo tracked with enrichment deferred, then edited
// once, would claim repo-wide coverage from a one-file pass and never re-arm.
// That is a worse bug than the one being fixed.
func TestAFileScopedPassDoesNotPromoteAProviderThatNeverRan(t *testing.T) {
	f := newScopedStampFixture(t)
	f.declareNeverRan(t, []string{f.provider.Name()}, "go-types")
	f.disarm()

	owed, known := semantic.EnrichmentOwed(f.store, "r")
	require.True(t, known)
	require.True(t, owed, "the fixture must start owed, or the test proves nothing")

	f.save(t, map[string]string{"app.py": "def value(arg):\n    return arg\n"})
	f.idx.runDeferredEnrich()

	gens := f.providerGens(t)
	assert.Equal(t, f.contentGen(t), gens[f.provider.Name()], "the completed provider is renewed")
	assert.Zero(t, gens["go-types"], "a provider that never ran must not be promoted by a scoped pass")

	owed, known = semantic.EnrichmentOwed(f.store, "r")
	assert.True(t, known)
	assert.True(t, owed, "the never-ran re-arm must survive; nothing else can restore it")
}

// The other half of the nil-result decision. EnrichFiles returns (nil, nil)
// both when no provider is registered for a language and when one is registered
// but ran nothing, and only ProviderForLanguage tells them apart.
//
// Here the frontier carries a javascript file whose provider is registered,
// selected, and then screened out by Available(). Its edges were evicted by the
// re-parse and restored by nothing, so stamping it would publish a repo that
// reads ready while silently missing that provider's edges. Withholding lets
// the one row hold the minimum down, which is the true reading.
func TestAFileScopedPassWithholdsAProviderWhoseLanguageRanNothing(t *testing.T) {
	unavailable := &unavailableBatchProvider{language: "javascript"}
	f := newScopedStampFixture(t, unavailable)
	f.addFile(t, "app.js", "function value() { return 1; }\n")
	f.declareComplete(t, f.provider.Name(), unavailable.Name())
	f.disarm()

	f.save(t, map[string]string{
		"app.py": "def value(arg):\n    return arg\n",
		"app.js": "function value(arg) { return arg; }\n",
	})
	// Both languages must really be on the frontier: if javascript were absent
	// its row would be stamped as an untouched language and the assertion below
	// would be measuring the wrong thing.
	files, _, _ := f.idx.deferredEnrichScope()
	byLanguage := f.idx.deferredEnrichFrontiers(files)
	require.Contains(t, byLanguage, "python")
	require.Contains(t, byLanguage, "javascript")

	before := f.providerGens(t)[unavailable.Name()]
	f.idx.runDeferredEnrich()

	require.Equal(t, 1, f.provider.batchCalls, "the available provider still runs")
	require.Zero(t, unavailable.calls, "an unavailable provider is screened out before it is called")

	gens := f.providerGens(t)
	content := f.contentGen(t)
	assert.Equal(t, content, gens[f.provider.Name()], "the language that ran is current")
	assert.Equal(t, before, gens[unavailable.Name()],
		"a frontier language that ran nothing must not be stamped")
	assert.Less(t, minProviderGen(t, gens), content,
		"the withheld row holds the repo behind, which is the honest answer")
}

// A partial pass covered an unknown subset, so it may record nothing at all --
// not the content stamp, and not the ledger discharge. Both must stay behind so
// the next pass retries, which is what the early return above the stamp buys.
func TestAPartialFileScopedPassStampsNothing(t *testing.T) {
	f := newScopedStampFixture(t)
	f.declareComplete(t, f.provider.Name())
	f.disarm()
	f.provider.partial = true

	f.save(t, map[string]string{"app.py": "def value(arg):\n    return arg\n"})
	before := f.providerGens(t)

	f.idx.runDeferredEnrich()

	require.Equal(t, 1, f.provider.batchCalls, "the pass ran; it just did not finish")
	assert.Equal(t, before, f.providerGens(t), "a partial pass records no coverage")
	assert.True(t, f.idx.pendingEnrich.Load(), "and leaves the ledger armed so the next pass retries")
}

// The distinction the whole change rests on. The whole-repo completion MARKER
// asserts that every file was enriched at a revision, which a frontier cannot
// support and must never publish. The content COUNTER asserts only "this
// provider is current as of generation N", which a fully discharged frontier
// does support. Conflating the two is what withheld the counter for years.
func TestAFileScopedPassStillWithholdsTheWholeRepoMarker(t *testing.T) {
	f := newScopedStampFixture(t)
	f.declareComplete(t, f.provider.Name())
	f.disarm()

	f.save(t, map[string]string{"app.py": "def value(arg):\n    return arg\n"})
	f.idx.runDeferredEnrich()

	gens := f.providerGens(t)
	require.Equal(t, f.contentGen(t), gens[f.provider.Name()], "the counter did move")
	assert.NotContains(t, gens, graph.EnrichProviderRepoMarker,
		"the scoped pass must never publish the whole-repository completion marker")
}

// clearPendingEnrich declines when the generation moved, which proves a save
// landed while the pass was in flight. The stamp sits behind that guard on
// purpose: the content this pass observed is no longer the content the repo
// holds, so claiming coverage of it would be claiming an edit the pass never
// saw -- fail-open, and permanently, since nothing later re-examines it.
func TestAFileScopedPassStampsNothingWhenAnotherSaveArrivesMidPass(t *testing.T) {
	f := newScopedStampFixture(t)
	f.declareComplete(t, f.provider.Name())
	f.disarm()

	f.save(t, map[string]string{"app.py": "def value(arg):\n    return arg\n"})
	before := f.providerGens(t)
	// The watcher lands another save while the provider is still working. Any
	// non-empty frontier advances the generation, which is all this needs.
	f.provider.onBatch = func() { f.idx.markPendingEnrichFiles([]string{"r/app.py"}) }

	f.idx.runDeferredEnrich()

	require.Equal(t, 1, f.provider.batchCalls, "the pass ran to completion")
	assert.Equal(t, before, f.providerGens(t),
		"content that arrived mid-pass belongs to the next pass, not this stamp")
	assert.True(t, f.idx.pendingEnrich.Load(), "the newer work stays armed")
}
