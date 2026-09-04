package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// newCrossRepoPlanFixture builds a store shaped like a real multi-repo
// workspace for the cross-repo candidate plan lock: two repositories, and
// — the part that matters — far more edges than nodes.
//
// The shared newPlanLockFixture cannot host this lock. It carries ~1200
// nodes against ~160 edges, and with edges that sparse the planner picks
// the edges-driven order whether or not the join is pinned, so the row
// passes on a query that would hang in production. A real store inverts
// that ratio (measured: 234k nodes, 572k base-kind edges), which is
// exactly when scanning `nodes` starts to look cheap to the planner.
//
// ANALYZE is equally load-bearing. Without planner statistics SQLite picks
// the right order on its own; production stores refresh statistics at the
// end of a cold index, so they run in the state that misleads it.
func newCrossRepoPlanFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cross_repo_plan.sqlite"))
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const perRepo = 1500
	const fanout = 4
	repos := []string{"repoA", "repoB"}
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < perRepo; i++ {
		for _, repo := range repos {
			file := fmt.Sprintf("%s/pkg/file%04d.go", repo, i)
			nodes = append(nodes, &graph.Node{
				ID: fmt.Sprintf("%s::sym%04d", file, i), Name: fmt.Sprintf("sym%04d", i),
				Kind: graph.KindFunction, FilePath: file, Language: "go", RepoPrefix: repo,
				StartLine: 1, EndLine: 9,
			})
		}
	}
	for i := 0; i < perRepo; i++ {
		from := fmt.Sprintf("repoA/pkg/file%04d.go::sym%04d", i, i)
		file := fmt.Sprintf("repoA/pkg/file%04d.go", i)
		for k := 0; k < fanout; k++ {
			// Overwhelmingly same-repo, as a real workspace is: the query
			// must stay cheap when almost every edge is a reject.
			target := (i + k + 1) % perRepo
			edges = append(edges, &graph.Edge{
				From: from, To: fmt.Sprintf("repoA/pkg/file%04d.go::sym%04d", target, target),
				Kind: graph.EdgeCalls, FilePath: file, Line: k + 1,
			})
		}
		if i%50 == 0 {
			edges = append(edges, &graph.Edge{
				From: from, To: fmt.Sprintf("repoB/pkg/file%04d.go::sym%04d", i, i),
				Kind: graph.EdgeCalls, FilePath: file, Line: 99,
			})
		}
	}
	s.AddBatch(nodes, edges)

	s.writeMu.Lock()
	statsErr := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if statsErr != nil {
		t.Fatalf("refresh planner stats: %v", statsErr)
	}
	return s
}

// The cross-repo candidate query joins BOTH endpoints of every base-kind
// edge against nodes. Left free to reorder, a stats-fed planner drives it
// from nodes twice — every node paired against every node — because
// nodes_by_repo is a covering partial index and `repo_prefix <> ”` reads
// as selective while excluding almost nothing.
//
// That plan ran past 745 seconds without finishing on a 117k-node
// single-repository workspace, and was still running after 330s on a
// two-repository one. Pinned, the same question answers in ~6s, returning
// 1,909 rows out of 572,733 base-kind edges — the cost was never the
// result size.
//
// WHAT GUARDS WHAT. The text assertion below is the discriminator: it
// fails the moment the CROSS JOIN is relaxed back to a JOIN. The plan
// assertion cannot do that job — SQLite only makes the bad choice on a
// store with production-scale statistics (~234k nodes against ~572k
// base-kind edges), roughly a hundred times this fixture, and a fixture
// that large does not belong in a unit test. It is kept as corroboration
// that the pinned order does yield the intended access path, not as the
// thing that catches a regression.
func TestCrossRepoCandidateQuery_PinsJoinOrder(t *testing.T) {
	kinds := graph.BaseKindsForCrossRepo()
	cases := map[string]struct{ repos, edgeFiles, incidentFiles []string }{
		"whole_graph":       {},
		"by_repos":          {repos: []string{"repoA"}},
		"by_edge_files":     {edgeFiles: []string{"repoA/pkg/file0000.go"}},
		"by_mutation_files": {edgeFiles: []string{"repoA/pkg/file0000.go"}, incidentFiles: []string{"repoA/pkg/file0000.go"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			q, _, ok := crossRepoCandidatesQuery(0, kinds, tc.repos, tc.edgeFiles, tc.incidentFiles)
			if !ok {
				t.Fatal("cross-repo candidate query must build")
			}
			for _, pinned := range []string{
				"CROSS JOIN nodes nf ON nf.id = e.from_id",
				"CROSS JOIN nodes nt ON nt.id = e.to_id",
			} {
				if !strings.Contains(q, pinned) {
					t.Fatalf("endpoint join is not order-pinned (%q missing); a plain JOIN lets the planner\n"+
						"drive from nodes x nodes, which does not finish on a real workspace:\n%s", pinned, q)
				}
			}
			if strings.Contains(q, "candidate_edges ce JOIN edges") {
				t.Fatalf("the candidate-edge CTE must drive its own join:\n%s", q)
			}
		})
	}
}

// Corroboration, not the guard — see the note above.
func TestCrossRepoCandidateQuery_PinnedOrderYieldsEdgeDrivenPlan(t *testing.T) {
	s := newCrossRepoPlanFixture(t)
	kinds := graph.BaseKindsForCrossRepo()

	q, args, ok := crossRepoCandidatesQuery(0, kinds, nil, nil, nil)
	if !ok {
		t.Fatal("cross-repo candidate query must build")
	}
	plan := strings.Join(explainQueryPlan(t, s, q, len(args)), "\n")
	if !strings.Contains(plan, "SEARCH e USING INDEX edges_by_kind") {
		t.Fatalf("expected one index scan of the base kinds:\n%s", plan)
	}
	for _, forbidden := range []string{"SCAN nf", "SCAN nt"} {
		if strings.Contains(plan, forbidden) {
			t.Fatalf("nodes must be probed, never scanned (%q):\n%s", forbidden, plan)
		}
	}

	// The pinned plan must still answer correctly: one cross-repo edge
	// every 50 source files.
	if got, want := len(s.CrossRepoCandidates(kinds)), 30; got != want {
		t.Fatalf("cross-repo candidates = %d, want %d", got, want)
	}
}
