package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func edgeAuditIntegrityRequest(args map[string]any) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	return req
}

func edgeAuditIntegrityText(t *testing.T, srv *Server, ctx context.Context, args map[string]any) string {
	t.Helper()
	res, err := srv.handleAnalyzeEdgeAudit(ctx, edgeAuditIntegrityRequest(args))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("edge_audit returned tool error: %+v", res.Content)
	}
	return res.Content[0].(mcplib.TextContent).Text
}

func addRejectedIntegrityAttempt(store graph.Store, repo, suffix string) {
	source := repo + "/source-" + suffix
	store.AddNode(&graph.Node{ID: source, Name: source, Kind: graph.KindFunction, RepoPrefix: repo, FilePath: repo + "/source.go"})
	store.AddEdge(&graph.Edge{
		From: source, To: repo + "/target#param:" + suffix,
		Kind: graph.EdgeImplements, Origin: "LSP_DISPATCH", FilePath: repo + "/source.go", Line: 5,
	})
}

func TestAnalyzeEdgeAuditGraphIntegrityJSONCompactAndGCX(t *testing.T) {
	srv, _ := setupTestServer(t)
	addRejectedIntegrityAttempt(srv.graph, "repo-a", "x")

	text := edgeAuditIntegrityText(t, srv, context.Background(), map[string]any{"limit": 10.0})
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode edge_audit JSON: %v\n%s", err, text)
	}
	integrity := payload["graph_integrity"].(map[string]any)
	sinceOpen := integrity["since_open"].(map[string]any)
	if sinceOpen["status"] != graph.StructuralAuditSupported {
		t.Fatalf("since-open capability status: %+v", sinceOpen)
	}
	totals := sinceOpen["totals"].(map[string]any)
	if totals["write_rejected"] != float64(1) {
		t.Fatalf("missing rejection total: %+v", totals)
	}
	samples := sinceOpen["samples"].([]any)
	if len(samples) != 1 || samples[0].(map[string]any)["from"] != "repo-a/source-x" {
		t.Fatalf("explicit audit must expose its attributed diagnostic sample: %+v", samples)
	}
	persisted := integrity["persisted"].(map[string]any)
	if persisted["status"] != graph.StructuralAuditUnsupported {
		t.Fatalf("memory graph must distinguish unsupported persisted audit: %+v", persisted)
	}

	compact := edgeAuditIntegrityText(t, srv, context.Background(), map[string]any{"compact": true})
	for _, want := range []string{"graph_integrity", "since_open_status=supported", "writes=1", "persisted_status=unsupported"} {
		if !strings.Contains(compact, want) {
			t.Fatalf("compact output missing %q: %s", want, compact)
		}
	}

	gcx := edgeAuditIntegrityText(t, srv, context.Background(), map[string]any{"format": "gcx"})
	for _, want := range []string{"GCX1", "analyze.edge_audit", "graph_integrity", "write_rejected", "persisted"} {
		if !strings.Contains(gcx, want) {
			t.Fatalf("GCX output missing %q: %s", want, gcx)
		}
	}
}

type scopedIntegrityAuditStore struct {
	*graph.Graph
	exactSamples []graph.StructuralIntegrityExactSample
}

func (s *scopedIntegrityAuditStore) AuditStructuralIntegrity(ctx context.Context, opts graph.StructuralIntegrityAuditOptions) (graph.StructuralIntegrityExactAudit, error) {
	out := graph.StructuralIntegrityExactAudit{Status: graph.StructuralAuditSupported}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	allowed := make(map[string]struct{}, len(opts.RepoPrefixes))
	for _, repo := range opts.RepoPrefixes {
		allowed[repo] = struct{}{}
	}
	for _, sample := range s.exactSamples {
		if opts.RepoScopeActive {
			if _, ok := allowed[sample.Repo]; !ok {
				continue
			}
		}
		out.TotalRows++
		out.Groups = append(out.Groups, graph.StructuralIntegrityExactGroup{
			Repo: sample.Repo, Kind: sample.Kind, Reason: sample.Reason, Origin: sample.Origin, Count: 1,
		})
		out.Samples = append(out.Samples, sample)
	}
	limit := opts.SampleLimit
	if limit > 0 && len(out.Samples) > limit {
		out.Samples = out.Samples[:limit]
		out.Truncated = true
	}
	return out, nil
}

func TestAnalyzeEdgeAuditGraphIntegrityRepoScopeIsolation(t *testing.T) {
	store := &scopedIntegrityAuditStore{Graph: graph.New()}
	addRejectedIntegrityAttempt(store, "repo-a", "a")
	addRejectedIntegrityAttempt(store, "repo-b", "b")
	store.exactSamples = []graph.StructuralIntegrityExactSample{
		{Repo: "repo-a", Kind: graph.EdgeImplements, Reason: graph.StructuralReasonParameterTarget, Origin: "lsp", From: "repo-a/source-a", To: "repo-a/target#param:a"},
		{Repo: "repo-b", Kind: graph.EdgeImplements, Reason: graph.StructuralReasonParameterTarget, Origin: "lsp", From: "repo-b/source-b", To: "repo-b/target#param:b"},
	}
	srv := &Server{graph: store}
	ctx := withRepoAllow(context.Background(), map[string]bool{"repo-a": true})
	text := edgeAuditIntegrityText(t, srv, ctx, map[string]any{"limit": 10.0})
	if strings.Contains(text, "repo-b") {
		t.Fatalf("scoped audit leaked another repository: %s", text)
	}
	if !strings.Contains(text, "repo-a") {
		t.Fatalf("scoped audit lost allowed repository: %s", text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	integrity := payload["graph_integrity"].(map[string]any)
	sinceTotals := integrity["since_open"].(map[string]any)["totals"].(map[string]any)
	if sinceTotals["write_rejected"] != float64(1) {
		t.Fatalf("scoped since-open total mismatch: %+v", sinceTotals)
	}
	persisted := integrity["persisted"].(map[string]any)
	if persisted["total_rows"] != float64(1) {
		t.Fatalf("scoped persisted total mismatch: %+v", persisted)
	}

	noneCtx := withRepoAllow(context.Background(), map[string]bool{"repo-none": true})
	noneText := edgeAuditIntegrityText(t, srv, noneCtx, map[string]any{})
	if strings.Contains(noneText, "repo-a") || strings.Contains(noneText, "repo-b") {
		t.Fatalf("active empty-result scope leaked diagnostic samples: %s", noneText)
	}
}

func TestEdgeAuditGraphIntegrityHonorsCancellation(t *testing.T) {
	store := &scopedIntegrityAuditStore{Graph: graph.New()}
	srv := &Server{graph: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := srv.edgeAuditGraphIntegrity(ctx, 10, false, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
