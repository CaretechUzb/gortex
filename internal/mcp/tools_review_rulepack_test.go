package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// reviewRulepackFixture is a Go source the `go-unchecked-type-assertion`
// review detector fires on. It is deliberately not an N+1 / check-then-act
// shape so the graph-grounding post-pass keeps the match unconditionally.
const reviewRulepackFixture = `package pkg

func Widen(v any) string {
	s := v.(string)
	return s
}
`

// prefixedServerOver indexes an existing directory through the MultiIndexer
// under the given repo name, so every graph node carries a repo prefix — the
// daemon's shape, where file nodes are keyed "<prefix>/<rel>" while git emits
// repo-relative paths. Returns the server and the graph repo prefix.
func prefixedServerOver(t *testing.T, dir, name string) (*Server, string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: name}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())

	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	srv.RunAnalysis()

	// Precondition: the graph really is prefixed, otherwise these tests would
	// pass for the wrong reason — the pre-fix join worked in unprefixed mode,
	// which is exactly why the bug reached a release.
	var prefix string
	for n := range g.NodesByKind(graph.KindFile) {
		require.NotEmpty(t, n.RepoPrefix, "file node %q must carry a repo prefix", n.ID)
		prefix = n.RepoPrefix
	}
	require.NotEmpty(t, prefix, "fixture repo produced no indexed file nodes")
	return srv, prefix
}

// setupPrefixedReviewServer writes the given repo-relative sources into a fresh
// directory and indexes it prefixed.
func setupPrefixedReviewServer(t *testing.T, files map[string]string) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo-a")
	for rel, src := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	}
	return prefixedServerOver(t, dir, "repo-a")
}

// TestReviewRulepackMatches_JoinsRepoRelativeChangedFiles pins the join the
// review tool depends on: `git diff` names changed files repo-relative while
// the graph keys them "<prefix>/<rel>", so intersecting the two vocabularies
// raw matched nothing and `review` reported zero findings on code its own
// detector bundle flags.
func TestReviewRulepackMatches_JoinsRepoRelativeChangedFiles(t *testing.T) {
	srv, prefix := setupPrefixedReviewServer(t, map[string]string{
		"pkg/widget.go": reviewRulepackFixture,
	})

	matches := srv.reviewRulepackMatches(context.Background(), []string{"pkg/widget.go"}, prefix, nil)
	require.NotEmpty(t, matches,
		"repo-relative changed file must join the prefixed graph path %q/pkg/widget.go", prefix)

	// Findings travel onward to rule resolution, the risk ranking, and the
	// forge comment API — all of which speak repo-relative paths.
	for _, m := range matches {
		require.NotEmpty(t, m.SymbolID,
			"review grounding must receive post-match enclosing-symbol enrichment")
		require.Equal(t, "pkg/widget.go", m.File,
			"match paths must be repo-relative, not graph-prefixed")
	}
}

// TestReviewRulepackMatches_IgnoresUnchangedFiles keeps the narrowing honest:
// the rulepack must scan the changeset, not the whole repository.
func TestReviewRulepackMatches_IgnoresUnchangedFiles(t *testing.T) {
	srv, prefix := setupPrefixedReviewServer(t, map[string]string{
		"pkg/widget.go": reviewRulepackFixture,
		"pkg/other.go":  "package pkg\n\nfunc Other() {}\n",
	})

	matches := srv.reviewRulepackMatches(context.Background(), []string{"pkg/other.go"}, prefix, nil)
	require.Empty(t, matches, "a file outside the changeset must not be scanned")
}

// TestReviewRulepackMatches_AcceptsAlreadyPrefixedChangedFiles covers the
// callers that hand in graph-keyed paths (a changed symbol's FilePath) rather
// than git's repo-relative spelling.
func TestReviewRulepackMatches_AcceptsAlreadyPrefixedChangedFiles(t *testing.T) {
	srv, prefix := setupPrefixedReviewServer(t, map[string]string{
		"pkg/widget.go": reviewRulepackFixture,
	})

	matches := srv.reviewRulepackMatches(context.Background(),
		[]string{prefix + "/pkg/widget.go"}, prefix, nil)
	require.NotEmpty(t, matches, "an already-prefixed changed file must still join")
	require.Equal(t, "pkg/widget.go", matches[0].File)
}

// TestReview_PrefixedGraphReportsRulepackFinding is the end-to-end regression
// for the reported failure: against a prefixed graph — the only shape a daemon
// produces — `review` returned `total: 0` on a changeset whose code the review
// detector bundle flags under `analyze --kind review`. The BLOCK verdict it
// paired that with carried nothing to act on.
func TestReview_PrefixedGraphReportsRulepackFinding(t *testing.T) {
	dir, file := reviewGitRepo(t)
	srv, _ := prefixedServerOver(t, dir, "svc-repo")

	out := decodeReview(t, callReview(t, srv, map[string]any{
		"repo": dir,
		"base": "base-ref",
	}))

	require.GreaterOrEqual(t, out.Total, 1,
		"the planted inverted-err-check must surface; got %+v", out)
	require.Equal(t, "BLOCK", out.Verdict, "an error-severity finding must block")

	var found bool
	for _, c := range out.Comments {
		if c.Rule != "go-inverted-err-check" {
			continue
		}
		found = true
		require.Equal(t, filepath.ToSlash(file), filepath.ToSlash(c.File),
			"the comment path must be repo-relative — a forge rejects a prefixed path")
		require.Greater(t, c.Line, 0, "the finding must be anchored to a real line")
		require.Equal(t, "rulepack", c.Source)
	}
	require.True(t, found, "expected a go-inverted-err-check finding; got %+v", out.Comments)

	for _, fr := range out.FileRisk {
		require.NotContains(t, fr.File, "svc-repo/", "risk rows stay repo-relative")
	}
}
