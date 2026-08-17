package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestConstructOverlayLayerUsesRepositoryTemporalOptions(t *testing.T) {
	t.Setenv(config.LocalTemporalOptInEnv, "true")
	root := t.TempDir()
	allowlistDir := filepath.Join(root, ".gortex")
	if err := os.MkdirAll(allowlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowlistDir, "temporal-allowlist.yaml"), []byte("env_helpers:\n  - OverlayEnvHelper\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	g := graph.New()
	idx := indexer.New(g, reg, config.IndexConfig{}, zap.NewNop())
	idx.SetRootPath(root)
	defer idx.Close()
	srv := &Server{graph: g, indexer: idx, logger: zap.NewNop()}

	source := `package sample
import "go.temporal.io/sdk/workflow"
func Run(ctx workflow.Context) {
	name := OverlayEnvHelper("ACTIVITY", "MyActivity")
	workflow.ExecuteActivity(ctx, name)
}
`
	layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: "overlay.go", Content: source}})
	if err != nil {
		t.Fatal(err)
	}
	if layer == nil || len(paths) != 1 || paths[0] != "overlay.go" {
		t.Fatalf("overlay layer/paths = %#v, %#v", layer, paths)
	}
	view := graph.NewOverlaidView(g, layer)
	var found bool
	for _, node := range view.GetFileNodes("overlay.go") {
		if node == nil || node.Kind != graph.KindFunction || node.Name != "Run" {
			continue
		}
		for _, edge := range view.GetOutEdges(node.ID) {
			if edge == nil || edge.Meta == nil || edge.Meta["via"] != "temporal.stub" {
				continue
			}
			found = true
			if got := edge.Meta["temporal_env_source"]; got != "allowlist" {
				t.Fatalf("overlay Temporal source = %#v", got)
			}
			if got := edge.Meta["temporal_name"]; got != "MyActivity" {
				t.Fatalf("overlay Temporal target = %#v", got)
			}
		}
	}
	if !found {
		t.Fatal("overlay Temporal dispatch edge not found")
	}
}
