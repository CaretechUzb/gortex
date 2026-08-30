package analyzer_test

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/analyzer"
)

// TestAnalyzeSynthesizers_RepoScope verifies WithSynthesizerRepoScope
// drops synthesized edges whose source node lives outside the given
// workspace repos — the clamp that keeps `analyze synthesizers` inside
// the session workspace boundary even though the kind is not
// repo-narrowed in v1.
func TestAnalyzeSynthesizers_RepoScope(t *testing.T) {
	g := newTestGraph()
	// repo-a is the session workspace; repo-b stands in for a sibling
	// workspace whose synthesized edges must never surface.
	addSynthEdge(g, "repo-a/a.go::A", "repo-a/b.go::B", "event-channel", "event.channel")
	addSynthEdge(g, "repo-a/a.go::A", "repo-a/c.go::C", "event-channel", "event.channel")
	addSynthEdge(g, "repo-b/x.go::X", "repo-b/y.go::Y", "event-channel", "event.channel")
	addSynthEdge(g, "repo-b/cli.go::run", "repo-b/svc.go::Handle", "grpc-stub", "grpc.stub")

	// No clamp (and an empty set) is a no-op: every edge counts.
	if res := mustAnalyzeSynthesizers(t, g); res.TotalEdges != 4 {
		t.Fatalf("unclamped: expected TotalEdges=4, got %d", res.TotalEdges)
	}
	if res := mustAnalyzeSynthesizers(t, g, analyzer.WithSynthesizerRepoScope(nil)); res.TotalEdges != 4 {
		t.Fatalf("nil clamp: expected TotalEdges=4, got %d", res.TotalEdges)
	}

	// Clamp to repo-a: only the two repo-a edges survive, and the
	// repo-b-only grpc-stub group disappears entirely.
	res := mustAnalyzeSynthesizers(t, g, analyzer.WithSynthesizerRepoScope(map[string]bool{"repo-a": true}))
	if res.TotalEdges != 2 {
		t.Fatalf("clamped: expected TotalEdges=2, got %d", res.TotalEdges)
	}
	if len(res.Synthesizers) != 1 {
		t.Fatalf("clamped: expected 1 synthesizer group (event-channel), got %d", len(res.Synthesizers))
	}
	row := res.Synthesizers[0]
	if row.Name != "event-channel" || row.Edges != 2 {
		t.Fatalf("clamped: expected event-channel with 2 edges, got %q/%d", row.Name, row.Edges)
	}
	for _, smp := range row.Samples {
		if !strings.HasPrefix(smp.From, "repo-a/") {
			t.Errorf("clamped sample leaked a non-repo-a source: %s", smp.From)
		}
	}
}
