package indexer

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// frameworkGateTestIndexer builds the minimum an allow-list fold reads: the
// repo's own list and the workspace it belongs to.
func frameworkGateTestIndexer(workspace string, allow ...string) *Indexer {
	cfg := config.IndexConfig{}
	cfg.Frameworks.Allow = allow
	return &Indexer{workspaceID: workspace, config: cfg}
}

// TestAllowedFrameworksForScope_StopsAtTheWorkspaceBoundary is the direct
// assertion behind the change: a pass no repository in the run's workspace
// asks for must not execute against that workspace's scope.
//
// Scope of what this catches, stated because an earlier draft of this comment
// got it wrong: these tests exercise the fold DIRECTLY, so they pin its
// semantics and nothing else. Reverting the call site in
// runIncrementalDerivedPassesTopologyHeld back to mi.allowedFrameworks()
// leaves every test in this function green — measured, not assumed. The call
// site is guarded by
// TestIncrementalDerivedPassesGateFrameworksByWorkspace below, which is the
// test that actually fails on that mutant.
func TestAllowedFrameworksForScope_StopsAtTheWorkspaceBoundary(t *testing.T) {
	mi := &MultiIndexer{
		indexers: map[string]*Indexer{
			"local":   frameworkGateTestIndexer("his", "odoo"),
			"addons":  frameworkGateTestIndexer("his", "odoo"),
			"gortex":  frameworkGateTestIndexer("gortex", "fastapi-resolve", "fn-value-callback"),
			"unrelat": frameworkGateTestIndexer("gortex", "fastapi-resolve"),
		},
	}

	scoped := mi.allowedFrameworksForScope(map[string]bool{"local": true})
	if !scoped.Allows("odoo") {
		t.Fatal("the run's own workspace narrowed to [odoo]; odoo must still run")
	}
	for _, name := range []string{"fastapi-resolve", "fn-value-callback"} {
		if scoped.Allows(name) {
			t.Errorf("%s is asked for only by the gortex workspace and must not execute against his", name)
		}
	}

	// The half that makes this a mutation guard: today's daemon-wide fold
	// admits exactly what the scoped fold just refused.
	union := mi.allowedFrameworks()
	if !union.Allows("fastapi-resolve") {
		t.Fatal("precondition: the daemon-wide union must admit the sibling workspace's pass")
	}
}

// TestAllowedFrameworksForScope_SpansEveryWorkspaceItTouches covers the
// multi-repo plan: a run whose frontier crosses two workspaces must get the
// union of both, not of one.
func TestAllowedFrameworksForScope_SpansEveryWorkspaceItTouches(t *testing.T) {
	mi := &MultiIndexer{
		indexers: map[string]*Indexer{
			"local":  frameworkGateTestIndexer("his", "odoo"),
			"gortex": frameworkGateTestIndexer("gortex", "fastapi-resolve"),
		},
	}

	scoped := mi.allowedFrameworksForScope(map[string]bool{"local": true, "gortex": true})
	if !scoped.Allows("odoo") || !scoped.Allows("fastapi-resolve") {
		t.Fatalf("a run touching both workspaces must union both lists, got %v", scoped.Patterns())
	}
}

// TestAllowedFrameworksForScope_FallsBackWhenItCannotNarrow pins the two
// fail-open cases. Allowing everything is the pre-existing behaviour and the
// safe direction; silently narrowing here would drop edges.
func TestAllowedFrameworksForScope_FallsBackWhenItCannotNarrow(t *testing.T) {
	mi := &MultiIndexer{
		indexers: map[string]*Indexer{
			"local":  frameworkGateTestIndexer("his", "odoo"),
			"gortex": frameworkGateTestIndexer("gortex", "fastapi-resolve"),
		},
	}

	t.Run("empty scope keeps the daemon-wide union", func(t *testing.T) {
		got := mi.allowedFrameworksForScope(nil)
		if !got.Allows("odoo") || !got.Allows("fastapi-resolve") {
			t.Fatalf("an unscoped run must keep the full union, got %v", got.Patterns())
		}
	})

	t.Run("scope naming no tracked repo allows everything", func(t *testing.T) {
		got := mi.allowedFrameworksForScope(map[string]bool{"never-tracked": true})
		if got.Configured() {
			t.Fatalf("an unresolvable scope must stay unset (allow all), got %v", got.Patterns())
		}
		if !got.Allows("anything-at-all") {
			t.Fatal("the unset Set must allow everything")
		}
	})
}

// TestAllowedFrameworksForScope_UnconfiguredRepoStaysInItsOwnWorkspace is the
// strict improvement over the daemon-wide fold. One unconfigured repository
// re-admits the whole registry, because frameworkgate.Union returns the unset
// Set on contact with one. Scoping by workspace confines that blast radius to
// the workspace that actually contains it.
func TestAllowedFrameworksForScope_UnconfiguredRepoStaysInItsOwnWorkspace(t *testing.T) {
	mi := &MultiIndexer{
		indexers: map[string]*Indexer{
			"local":        frameworkGateTestIndexer("his", "odoo"),
			"unconfigured": frameworkGateTestIndexer("other"), // no allow list at all
		},
	}

	if union := mi.allowedFrameworks(); union.Configured() {
		t.Fatal("precondition: one unconfigured repo must leave the daemon-wide union unset")
	}
	scoped := mi.allowedFrameworksForScope(map[string]bool{"local": true})
	if !scoped.Configured() {
		t.Fatal("the his workspace is fully narrowed; a sibling workspace's unconfigured repo must not widen it")
	}
	if scoped.Allows("fastapi-resolve") {
		t.Error("an unconfigured repo in another workspace re-admitted the registry for this one")
	}
}

// TestWorkspaceIDForPrefixLocked_SingletonFallback pins the fallback
// ReposInWorkspace already relies on: a repository declaring no workspace is
// its own. That is what keeps the case above from collapsing into one shared
// bucket.
func TestWorkspaceIDForPrefixLocked_SingletonFallback(t *testing.T) {
	mi := &MultiIndexer{
		indexers: map[string]*Indexer{
			"declared": frameworkGateTestIndexer("his"),
			"bare":     frameworkGateTestIndexer(""),
		},
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	if got := mi.workspaceIDForPrefixLocked("declared"); got != "his" {
		t.Errorf("declared workspace = %q, want %q", got, "his")
	}
	if got := mi.workspaceIDForPrefixLocked("bare"); got != "bare" {
		t.Errorf("bare repo must key on its own prefix, got %q", got)
	}
	if got := mi.workspaceIDForPrefixLocked("absent"); got != "absent" {
		t.Errorf("untracked prefix must key on itself, got %q", got)
	}
}

// TestIncrementalDerivedPassesGateFrameworksByWorkspace is the CALL-SITE
// guard. The fold tests above pass unchanged against the daemon-wide union, so
// without this one the whole change could be reverted at its single call site
// with a green suite.
//
// It asserts on the pass report rather than on elapsed time: `Disabled` is set
// straight from the allow-list the call site handed in, so it answers "was this
// pass admitted for this run" exactly, and a slow machine cannot make it flap.
func TestIncrementalDerivedPassesGateFrameworksByWorkspace(t *testing.T) {
	mi := &MultiIndexer{
		graph:  graph.New(),
		logger: zap.NewNop(),
		indexers: map[string]*Indexer{
			"local":  frameworkGateTestIndexer("his", "odoo"),
			"gortex": frameworkGateTestIndexer("gortex", "fastapi-resolve", "fn-value-callback"),
		},
	}

	report := mi.RunIncrementalDerivedPasses(context.Background(), map[string]DerivedInvalidationPlan{
		"local": {
			Files: []string{"local/a.py"},
			Flags: DerivedInvalidatesRuntime,
		},
	})
	if len(report.FrameworkPer) == 0 {
		t.Fatal("precondition: the framework branch did not run, so this guards nothing")
	}

	disabled := make(map[string]bool, len(report.FrameworkPer))
	seen := make(map[string]bool, len(report.FrameworkPer))
	for _, p := range report.FrameworkPer {
		seen[p.Name] = true
		disabled[p.Name] = p.Disabled
	}

	for _, name := range []string{"fastapi-resolve", "fn-value-callback"} {
		if !seen[name] {
			t.Fatalf("precondition: %s is not a registered pass under this name", name)
		}
		if !disabled[name] {
			t.Errorf("%s is asked for only by the gortex workspace but was admitted for a his-workspace run", name)
		}
	}
	if !seen["odoo"] {
		t.Fatal("precondition: odoo is not a registered pass under this name")
	}
	if disabled["odoo"] {
		t.Error("odoo is allowed by the run's own workspace and must stay admitted")
	}
}

// --- The oracle -------------------------------------------------------------
//
// Everything above pins WHICH passes run. This pins what the change was
// actually claimed to preserve: the resulting graph.
//
// The claim is not self-evident. The union decides EXECUTION while the per-repo
// gate decides where edges LAND, so narrowing the union is output-preserving
// only if the gate already refuses everything the newly-skipped passes would
// have produced. That is an empirical property of two independent mechanisms,
// and the scoped dispatch hands several passes the full store rather than a
// scope-limited one, so a pass that ignores its execution scope can reach
// outside its frontier. This test is what earns the claim; the measurement only
// motivated it.

// frameworkUnionOracleFixture plants one function-as-value callback candidate
// in each workspace's repository.
//
// fn-value-callback is the right pass to build this on and the choice is not
// arbitrary — it is the exact shape the change is about. The `gortex` workspace
// legitimately asks for it, it is registered once for the whole daemon, and
// before this change it EXECUTED against the `his` workspace's scope, where the
// per-repo gate discarded every edge it staged: 1,297 of them on the run that
// motivated the change.
//
// The placeholder is the contract ResolveFnValueCallbacks consumes — a
// reference edge parked in the fn-value namespace, carrying via and the
// captured name, which the gate binds to a same-file function. Two of those
// three spellings are unexported in resolver and are written out here, so the
// fixture can silently stop firing if either drifts. The RepoGated precondition
// in the test below is what turns that from a silent pass into a failure.
func frameworkUnionOracleFixture() *graph.Graph {
	g := graph.New()
	for _, repo := range []string{"local", "gortex"} {
		file := repo + "/svc/handlers.py"
		g.AddNode(&graph.Node{
			ID: file, Kind: graph.KindFile, Name: "handlers.py",
			FilePath: file, Language: "python", RepoPrefix: repo,
		})
		for _, fn := range []string{"handler", "register"} {
			g.AddNode(&graph.Node{
				ID: file + "::" + fn, Kind: graph.KindFunction, Name: fn,
				FilePath: file, Language: "python", RepoPrefix: repo,
			})
		}
		// FilePath is load-bearing, not decoration: the gate binds a plain
		// candidate through a same-file lookup keyed on the EDGE's file, so an
		// edge without one resolves to nothing and the fixture goes quiet.
		g.AddEdge(&graph.Edge{
			From:     file + "::register",
			To:       graph.FnValuePlaceholderMarker + "handler",
			Kind:     graph.EdgeReferences,
			FilePath: file,
			Meta:     map[string]any{"via": "callback_candidate", "fn_value_name": "handler"},
		})
	}
	return g
}

// frameworkEdgeSet is the whole observable output of a synthesis run: which
// edge points where, and under what provenance. Sorted so two runs compare as
// sets rather than as traversal orders, and carrying `via` so a bound
// registration is never mistaken for the placeholder it replaced.
func frameworkEdgeSet(g *graph.Graph) []string {
	edges := g.AllEdges()
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		via, _ := e.Meta["via"].(string)
		out = append(out, fmt.Sprintf("%s -[%s]-> %s via=%s", e.From, e.Kind, e.To, via))
	}
	sort.Strings(out)
	return out
}

func frameworkPassRow(t *testing.T, report resolver.FrameworkSynthReport, name string) resolver.SynthCount {
	t.Helper()
	for _, p := range report.Per {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("precondition: %s is not a registered pass under this name", name)
	return resolver.SynthCount{}
}

// TestWorkspaceScopedUnionIsOutputPreserving runs the same scoped synthesis
// twice from an identical pre-state — once with the daemon-wide union this
// change replaced, once with the workspace-scoped fold — and requires the two
// graphs to be identical.
func TestWorkspaceScopedUnionIsOutputPreserving(t *testing.T) {
	mi := &MultiIndexer{
		graph:  graph.New(),
		logger: zap.NewNop(),
		indexers: map[string]*Indexer{
			"local":  frameworkGateTestIndexer("his", "odoo"),
			"gortex": frameworkGateTestIndexer("gortex", resolver.SynthFnValueCallback),
		},
	}
	scope := map[string]bool{"local": true}
	files := []string{"local/svc/handlers.py"}

	union := mi.allowedFrameworks()
	scoped := mi.allowedFrameworksForScope(scope)
	byRepo := mi.allowedFrameworksByRepo()

	require.True(t, union.Allows(resolver.SynthFnValueCallback),
		"precondition: the daemon-wide union admits the sibling workspace's pass")
	require.False(t, scoped.Allows(resolver.SynthFnValueCallback),
		"precondition: the workspace fold refuses it for a his-workspace run")

	wide, narrow := frameworkUnionOracleFixture(), frameworkUnionOracleFixture()
	run := func(g *graph.Graph, allowed frameworkgate.Set) resolver.FrameworkSynthReport {
		return resolver.RunFrameworkSynthesizersScopedForFiles(g, scope, files, false,
			resolver.WithAllowedFrameworks(allowed),
			resolver.WithFrameworkAllowByRepo(byRepo))
	}
	wideReport, narrowReport := run(wide, union), run(narrow, scoped)

	wideRow := frameworkPassRow(t, wideReport, resolver.SynthFnValueCallback)
	narrowRow := frameworkPassRow(t, narrowReport, resolver.SynthFnValueCallback)

	require.False(t, wideRow.Disabled,
		"precondition: the wide run must EXECUTE the sibling workspace's pass, or there is nothing to preserve")
	require.True(t, narrowRow.Disabled,
		"the workspace fold must not execute a pass only another workspace asks for")

	// The anti-vacuity assertion, and the reason this test has teeth. Without
	// it a fixture that stopped matching the pass's placeholder contract would
	// compare two runs that both did nothing, and pass forever.
	require.Positive(t, wideRow.RepoGated,
		"precondition: the wide run must STAGE an edge the per-repo gate then refuses — "+
			"the whole claim is that the gate already catches what the fold now skips")

	assert.Equal(t, frameworkEdgeSet(wide), frameworkEdgeSet(narrow),
		"narrowing the allow-list by workspace changed the graph: a pass only another "+
			"workspace asks for landed an edge here that the per-repo gate did not refuse")
}
