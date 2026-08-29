package graph

import "testing"

// The default has to be "nothing is a sibling". A store that was never
// wired, a test graph, and a workspace with no worktrees must all behave
// exactly as they did before checkout groups existed — suppressing edges
// on an unwired store would be a silent, graph-wide data loss.
func TestSiblingCheckouts_UngroupedStoreSuppressesNothing(t *testing.T) {
	g := New()
	if g.HasCheckoutGroups() {
		t.Fatal("a fresh graph must publish no checkout grouping")
	}
	if SiblingCheckouts(g, "local", "local-bench") {
		t.Fatal("no grouping published: no two prefixes may be siblings")
	}
	if SiblingCheckouts(struct{}{}, "local", "local-bench") {
		t.Fatal("a store without the capability must group nothing")
	}
}

func TestSiblingCheckouts_SameGroupDifferentPrefixes(t *testing.T) {
	g := New()
	g.SetCheckoutGroups(map[string]string{
		"local":       "/src/local",
		"local-bench": "/src/local",
		"other":       "/src/other",
	})

	if !SiblingCheckouts(g, "local", "local-bench") {
		t.Fatal("two prefixes sharing a checkout must be siblings")
	}
	if !SiblingCheckouts(g, "local-bench", "local") {
		t.Fatal("siblinghood is symmetric")
	}
	if SiblingCheckouts(g, "local", "other") {
		t.Fatal("independent repositories are not siblings")
	}
}

// A prefix compared with itself is the SAME checkout. Callers use the
// sibling test to reject cross-checkout work, so answering true here
// would make every same-repo edge illegal.
func TestSiblingCheckouts_SelfIsNotASibling(t *testing.T) {
	g := New()
	g.SetCheckoutGroups(map[string]string{"local": "/src/local", "local-bench": "/src/local"})
	if SiblingCheckouts(g, "local", "local") {
		t.Fatal("a prefix is not a sibling of itself")
	}
}

// dep::, external::, module:: and unresolved:: targets carry no repo
// prefix. They are visible from every repository and must never be
// suppressed.
func TestSiblingCheckouts_EmptyPrefixNeverMatches(t *testing.T) {
	g := New()
	g.SetCheckoutGroups(map[string]string{"local": "/src/local", "local-bench": "/src/local"})
	if SiblingCheckouts(g, "local", "") || SiblingCheckouts(g, "", "local") {
		t.Fatal("a repo-independent endpoint must never be treated as a sibling")
	}
}

// The map is a full replacement, not a merge: an untracked repository has
// to stop being anyone's sibling.
func TestSetCheckoutGroups_ReplacesRatherThanMerges(t *testing.T) {
	g := New()
	g.SetCheckoutGroups(map[string]string{"local": "/src/local", "local-bench": "/src/local"})
	g.SetCheckoutGroups(map[string]string{})

	if g.HasCheckoutGroups() {
		t.Fatal("publishing an empty grouping must clear the previous one")
	}
	if SiblingCheckouts(g, "local", "local-bench") {
		t.Fatal("an untracked worktree must stop being a sibling")
	}
}

func TestSetCheckoutGroups_IgnoresBlankEntries(t *testing.T) {
	g := New()
	g.SetCheckoutGroups(map[string]string{"": "/src/local", "local": ""})
	if g.HasCheckoutGroups() {
		t.Fatal("blank prefix or blank group carries no information")
	}
}

// This is the count crossRepoPossible gates on: a repo plus two worktrees
// of it is ONE repository, and no edge among the three can be a genuine
// cross-repo relationship.
func TestDistinctCheckoutGroups(t *testing.T) {
	g := New()
	g.SetCheckoutGroups(map[string]string{
		"local":  "/src/local",
		"wt-a":   "/src/local",
		"wt-b":   "/src/local",
		"addons": "/src/addons",
		"core":   "/src/addons",
	})

	if got := DistinctCheckoutGroups(g, []string{"local", "wt-a", "wt-b"}); got != 1 {
		t.Fatalf("a repo and its worktrees are one repository, got %d", got)
	}
	if got := DistinctCheckoutGroups(g, []string{"local", "wt-a", "addons"}); got != 2 {
		t.Fatalf("want 2 repositories, got %d", got)
	}
	// An ungrouped prefix counts as its own repository — the reading that
	// keeps an unwired workspace's count equal to its prefix count.
	if got := DistinctCheckoutGroups(g, []string{"unknown-a", "unknown-b"}); got != 2 {
		t.Fatalf("ungrouped prefixes are independent repositories, got %d", got)
	}
	if got := DistinctCheckoutGroups(g, []string{"local", "", "wt-a"}); got != 1 {
		t.Fatalf("the empty prefix is not a repository, got %d", got)
	}
}

func TestSiblingCheckoutIDs_ParsesPrefixesFromNodeIDs(t *testing.T) {
	g := New()
	g.SetCheckoutGroups(map[string]string{"local": "/src/local", "local-bench": "/src/local"})

	if !SiblingCheckoutIDs(g, "local/a.py::A", "local-bench/a.py::A") {
		t.Fatal("node IDs in two checkouts of one repo must read as siblings")
	}
	if SiblingCheckoutIDs(g, "local/a.py::A", "unresolved::A") {
		t.Fatal("a repo-independent target must not read as a sibling")
	}
}
