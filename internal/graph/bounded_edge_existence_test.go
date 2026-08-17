package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestGraphFindExistingEdgeEndpointsBoundsKindsAndCancellation(t *testing.T) {
	g := New()
	g.AddEdge(&Edge{From: "source", To: "owner", Kind: EdgeMemberOf, FilePath: "a.go", Line: 1})
	g.AddEdge(&Edge{From: "source", To: "owner", Kind: EdgeMemberOf, FilePath: "a.go", Line: 2})
	g.AddEdge(&Edge{From: "source", To: "owner", Kind: EdgeCalls})
	wanted := TypedEdgeEndpoint{From: "source", To: "owner", Kind: EdgeMemberOf}
	missing := TypedEdgeEndpoint{From: "source", To: "other", Kind: EdgeMemberOf}
	input := []TypedEdgeEndpoint{wanted, wanted, missing, {}, {From: "source", To: "owner", Kind: EdgeCalls}}
	before := append([]TypedEdgeEndpoint(nil), input...)
	found, err := g.FindExistingEdgeEndpoints(context.Background(), input, 4)
	if err != nil {
		t.Fatalf("bounded edge existence: %v", err)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("caller predicates mutated: got %#v want %#v", input, before)
	}
	if _, ok := found[wanted]; !ok {
		t.Fatalf("missing exact member_of predicate: %#v", found)
	}
	if _, ok := found[missing]; ok {
		t.Fatalf("reported missing predicate: %#v", found)
	}
	if _, ok := found[TypedEdgeEndpoint{From: "source", To: "owner", Kind: EdgeCalls}]; !ok {
		t.Fatalf("kind-specific predicate missing: %#v", found)
	}

	boundary := make([]TypedEdgeEndpoint, 0, MaxBoundedEdgeExistencePredicates+1)
	for index := 0; index <= MaxBoundedEdgeExistencePredicates; index++ {
		boundary = append(boundary, TypedEdgeEndpoint{
			From: "source", To: fmt.Sprintf("target-%03d", index), Kind: EdgeCalls,
		})
	}
	if result, err := g.FindExistingEdgeEndpoints(
		context.Background(), boundary[:MaxBoundedEdgeExistencePredicates], MaxBoundedEdgeExistencePredicates,
	); err != nil || len(result) != 0 {
		t.Fatalf("exact boundary = %#v, %v", result, err)
	}
	result, err := g.FindExistingEdgeEndpoints(context.Background(), boundary, MaxBoundedEdgeExistencePredicates)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || len(result) != 0 || limitErr.Limit != MaxBoundedEdgeExistencePredicates {
		t.Fatalf("over boundary = %#v, %v", result, err)
	}
	result, err = g.FindExistingEdgeEndpoints(context.Background(), []TypedEdgeEndpoint{wanted}, MaxBoundedEdgeExistencePredicates+1)
	if !errors.As(err, &limitErr) || len(result) != 0 {
		t.Fatalf("oversized declared limit = %#v, %v", result, err)
	}
	for _, invalidLimit := range []int{0, MaxBoundedEdgeExistencePredicates + 1} {
		result, err = g.FindExistingEdgeEndpoints(context.Background(), nil, invalidLimit)
		if !errors.As(err, &limitErr) || len(result) != 0 {
			t.Fatalf("empty input with invalid limit %d = %#v, %v", invalidLimit, result, err)
		}
	}

	for index := 0; index < 512; index++ {
		g.AddEdge(&Edge{From: "hot", To: fmt.Sprintf("noise-%03d", index), Kind: EdgeCalls})
	}
	ctx := &cancelAfterLocalizationChecksContext{Context: context.Background(), remaining: 5, done: make(chan struct{})}
	result, err = g.FindExistingEdgeEndpoints(ctx, []TypedEdgeEndpoint{{From: "hot", To: "absent", Kind: EdgeMemberOf}}, 1)
	if !errors.Is(err, context.Canceled) || len(result) != 0 {
		t.Fatalf("cancelled scan returned partial evidence: %#v, %v", result, err)
	}
}

func TestOverlaidViewFindExistingEdgeEndpointsReplacementAndTombstone(t *testing.T) {
	const (
		sourceFile = "repo/source.go"
		targetFile = "repo/target.go"
		sourceID   = sourceFile + "::source"
		targetID   = targetFile + "::target"
	)
	key := TypedEdgeEndpoint{From: sourceID, To: targetID, Kind: EdgeMemberOf}
	base := New()
	base.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeMemberOf})
	assertFound := func(t *testing.T, layer *OverlayLayer, want bool) {
		t.Helper()
		found, err := NewOverlaidView(base, layer).FindExistingEdgeEndpoints(
			context.Background(), []TypedEdgeEndpoint{key}, 1,
		)
		if err != nil {
			t.Fatalf("overlay edge existence: %v", err)
		}
		_, got := found[key]
		if got != want {
			t.Fatalf("found = %v (%#v), want %v", got, found, want)
		}
	}

	t.Run("source tombstone rejects stray edge", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(sourceFile, false)
		layer.MarkRemoved("source", sourceID)
		layer.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeMemberOf})
		assertFound(t, layer, false)
	})
	t.Run("same ID source replacement uses overlay edge", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(sourceFile, false)
		layer.MarkRemoved("source", sourceID)
		layer.AddNode(sourceFile, &Node{ID: sourceID, Name: "source", Kind: KindMethod, FilePath: sourceFile})
		layer.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeMemberOf})
		assertFound(t, layer, true)
	})
	t.Run("target tombstone suppresses base edge", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(targetFile, false)
		layer.MarkRemoved("target", targetID)
		assertFound(t, layer, false)
	})
	t.Run("same ID target replacement preserves base edge", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(targetFile, false)
		layer.MarkRemoved("target", targetID)
		layer.AddNode(targetFile, &Node{ID: targetID, Name: "target", Kind: KindType, FilePath: targetFile})
		assertFound(t, layer, true)
	})
	t.Run("overlay source cannot point to tombstoned target", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(sourceFile, false)
		layer.MarkFile(targetFile, false)
		layer.AddNode(sourceFile, &Node{ID: sourceID, Name: "source", Kind: KindMethod, FilePath: sourceFile})
		layer.MarkRemoved("target", targetID)
		layer.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeMemberOf})
		assertFound(t, layer, false)
	})
	t.Run("overlay source can point to same ID target replacement", func(t *testing.T) {
		layer := NewOverlayLayer()
		layer.MarkFile(sourceFile, false)
		layer.MarkFile(targetFile, false)
		layer.AddNode(sourceFile, &Node{ID: sourceID, Name: "source", Kind: KindMethod, FilePath: sourceFile})
		layer.MarkRemoved("target", targetID)
		layer.AddNode(targetFile, &Node{ID: targetID, Name: "target", Kind: KindType, FilePath: targetFile})
		layer.AddEdge(&Edge{From: sourceID, To: targetID, Kind: EdgeMemberOf})
		assertFound(t, layer, true)
	})
}

func TestOverlaidViewFindExistingEdgeEndpointsDetachedIdentityParity(t *testing.T) {
	key := TypedEdgeEndpoint{From: "legacy-source", To: "legacy-target", Kind: EdgeMemberOf}
	base := New()
	base.AddEdge(&Edge{From: key.From, To: key.To, Kind: key.Kind})

	tombstone := NewOverlayLayer()
	tombstone.MarkRemoved("legacy-source", key.From)
	tombstone.AddEdge(&Edge{From: key.From, To: key.To, Kind: key.Kind})
	found, err := NewOverlaidView(base, tombstone).FindExistingEdgeEndpoints(context.Background(), []TypedEdgeEndpoint{key}, 1)
	if err != nil || len(found) != 0 {
		t.Fatalf("detached source tombstone authenticated edge: %#v, %v", found, err)
	}

	replacement := NewOverlayLayer()
	replacement.MarkRemoved("legacy-source", key.From)
	replacement.AddNode("overlay.go", &Node{ID: key.From, Name: "legacy-source", Kind: KindMethod, FilePath: "overlay.go"})
	replacement.AddEdge(&Edge{From: key.From, To: key.To, Kind: key.Kind})
	found, err = NewOverlaidView(base, replacement).FindExistingEdgeEndpoints(context.Background(), []TypedEdgeEndpoint{key}, 1)
	if err != nil {
		t.Fatalf("detached replacement: %v", err)
	}
	if _, ok := found[key]; !ok {
		t.Fatalf("detached same-ID replacement missing: %#v", found)
	}

	targetKey := TypedEdgeEndpoint{From: "stable-source", To: key.To, Kind: key.Kind}
	base.AddEdge(&Edge{From: targetKey.From, To: targetKey.To, Kind: targetKey.Kind})
	targetTombstone := NewOverlayLayer()
	targetTombstone.MarkRemoved("legacy-target", targetKey.To)
	found, err = NewOverlaidView(base, targetTombstone).FindExistingEdgeEndpoints(
		context.Background(), []TypedEdgeEndpoint{targetKey}, 1,
	)
	if err != nil || len(found) != 0 {
		t.Fatalf("detached target tombstone authenticated base edge: %#v, %v", found, err)
	}

	targetReplacement := NewOverlayLayer()
	targetReplacement.MarkRemoved("legacy-target", targetKey.To)
	targetReplacement.AddNode("overlay.go", &Node{
		ID: targetKey.To, Name: "legacy-target", Kind: KindType, FilePath: "overlay.go",
	})
	found, err = NewOverlaidView(base, targetReplacement).FindExistingEdgeEndpoints(
		context.Background(), []TypedEdgeEndpoint{targetKey}, 1,
	)
	if err != nil {
		t.Fatalf("detached target replacement: %v", err)
	}
	if _, ok := found[targetKey]; !ok {
		t.Fatalf("detached same-ID target replacement hid base edge: %#v", found)
	}

	overlaySourceTombstone := NewOverlayLayer()
	overlaySourceTombstone.AddNode("overlay.go", &Node{
		ID: targetKey.From, Name: "stable-source", Kind: KindMethod, FilePath: "overlay.go",
	})
	overlaySourceTombstone.MarkRemoved("legacy-target", targetKey.To)
	overlaySourceTombstone.AddEdge(&Edge{From: targetKey.From, To: targetKey.To, Kind: targetKey.Kind})
	found, err = NewOverlaidView(base, overlaySourceTombstone).FindExistingEdgeEndpoints(
		context.Background(), []TypedEdgeEndpoint{targetKey}, 1,
	)
	if err != nil || len(found) != 0 {
		t.Fatalf("overlay source authenticated detached target tombstone: %#v, %v", found, err)
	}

	overlaySourceReplacement := NewOverlayLayer()
	overlaySourceReplacement.AddNode("overlay.go", &Node{
		ID: targetKey.From, Name: "stable-source", Kind: KindMethod, FilePath: "overlay.go",
	})
	overlaySourceReplacement.MarkRemoved("legacy-target", targetKey.To)
	overlaySourceReplacement.AddNode("overlay.go", &Node{
		ID: targetKey.To, Name: "legacy-target", Kind: KindType, FilePath: "overlay.go",
	})
	overlaySourceReplacement.AddEdge(&Edge{From: targetKey.From, To: targetKey.To, Kind: targetKey.Kind})
	found, err = NewOverlaidView(base, overlaySourceReplacement).FindExistingEdgeEndpoints(
		context.Background(), []TypedEdgeEndpoint{targetKey}, 1,
	)
	if err != nil {
		t.Fatalf("overlay source detached target replacement: %v", err)
	}
	if _, ok := found[targetKey]; !ok {
		t.Fatalf("overlay source missed detached target replacement: %#v", found)
	}
}

type edgeExistenceUnsupportedReader struct{ Reader }

func TestOverlaidViewFindExistingEdgeEndpointsFailsClosedWithoutCapability(t *testing.T) {
	base := edgeExistenceUnsupportedReader{Reader: New()}
	baseKey := TypedEdgeEndpoint{From: "source", To: "target", Kind: EdgeCalls}
	overlayKey := TypedEdgeEndpoint{From: "overlay.go::source", To: "target", Kind: EdgeCalls}
	layer := NewOverlayLayer()
	layer.AddNode("overlay.go", &Node{
		ID: overlayKey.From, Name: "source", Kind: KindMethod, FilePath: "overlay.go",
	})
	layer.AddEdge(&Edge{From: overlayKey.From, To: overlayKey.To, Kind: overlayKey.Kind})
	found, err := NewOverlaidView(base, layer).FindExistingEdgeEndpoints(
		context.Background(), []TypedEdgeEndpoint{overlayKey, baseKey}, 2,
	)
	if !errors.Is(err, ErrBoundedLocalizationUnavailable) || len(found) != 0 {
		t.Fatalf("unsupported base leaked partial overlay evidence: %#v, %v", found, err)
	}
}
