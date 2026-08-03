package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
)

type enrichmentTestNotificationSender struct{}

func (enrichmentTestNotificationSender) SendNotificationToSpecificClient(string, string, map[string]any) error {
	return nil
}

func serverAtEnrichmentPhase(phase string, ready, enriched bool) *Server {
	b := newReadinessBroadcaster(enrichmentTestNotificationSender{}, zap.NewNop())
	payload := map[string]any{
		"phase": phase,
		"ready": ready,
	}
	if enriched {
		payload["enriched"] = true
	}
	b.publish(payload)
	return &Server{
		readinessBroadcaster: b,
		ToolCallTimeout:      -1,
	}
}

func TestWarmupStateDistinguishesQueryableFromEnriched(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   map[string]any
		incomplete bool
	}{
		{name: "embedded server has no warmup", snapshot: nil, incomplete: false},
		{name: "parsing", snapshot: map[string]any{"phase": "parallel_parse", "ready": false}, incomplete: true},
		{name: "queryable", snapshot: map[string]any{"phase": "ready", "ready": true}, incomplete: true},
		{name: "post-ready derived pass", snapshot: map[string]any{"phase": "end_batch", "ready": true}, incomplete: true},
		{name: "terminal phase implies enriched", snapshot: map[string]any{"phase": "enrichment_complete", "ready": true}, incomplete: false},
		{name: "explicit enriched flag", snapshot: map[string]any{"phase": "future_terminal", "ready": true, "enriched": true}, incomplete: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := warmupStateFromSnapshot(tt.snapshot).enrichmentIncomplete()
			if got != tt.incomplete {
				t.Fatalf("enrichmentIncomplete() = %v, want %v", got, tt.incomplete)
			}
		})
	}
}

func TestAnalyzeAdmissionDefersOnlyExpensiveKindsUntilEnriched(t *testing.T) {
	warming := serverAtEnrichmentPhase("end_batch", true, false)
	if release, err := warming.acquireAnalyzeAdmission(context.Background(), "hotspots"); !errors.Is(err, errAnalysisWarmup) {
		if release != nil {
			release()
		}
		t.Fatalf("hotspots admission error = %v, want %v", err, errAnalysisWarmup)
	}

	releaseLight, err := warming.acquireAnalyzeAdmission(context.Background(), "todos")
	if err != nil {
		t.Fatalf("lightweight analysis was deferred during enrichment: %v", err)
	}
	releaseLight()

	enriched := serverAtEnrichmentPhase("enrichment_complete", true, true)
	release, err := enriched.acquireAnalyzeAdmission(context.Background(), "hotspots")
	if err != nil {
		t.Fatalf("hotspots remained deferred after enrichment: %v", err)
	}
	release()
}

func TestAnalyzerResourcesAreDeferredBeforeHandlerStarts(t *testing.T) {
	warming := serverAtEnrichmentPhase("end_batch", true, false)
	for uri := range enrichmentDeferredResources {
		t.Run(uri, func(t *testing.T) {
			called := false
			h := warming.boundResourceHandler(uri, func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				called = true
				return nil, nil
			})
			contents, err := h(context.Background(), mcp.ReadResourceRequest{})
			if err != nil {
				t.Fatalf("resource gate error = %v", err)
			}
			if len(contents) != 1 {
				t.Fatalf("resource gate returned %d contents, want 1", len(contents))
			}
			text, ok := contents[0].(mcp.TextResourceContents)
			if !ok || !strings.Contains(text.Text, `"retriable":true`) || !strings.Contains(text.Text, "enrichment_complete") {
				t.Fatalf("resource gate = %#v, want retriable enrichment envelope", contents)
			}
			if called {
				t.Fatal("whole-graph resource handler started during enrichment")
			}
		})
	}

	calledLight := false
	light := warming.boundResourceHandler("gortex://index-health", func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		calledLight = true
		return nil, nil
	})
	if _, err := light(context.Background(), mcp.ReadResourceRequest{}); err != nil {
		t.Fatalf("lightweight resource was deferred: %v", err)
	}
	if !calledLight {
		t.Fatal("lightweight resource handler did not run")
	}

	enriched := serverAtEnrichmentPhase("enrichment_complete", true, true)
	calledHeavy := false
	heavy := enriched.boundResourceHandler("gortex://report", func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		calledHeavy = true
		return nil, nil
	})
	if _, err := heavy(context.Background(), mcp.ReadResourceRequest{}); err != nil {
		t.Fatalf("whole-graph resource remained deferred after enrichment: %v", err)
	}
	if !calledHeavy {
		t.Fatal("whole-graph resource handler did not run after enrichment")
	}
}

func TestWakeupDefersBeforeTouchingGraphDuringEnrichment(t *testing.T) {
	s := serverAtEnrichmentPhase("end_batch", true, false)
	res, err := s.handleGortexWakeup(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleGortexWakeup() error = %v", err)
	}
	text, ok := singleTextContent(res)
	if !ok {
		t.Fatalf("handleGortexWakeup() result = %#v, want text", res)
	}
	if !strings.Contains(text, `"retriable":true`) || !strings.Contains(text, "enrichment_complete") {
		t.Fatalf("wakeup gate = %s, want retriable enrichment response", text)
	}
}
