package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestRepoPrefixParityAfterIndexing is the invariant that would have caught
// most of the single-repo-mode bug class in one assertion.
//
// A node's repo prefix is the graph's ownership key — it keys the byRepo
// bucket, the per-repo memory estimates, the `nodes_by_repo` partial index
// and every repo-scoped query. Its ID and FilePath are produced by
// Indexer.applyRepoPrefix concatenating the same prefix. When those three
// disagree, nothing errors: repo-scoped reads return a subset, per-repo
// counts disagree with whole-store counts, and index_health stays green.
//
// The specific failures this covers:
//
//   - a repo indexed with no prefix at all while a prefixed sibling exists
//     (the ghost graph an upgrade used to produce)
//   - a node stamped with one repo's prefix whose id carries another's, or
//     none (the desync `daemon status` used to reconstruct around)
//
// graph.ClassifyNodePrefix is the shared definition; store_sqlite transcribes
// it into SQL and a storetest conformance case fences the two against drift.
// This test is the end of that chain: it runs a real index and asserts the
// audit comes back clean.
func TestRepoPrefixParityAfterIndexing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		repos []string
	}{
		{"one tracked repo", []string{"solo"}},
		{"several tracked repos", []string{"alpha", "beta", "gamma"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			entries := make([]config.RepoEntry, 0, len(tc.repos))
			for _, name := range tc.repos {
				dir := filepath.Join(base, name)
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
				writeFile(t, filepath.Join(dir, "main.go"),
					"package main\n\nimport \"fmt\"\n\nfunc Hello() { fmt.Println(\"hi\") }\n")
				writeFile(t, filepath.Join(dir, "pkg", "lib.go"),
					"package pkg\n\ntype T struct{}\n\nfunc (T) M() {}\n")
				entries = append(entries, config.RepoEntry{Path: dir, Name: name})
			}

			cfgPath := filepath.Join(base, "config.yaml")
			gc := &config.GlobalConfig{Repos: entries}
			gc.SetConfigPath(cfgPath)
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(cfgPath)
			require.NoError(t, err)

			g := graph.New()
			mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
			for _, e := range entries {
				_, err := mi.TrackRepoCtx(context.Background(), e)
				require.NoError(t, err)
			}

			d := g.PrefixDiagnostics(5)
			require.Positive(t, d.OwnedCodeNodes,
				"sanity: the fixture must have produced owned code nodes (%s)", d.Summary())
			require.Zero(t, d.UnownedCodeNodes,
				"every file-backed node the daemon indexes must be owned by a repo (%s)", d.Summary())
			require.Zero(t, d.MisprefixedNodes,
				"a node's stamped repo prefix must match the prefix its id and file path carry (%s)", d.Summary())
			require.True(t, d.Clean(), "repo ownership audit must be clean (%s)", d.Summary())
		})
	}
}
