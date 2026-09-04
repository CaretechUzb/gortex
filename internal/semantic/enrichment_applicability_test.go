package semantic

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

const applRepo = "applrepo"

// applStore returns a real store carrying one Go file's worth of symbol nodes
// for applRepo, so the language census has something to see. Applicability is
// gated on positive evidence, and an empty graph is deliberately no evidence.
func applStore(t *testing.T, langs ...string) *store_sqlite.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "appl.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	ext := map[string]string{"go": "go", "python": "py", "typescript": "ts"}
	var nodes []*graph.Node
	for _, lang := range langs {
		path := applRepo + "/src/main." + ext[lang]
		nodes = append(nodes, &graph.Node{
			ID: path + "::F", Kind: graph.KindFunction, Name: "F",
			FilePath: path, RepoPrefix: applRepo, Language: lang,
		})
	}
	s.AddBatch(nodes, nil)
	return s
}

func applManager(t *testing.T, providers ...*mockProvider) *Manager {
	t.Helper()
	cfg := Config{Enabled: true}
	for _, p := range providers {
		cfg.Providers = append(cfg.Providers, ProviderConfig{
			Name: p.name, Languages: p.languages, Priority: 1, Enabled: true,
		})
	}
	mgr := NewManager(cfg, zap.NewNop())
	for _, p := range providers {
		mgr.RegisterProvider(p)
	}
	return mgr
}

func applProvider(name, lang string, available bool) *mockProvider {
	return &mockProvider{
		name: name, languages: []string{lang}, available: available,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			return &EnrichResult{Provider: name, Language: lang, CoveragePercent: 90}, nil
		},
	}
}

// Applicability is per-REPO, not per-workspace. The census that drives it is
// workspace-wide, so hanging python-types on a pure-Go sibling checkout is the
// natural mistake — and it would hold that repo below ready forever over a
// language it does not contain.
func TestApplicabilityFollowsTheRepoNotTheWorkspace(t *testing.T) {
	g := applStore(t, "go")
	mgr := applManager(t,
		applProvider("test-go", "go", true),
		applProvider("test-py", "python", true))

	_, _, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	require.Contains(t, gens, "test-go")
	require.NotContains(t, gens, "test-py", "the repo has no Python; its provider does not apply")
}

// A provider whose binary is not installed is not part of what this
// installation can deliver. Declaring it applicable would hold every repo short
// of ready with no action that clears it, and a column nobody can satisfy is a
// column everybody ignores.
func TestAnUnavailableProviderIsNotApplicable(t *testing.T) {
	g := applStore(t, "go")
	mgr := applManager(t, applProvider("test-go", "go", false))

	_, _, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{graph.EnrichProviderNone: 0}, gens,
		"nothing this install can run applies, which is n/a — not an unmet requirement")
}

// The applicable set is written BEFORE any provider runs, so a pass that dies
// mid-flight still leaves evidence that the provider was owed. Without it, a
// crashed enrichment is indistinguishable from one that was never needed.
func TestApplicabilityIsRecordedEvenWhenNoProviderCompletes(t *testing.T) {
	g := applStore(t, "go")
	failing := applProvider("test-go", "go", true)
	failing.enrichFunc = func(g graph.Store, root string) (*EnrichResult, error) {
		return &EnrichResult{Provider: "test-go", Language: "go", Partial: true}, nil
	}
	mgr := applManager(t, failing)

	_, partial, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)
	require.True(t, partial[applRepo])

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"test-go": 0}, gens,
		"applicable and owed, recorded before the pass that did not finish")
}

// An empty or unindexed graph is no evidence either way. Declaring an empty set
// there would assert "no provider applies" for a repo nobody has looked at yet,
// and — because the declaration is authoritative — would delete real completions
// on the way.
func TestNoLanguageCensusDeclaresNothing(t *testing.T) {
	g := applStore(t) // no nodes at all
	require.NoError(t, g.DeclareEnrichmentProviders(applRepo, []string{"test-go"}))
	require.NoError(t, g.CompleteEnrichmentProvider(applRepo, "test-go", 0))

	mgr := applManager(t, applProvider("test-go", "go", true))
	_, _, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"test-go": 0}, gens,
		"an absent census must leave the recorded set exactly as it was")
}

// Enrichment switched off is a real "nothing applies", not an absence of
// information. Saying so is what keeps every repo on such an install from
// reading unknown forever.
func TestDisabledEnrichmentDeclaresNothingApplies(t *testing.T) {
	g := applStore(t, "go")
	mgr := NewManager(Config{Enabled: false}, zap.NewNop())

	_, _, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{graph.EnrichProviderNone: 0}, gens)
}

// A warm restart correctly skips every provider whose marker is current. The
// skip IS the system asserting the enrichment describes this content — so it
// has to stamp. Without this the most common event in the daemon's life would
// leave every repo reading "partial" against content the skip gate had just
// certified.
func TestASkippedProviderStillRecordsTheContentItCovers(t *testing.T) {
	g := applStore(t, "go")
	require.NoError(t, g.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 1}))
	require.NoError(t, g.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: applRepo, Provider: "test-go", IndexedSHA: "abc123",
	}))

	provider := applProvider("test-go", "go", true)
	ran := false
	provider.enrichFunc = func(g graph.Store, root string) (*EnrichResult, error) {
		ran = true
		return &EnrichResult{Provider: "test-go", Language: "go"}, nil
	}
	mgr := applManager(t, provider)

	opts := EnrichOptions{RepoState: map[string]RepoEnrichState{
		applRepo: {SHA: "abc123", Dirty: false},
	}}
	_, _, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, opts)
	require.NoError(t, err)
	require.False(t, ran, "the marker is current, so the pass must be skipped")

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	assert.Equal(t, int64(1), gens["test-go"],
		"a skip is an assertion of currency, and must be stamped like one")
}

// A completed pass records the content it covered, and a later edit strands
// that stamp. This is the end-to-end shape of the enrichment half of readiness.
func TestACompletedPassIsStrandedByALaterEdit(t *testing.T) {
	g := applStore(t, "go")
	require.NoError(t, g.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 1}))
	mgr := applManager(t, applProvider("test-go", "go", true))

	_, _, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	covered := gens["test-go"]
	current, err := g.RepoContentGen(applRepo)
	require.NoError(t, err)
	require.Equal(t, current, covered, "READY: the pass covers the current content")

	require.NoError(t, g.BulkSetFileMtimes(applRepo, map[string]int64{"src/main.go": 2}))
	current, err = g.RepoContentGen(applRepo)
	require.NoError(t, err)
	require.Greater(t, current, covered, "PARTIAL: an edit strands the enrichment stamp")
}

// supplementalMock runs outside arbitration the way every tstypes spec does:
// it holds no language slot, so selectProviders skips it, but EnrichAll runs it
// unconditionally after the winners.
type supplementalMock struct{ *mockProvider }

func (supplementalMock) Supplemental() bool { return true }

// TestASupplementalProviderIsDeclaredApplicable is the regression gate for a
// repo that read "ready" while its main language was ten content generations
// behind.
//
// The declaration is authoritative — it deletes every row outside the declared
// set — and it was built from the arbitration winners alone. Supplemental
// providers were therefore deleted on every pass and restored only by their own
// completion, so a supplemental pass that ended Partial left no row at all, and
// readiness computed MIN(content_gen) over the survivors and called the repo
// ready. Measured 2026-09-02 on three Odoo checkouts, none of which had a
// python-types row after their python passes overran their budgets.
func TestASupplementalProviderIsDeclaredApplicable(t *testing.T) {
	g := applStore(t, "go", "python")

	cfg := Config{Enabled: true, Providers: []ProviderConfig{
		{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		{Name: "test-py", Languages: []string{"python"}, Priority: 1, Enabled: true},
	}}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(applProvider("test-go", "go", true))

	// Ends Partial, exactly as the python apply does when it overruns its
	// budget: it writes no completion row of its own, so the declaration is the
	// only thing that can record that it was owed.
	supp := applProvider("test-py", "python", true)
	supp.enrichFunc = func(graph.Store, string) (*EnrichResult, error) {
		return &EnrichResult{Provider: "test-py", Language: "python", Partial: true}, nil
	}
	mgr.RegisterProvider(supplementalMock{supp})

	_, partial, err := mgr.EnrichAll(g, map[string]string{applRepo: t.TempDir()}, EnrichOptions{})
	require.NoError(t, err)
	require.True(t, partial[applRepo])

	gens, err := g.EnrichmentContentGens(applRepo)
	require.NoError(t, err)
	require.Contains(t, gens, "test-py",
		"a supplemental provider that runs and does not finish must stay visible as owed")
	assert.EqualValues(t, 0, gens["test-py"],
		"owed and never completed is a gen-0 row, which readiness reads as behind")
}
