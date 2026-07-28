package query

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

func rerankCand(id, repo string, textRank int) *rerank.Candidate {
	return &rerank.Candidate{
		Node:       &graph.Node{ID: id, RepoPrefix: repo, FilePath: id},
		TextRank:   textRank,
		VectorRank: -1,
	}
}

// TestCrossRepoRerankSoloWorkspaceIsNoOp pins the property that makes
// crossRepoRerank safe on a workspace holding one repository: the merged
// BM25 order is already a fair within-repo order, so nothing is renumbered.
//
// This is a fence, not a feature test. Synthetic global externals
// (dep::, external::, non-Go module::) carry RepoPrefix == "" by design and
// belong to no repository, so a solo-repo candidate set can still contain
// two DISTINCT RepoPrefix values — "" and the repo's. If the repo-count gate
// ever starts counting "" as a repository, this function silently activates
// on every solo workspace and hands the top external stub the same rank-0
// RRF weight as the top first-party hit. Nothing errors; results just get
// quietly worse. The assertion below is where that shows up.
func TestCrossRepoRerankSoloWorkspaceIsNoOp(t *testing.T) {
	cands := []*rerank.Candidate{
		rerankCand("gortex/a.go::First", "gortex", 0),
		rerankCand("gortex/b.go::Second", "gortex", 1),
		// A synthetic global external: no repository owns it.
		{Node: &graph.Node{ID: "dep::go.uber.org/zap::Logger"}, TextRank: 2, VectorRank: -1},
	}

	crossRepoRerank(cands)

	for i, c := range cands {
		if c.TextRank != i {
			t.Fatalf("candidate %q TextRank = %d, want %d — crossRepoRerank renumbered a "+
				"single-repo candidate set, which means the synthetic global-external prefix %q "+
				"is being counted as a second repository",
				c.Node.ID, c.TextRank, i, "")
		}
	}
}

// TestCrossRepoRerankRenumbersPerRepo is the positive case: two genuine
// repositories, so each repo's best hit becomes rank 0.
func TestCrossRepoRerankRenumbersPerRepo(t *testing.T) {
	cands := []*rerank.Candidate{
		rerankCand("big/a.go::A", "big", 0),
		rerankCand("big/b.go::B", "big", 1),
		rerankCand("small/x.go::X", "small", 2),
	}

	crossRepoRerank(cands)

	want := map[string]int{
		"big/a.go::A":   0,
		"big/b.go::B":   1,
		"small/x.go::X": 0, // the small repo's best hit is now rank 0
	}
	for _, c := range cands {
		if got := c.TextRank; got != want[c.Node.ID] {
			t.Errorf("%s TextRank = %d, want %d", c.Node.ID, got, want[c.Node.ID])
		}
	}
}
