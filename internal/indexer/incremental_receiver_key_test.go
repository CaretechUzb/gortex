package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestEdgeReceiverTypeReadsBuiltinStamp: the incremental reuse key must
// see builtin-receiver evidence — a key blind to it re-pins a stale
// typed bind after the local's type changes, while fresh resolution
// would refuse or rebind.
func TestEdgeReceiverTypeReadsBuiltinStamp(t *testing.T) {
	builtin := &graph.Edge{Meta: map[string]any{"receiver_builtin": "int"}}
	if got := edgeReceiverType(builtin); got != "int" {
		t.Fatalf("receiver_builtin ignored by the reuse key, got %q", got)
	}
	both := &graph.Edge{Meta: map[string]any{"receiver_type": "Widget", "receiver_builtin": "int"}}
	if got := edgeReceiverType(both); got != "Widget" {
		t.Fatalf("receiver_type must keep precedence, got %q", got)
	}
}
