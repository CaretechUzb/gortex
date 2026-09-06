package tstypes

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// pyCaller is one call site per fixture file: `Svc()` then `.run()`, so every
// caller file contributes exactly the same, independently observable, work.
func pyCaller(fn string) string {
	return `from app.svc import Svc


def ` + fn + `():
    s = Svc()
    s.run()
`
}

// frontierFixture is one Svc plus four caller files, so a two-file frontier
// leaves two files that must be provably untouched.
func frontierFixture() map[string]string {
	return map[string]string{
		"app/svc.py": pySvc,
		"app/a.py":   pyCaller("a"),
		"app/b.py":   pyCaller("b"),
		"app/c.py":   pyCaller("c"),
		"app/d.py":   pyCaller("d"),
	}
}

// The whole point of the batch entry: a frontier pass parses, stages and
// applies the FRONTIER, not the repository.
//
// Before EnrichFilesContext existed, Manager.EnrichFilesContext's dispatch
// ladder found no batch face on a tstypes Provider and fell through to
// EnrichRepoContext, which re-collects languageFiles for the whole repository
// and never looks at the frontier. This test fails against that behaviour on
// the assertUntouched assertions below: the fall-through stamps app/c.py and
// app/d.py too.
func TestEnrichFilesContextStagesOnlyTheFrontier(t *testing.T) {
	g, dir := buildFixture(t, frontierFixture())
	p := NewProvider(PythonSpec(), zap.NewNop())

	result, err := p.EnrichFilesContext(context.Background(), g, "", dir,
		[]string{"app/a.py", "app/b.py"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Partial, "a completed frontier pass is not partial")

	target := nodeByNameKind(t, g, "run", graph.KindMethod)
	for _, fn := range []string{"a", "b"} {
		caller := nodeByNameKind(t, g, fn, graph.KindFunction)
		edge := callEdgeTo(g, caller.ID, target.ID)
		require.NotNilf(t, edge, "frontier file app/%s.py was not enriched", fn)
		assertASTProvenance(t, edge, "python-types")
	}
	for _, fn := range []string{"c", "d"} {
		caller := nodeByNameKind(t, g, fn, graph.KindFunction)
		assertUntouched(t, g, caller.ID, "run", "python-types")
	}

	// Coverage is the frontier's own fraction. The whole-repo denominator
	// would report a complete 2-of-5-file pass as a fraction of the
	// repository, which reads as "did almost nothing".
	whole := NewProvider(PythonSpec(), zap.NewNop()).
		countSymbolsInFiles(g, "", languageFiles(g, PythonSpec(), "", dir))
	assert.Greater(t, result.SymbolsTotal, 0)
	assert.Less(t, result.SymbolsTotal, whole,
		"the denominator must be the frontier's candidates, not the repository's")
	assert.Equal(t, result.SymbolsTotal, result.SymbolsCovered,
		"every frontier file was staged, so coverage is complete")
	assert.Equal(t, float64(100), result.CoveragePercent)
}

// The most important correctness property of a frontier pass: it must not
// evict or replace edges owned by files outside the frontier.
//
// applyStagedFacts performs no repo-wide deletion — its only removal
// (applier.removeEdge, from applySuper) is an out-edge of a type node taken
// from ONE staged file's own index — but that is a property of the apply
// phase, not of a signature, and re-scoping the pass is exactly the change
// that could break it.
func TestEnrichFilesContextDoesNotEvictEdgesOutsideTheFrontier(t *testing.T) {
	g, dir := buildFixture(t, frontierFixture())
	p := NewProvider(PythonSpec(), zap.NewNop())

	// Stamp the whole repository first, so every caller file owns provider
	// edges an over-broad frontier pass could drop.
	_, err := p.EnrichRepo(g, "", dir)
	require.NoError(t, err)

	type edgeState struct {
		to         string
		kind       graph.EdgeKind
		source     any
		confidence float64
		origin     string
	}
	snapshot := func(fn string) []edgeState {
		caller := nodeByNameKind(t, g, fn, graph.KindFunction)
		var out []edgeState
		for _, e := range g.GetOutEdges(caller.ID) {
			state := edgeState{to: e.To, kind: e.Kind, confidence: e.Confidence, origin: e.Origin}
			if e.Meta != nil {
				state.source = e.Meta["semantic_source"]
			}
			out = append(out, state)
		}
		return out
	}
	before := map[string][]edgeState{"c": snapshot("c"), "d": snapshot("d")}
	require.NotEmpty(t, before["c"])
	for _, state := range before["c"] {
		require.NotEqual(t, "", state.to)
	}

	result, err := p.EnrichFilesContext(context.Background(), g, "", dir, []string{"app/a.py"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Partial)

	for _, fn := range []string{"c", "d"} {
		assert.Equal(t, before[fn], snapshot(fn),
			"app/%s.py is outside the frontier; its edges must survive verbatim", fn)
	}
}

// An empty intersection is an answer, not a failure, and above all not a
// licence to enrich the repository instead. A frontier that names no file of
// this provider's languages returns a complete, empty, non-partial result.
func TestEnrichFilesContextWithNoFrontierFilesOfThisLanguageIsAnEmptyNonPartialResult(t *testing.T) {
	files := frontierFixture()
	files["src/svc.ts"] = tsSvc
	g, dir := buildFixture(t, files)
	p := NewProvider(PythonSpec(), zap.NewNop())

	for name, frontier := range map[string][]string{
		"another language": {"src/svc.ts"},
		"nothing at all":   nil,
		"an unknown path":  {"app/does-not-exist.py"},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := p.EnrichFilesContext(context.Background(), g, "", dir, frontier)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.False(t, result.Partial, "an empty frontier completed; it did not abort")
			assert.Zero(t, result.SymbolsTotal)
			assert.Zero(t, result.SymbolsCovered)
			assert.Zero(t, result.CoveragePercent)
			assert.Zero(t, result.EdgesAdded)
			assert.Zero(t, result.EdgesConfirmed)
			assert.Equal(t, "python-types", result.Provider)

			// The proof that no whole-repo fallback happened.
			for _, fn := range []string{"a", "b", "c", "d"} {
				caller := nodeByNameKind(t, g, fn, graph.KindFunction)
				assertUntouched(t, g, caller.ID, "run", "python-types")
			}
		})
	}
}

// The Manager wraps this call in the deadline it chose
// (EnrichFilesWithDeadline: the save timeout, or a copy repair's
// CopyRepairDeadline), so ctx is the pass's ONLY bound — no
// EnrichDeadlinePolicy is derived inside. An expired ctx must surface as
// Partial with a reason, because Indexer.runScopedDeferredEnrich keys its
// copy-repair escalation on exactly that flag.
func TestEnrichFilesContextHonoursTheCallerDeadline(t *testing.T) {
	g, dir := buildFixture(t, frontierFixture())
	p := NewProvider(PythonSpec(), zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := p.EnrichFilesContext(ctx, g, "", dir, []string{"app/a.py", "app/b.py"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Partial)
	assert.NotEmpty(t, result.AbortReason)
	assert.Equal(t, semantic.EnrichBoundBudget, result.BoundReason)
	assert.Zero(t, result.EdgesAdded)
	assert.Zero(t, result.EdgesConfirmed)
	for _, fn := range []string{"a", "b"} {
		caller := nodeByNameKind(t, g, fn, graph.KindFunction)
		assertUntouched(t, g, caller.ID, "run", "python-types")
	}

	// And with a live ctx the same pass is not partial — so the assertion
	// above is about the deadline, not about the entry point.
	result, err = p.EnrichFilesContext(context.Background(), g, "", dir, []string{"app/a.py"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Partial)
	assert.Zero(t, result.BudgetSeconds,
		"no EnrichDeadlinePolicy is derived here; the caller's ctx is the bound")
}

// The dispatcher hands over exact graph file keys. The normaliser accepts the
// two other shapes a caller can reasonably produce, and drops the one shape it
// must never guess at — a path outside the repository root.
func TestFrontierFileKeysNormalisesEveryCallerShape(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "repos", "local")
	has := func(t *testing.T, keys map[string]struct{}, want string) {
		t.Helper()
		_, ok := keys[want]
		assert.Truef(t, ok, "missing key %q in %v", want, keys)
	}

	t.Run("single repo keys pass through", func(t *testing.T) {
		keys := frontierFileKeys("", root, []string{"app/main.py"})
		require.Len(t, keys, 1)
		has(t, keys, "app/main.py")
	})

	t.Run("a prefixed graph key is kept verbatim", func(t *testing.T) {
		keys := frontierFileKeys("local", root, []string{"local/app/main.py"})
		has(t, keys, "local/app/main.py")
	})

	t.Run("a repo-relative path gains the prefix", func(t *testing.T) {
		keys := frontierFileKeys("local", root, []string{"app/main.py"})
		has(t, keys, "local/app/main.py")
		// Both candidate readings are registered because "local/app/main.py"
		// is ambiguous in multi-repo mode; the bare form selects nothing
		// unless the graph really holds it.
		has(t, keys, "app/main.py")
	})

	t.Run("an absolute path under the root is relativised", func(t *testing.T) {
		keys := frontierFileKeys("local", root, []string{filepath.Join(root, "app", "main.py")})
		has(t, keys, "local/app/main.py")
	})

	t.Run("windows separators are normalised", func(t *testing.T) {
		keys := frontierFileKeys("", root, []string{filepath.FromSlash("app/pkg/main.py")})
		has(t, keys, "app/pkg/main.py")
	})

	t.Run("a path outside the root is dropped", func(t *testing.T) {
		outside := filepath.Join(string(filepath.Separator), "repos", "other", "app", "main.py")
		assert.Empty(t, frontierFileKeys("local", root, []string{outside}))
	})

	t.Run("empty inputs contribute nothing", func(t *testing.T) {
		assert.Empty(t, frontierFileKeys("local", root, []string{"", ".", "/"}))
	})

	t.Run("an absolute path with no root to relativise against is dropped", func(t *testing.T) {
		abs := filepath.Join(string(filepath.Separator), "repos", "local", "app", "main.py")
		assert.Empty(t, frontierFileKeys("local", "", []string{abs}))
	})
}

// The frontier must be intersected INSIDE the repo the pass is enriching. Two
// tracked repos can share a relative path, and a frontier naming one of them
// must never select the other's node — that is the same contamination
// languageFiles' RepoPrefix gate exists to prevent, and the reason the
// intersection filters languageFiles' output rather than statting paths.
func TestEnrichFilesContextScopesTheFrontierToTheRepoPrefix(t *testing.T) {
	g := graph.New()
	rootA := indexRepoInto(t, g, "repoA", map[string]string{
		"app/svc.py":  pySvc,
		"app/main.py": pyCaller("main"),
	})
	indexRepoInto(t, g, "repoB", map[string]string{
		"app/svc.py":  pySvc,
		"app/main.py": pyCaller("main"),
	})

	p := NewProvider(PythonSpec(), zap.NewNop())
	result, err := p.EnrichFilesContext(context.Background(), g, "repoA", rootA,
		[]string{"repoA/app/main.py"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Partial)

	callerA := "repoA/app/main.py::main"
	require.NotNil(t, callEdgeTo(g, callerA, "repoA/app/svc.py::Svc.run"),
		"repoA's frontier file was not enriched; edges: %v", g.GetOutEdges(callerA))

	callerB := "repoB/app/main.py::main"
	for _, e := range g.GetOutEdges(callerB) {
		if e.Meta != nil {
			assert.Nil(t, e.Meta["semantic_source"],
				"repoB shares the relative path but was not in the frontier")
		}
	}
}
