package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type healthIntegritySnapshotStore struct {
	graph.Store
	calls    atomic.Int32
	snapshot graph.StructuralIntegritySnapshot
}

func (s *healthIntegritySnapshotStore) StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions) graph.StructuralIntegritySnapshot {
	s.calls.Add(1)
	return s.snapshot
}

func TestBuildDaemonHealthSnapshotUsesQueryFreeIntegrityCapability(t *testing.T) {
	store := &healthIntegritySnapshotStore{
		Store: graph.New(),
		snapshot: graph.StructuralIntegritySnapshot{
			Totals: graph.StructuralIntegrityTotals{WriteRejected: 4, ReadSuppressed: 2},
		},
	}
	out := buildDaemonHealthSnapshot(time.Now(), nil, &daemonState{graph: store}, nil)
	if store.calls.Load() != 1 {
		t.Fatalf("health must read exactly one in-memory snapshot, got %d calls", store.calls.Load())
	}
	integrity, ok := out["graph_integrity"]
	if !ok || integrity == nil {
		t.Fatalf("health omitted graph_integrity: %+v", out)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"graph_integrity"`, `"status":"warn"`, `"degraded":true`, `"write_rejected":4`, `"read_suppressed":2`} {
		if !strings.Contains(text, want) {
			t.Fatalf("health JSON missing %s: %s", want, text)
		}
	}
	for _, forbidden := range []string{`"samples"`, `"attribution"`, `"file_path"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("routine health exposed raw audit detail %s: %s", forbidden, text)
		}
	}
}

func TestBuildDaemonHealthSnapshotOmitsZeroIntegrity(t *testing.T) {
	store := &healthIntegritySnapshotStore{Store: graph.New()}
	out := buildDaemonHealthSnapshot(time.Now(), nil, &daemonState{graph: store}, nil)
	if _, exists := out["graph_integrity"]; exists {
		t.Fatalf("zero integrity telemetry must be omitted: %+v", out)
	}
}
