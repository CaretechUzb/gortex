package mcp

import (
	"context"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func TestComputeCrossCommunityWarningDoesNotScanGraph(t *testing.T) {
	affected := []string{"community-a", "community-b"}
	warning := (&Server{}).computeCrossCommunityWarning(affected, nil)
	if warning == nil {
		t.Fatal("expected a cross-community warning")
	}
	if !reflect.DeepEqual(warning.AffectedCommunities, affected) {
		t.Fatalf("affected communities = %v, want %v", warning.AffectedCommunities, affected)
	}
}

func TestImpactCompleteRejectsDispatchLowerBound(t *testing.T) {
	const (
		seedID   = "repo/impl.go::Service.Run"
		targetID = "repo/api.go::Runner.Run"
	)
	g := graph.New()
	g.AddNode(&graph.Node{ID: seedID, Name: "Run", Kind: graph.KindMethod})
	g.AddNode(&graph.Node{ID: targetID, Name: "Run", Kind: graph.KindMethod})
	g.AddEdge(&graph.Edge{From: seedID, To: targetID, Kind: graph.EdgeImplements})

	impact := analysis.AnalyzeImpactContext(context.Background(), g, []string{seedID}, nil, nil)
	if !impact.LowerBound {
		t.Fatal("implements boundary must make impact a lower bound")
	}
	if impactComplete(impact) {
		t.Fatal("lower-bound impact must serialize complete=false")
	}
}
