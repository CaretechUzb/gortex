package crashpool

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func crashpoolTemporalSource(helper string) []byte {
	return []byte(fmt.Sprintf(`package sample
import "go.temporal.io/sdk/workflow"
func Run(ctx workflow.Context) {
	name := %s("ACTIVITY", "MyActivity")
	workflow.ExecuteActivity(ctx, name)
}
`, helper))
}

func responseTemporalMeta(resp extractResponse) map[string]any {
	for _, edge := range resp.Edges {
		if edge != nil && edge.Meta != nil && edge.Meta["via"] == "temporal.stub" {
			return edge.Meta
		}
	}
	return nil
}

func TestWorkerRequestOptionsAreInterleavedWithoutContamination(t *testing.T) {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	requests := []extractRequest{
		{Seq: 1, RelPath: "a.go", Language: "go", Content: crashpoolTemporalSource("RepoAHelper"), TemporalEnvHelpers: []string{"RepoAHelper"}},
		{Seq: 2, RelPath: "b.go", Language: "go", Content: crashpoolTemporalSource("RepoBHelper"), TemporalEnvHelpers: []string{"RepoBHelper"}},
		{Seq: 3, RelPath: "a-again.go", Language: "go", Content: crashpoolTemporalSource("RepoAHelper"), TemporalEnvHelpers: []string{"RepoBHelper"}},
	}
	var in bytes.Buffer
	enc := gob.NewEncoder(&in)
	for _, req := range requests {
		if err := enc.Encode(&req); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := serveWorker(reg, &in, &out); err != nil {
		t.Fatal(err)
	}

	dec := gob.NewDecoder(&out)
	for i, wantSource := range []any{"allowlist", "allowlist", nil} {
		var resp extractResponse
		if err := dec.Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Err != "" {
			t.Fatalf("response %d: %s", i, resp.Err)
		}
		meta := responseTemporalMeta(resp)
		if meta == nil {
			t.Fatalf("response %d has no Temporal edge", i)
		}
		if got := meta["temporal_env_source"]; got != wantSource {
			t.Fatalf("response %d source = %#v, want %#v", i, got, wantSource)
		}
	}
}

func TestWorkerAndInProcessOptionsParity(t *testing.T) {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	src := crashpoolTemporalSource("CorporateHelper")
	opts := parser.NewExtractionOptions([]string{"CorporateHelper"})

	worker := serveOne(reg, extractRequest{
		Seq: 1, RelPath: "worker.go", Language: "go", Content: src,
		TemporalEnvHelpers: opts.TemporalEnvHelpers(),
	})
	workerMeta := responseTemporalMeta(worker)
	if workerMeta == nil {
		t.Fatal("worker Temporal edge missing")
	}

	ext, _ := reg.GetByLanguage("go")
	result, err := parser.Extract(ext, "worker.go", src, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer result.ReleaseTree()
	var inProcessMeta map[string]any
	for _, edge := range result.Edges {
		if edge != nil && edge.Meta != nil && edge.Meta["via"] == "temporal.stub" {
			inProcessMeta = edge.Meta
			break
		}
	}
	if inProcessMeta == nil {
		t.Fatal("in-process Temporal edge missing")
	}
	for _, key := range []string{"temporal_name", "temporal_name_origin", "temporal_env_source", "temporal_kind"} {
		if workerMeta[key] != inProcessMeta[key] {
			t.Fatalf("%s mismatch: worker=%#v in-process=%#v", key, workerMeta[key], inProcessMeta[key])
		}
	}
}
