package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type storeWithoutIntegrity struct {
	graph.Store
}

func TestGraphIntegrityStatusOptionalZeroAndWarning(t *testing.T) {
	if got := GraphIntegrityStatusFor(nil); got != nil {
		t.Fatalf("nil store returned status: %+v", got)
	}
	if got := GraphIntegrityStatusFor(storeWithoutIntegrity{Store: graph.New()}); got != nil {
		t.Fatalf("store without optional capability returned status: %+v", got)
	}

	store := graph.New()
	if got := GraphIntegrityStatusFor(store); got != nil {
		t.Fatalf("zero telemetry must be omitted: %+v", got)
	}
	store.AddNode(&graph.Node{ID: "source", Kind: graph.KindFunction, RepoPrefix: "repo-a"})
	store.AddEdge(&graph.Edge{From: "source", To: "target#param:x", Kind: graph.EdgeImplements, Origin: "lsp"})
	got := GraphIntegrityStatusFor(store)
	if got == nil || got.Status != "warn" || !got.Degraded || got.WriteRejected != 1 || got.ReadSuppressed != 0 {
		t.Fatalf("unexpected warning status: %+v", got)
	}
}

func TestStatusResponseGraphIntegrityJSONAndReadinessIndependence(t *testing.T) {
	without, err := json.Marshal(StatusResponse{Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "graph_integrity") {
		t.Fatalf("zero integrity status must be omitted: %s", without)
	}

	response := StatusResponse{
		Ready: true,
		GraphIntegrity: &GraphIntegrityStatus{
			Status: "warn", Degraded: true, WriteRejected: 2, ReadSuppressed: 3,
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if ready, ok := decoded["ready"].(bool); !ok || !ready {
		t.Fatalf("integrity warning changed readiness: %s", encoded)
	}
	integrity, ok := decoded["graph_integrity"].(map[string]any)
	if !ok || integrity["status"] != "warn" || integrity["degraded"] != true {
		t.Fatalf("missing integrity warning JSON: %s", encoded)
	}
	text := string(encoded)
	for _, forbidden := range []string{"samples", "attribution", "file_path", "from", "to"} {
		if strings.Contains(text, `"`+forbidden+`"`) {
			t.Fatalf("routine status exposed audit detail %q: %s", forbidden, encoded)
		}
	}
}
