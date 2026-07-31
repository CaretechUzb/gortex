package query

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type substringProbeStore struct {
	*graph.Graph

	scanCalls        int
	pageCalls        int
	maxPage          int
	allNodesCalls    int
	hydrationBatches [][]string
	revisions        []uint64
	revisionCalls    int
}

func (s *substringProbeStore) ScanNodeSearchKeys(
	ctx context.Context,
	pageSize int,
	yield func([]graph.NodeSearchKey) bool,
) error {
	s.scanCalls++
	return s.Graph.ScanNodeSearchKeys(ctx, pageSize, func(page []graph.NodeSearchKey) bool {
		s.pageCalls++
		if len(page) > s.maxPage {
			s.maxPage = len(page)
		}
		return yield(page)
	})
}

func (s *substringProbeStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.hydrationBatches = append(s.hydrationBatches, append([]string(nil), ids...))
	return s.Graph.GetNodesByIDs(ids)
}

func (s *substringProbeStore) AllNodes() []*graph.Node {
	s.allNodesCalls++
	return s.Graph.AllNodes()
}

func (s *substringProbeStore) MutationRevision() uint64 {
	s.revisionCalls++
	if len(s.revisions) == 0 {
		return s.Graph.MutationRevision()
	}
	idx := s.revisionCalls - 1
	if idx >= len(s.revisions) {
		return s.revisions[len(s.revisions)-1]
	}
	return s.revisions[idx]
}

func newSubstringGraph(nodes ...*graph.Node) *graph.Graph {
	g := graph.New()
	for _, node := range nodes {
		g.AddNode(node)
	}
	return g
}

func substringNodeIDs(nodes []*graph.Node) []string {
	ids := make([]string, len(nodes))
	for i, node := range nodes {
		ids[i] = node.ID
	}
	return ids
}

// legacySubstringSearch reproduces the pre-paging implementation. Keeping the
// oracle in the test makes ranking changes visible independently of scan order.
func legacySubstringSearch(r graph.Reader, query string, limit int) []*graph.Node {
	if limit <= 0 {
		return nil
	}
	lower := strings.ToLower(query)
	type scored struct {
		node  *graph.Node
		score int
	}
	var results []scored
	seen := make(map[string]bool)
	for _, node := range r.FindNodesByName(query) {
		if node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			continue
		}
		seen[node.ID] = true
		results = append(results, scored{node: node, score: 0})
	}
	for _, node := range r.AllNodes() {
		if seen[node.ID] || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			continue
		}
		score := 0
		switch {
		case hasPrefixFold(node.Name, lower):
			score = 1
		case containsFold(node.Name, lower):
			score = 2
		case containsFold(node.ID, lower):
			score = 3
		default:
			continue
		}
		seen[node.ID] = true
		results = append(results, scored{node: node, score: score})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score < results[j].score
		}
		if len(results[i].node.Name) != len(results[j].node.Name) {
			return len(results[i].node.Name) < len(results[j].node.Name)
		}
		return results[i].node.ID < results[j].node.ID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]*graph.Node, len(results))
	for i := range results {
		out[i] = results[i].node
	}
	return out
}

func TestSearchSubstringMatchesLegacyRanking(t *testing.T) {
	g := newSubstringGraph(
		&graph.Node{ID: "exact", Kind: graph.KindFunction, Name: "needle"},
		&graph.Node{ID: "prefix", Kind: graph.KindMethod, Name: "NeedleWorker"},
		&graph.Node{ID: "contains", Kind: graph.KindType, Name: "useNEEDLEValue"},
		&graph.Node{ID: "repo::needle::id-only", Kind: graph.KindFunction, Name: "Elsewhere"},
		&graph.Node{ID: "unicode-prefix", Kind: graph.KindFunction, Name: "Ångström"},
		&graph.Node{ID: "unicode-contains", Kind: graph.KindFunction, Name: "preÅNGpost"},
		&graph.Node{ID: "tie-z", Kind: graph.KindFunction, Name: "xfoo"},
		&graph.Node{ID: "tie-a", Kind: graph.KindFunction, Name: "yfoo"},
		&graph.Node{ID: "needle.go", Kind: graph.KindFile, Name: "needle"},
		&graph.Node{ID: "import::needle", Kind: graph.KindImport, Name: "needle"},
		&graph.Node{ID: "unrelated", Kind: graph.KindVariable, Name: "Other"},
	)
	engine := NewEngine(g)
	for _, tc := range []struct {
		query string
		limit int
	}{
		{query: "needle", limit: 20},
		{query: "ång", limit: 20},
		{query: "foo", limit: 1},
		{query: "foo", limit: 20},
		{query: "missing", limit: 20},
	} {
		t.Run(fmt.Sprintf("%s/limit=%d", tc.query, tc.limit), func(t *testing.T) {
			want := substringNodeIDs(legacySubstringSearch(g, tc.query, tc.limit))
			got := substringNodeIDs(engine.searchSubstring(tc.query, tc.limit))
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("searchSubstring(%q, %d) = %v, legacy = %v", tc.query, tc.limit, got, want)
			}
		})
	}
}

func TestSearchSubstringCaseFoldIDAndExcludedKinds(t *testing.T) {
	g := newSubstringGraph(
		&graph.Node{ID: "exact", Kind: graph.KindFunction, Name: "needle"},
		&graph.Node{ID: "prefix-short", Kind: graph.KindFunction, Name: "NeedleX"},
		&graph.Node{ID: "prefix-long", Kind: graph.KindFunction, Name: "NEEDLEHandler"},
		&graph.Node{ID: "name-substring", Kind: graph.KindFunction, Name: "useNEEDLE"},
		&graph.Node{ID: "repo::needle::id-only", Kind: graph.KindFunction, Name: "Elsewhere"},
		&graph.Node{ID: "needle.go", Kind: graph.KindFile, Name: "needle"},
		&graph.Node{ID: "import::needle", Kind: graph.KindImport, Name: "needle"},
	)
	got := substringNodeIDs(NewEngine(g).searchSubstring("needle", 20))
	want := []string{"exact", "prefix-short", "prefix-long", "name-substring", "repo::needle::id-only"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
}

func TestSearchSubstringUnicodeMatchesCurrentFoldHelpers(t *testing.T) {
	g := newSubstringGraph(
		&graph.Node{ID: "unicode-prefix", Kind: graph.KindFunction, Name: "Ångström"},
		&graph.Node{ID: "unicode-substring", Kind: graph.KindFunction, Name: "preÅNGpost"},
		&graph.Node{ID: "length-changing-fold", Kind: graph.KindFunction, Name: "Kelvin"},
	)
	engine := NewEngine(g)
	if got, want := substringNodeIDs(engine.searchSubstring("ång", 20)), []string{"unicode-prefix", "unicode-substring"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Unicode fold results = %v, want %v", got, want)
	}
	if got := engine.searchSubstring("kel", 20); len(got) != 0 {
		t.Fatalf("length-changing Kelvin fold unexpectedly matched: %v", substringNodeIDs(got))
	}
}

func TestSearchSubstringTiesUseByteLengthThenID(t *testing.T) {
	g := newSubstringGraph(
		&graph.Node{ID: "z-short", Kind: graph.KindFunction, Name: "xfoo"},
		&graph.Node{ID: "a-short", Kind: graph.KindFunction, Name: "yfoo"},
		&graph.Node{ID: "m-short", Kind: graph.KindFunction, Name: "wfoo"},
		&graph.Node{ID: "a-long", Kind: graph.KindFunction, Name: "éfoo"},
	)
	got := substringNodeIDs(NewEngine(g).searchSubstring("foo", 20))
	want := []string{"a-short", "m-short", "z-short", "a-long"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("results = %v, want byte-length then ID order %v", got, want)
	}
}

func TestSearchSubstringHonorsLimit(t *testing.T) {
	g := newSubstringGraph(
		&graph.Node{ID: "exact", Kind: graph.KindFunction, Name: "needle"},
		&graph.Node{ID: "prefix", Kind: graph.KindFunction, Name: "NeedleWorker"},
		&graph.Node{ID: "substring", Kind: graph.KindFunction, Name: "useNeedle"},
	)
	engine := NewEngine(g)
	if got, want := substringNodeIDs(engine.searchSubstring("needle", 2)), []string{"exact", "prefix"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("limited results = %v, want %v", got, want)
	}
	if got := engine.searchSubstring("needle", 0); got != nil {
		t.Fatalf("zero limit returned %v, want nil", substringNodeIDs(got))
	}
	if got := engine.searchSubstring("needle", -1); got != nil {
		t.Fatalf("negative limit returned %v, want nil", substringNodeIDs(got))
	}
}

func TestSearchSubstringBoundsPagesAndHydration(t *testing.T) {
	g := graph.New()
	for i := 0; i < substringSearchPageSize*3+17; i++ {
		g.AddNode(&graph.Node{
			ID:   fmt.Sprintf("bulk/%04d", i),
			Kind: graph.KindFunction,
			Name: fmt.Sprintf("candidate-%04d-needle", i),
			Meta: map[string]any{"payload": strings.Repeat("x", 256)},
		})
	}
	g.AddNode(&graph.Node{ID: "winner-exact", Kind: graph.KindFunction, Name: "needle"})
	g.AddNode(&graph.Node{ID: "winner-prefix", Kind: graph.KindFunction, Name: "NeedleX"})

	probe := &substringProbeStore{Graph: g}
	const limit = 11
	got := NewEngine(probe).searchSubstring("needle", limit)
	want := legacySubstringSearch(g, "needle", limit)
	if gotIDs, wantIDs := substringNodeIDs(got), substringNodeIDs(want); fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("top-K results = %v, legacy = %v", gotIDs, wantIDs)
	}
	if probe.scanCalls != 1 {
		t.Fatalf("scan calls = %d, want 1", probe.scanCalls)
	}
	if probe.pageCalls < 4 {
		t.Fatalf("page callbacks = %d, want at least 4", probe.pageCalls)
	}
	if probe.maxPage > substringSearchPageSize {
		t.Fatalf("largest page = %d, want <= %d", probe.maxPage, substringSearchPageSize)
	}
	if probe.allNodesCalls != 0 {
		t.Fatalf("AllNodes calls = %d, want 0", probe.allNodesCalls)
	}
	if len(probe.hydrationBatches) != 1 {
		t.Fatalf("hydration calls = %d, want 1", len(probe.hydrationBatches))
	}
	if got := len(probe.hydrationBatches[0]); got != limit {
		t.Fatalf("hydrated IDs = %d, want exactly top-K limit %d", got, limit)
	}
}

func TestSearchSubstringRetriesExactlyOnceAfterMutation(t *testing.T) {
	g := newSubstringGraph(
		&graph.Node{ID: "match", Kind: graph.KindFunction, Name: "Needle"},
		&graph.Node{ID: "other", Kind: graph.KindFunction, Name: "Other"},
	)
	// The first attempt sees a stable scan (7 -> 7), hydrates, then observes
	// revision 8. The second attempt starts at 8 and is returned without a
	// third attempt even though the implementation deliberately caps retries.
	probe := &substringProbeStore{Graph: g, revisions: []uint64{7, 7, 8, 8}}
	got := NewEngine(probe).searchSubstring("needle", 5)
	if gotIDs, want := substringNodeIDs(got), []string{"match"}; fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("results = %v, want %v", gotIDs, want)
	}
	if probe.scanCalls != 2 {
		t.Fatalf("scan calls = %d, want 2 (one retry)", probe.scanCalls)
	}
	if len(probe.hydrationBatches) != 2 {
		t.Fatalf("hydration calls = %d, want 2 (one retry)", len(probe.hydrationBatches))
	}
	if probe.revisionCalls != 4 {
		t.Fatalf("revision calls = %d, want 4", probe.revisionCalls)
	}
}
