package store_sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func danglingFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, n := range []struct{ id, repo, file string }{
		{"local/a.py", "local", "local/a.py"},
		{"local/a.py::Live", "local", "local/a.py"},
		{"local@wt/a.py", "local@wt", "local@wt/a.py"},
		{"local@wt/a.py::Live", "local@wt", "local@wt/a.py"},
	} {
		s.AddNode(&graph.Node{
			ID: n.id, Kind: graph.KindType, Name: "Live",
			FilePath: n.file, Language: "python", RepoPrefix: n.repo,
		})
	}
	edges := []*graph.Edge{
		{From: "local/a.py::Live", To: "local/a.py::Live", Kind: graph.EdgeReferences},
		{From: "local/a.py::Live", To: "local/gone.py::Gone", Kind: graph.EdgeReferences},
		{From: "local/a.py::Live", To: "local::synthetic::Gone", Kind: graph.EdgeExtends},
		{From: "local/a.py::Live", To: "local/other.py::Gone", Kind: graph.EdgeCalls},
		{From: "local@wt/a.py::Live", To: "local@wt/gone.py::Gone", Kind: graph.EdgeReferences},
	}
	for _, e := range edges {
		e.Confidence = 1
		e.Origin = graph.OriginASTResolved
		s.AddEdge(e)
	}
	return s
}

// The kind predicate is written `+e.kind IN (…)` so the planner cannot use it
// as an index constraint and is left with the to_id range. Dropping the plus
// costs nothing visible — same rows, same test outcome — and turns a 0.39s
// query into a 48s one on a real store, because the planner then drives from
// edges_by_kind and scans whole kind ranges. Only the plan shows it.
func TestDanglingEdgeTargetsQueryUsesTargetIndex(t *testing.T) {
	s := danglingFixture(t)

	// The leading argument is the view generation the query gained when the
	// payload tables were keyed by it; the plan assertions below are what prove
	// the added predicate did not cost the to_id range scan.
	plan := queryPlan(t, s, danglingEdgeTargetsQuery(),
		int64(0), "local/", "local0", `["references","extends"]`)
	if !strings.Contains(plan, "edges_by_to") {
		t.Fatalf("dangling-target plan does not use edges_by_to:\n%s", plan)
	}
	if strings.Contains(plan, "edges_by_kind") {
		t.Fatalf("dangling-target plan drives from edges_by_kind, which scans whole kind ranges:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN edges") || strings.Contains(plan, "SCAN e ") {
		t.Fatalf("dangling-target plan scans edges instead of ranging the target index:\n%s", plan)
	}
}

func TestDanglingEdgeTargets(t *testing.T) {
	s := danglingFixture(t)

	got := s.DanglingEdgeTargets(
		[]string{"local/", "local::"},
		[]graph.EdgeKind{graph.EdgeReferences, graph.EdgeExtends},
	)
	want := []string{"local/gone.py::Gone", "local::synthetic::Gone"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dangling targets = %v, want %v", got, want)
	}
}

// A sibling checkout's prefix starts with this one's — `local@wt/…` begins with
// `local` — so a sweep anchored on the bare prefix would pull the sibling's
// dangling targets into this repository's frontier. Both id grammars are named
// explicitly for exactly this reason.
func TestDanglingEdgeTargetsExcludesSiblingCheckout(t *testing.T) {
	s := danglingFixture(t)

	for _, id := range s.DanglingEdgeTargets(
		[]string{"local/", "local::"},
		[]graph.EdgeKind{graph.EdgeReferences, graph.EdgeExtends, graph.EdgeCalls},
	) {
		if strings.HasPrefix(id, "local@wt") {
			t.Fatalf("sweep of local reached sibling checkout target %q", id)
		}
	}
}

// The SQL and the generic fallback answer the same question. A backend without
// the capability that silently skipped un-binding would leave references
// pointing at deleted declarations — the failure the sweep exists to prevent —
// so the two paths are pinned to each other rather than each to its own
// expectations.
func TestDanglingEdgeTargetsMatchesGenericFallback(t *testing.T) {
	s := danglingFixture(t)
	kinds := []graph.EdgeKind{graph.EdgeReferences, graph.EdgeExtends, graph.EdgeCalls}
	prefixes := []string{"local/", "local::"}

	mem := graph.New()
	for node := range s.NodesByKind(graph.KindType) {
		mem.AddNode(node)
	}
	for _, kind := range kinds {
		for edge := range s.EdgesByKind(kind) {
			mem.AddEdge(edge)
		}
	}

	backend := s.DanglingEdgeTargets(prefixes, kinds)
	fallback := graph.DanglingEdgeTargets(mem, prefixes, kinds)
	if strings.Join(backend, ",") != strings.Join(fallback, ",") {
		t.Fatalf("backend %v and fallback %v disagree", backend, fallback)
	}
}
