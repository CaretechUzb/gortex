package rerank

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// CommunitySignal takes the max of cluster cohesion and same-repo /
// same-project affinity. The same-repo arm saturates at 1.0, so when it
// fires it OVERRIDES the cluster score.
//
// That is right when candidates span repositories — the session's home repo
// should win. It is useless on a single-repo batch, where every first-party
// candidate is in the home repo: the signal becomes a constant and the one
// part of it that still separates candidates is thrown away.
//
// The arm used to be unreachable on a solo workspace by accident, because
// candidates carried no repo prefix at all and `ctx.RepoPrefix != ""` was
// the only guard. Now they carry one, so the batch-spans-repos check is what
// keeps it from flattening the score.

func communityCtx(t *testing.T, homeRepo string, cands []*Candidate, communityOf func(string) string) *Context {
	t.Helper()
	ctx := &Context{RepoPrefix: homeRepo, CommunityOf: communityOf}
	ctx.prepare(cands)
	return ctx
}

func candWithRepo(id, repo string) *Candidate {
	return &Candidate{Node: &graph.Node{ID: id, Name: id, RepoPrefix: repo, FilePath: id}}
}

func TestCommunitySignalKeepsClusterScoreOnSingleRepoBatch(t *testing.T) {
	// Three candidates in one repo. Two share a community, one does not —
	// so cluster cohesion has something to say.
	a := candWithRepo("gortex/a.go::A", "gortex")
	b := candWithRepo("gortex/b.go::B", "gortex")
	lonely := candWithRepo("gortex/z.go::Z", "gortex")
	cands := []*Candidate{a, b, lonely}

	communities := map[string]string{a.Node.ID: "c1", b.Node.ID: "c1", lonely.Node.ID: "c2"}
	ctx := communityCtx(t, "gortex", cands, func(id string) string { return communities[id] })

	sig := CommunitySignal{}
	clustered := sig.Contribute("q", a, ctx)
	isolated := sig.Contribute("q", lonely, ctx)

	if clustered == isolated {
		t.Fatalf("clustered and isolated candidates scored identically (%v) — the same-repo arm "+
			"saturated every first-party candidate and flattened the cluster score", clustered)
	}
	if clustered <= isolated {
		t.Fatalf("clustered candidate scored %v, isolated %v — the clustered one must rank higher", clustered, isolated)
	}
}

func TestCommunitySignalPrefersHomeRepoAcrossRepos(t *testing.T) {
	// Two repos, so the affinity arm is a real discriminator again.
	home := candWithRepo("gortex/a.go::A", "gortex")
	away := candWithRepo("other/a.go::A", "other")
	cands := []*Candidate{home, away}

	ctx := communityCtx(t, "gortex", cands, nil)

	sig := CommunitySignal{}
	if got := sig.Contribute("q", home, ctx); got != 1.0 {
		t.Fatalf("home-repo candidate scored %v, want 1.0 — same-repo affinity must fire "+
			"when the batch spans repositories", got)
	}
	if got := sig.Contribute("q", away, ctx); got == 1.0 {
		t.Fatalf("out-of-repo candidate also scored 1.0 — the affinity arm is not discriminating")
	}
}

// A synthetic global external carries no repo prefix and belongs to no
// repository, so it must not by itself make a batch look multi-repo.
func TestCommunitySignalGlobalExternalDoesNotMakeBatchMultiRepo(t *testing.T) {
	a := candWithRepo("gortex/a.go::A", "gortex")
	b := candWithRepo("gortex/b.go::B", "gortex")
	global := &Candidate{Node: &graph.Node{ID: "dep::go.uber.org/zap::Logger", Name: "Logger"}}

	ctx := communityCtx(t, "gortex", []*Candidate{a, b, global}, nil)
	if ctx.multiRepoBatch {
		t.Fatal("a batch of one repo plus a prefix-less global external must not count as multi-repo — " +
			"the global belongs to no repository, so there is still only one repo in play")
	}

	// A second real repo does make it multi-repo.
	away := candWithRepo("other/a.go::A", "other")
	ctx = communityCtx(t, "gortex", []*Candidate{a, b, global, away}, nil)
	if !ctx.multiRepoBatch {
		t.Fatal("two distinct repo prefixes must count as multi-repo")
	}
}
