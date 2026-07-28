package query

import (
	"sort"

	"github.com/zzet/gortex/internal/search/rerank"
)

// crossRepoRerank reassigns per-channel candidate ranks repository by
// repository so a large repo's corpus size cannot bury a small repo's
// best hits in a multi-repo workspace.
//
// The backend indexes every tracked repo into one merged corpus, so a
// raw BM25 / vector rank reflects how a hit fares against ALL repos at
// once — a 30k-symbol repo's matches crowd out a 500-symbol repo's
// matches even when the latter are a better answer. Reassigning the
// TextRank / VectorRank counters per repo means each repo's #1 hit is
// rank 0, each repo's #2 hit is rank 1, and so on. The downstream
// rerank's bm25 and semantic signals use the reciprocal-rank (RRF)
// kernel, so per-repo ranks turn those signals into a genuine
// cross-repo RRF fusion — the structural and session signals then
// discriminate between the repos' top hits.
//
// No-op when the candidate set comes from a single repository (or is
// empty): the merged ranks are already a fair within-repo order.
//
// Only NON-EMPTY prefixes count toward "a repository". Synthetic global
// externals (dep::, external::, non-Go module::) carry RepoPrefix == "" by
// design — they model third-party symbols owned by no repo. Counting "" as
// a repository made a one-repo result set look like two, activating the
// renumbering and promoting the top external stub to rank 0 alongside the
// top first-party hit. Nothing errored; the results were just quietly
// worse, which is why this is fenced by a test rather than an assertion.
func crossRepoRerank(cands []*rerank.Candidate) {
	repos := make(map[string]struct{}, 4)
	for _, c := range cands {
		if c != nil && c.Node != nil && c.Node.RepoPrefix != "" {
			repos[c.Node.RepoPrefix] = struct{}{}
		}
	}
	if len(repos) < 2 {
		return
	}
	reassignChannelPerRepo(cands, func(c *rerank.Candidate) *int { return &c.TextRank })
	reassignChannelPerRepo(cands, func(c *rerank.Candidate) *int { return &c.VectorRank })
}

// reassignChannelPerRepo renumbers one channel's ranks (selected by
// rankOf) so the counter resets at each repository. Candidates absent
// from the channel (rank < 0) are left untouched. Ties on the prior
// global rank — the exact-name fallback tier assigns several
// candidates the same rank — break on node ID so the renumbering is
// deterministic.
func reassignChannelPerRepo(cands []*rerank.Candidate, rankOf func(*rerank.Candidate) *int) {
	ranked := make([]*rerank.Candidate, 0, len(cands))
	for _, c := range cands {
		if c != nil && c.Node != nil && *rankOf(c) >= 0 {
			ranked = append(ranked, c)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		ri, rj := *rankOf(ranked[i]), *rankOf(ranked[j])
		if ri != rj {
			return ri < rj
		}
		return ranked[i].Node.ID < ranked[j].Node.ID
	})
	counter := make(map[string]int, 4)
	for _, c := range ranked {
		repo := c.Node.RepoPrefix
		*rankOf(c) = counter[repo]
		counter[repo]++
	}
}
