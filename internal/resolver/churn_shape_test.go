package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestChurnShapeClassification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		oldTo, newTo  string
		oldKind, kind graph.EdgeKind
		want          churnShapeClass
	}{
		{
			name:  "unresolved retarget",
			oldTo: graph.UnresolvedMarker + "Work", newTo: graph.UnresolvedMarker + "pkg::Work",
			oldKind: graph.EdgeCalls, kind: graph.EdgeCalls,
			want: churnShapeRetarget,
		},
		{
			name:  "kind promotion, target unchanged",
			oldTo: "pkg/a.go::Work", newTo: "pkg/a.go::Work",
			oldKind: graph.EdgeCalls, kind: graph.EdgeReferences,
			want: churnShapeKind,
		},
		{
			name:  "payload-only rewrite",
			oldTo: "pkg/a.go::Work", newTo: "pkg/a.go::Work",
			oldKind: graph.EdgeCalls, kind: graph.EdgeCalls,
			want: churnShapeAttrs,
		},
		{
			name:  "retarget wins over kind change",
			oldTo: graph.UnresolvedMarker + "A", newTo: graph.UnresolvedMarker + "B",
			oldKind: graph.EdgeCalls, kind: graph.EdgeReferences,
			want: churnShapeRetarget,
		},
	} {
		if got := churnShape(tc.oldTo, tc.newTo, tc.oldKind, tc.kind); got != tc.want {
			t.Fatalf("%s: churnShape = %v, want %v", tc.name, got, tc.want)
		}
	}
}
