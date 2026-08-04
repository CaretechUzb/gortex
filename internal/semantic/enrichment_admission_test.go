package semantic

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type admissionCaptureProvider struct {
	nodes int
	ok    bool
}

func (p *admissionCaptureProvider) Name() string        { return "admission-capture" }
func (p *admissionCaptureProvider) Languages() []string { return []string{"go"} }
func (p *admissionCaptureProvider) Available() bool     { return true }
func (p *admissionCaptureProvider) Close() error        { return nil }
func (p *admissionCaptureProvider) Enrich(graph.Store, string) (*EnrichResult, error) {
	return nil, nil
}
func (p *admissionCaptureProvider) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}
func (p *admissionCaptureProvider) EnrichRepoContext(
	ctx context.Context,
	_ graph.Store,
	_, _ string,
	_ EnrichDeadlinePolicy,
) (*EnrichResult, error) {
	p.nodes, p.ok = EnrichmentAdmissionNodes(ctx)
	return &EnrichResult{Provider: p.Name(), Language: "go"}, nil
}

func TestEnrichmentAdmissionNodesRoundTrip(t *testing.T) {
	if nodes, ok := EnrichmentAdmissionNodes(context.Background()); ok || nodes != 0 {
		t.Fatalf("missing hint = (%d, %v), want (0, false)", nodes, ok)
	}
	ctx := WithEnrichmentAdmissionNodes(context.Background(), 123)
	if nodes, ok := EnrichmentAdmissionNodes(ctx); !ok || nodes != 123 {
		t.Fatalf("hint = (%d, %v), want (123, true)", nodes, ok)
	}
}

func TestManagerPassesRepositoryCensusToContextEnricher(t *testing.T) {
	manager := NewManager(Config{Enabled: true}, zap.NewNop())
	defer manager.Close()
	provider := &admissionCaptureProvider{}
	partial := make(map[string]bool)

	results := manager.runEnrichOne(
		graph.New(), "repo", "/repo", "go", provider, 987,
		RepoEnrichState{}, nil, nil, partial,
	)

	if !provider.ok || provider.nodes != 987 {
		t.Fatalf("provider admission hint = (%d, %v), want (987, true)", provider.nodes, provider.ok)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if partial["repo"] {
		t.Fatal("successful admission capture unexpectedly marked partial")
	}
}
