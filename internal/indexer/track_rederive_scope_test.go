package indexer

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// trackRederiveScopeFixture publishes a checkout grouping and a tracked
// repository set without indexing anything: rederiveScope reads only those
// two, and a real worktree on disk would make the test about git.
func trackRederiveScopeFixture(t *testing.T, groups map[string]string, tracked ...string) *MultiIndexer {
	t.Helper()
	mi, _ := newRederiveTestIndexer(t, 0)
	if g, ok := mi.graph.(*graph.Graph); ok {
		g.SetCheckoutGroups(groups)
	} else {
		t.Fatalf("test graph does not publish checkout groups: %T", mi.graph)
	}
	for _, prefix := range tracked {
		mi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix}
	}
	return mi
}

// The case the scoping exists for: a worktree of a repository already
// tracked has never been in the graph, so no neighbour lost an edge into
// it and no neighbour has anything to re-derive.
func TestRederiveScope_NarrowsToANewSiblingCheckout(t *testing.T) {
	mi := trackRederiveScopeFixture(t,
		map[string]string{"local": "grp", "local@wt": "grp"},
		"local", "local@wt", "odoo", "addons")

	scope := mi.rederiveScope(map[string]struct{}{"local@wt": {}})

	require.NotNil(t, scope, "a new sibling checkout must not re-derive the workspace")
	assert.Equal(t, map[string]struct{}{"local@wt": {}}, scope)
}

// A repository that is nobody's sibling keeps the whole-workspace
// frontier: the bindings a retracked repo loses are sourced in its
// neighbours, and only a whole-store pass mints those again.
func TestRederiveScope_KeepsWholeStoreForAnOrdinaryRepo(t *testing.T) {
	mi := trackRederiveScopeFixture(t,
		map[string]string{"local": "grp", "local@wt": "grp"},
		"local", "local@wt", "odoo")

	assert.Nil(t, mi.rederiveScope(map[string]struct{}{"odoo": {}}))
}

// All-or-nothing. A burst that adds a worktree AND an unrelated repository
// still owes the unrelated one a full derivation, so the whole pass widens.
func TestRederiveScope_OneNonSiblingWidensTheWholeFrontier(t *testing.T) {
	mi := trackRederiveScopeFixture(t,
		map[string]string{"local": "grp", "local@wt": "grp"},
		"local", "local@wt", "newthing")

	assert.Nil(t, mi.rederiveScope(map[string]struct{}{
		"local@wt": {}, "newthing": {},
	}))
}

// The overwhelmingly common workspace tracks no worktree at all, and must
// not pay a scope decision — or change behaviour — for the feature.
func TestRederiveScope_NilWithoutAnyCheckoutGrouping(t *testing.T) {
	mi := trackRederiveScopeFixture(t, nil, "odoo", "addons")

	assert.Nil(t, mi.rederiveScope(map[string]struct{}{"odoo": {}}))
	assert.Nil(t, mi.rederiveScope(nil))
}

// A repository is not its own sibling: a lone checkout in a published
// group must not scope itself out of a derivation it needs.
func TestRederiveScope_RepoIsNotItsOwnSibling(t *testing.T) {
	mi := trackRederiveScopeFixture(t,
		map[string]string{"local": "grp", "local@wt": "grp"},
		"local@wt")

	assert.Nil(t, mi.rederiveScope(map[string]struct{}{"local@wt": {}}))
}

// Coalescing must accumulate prefixes, not keep the first. A burst of
// tracks used to collapse to one pass named after whichever landed first;
// now that the name IS the frontier, dropping the rest would derive one
// repository and silently skip the others.
func TestScheduleWorkspaceRederive_CoalescesEveryTriggeringPrefix(t *testing.T) {
	mi, logs := newRederiveTestIndexer(t, 20*time.Millisecond)

	mi.scheduleWorkspaceRederive("repo-a")
	mi.scheduleWorkspaceRederive("repo-b")
	mi.WaitWorkspaceRederive()

	entries := logs.FilterMessage(rederiveStartLog).All()
	require.NotEmpty(t, entries)
	named := map[string]bool{}
	for _, e := range entries {
		for _, p := range strings.Split(e.ContextMap()["triggered_by"].(string), ",") {
			named[p] = true
		}
	}
	assert.True(t, named["repo-a"], "first triggering prefix must be derived")
	assert.True(t, named["repo-b"], "a prefix tracked during the debounce must be derived too")
}

// rederiveReason is the breadcrumb, and it is sorted so a two-repo burst
// reads the same in every log whichever track landed first.
func TestRederiveReason_IsSortedAndStable(t *testing.T) {
	assert.Equal(t, "", rederiveReason(nil))
	assert.Equal(t, "a", rederiveReason(map[string]struct{}{"a": {}}))
	assert.Equal(t, "a,b,c", rederiveReason(map[string]struct{}{
		"c": {}, "a": {}, "b": {},
	}))
}

// A repository tracked while a batch suppressed the global passes used to
// be lost outright on the warm-restart fast paths: nothing scheduled a
// derivation, and the next restart saw its nodes already on disk and took
// the same fast path again.
func TestDeferredWorkspaceRederive_IsHandedBackWhenNoBatchPassRan(t *testing.T) {
	mi, logs := newRederiveTestIndexer(t, 0)

	mi.deferWorkspaceRederive("repo-a")
	mi.deferWorkspaceRederive("repo-b")
	mi.deferWorkspaceRederive("repo-a") // idempotent

	assert.Equal(t, []string{"repo-a", "repo-b"}, mi.FlushDeferredWorkspaceRederive())
	mi.WaitWorkspaceRederive()
	require.NotEmpty(t, logs.FilterMessage(rederiveStartLog).All(),
		"a handed-back repository must actually be derived")

	assert.Nil(t, mi.FlushDeferredWorkspaceRederive(),
		"the set is taken, not copied — a second flush owes nothing")
}

// EndBatch's own pass IS that derivation, so it clears the set. Scheduling
// another would double every cold index.
func TestDeferredWorkspaceRederive_ClearedByABatchThatDerived(t *testing.T) {
	mi, _ := newRederiveTestIndexer(t, 0)

	mi.deferWorkspaceRederive("repo-a")
	mi.ClearDeferredWorkspaceRederive()

	assert.Nil(t, mi.FlushDeferredWorkspaceRederive())
}
