package indexer

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestGitWatcher_SmallCommitResolvesIncoming proves a commit-sized ref
// change resolves cross-file *incoming* references — the caller's edge to
// a symbol newly defined in the committed file is lifted from its
// unresolved stub to the real node. This must hold whether the reconcile
// took the scoped incremental path (the default, exercised with a normal
// cap) or fell back to the whole-graph ResolveAll (cap forced to 0), so
// the scoped path is verified to be resolution-equivalent to the global
// one it replaces for small change sets.
func TestGitWatcher_SmallCommitResolvesIncoming(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	cases := []struct {
		name     string
		maxFiles int
	}{
		{"scoped", 100},
		{"wholeGraphFallback", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := gitWatcherScopedResolveMaxFiles
			gitWatcherScopedResolveMaxFiles = tc.maxFiles
			t.Cleanup(func() { gitWatcherScopedResolveMaxFiles = prev })

			repoDir := t.TempDir()
			runGit(t, repoDir, "init", "-q", "-b", "main")
			runGit(t, repoDir, "config", "user.email", "test@example.com")
			runGit(t, repoDir, "config", "user.name", "Test")
			runGit(t, repoDir, "config", "commit.gpgsign", "false")

			// main: caller.go calls Target, which nothing defines yet —
			// so the call edge starts life as an unresolved stub.
			writeFile(t, filepath.Join(repoDir, "caller.go"),
				"package main\n\nfunc Caller() { Target() }\n")
			runGit(t, repoDir, "add", ".")
			runGit(t, repoDir, "commit", "-q", "-m", "main: caller")

			// feature: add callee.go defining Target, then return to main.
			runGit(t, repoDir, "checkout", "-q", "-b", "feature")
			writeFile(t, filepath.Join(repoDir, "callee.go"),
				"package main\n\nfunc Target() {}\n")
			runGit(t, repoDir, "add", "-A")
			runGit(t, repoDir, "commit", "-q", "-m", "feature: callee")
			runGit(t, repoDir, "checkout", "-q", "main")

			g := graph.New()
			idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
			idx.search = search.NewNull()
			idx.SetRootPath(repoDir)
			_, err := idx.IndexCtx(testCtx(), repoDir)
			require.NoError(t, err)
			require.Empty(t, g.FindNodesByName("Target"),
				"Target must not be defined on main")

			gw, err := NewGitWatcher(repoDir, idx, zap.NewNop())
			require.NoError(t, err)
			gw.debounce = 50 * time.Millisecond
			drained := make(chan int, 1)
			gw.drained = func(n int) {
				select {
				case drained <- n:
				default:
				}
			}
			require.NoError(t, gw.Start())
			t.Cleanup(func() { _ = gw.Stop() })

			runGit(t, repoDir, "checkout", "-q", "feature")
			select {
			case <-drained:
			case <-time.After(10 * time.Second):
				t.Fatal("git watcher did not reconcile within timeout")
			}

			targets := g.FindNodesByName("Target")
			require.NotEmpty(t, targets, "Target must be indexed after the reconcile")

			// The caller's previously-unresolved call edge must now point at
			// the real Target node — only the incoming (reverse) resolution
			// pass binds a caller in another file to a symbol defined here.
			resolved := false
			for _, tn := range targets {
				for _, e := range g.GetInEdges(tn.ID) {
					if e.Kind == graph.EdgeCalls && strings.Contains(e.From, "caller.go") {
						resolved = true
					}
				}
			}
			assert.True(t, resolved,
				"Caller->Target must resolve into callee.go::Target after the reconcile")
		})
	}
}
