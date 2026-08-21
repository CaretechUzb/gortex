package graph

import (
	"fmt"
	"iter"
	"reflect"
	"sort"
	"testing"
)

const (
	consistencyRepo    = "repo"
	consistencyKeepFil = "repo/keep.go"
	consistencyEditFil = "repo/edit.go"
	consistencyKeepID  = consistencyKeepFil + "::Keeper"
	consistencyKeptID  = consistencyEditFil + "::Kept"
	consistencyGoneID  = consistencyEditFil + "::Gone"
)

// consistencyFixture builds a base graph with one untouched file and
// one file the overlay covers, plus the layer that replaces one symbol
// in the covered file under the same ID and hides the other.
//
//	base: Keeper -> Kept      (unchanged source, covered target re-emitted)
//	      Keeper -> Gone      (unchanged source, covered target hidden)
//	      Kept   -> Keeper    (covered source: replaced by the layer's edge)
//	layer: Kept' -> Keeper    (the overlay's own version of the call)
func consistencyFixture(t *testing.T) (*Graph, *OverlayLayer, *Node) {
	t.Helper()
	base := New()
	base.AddNode(&Node{ID: consistencyKeepID, Name: "Keeper", Kind: KindFunction, FilePath: consistencyKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&Node{ID: consistencyKeptID, Name: "Kept", Kind: KindFunction, FilePath: consistencyEditFil, RepoPrefix: consistencyRepo, StartLine: 10})
	base.AddNode(&Node{ID: consistencyGoneID, Name: "Gone", Kind: KindFunction, FilePath: consistencyEditFil, RepoPrefix: consistencyRepo})
	base.AddEdge(&Edge{From: consistencyKeepID, To: consistencyKeptID, Kind: EdgeCalls, FilePath: consistencyKeepFil, Line: 10})
	base.AddEdge(&Edge{From: consistencyKeepID, To: consistencyGoneID, Kind: EdgeCalls, FilePath: consistencyKeepFil, Line: 11})
	base.AddEdge(&Edge{From: consistencyKeptID, To: consistencyKeepID, Kind: EdgeCalls, FilePath: consistencyEditFil, Line: 20})

	replacement := &Node{
		ID: consistencyKeptID, Name: "Kept", Kind: KindFunction,
		FilePath: consistencyEditFil, RepoPrefix: consistencyRepo,
		StartLine: 40,
	}
	layer := NewOverlayLayer()
	layer.MarkFile(consistencyEditFil, false)
	layer.AddNode(consistencyEditFil, replacement)
	layer.MarkRemoved("Gone", consistencyGoneID)
	layer.AddEdge(&Edge{From: consistencyKeptID, To: consistencyKeepID, Kind: EdgeCalls, FilePath: consistencyEditFil, Line: 21})
	return base, layer, replacement
}

func nodeIDs(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			out = append(out, n.ID)
		}
	}
	sort.Strings(out)
	return out
}

// overlayEdgeKey renders one edge's identity for set comparison across
// the point, batched and bulk readers.
func overlayEdgeKey(e *Edge) string {
	return fmt.Sprintf("%s->%s|%s|%s:%d", e.From, e.To, e.Kind, e.FilePath, e.Line)
}

func edgeKeys(edges []*Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			out = append(out, overlayEdgeKey(e))
		}
	}
	sort.Strings(out)
	return out
}

// TestOverlaidViewReEmittedNodeSurfacesOnce pins the duplicate-identity
// case: when the overlay re-emits a node under an ID base already has,
// every node reader must return exactly one copy — the layer's payload,
// not base's.
func TestOverlaidViewReEmittedNodeSurfacesOnce(t *testing.T) {
	base, layer, replacement := consistencyFixture(t)
	view := NewOverlaidView(base, layer)

	readers := map[string]func() []*Node{
		"GetRepoNodes": func() []*Node { return view.GetRepoNodes(consistencyRepo) },
		"AllNodes":     view.AllNodes,
		"GetFileNodes": func() []*Node { return view.GetFileNodes(consistencyEditFil) },
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			var hits []*Node
			for _, n := range read() {
				if n != nil && n.ID == consistencyKeptID {
					hits = append(hits, n)
				}
			}
			if len(hits) != 1 {
				t.Fatalf("%s returned %d copies of %s, want exactly 1", name, len(hits), consistencyKeptID)
			}
			if hits[0] != replacement {
				t.Fatalf("%s returned base's payload (line %d), want the layer's (line %d)",
					name, hits[0].StartLine, replacement.StartLine)
			}
		})
	}

	// The bulk readers must also agree with the point lookups over the
	// visible ID set: no hidden symbol resurfaces, nothing is dropped.
	wantVisible := []string{consistencyKeepID, consistencyKeptID}
	sort.Strings(wantVisible)
	if got := nodeIDs(view.GetRepoNodes(consistencyRepo)); !reflect.DeepEqual(got, wantVisible) {
		t.Fatalf("GetRepoNodes = %v, want %v", got, wantVisible)
	}
	if got := nodeIDs(view.AllNodes()); !reflect.DeepEqual(got, wantVisible) {
		t.Fatalf("AllNodes = %v, want %v", got, wantVisible)
	}
	for _, id := range wantVisible {
		if view.GetNode(id) == nil {
			t.Fatalf("GetNode(%q) = nil but the bulk readers list it", id)
		}
	}
	if view.GetNode(consistencyGoneID) != nil {
		t.Fatalf("GetNode(%q) resurfaced a symbol the overlay hid", consistencyGoneID)
	}
}

// TestOverlaidViewEdgeReadersAgree pins the point / batched / bulk
// contract: all three expose the same visible-edge relation, including
// the covered-target-re-emitted and covered-target-hidden cases.
func TestOverlaidViewEdgeReadersAgree(t *testing.T) {
	base, layer, _ := consistencyFixture(t)
	view := NewOverlaidView(base, layer)
	visible := []string{consistencyKeepID, consistencyKeptID}
	hidden := consistencyGoneID

	t.Run("out edges by source", func(t *testing.T) {
		cases := []struct {
			name   string
			source string
			want   []string
		}{
			{
				name:   "unchanged source keeps the edge to a re-emitted target",
				source: consistencyKeepID,
				want: []string{overlayEdgeKey(&Edge{From: consistencyKeepID, To: consistencyKeptID,
					Kind: EdgeCalls, FilePath: consistencyKeepFil, Line: 10})},
			},
			{
				name:   "covered source uses only the layer's edges",
				source: consistencyKeptID,
				want: []string{overlayEdgeKey(&Edge{From: consistencyKeptID, To: consistencyKeepID,
					Kind: EdgeCalls, FilePath: consistencyEditFil, Line: 21})},
			},
			{
				name:   "hidden source has no edges at all",
				source: hidden,
				want:   []string{},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := edgeKeys(view.GetOutEdges(tc.source)); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("GetOutEdges(%q) = %v, want %v", tc.source, got, tc.want)
				}
			})
		}
	})

	t.Run("in edges by target", func(t *testing.T) {
		cases := []struct {
			name   string
			target string
			want   []string
		}{
			{
				name:   "base edge from a covered source is replaced by the layer's",
				target: consistencyKeepID,
				want: []string{overlayEdgeKey(&Edge{From: consistencyKeptID, To: consistencyKeepID,
					Kind: EdgeCalls, FilePath: consistencyEditFil, Line: 21})},
			},
			{
				name:   "re-emitted target keeps its base in-edge",
				target: consistencyKeptID,
				want: []string{overlayEdgeKey(&Edge{From: consistencyKeepID, To: consistencyKeptID,
					Kind: EdgeCalls, FilePath: consistencyKeepFil, Line: 10})},
			},
			{
				name:   "hidden target drops its base in-edge",
				target: hidden,
				want:   []string{},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := edgeKeys(view.GetInEdges(tc.target)); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("GetInEdges(%q) = %v, want %v", tc.target, got, tc.want)
				}
			})
		}
	})

	t.Run("batched matches the point loop", func(t *testing.T) {
		ids := append(append([]string{}, visible...), hidden)
		batchedOut := view.GetOutEdgesByNodeIDs(ids)
		batchedIn := view.GetInEdgesByNodeIDs(ids)
		for _, id := range ids {
			if got, want := edgeKeys(batchedOut[id]), edgeKeys(view.GetOutEdges(id)); !reflect.DeepEqual(got, want) {
				t.Fatalf("GetOutEdgesByNodeIDs[%q] = %v, GetOutEdges = %v", id, got, want)
			}
			if got, want := edgeKeys(batchedIn[id]), edgeKeys(view.GetInEdges(id)); !reflect.DeepEqual(got, want) {
				t.Fatalf("GetInEdgesByNodeIDs[%q] = %v, GetInEdges = %v", id, got, want)
			}
		}
	})

	t.Run("bulk matches the union of the point reads", func(t *testing.T) {
		var union []*Edge
		for _, n := range view.AllNodes() {
			union = append(union, view.GetOutEdges(n.ID)...)
		}
		if got, want := edgeKeys(view.AllEdges()), edgeKeys(union); !reflect.DeepEqual(got, want) {
			t.Fatalf("AllEdges = %v, union of GetOutEdges over visible sources = %v", got, want)
		}
		// The in-edge side must cover the same relation.
		var inUnion []*Edge
		for _, n := range view.AllNodes() {
			inUnion = append(inUnion, view.GetInEdges(n.ID)...)
		}
		if got, want := edgeKeys(inUnion), edgeKeys(view.AllEdges()); !reflect.DeepEqual(got, want) {
			t.Fatalf("union of GetInEdges = %v, AllEdges = %v", got, want)
		}
	})
}

// TestOverlaidViewEdgeReadersAgreeOnTombstonedFile is the tombstone
// variant: nothing in the covered file survives, in either direction.
func TestOverlaidViewEdgeReadersAgreeOnTombstonedFile(t *testing.T) {
	base, _, _ := consistencyFixture(t)
	layer := NewOverlayLayer()
	layer.MarkFile(consistencyEditFil, true)
	layer.MarkRemoved("Kept", consistencyKeptID)
	layer.MarkRemoved("Gone", consistencyGoneID)
	view := NewOverlaidView(base, layer)

	if got := nodeIDs(view.AllNodes()); !reflect.DeepEqual(got, []string{consistencyKeepID}) {
		t.Fatalf("AllNodes over a tombstoned file = %v, want only %v", got, consistencyKeepID)
	}
	if got := edgeKeys(view.AllEdges()); len(got) != 0 {
		t.Fatalf("AllEdges over a tombstoned file = %v, want none", got)
	}
	for _, id := range []string{consistencyKeepID, consistencyKeptID, consistencyGoneID} {
		if got := view.GetOutEdges(id); len(got) != 0 {
			t.Fatalf("GetOutEdges(%q) = %v, want none", id, edgeKeys(got))
		}
		if got := view.GetInEdges(id); len(got) != 0 {
			t.Fatalf("GetInEdges(%q) = %v, want none", id, edgeKeys(got))
		}
	}
	batchedOut := view.GetOutEdgesByNodeIDs([]string{consistencyKeepID, consistencyKeptID})
	batchedIn := view.GetInEdgesByNodeIDs([]string{consistencyKeepID, consistencyKeptID})
	for _, id := range []string{consistencyKeepID, consistencyKeptID} {
		if len(batchedOut[id]) != 0 || len(batchedIn[id]) != 0 {
			t.Fatalf("batched adjacency for %q survived a tombstone: out=%v in=%v",
				id, edgeKeys(batchedOut[id]), edgeKeys(batchedIn[id]))
		}
	}
}

// TestOverlayLayerAddNodeIsIdempotent pins the doc-comment's promise:
// re-adding the same (path, ID) replaces the node everywhere instead of
// leaving a second entry in the per-file or per-name index.
func TestOverlayLayerAddNodeIsIdempotent(t *testing.T) {
	const path = "repo/edit.go"
	first := &Node{ID: path + "::Fn", Name: "Fn", QualName: "repo.Fn", Kind: KindFunction, FilePath: path, StartLine: 1}
	second := &Node{ID: first.ID, Name: first.Name, QualName: first.QualName, Kind: KindFunction, FilePath: path, StartLine: 7}

	layer := NewOverlayLayer()
	layer.MarkFile(path, false)
	layer.AddNode(path, first)
	layer.AddNode(path, second)

	fileNodes := layer.nodesForFile(path)
	if len(fileNodes) != 1 || fileNodes[0] != second {
		t.Fatalf("file index = %#v, want exactly the second node", nodeIDs(fileNodes))
	}
	byName := layer.NodesByName("Fn")
	if len(byName) != 1 || byName[0] != second {
		t.Fatalf("name index holds %d entries, want exactly the second node", len(byName))
	}
	if got := layer.nodesByQual["repo.Fn"]; got != second {
		t.Fatalf("qual index = %#v, want the second node", got)
	}
	if got := layer.nodeByID[first.ID]; got != second {
		t.Fatalf("id index = %#v, want the second node", got)
	}

	// A view over the idempotent layer still reports one node.
	view := NewOverlaidView(New(), layer)
	if got := nodeIDs(view.GetFileNodes(path)); len(got) != 1 {
		t.Fatalf("GetFileNodes = %v, want a single entry", got)
	}
	if got := view.FindNodesByName("Fn"); len(got) != 1 {
		t.Fatalf("FindNodesByName returned %d entries, want 1", len(got))
	}
}

// TestOverlaidViewStatsReflectOverlay pins the counters: Stats and
// RepoStats report the overlay's totals, not base's, and agree with
// NodeCount / EdgeCount.
func TestOverlaidViewStatsReflectOverlay(t *testing.T) {
	base, layer, _ := consistencyFixture(t)

	baseStats := base.Stats()
	if baseStats.TotalNodes != 3 || baseStats.TotalEdges != 3 {
		t.Fatalf("fixture base stats = %d nodes / %d edges, want 3 / 3", baseStats.TotalNodes, baseStats.TotalEdges)
	}

	view := NewOverlaidView(base, layer)
	// The overlay hides one of the covered file's two symbols and
	// replaces the other, so one node and one edge go: base's
	// Keeper->Gone call loses its target and Kept's own call is
	// re-emitted by the layer.
	if got := view.NodeCount(); got != 2 {
		t.Fatalf("NodeCount = %d, want 2", got)
	}
	if got := view.EdgeCount(); got != 2 {
		t.Fatalf("EdgeCount = %d, want 2", got)
	}
	stats := view.Stats()
	if stats.TotalNodes != view.NodeCount() || stats.TotalEdges != view.EdgeCount() {
		t.Fatalf("Stats = %d nodes / %d edges, want %d / %d",
			stats.TotalNodes, stats.TotalEdges, view.NodeCount(), view.EdgeCount())
	}
	// The breakdowns stay base-derived by design.
	if !reflect.DeepEqual(stats.ByKind, baseStats.ByKind) {
		t.Fatalf("Stats.ByKind = %v, want base's %v", stats.ByKind, baseStats.ByKind)
	}

	repoStats := view.RepoStats()[consistencyRepo]
	if repoStats.TotalNodes != 2 || repoStats.TotalEdges != 2 {
		t.Fatalf("RepoStats = %d nodes / %d edges, want 2 / 2", repoStats.TotalNodes, repoStats.TotalEdges)
	}
	if base.RepoStats()[consistencyRepo].TotalNodes != 3 {
		t.Fatalf("the overlay adjustment leaked into base's own RepoStats")
	}

	t.Run("added symbols raise the totals", func(t *testing.T) {
		added := NewOverlayLayer()
		added.MarkFile(consistencyEditFil, false)
		for _, n := range base.GetFileNodes(consistencyEditFil) {
			added.AddNode(consistencyEditFil, n)
		}
		fresh := &Node{
			ID: consistencyEditFil + "::Fresh", Name: "Fresh", Kind: KindFunction,
			FilePath: consistencyEditFil, RepoPrefix: consistencyRepo,
		}
		added.AddNode(consistencyEditFil, fresh)
		added.AddEdge(&Edge{From: fresh.ID, To: consistencyKeepID, Kind: EdgeCalls, FilePath: consistencyEditFil, Line: 30})
		addedView := NewOverlaidView(base, added)

		if got := addedView.NodeCount(); got != 4 {
			t.Fatalf("NodeCount after an overlay add = %d, want 4", got)
		}
		if got := addedView.Stats().TotalNodes; got != 4 {
			t.Fatalf("Stats.TotalNodes after an overlay add = %d, want 4", got)
		}
		repo := addedView.RepoStats()[consistencyRepo]
		if repo.TotalNodes != 4 {
			t.Fatalf("RepoStats.TotalNodes after an overlay add = %d, want 4", repo.TotalNodes)
		}
		// Base's own out-edge from the covered file is replaced by the
		// layer's single new call, so the repo nets one edge less.
		if repo.TotalEdges != addedView.EdgeCount() {
			t.Fatalf("RepoStats.TotalEdges = %d, EdgeCount = %d", repo.TotalEdges, addedView.EdgeCount())
		}
	})
}

const (
	churnKeepFil  = "repo/handler.go"
	churnEditFil  = "repo/morph.go"
	churnHandleID = churnKeepFil + "::Handler"
	churnConfigID = churnKeepFil + "::Config"
	churnMorphID  = churnEditFil + "::Morph"
	churnDropID   = churnEditFil + "::Dropped"
	churnFreshID  = churnEditFil + "::Fresh"
)

// kindChurnFixture is the kind-bounded readers' hard case: the overlay
// re-emits one covered symbol under the same ID but a *different* Kind,
// introduces a symbol of a third Kind, hides a fourth, and replaces the
// covered file's outgoing edges with edges of two kinds. A kind-bounded
// reader that trusted base's kind index alone would report the stale
// kind for the re-emitted ID.
func kindChurnFixture() (*Graph, *OverlayLayer) {
	base := New()
	base.AddNode(&Node{ID: churnHandleID, Name: "Handler", Kind: KindFunction, FilePath: churnKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&Node{ID: churnConfigID, Name: "Config", Kind: KindType, FilePath: churnKeepFil, RepoPrefix: consistencyRepo})
	base.AddNode(&Node{ID: churnMorphID, Name: "Morph", Kind: KindFunction, FilePath: churnEditFil, RepoPrefix: consistencyRepo, StartLine: 5})
	base.AddNode(&Node{ID: churnDropID, Name: "Dropped", Kind: KindType, FilePath: churnEditFil, RepoPrefix: consistencyRepo})
	base.AddEdge(&Edge{From: churnHandleID, To: churnMorphID, Kind: EdgeCalls, FilePath: churnKeepFil, Line: 3})
	base.AddEdge(&Edge{From: churnHandleID, To: churnDropID, Kind: EdgeReferences, FilePath: churnKeepFil, Line: 4})
	base.AddEdge(&Edge{From: churnMorphID, To: churnConfigID, Kind: EdgeReferences, FilePath: churnEditFil, Line: 6})

	layer := NewOverlayLayer()
	layer.MarkFile(churnEditFil, false)
	layer.AddNode(churnEditFil, &Node{ID: churnMorphID, Name: "Morph", Kind: KindMethod, FilePath: churnEditFil, RepoPrefix: consistencyRepo, StartLine: 50})
	layer.AddNode(churnEditFil, &Node{ID: churnFreshID, Name: "Fresh", Kind: KindType, FilePath: churnEditFil, RepoPrefix: consistencyRepo, StartLine: 60})
	layer.MarkRemoved("Dropped", churnDropID)
	layer.AddEdge(&Edge{From: churnMorphID, To: churnHandleID, Kind: EdgeCalls, FilePath: churnEditFil, Line: 51})
	layer.AddEdge(&Edge{From: churnFreshID, To: churnConfigID, Kind: EdgeReferences, FilePath: churnEditFil, Line: 61})
	return base, layer
}

// overlayNodeKey renders one node's identity *and* payload, so a
// comparison catches a reader that returned base's copy where the layer
// re-emitted the ID.
func overlayNodeKey(n *Node) string {
	return fmt.Sprintf("%s|%s|%s:%d", n.ID, n.Kind, n.FilePath, n.StartLine)
}

func nodeKeys(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			out = append(out, overlayNodeKey(n))
		}
	}
	sort.Strings(out)
	return out
}

func collectNodeSeq(seq iter.Seq[*Node]) []*Node {
	var out []*Node
	for n := range seq {
		out = append(out, n)
	}
	return out
}

func collectEdgeSeq(seq iter.Seq[*Edge]) []*Edge {
	var out []*Edge
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func nodesOfKind(nodes []*Node, kind NodeKind) []*Node {
	var out []*Node
	for _, n := range nodes {
		if n != nil && n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func edgesOfKind(edges []*Edge, kind EdgeKind) []*Edge {
	var out []*Edge
	for _, e := range edges {
		if e != nil && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// overlayKindFixtures are the (base, layer) pairs the kind-bounded
// assertions run over: an unlayered pass-through, a covered file whose
// symbols are partly re-emitted and partly hidden, a tombstoned file,
// and the kind-churn case.
func overlayKindFixtures(t *testing.T) map[string]func() (*Graph, *OverlayLayer) {
	t.Helper()
	return map[string]func() (*Graph, *OverlayLayer){
		"no layer": func() (*Graph, *OverlayLayer) {
			base, _, _ := consistencyFixture(t)
			return base, nil
		},
		"replaced file": func() (*Graph, *OverlayLayer) {
			base, layer, _ := consistencyFixture(t)
			return base, layer
		},
		"tombstoned file": func() (*Graph, *OverlayLayer) {
			base, _, _ := consistencyFixture(t)
			layer := NewOverlayLayer()
			layer.MarkFile(consistencyEditFil, true)
			layer.MarkRemoved("Kept", consistencyKeptID)
			layer.MarkRemoved("Gone", consistencyGoneID)
			return base, layer
		},
		"kind churn": kindChurnFixture,
	}
}

// TestOverlaidViewKindScansMatchBulkReads is the equivalence contract
// for the kind-bounded readers: NodesByKind(k) must be exactly AllNodes
// filtered to k, and EdgesByKind(k) exactly AllEdges filtered to k, for
// every kind either side carries — plus a kind nobody carries, which
// must come back empty rather than leaking base rows.
func TestOverlaidViewKindScansMatchBulkReads(t *testing.T) {
	for name, build := range overlayKindFixtures(t) {
		t.Run(name, func(t *testing.T) {
			base, layer := build()
			view := NewOverlaidView(base, layer)
			allNodes := view.AllNodes()
			allEdges := view.AllEdges()

			nodeKindSet := map[NodeKind]bool{KindImport: true}
			for _, n := range base.AllNodes() {
				nodeKindSet[n.Kind] = true
			}
			for _, n := range allNodes {
				nodeKindSet[n.Kind] = true
			}
			for kind := range nodeKindSet {
				got := nodeKeys(collectNodeSeq(view.NodesByKind(kind)))
				want := nodeKeys(nodesOfKind(allNodes, kind))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("NodesByKind(%q) = %v, AllNodes filtered = %v", kind, got, want)
				}
			}

			edgeKindSet := map[EdgeKind]bool{EdgeImports: true}
			for _, e := range base.AllEdges() {
				edgeKindSet[e.Kind] = true
			}
			for _, e := range allEdges {
				edgeKindSet[e.Kind] = true
			}
			for kind := range edgeKindSet {
				got := edgeKeys(collectEdgeSeq(view.EdgesByKind(kind)))
				want := edgeKeys(edgesOfKind(allEdges, kind))
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("EdgesByKind(%q) = %v, AllEdges filtered = %v", kind, got, want)
				}
			}
		})
	}
}

// TestOverlaidViewKindScansHonourOverlayPayload pins the two things the
// equivalence check can't state outright: a re-emitted ID comes back
// once carrying the layer's payload (including its new Kind), and a
// symbol the overlay hid never resurfaces through the kind index.
func TestOverlaidViewKindScansHonourOverlayPayload(t *testing.T) {
	base, layer := kindChurnFixture()
	view := NewOverlaidView(base, layer)

	// Base filed Morph under "function"; the layer re-emitted it as a
	// method. The stale kind must be gone and the fresh one present.
	wantFunctions := nodeKeys([]*Node{base.GetNode(churnHandleID)})
	if got := nodeKeys(collectNodeSeq(view.NodesByKind(KindFunction))); !reflect.DeepEqual(got, wantFunctions) {
		t.Fatalf("NodesByKind(function) = %v, want only the untouched Handler %v", got, wantFunctions)
	}
	methods := collectNodeSeq(view.NodesByKind(KindMethod))
	if len(methods) != 1 || methods[0].ID != churnMorphID {
		t.Fatalf("NodesByKind(method) = %v, want exactly the re-emitted %s", nodeKeys(methods), churnMorphID)
	}
	if methods[0].StartLine != 50 {
		t.Fatalf("NodesByKind(method) returned base's payload (line %d), want the layer's (line 50)", methods[0].StartLine)
	}

	// Dropped was hidden: it must be absent from its own kind scan, and
	// base's reference into it must be absent from the edge scan.
	for _, n := range collectNodeSeq(view.NodesByKind(KindType)) {
		if n.ID == churnDropID {
			t.Fatalf("NodesByKind(type) resurfaced the hidden %s", churnDropID)
		}
	}
	for _, e := range collectEdgeSeq(view.EdgesByKind(EdgeReferences)) {
		if e.To == churnDropID {
			t.Fatalf("EdgesByKind(references) kept an edge into the hidden %s", churnDropID)
		}
		if e.From == churnMorphID {
			t.Fatalf("EdgesByKind(references) kept base's edge out of the re-emitted %s", churnMorphID)
		}
	}
	calls := edgeKeys(collectEdgeSeq(view.EdgesByKind(EdgeCalls)))
	wantCalls := edgeKeys([]*Edge{
		{From: churnHandleID, To: churnMorphID, Kind: EdgeCalls, FilePath: churnKeepFil, Line: 3},
		{From: churnMorphID, To: churnHandleID, Kind: EdgeCalls, FilePath: churnEditFil, Line: 51},
	})
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("EdgesByKind(calls) = %v, want %v", calls, wantCalls)
	}
}

// TestOverlaidViewKindScansStopEarly pins the iterator contract the
// Reader doc-comment promises: a consumer that breaks out of the range
// stops the scan instead of draining both legs.
func TestOverlaidViewKindScansStopEarly(t *testing.T) {
	base, layer := kindChurnFixture()
	view := NewOverlaidView(base, layer)

	seen := 0
	for range view.NodesByKind(KindType) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("NodesByKind yielded %d nodes after an early break, want 1", seen)
	}
	seen = 0
	for range view.EdgesByKind(EdgeCalls) {
		seen++
		break
	}
	if seen != 1 {
		t.Fatalf("EdgesByKind yielded %d edges after an early break, want 1", seen)
	}
}
