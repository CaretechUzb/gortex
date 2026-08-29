package indexer

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
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
