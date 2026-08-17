package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func writeIndexerTemporalAllowlist(t *testing.T, root, helper string) {
	t.Helper()
	dir := filepath.Join(root, ".gortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("env_helpers:\n  - " + helper + "\n")
	if err := os.WriteFile(filepath.Join(dir, "temporal-allowlist.yaml"), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func indexerTemporalSource(helper string) []byte {
	return []byte(fmt.Sprintf(`package sample
import "go.temporal.io/sdk/workflow"
func Run(ctx workflow.Context) {
	name := %s("ACTIVITY", "MyActivity")
	workflow.ExecuteActivity(ctx, name)
}
`, helper))
}

func indexerTemporalMeta(result *parser.ExtractionResult) map[string]any {
	if result == nil {
		return nil
	}
	defer result.ReleaseTree()
	for _, edge := range result.Edges {
		if edge != nil && edge.Meta != nil && edge.Meta["via"] == "temporal.stub" {
			return edge.Meta
		}
	}
	return nil
}

func TestRepositoryExtractionOptionsIsolationAndOverlayParity(t *testing.T) {
	t.Setenv(config.LocalTemporalOptInEnv, "true")
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeIndexerTemporalAllowlist(t, rootA, "RepoAHelper")
	writeIndexerTemporalAllowlist(t, rootB, "RepoBHelper")

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idxA := New(graph.New(), reg, config.IndexConfig{}, zap.NewNop())
	idxB := New(graph.New(), reg, config.IndexConfig{}, zap.NewNop())
	idxA.SetRootPath(rootA)
	idxB.SetRootPath(rootB)
	defer idxA.Close()
	defer idxB.Close()
	type outcome struct {
		owner string
		meta  map[string]any
		err   error
	}
	out := make(chan outcome, 4)
	var wg sync.WaitGroup
	for _, testCase := range []struct {
		idx    *Indexer
		owner  string
		helper string
	}{
		{idx: idxA, owner: "A-own", helper: "RepoAHelper"},
		{idx: idxA, owner: "A-foreign", helper: "RepoBHelper"},
		{idx: idxB, owner: "B-own", helper: "RepoBHelper"},
		{idx: idxB, owner: "B-foreign", helper: "RepoAHelper"},
	} {
		testCase := testCase
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := testCase.idx.ExtractBuffer("go", testCase.owner+".go", indexerTemporalSource(testCase.helper))
			out <- outcome{owner: testCase.owner, meta: indexerTemporalMeta(result), err: err}
		}()
	}
	wg.Wait()
	close(out)
	for result := range out {
		if result.err != nil {
			t.Errorf("%s: %v", result.owner, result.err)
			continue
		}
		want := any(nil)
		if result.owner == "A-own" || result.owner == "B-own" {
			want = "allowlist"
		}
		if result.meta == nil {
			t.Errorf("%s: Temporal edge missing", result.owner)
			continue
		}
		if got := result.meta["temporal_env_source"]; got != want {
			t.Errorf("%s source = %#v, want %#v", result.owner, got, want)
		}
	}

	ext, _ := reg.GetByLanguage("go")
	base, err := idxA.extractWithTimeoutDone(ext, "base.go", indexerTemporalSource("RepoAHelper"), nil)
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := idxA.ExtractBuffer("go", "overlay.go", indexerTemporalSource("RepoAHelper"))
	if err != nil {
		t.Fatal(err)
	}
	baseMeta := indexerTemporalMeta(base)
	overlayMeta := indexerTemporalMeta(overlay)
	for _, key := range []string{"temporal_name", "temporal_name_origin", "temporal_env_source", "temporal_kind"} {
		if baseMeta[key] != overlayMeta[key] {
			t.Fatalf("base/overlay %s mismatch: %#v != %#v", key, baseMeta[key], overlayMeta[key])
		}
	}
}
