package daemon

import "github.com/zzet/gortex/internal/graph"

// GraphIntegrityStatus is cheap, since-open telemetry for structural graph
// violations contained by the store backstop. A non-nil value is degraded but
// does not affect daemon readiness because the invalid edge was rejected.
type GraphIntegrityStatus struct {
	Status              string `json:"status"`
	Degraded            bool   `json:"degraded"`
	WriteRejected       uint64 `json:"write_rejected,omitempty"`
	ReadSuppressed      uint64 `json:"read_suppressed,omitempty"`
	AttributionOverflow uint64 `json:"attribution_overflow,omitempty"`
	SampleOverflow      uint64 `json:"sample_overflow,omitempty"`
	RepoTotalsOverflow  uint64 `json:"repo_totals_overflow,omitempty"`
}

// GraphIntegrityStatusFor reads only the optional store-scoped in-memory
// snapshot capability. It never scans the graph or durable SQLite rows.
func GraphIntegrityStatusFor(store graph.Store) *GraphIntegrityStatus {
	if store == nil {
		return nil
	}
	snapshotter, ok := store.(graph.StructuralIntegritySnapshotter)
	if !ok {
		return nil
	}
	snapshot := snapshotter.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{})
	if snapshot.Totals.Empty() && snapshot.AttributionOverflow == 0 && snapshot.SampleOverflow == 0 && snapshot.RepoTotalsOverflow == 0 {
		return nil
	}
	return &GraphIntegrityStatus{
		Status:              "warn",
		Degraded:            true,
		WriteRejected:       snapshot.Totals.WriteRejected,
		ReadSuppressed:      snapshot.Totals.ReadSuppressed,
		AttributionOverflow: snapshot.AttributionOverflow,
		SampleOverflow:      snapshot.SampleOverflow,
		RepoTotalsOverflow:  snapshot.RepoTotalsOverflow,
	}
}
