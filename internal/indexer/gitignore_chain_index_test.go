package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestIndex_GitignoreAboveTrackedRoot walks a repo tracked one level below
// its git root — the monorepo layout — with the exclude list a real
// ConfigManager hands the indexer. The git root's `.gitignore` must reach
// the walk: build output beneath the tracked root stays out of the graph
// while hand-written source is indexed.
func TestIndex_GitignoreAboveTrackedRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"),
		[]byte("junk/\n[Oo]bj/\n"), 0o644))

	tracked := filepath.Join(root, "projects", "app")
	mustWrite := func(rel, content string) {
		p := filepath.Join(tracked, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		writeFile(t, p, content)
	}
	php := func(fn string) string { return "<?php\nfunction " + fn + "() { return 1; }\n" }

	mustWrite("Real.php", php("realWork"))
	mustWrite("junk/noise.php", php("noise"))
	mustWrite("obj/gen.php", php("generated"))

	cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	cm.LoadWorkspaceConfig("app", tracked)

	reg := parser.NewRegistry()
	reg.Register(languages.NewPHPExtractor())
	cfg := config.Default().Index
	cfg.Workers = 2
	cfg.Exclude = cm.EffectiveExclude("app")

	g := graph.New()
	_, err = New(g, reg, cfg, zap.NewNop()).Index(tracked)
	require.NoError(t, err)

	hasFile := func(suffix string) bool {
		for _, n := range g.AllNodes() {
			if strings.Contains(filepath.ToSlash(n.FilePath), suffix) {
				return true
			}
		}
		return false
	}

	if !hasFile("Real.php") {
		t.Error("hand-written source Real.php was not indexed")
	}
	if hasFile("junk/noise.php") {
		t.Error("junk/noise.php leaked in: the git root's .gitignore did not reach the walk")
	}
	if hasFile("obj/gen.php") {
		t.Error("obj/gen.php leaked in: the git root's .gitignore did not reach the walk")
	}
}
