package graph

import "testing"

// The empty repo prefix is overloaded, and the two meanings are easy to
// confuse because they share a signature.
//
//	WILDCARD  — "no repo filter, every repo". An argument convention.
//	            ChurnRows(""), GetRepoNonContentNodes(""), NodeIDNamesByKindsSeq("").
//	EXACT     — "the repo whose prefix is empty". A node property.
//	            GetRepoContentNodes(""), GetRepoNodes("").
//
// Only the EXACT family becomes unreachable once every repo carries a
// prefix; the WILDCARD family must survive untouched. Collapsing a wildcard
// into an exact match does not fail — it returns an empty slice, and the
// global pass built on it silently stops doing anything. These tests are the
// fence that turns that into a test failure instead.

func TestEmptyPrefixIsWildcardForChurnRows(t *testing.T) {
	g := New()
	if err := g.BulkSetChurn("r1", []ChurnEnrichment{{NodeID: "r1/a.go::A", RepoPrefix: "r1", CommitCount: 3}}); err != nil {
		t.Fatalf("BulkSetChurn r1: %v", err)
	}
	if err := g.BulkSetChurn("r2", []ChurnEnrichment{{NodeID: "r2/b.go::B", RepoPrefix: "r2", CommitCount: 5}}); err != nil {
		t.Fatalf("BulkSetChurn r2: %v", err)
	}

	if got := len(g.ChurnRows("")); got != 2 {
		t.Fatalf("ChurnRows(\"\") returned %d rows, want 2 (every repo) — the empty prefix is a WILDCARD here, "+
			"not an exact match on RepoPrefix==\"\"; narrowing it silently empties the global churn pass", got)
	}
	if got := len(g.ChurnRows("r1")); got != 1 {
		t.Fatalf("ChurnRows(\"r1\") returned %d rows, want 1", got)
	}
}

func TestEmptyPrefixIsWildcardForCoverageReleaseBlameRows(t *testing.T) {
	g := New()
	if err := g.BulkSetCoverage("r1", []CoverageEnrichment{{NodeID: "r1/a.go::A", RepoPrefix: "r1"}}); err != nil {
		t.Fatalf("BulkSetCoverage r1: %v", err)
	}
	if err := g.BulkSetCoverage("r2", []CoverageEnrichment{{NodeID: "r2/b.go::B", RepoPrefix: "r2"}}); err != nil {
		t.Fatalf("BulkSetCoverage r2: %v", err)
	}
	if err := g.BulkSetReleases("r1", []ReleaseEnrichment{{NodeID: "r1/a.go::A", RepoPrefix: "r1"}}); err != nil {
		t.Fatalf("BulkSetRelease r1: %v", err)
	}
	if err := g.BulkSetReleases("r2", []ReleaseEnrichment{{NodeID: "r2/b.go::B", RepoPrefix: "r2"}}); err != nil {
		t.Fatalf("BulkSetRelease r2: %v", err)
	}
	if err := g.BulkSetBlame("r1", []BlameEnrichment{{NodeID: "r1/a.go::A", RepoPrefix: "r1"}}); err != nil {
		t.Fatalf("BulkSetBlame r1: %v", err)
	}
	if err := g.BulkSetBlame("r2", []BlameEnrichment{{NodeID: "r2/b.go::B", RepoPrefix: "r2"}}); err != nil {
		t.Fatalf("BulkSetBlame r2: %v", err)
	}

	if got := len(g.CoverageRows("")); got != 2 {
		t.Errorf("CoverageRows(\"\") = %d rows, want 2 (wildcard)", got)
	}
	if got := len(g.ReleaseRows("")); got != 2 {
		t.Errorf("ReleaseRows(\"\") = %d rows, want 2 (wildcard)", got)
	}
	if got := len(g.BlameRows("")); got != 2 {
		t.Errorf("BlameRows(\"\") = %d rows, want 2 (wildcard)", got)
	}
}

func TestEmptyPrefixIsWildcardForNonContentNodes(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "r1/a.go::A", Kind: KindFunction, Name: "A", FilePath: "r1/a.go", RepoPrefix: "r1"})
	g.AddNode(&Node{ID: "r2/b.go::B", Kind: KindFunction, Name: "B", FilePath: "r2/b.go", RepoPrefix: "r2"})

	if got := len(g.GetRepoNonContentNodes("")); got != 2 {
		t.Fatalf("GetRepoNonContentNodes(\"\") = %d nodes, want 2 (every repo) — the empty prefix is a WILDCARD "+
			"here; the search-index build, embedding and language-detection passes all depend on it", got)
	}
	if got := len(g.GetRepoNonContentNodes("r1")); got != 1 {
		t.Fatalf("GetRepoNonContentNodes(\"r1\") = %d nodes, want 1", got)
	}
}

// TestEmptyPrefixIsExactForContentNodes is the counterpart: the sibling of
// GetRepoNonContentNodes in the SAME FILE means the opposite thing. Pinning
// both together is what keeps a future reader from "fixing" one to match the
// other.
func TestEmptyPrefixIsExactForContentNodes(t *testing.T) {
	g := New()
	owned := &Node{ID: "r1/doc.md#1", Kind: KindDoc, Name: "s", FilePath: "r1/doc.md", RepoPrefix: "r1"}
	owned.Meta = map[string]any{"data_class": "content"}
	unowned := &Node{ID: "doc.md#1", Kind: KindDoc, Name: "s", FilePath: "doc.md"}
	unowned.Meta = map[string]any{"data_class": "content"}
	g.AddNode(owned)
	g.AddNode(unowned)

	got := g.GetRepoContentNodes("")
	if len(got) != 1 || got[0].ID != unowned.ID {
		t.Fatalf("GetRepoContentNodes(\"\") = %d nodes %v, want exactly the RepoPrefix==\"\" node — "+
			"unlike its GetRepoNonContentNodes sibling, the empty prefix here is an EXACT match, not a wildcard",
			len(got), nodeIDsOf(got))
	}
	if r1 := g.GetRepoContentNodes("r1"); len(r1) != 1 || r1[0].ID != owned.ID {
		t.Fatalf("GetRepoContentNodes(\"r1\") = %v, want [%s]", nodeIDsOf(r1), owned.ID)
	}
}

func TestEmptyPrefixIsWildcardForNodeIDNamesByKindsSeq(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "r1/a.go::A", Kind: KindFunction, Name: "A", FilePath: "r1/a.go", RepoPrefix: "r1"})
	g.AddNode(&Node{ID: "r2/b.go::B", Kind: KindFunction, Name: "B", FilePath: "r2/b.go", RepoPrefix: "r2"})

	count := 0
	for range g.NodeIDNamesByKindsSeq("", KindFunction) {
		count++
	}
	if count != 2 {
		t.Fatalf("NodeIDNamesByKindsSeq(\"\") yielded %d, want 2 (every repo) — the empty prefix is a WILDCARD here", count)
	}

	scoped := 0
	for range g.NodeIDNamesByKindsSeq("r1", KindFunction) {
		scoped++
	}
	if scoped != 1 {
		t.Fatalf("NodeIDNamesByKindsSeq(\"r1\") yielded %d, want 1", scoped)
	}
}

func nodeIDsOf(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			out = append(out, n.ID)
		}
	}
	return out
}
