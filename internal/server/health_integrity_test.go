package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

func TestHealthEndpointGraphIntegrityOptionalAndWarning(t *testing.T) {
	store := graph.New()
	handler := NewHandler(nil, store, "test-version", zap.NewNop())

	requestHealth := func() (HealthResponse, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("unexpected health status %d: %s", recorder.Code, recorder.Body.String())
		}
		var response HealthResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		return response, recorder.Body.String()
	}

	zero, body := requestHealth()
	if zero.Status != "ok" || zero.GraphIntegrity != nil {
		t.Fatalf("zero telemetry must leave health OK and omit graph_integrity: %+v", zero)
	}
	if strings.Contains(body, "graph_integrity") {
		t.Fatalf("zero telemetry was not omitted: %s", body)
	}

	store.AddNode(&graph.Node{ID: "source", Kind: graph.KindFunction, RepoPrefix: "repo-a"})
	store.AddEdge(&graph.Edge{
		From:   "source",
		To:     "target#param:x",
		Kind:   graph.EdgeImplements,
		Origin: "lsp",
	})

	degraded, body := requestHealth()
	integrity := degraded.GraphIntegrity
	if degraded.Status != "ok" || integrity == nil || integrity.Status != "warn" || !integrity.Degraded || integrity.WriteRejected != 1 {
		t.Fatalf("integrity warning must not change endpoint health/readiness: %+v", degraded)
	}
	for _, forbidden := range []string{"samples", "attribution", "file_path", `"from"`, `"to"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("routine health exposed audit detail %q: %s", forbidden, body)
		}
	}
}
