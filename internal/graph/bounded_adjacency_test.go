package graph

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestGraphBoundedAdjacencyFiltersKindsAndSitesBeforeLimit(t *testing.T) {
	g := New()
	const source = "repo/source.go::source"
	for index := 0; index < 300; index++ {
		g.AddEdge(&Edge{
			From: source, To: fmt.Sprintf("noise-%03d", index), Kind: EdgeReferences,
			FilePath: "repo/source.go", Line: 1_000 + index,
			Meta: map[string]any{"payload": "must not cross projection"},
		})
	}
	first := &Edge{From: source, To: "target", Kind: EdgeCalls, FilePath: "repo/source.go", Line: 10}
	second := &Edge{From: source, To: "other", Kind: EdgeCalls, FilePath: "generated.go", Line: 20}
	third := &Edge{From: "repo/other.go::caller", To: "target", Kind: EdgeCalls, FilePath: "repo/other.go", Line: 30}
	g.AddEdge(first)
	g.AddEdge(second)
	g.AddEdge(third)

	sources := []string{source, source, ""}
	kinds := []EdgeKind{EdgeCalls, EdgeCalls, ""}
	sourcesBefore := append([]string(nil), sources...)
	kindsBefore := append([]EdgeKind(nil), kinds...)
	outgoing, err := g.FindOutgoingEdgeIdentitiesBounded(context.Background(), sources, kinds, 2)
	if err != nil {
		t.Fatalf("outgoing projection: %v", err)
	}
	if !reflect.DeepEqual(sources, sourcesBefore) || !reflect.DeepEqual(kinds, kindsBefore) {
		t.Fatalf("caller inputs mutated: sources=%#v kinds=%#v", sources, kinds)
	}
	wantOutgoing := []EdgeIdentity{EdgeIdentityFor(second), EdgeIdentityFor(first)}
	sortEdgeIdentities(wantOutgoing)
	if !reflect.DeepEqual(outgoing.ByEndpoint[source], wantOutgoing) || outgoing.Truncated[source] {
		t.Fatalf("outgoing = %#v, want %#v complete", outgoing, wantOutgoing)
	}

	incoming, err := g.FindIncomingEdgeIdentitiesBounded(
		context.Background(), []string{"target"}, []EdgeKind{EdgeCalls}, 2,
	)
	if err != nil {
		t.Fatalf("incoming projection: %v", err)
	}
	wantIncoming := []EdgeIdentity{EdgeIdentityFor(first), EdgeIdentityFor(third)}
	sortEdgeIdentities(wantIncoming)
	if !reflect.DeepEqual(incoming.ByEndpoint["target"], wantIncoming) || incoming.Truncated["target"] {
		t.Fatalf("incoming = %#v, want %#v complete", incoming, wantIncoming)
	}

	site := EdgeSourceSite{From: source, Line: 10}
	sites := []EdgeSourceSite{site, site}
	sitesBefore := append([]EdgeSourceSite(nil), sites...)
	siteProjection, err := g.FindOutgoingSiteEdgeIdentitiesBounded(
		context.Background(), sites, []EdgeKind{EdgeCalls}, 1,
	)
	if err != nil {
		t.Fatalf("site projection: %v", err)
	}
	if !reflect.DeepEqual(sites, sitesBefore) {
		t.Fatalf("caller sites mutated: %#v", sites)
	}
	if got := siteProjection.BySite[site]; !reflect.DeepEqual(got, []EdgeIdentity{EdgeIdentityFor(first)}) || siteProjection.Truncated[site] {
		t.Fatalf("site = %#v, want exact source+line identity", siteProjection)
	}
}

func TestGraphBoundedAdjacencyRejectsInvalidRawEnvelopes(t *testing.T) {
	g := New()
	tooManyKeys := make([]string, MaxBoundedAdjacencyKeys+1)
	for index := range tooManyKeys {
		tooManyKeys[index] = "duplicate"
	}
	tooManyKinds := make([]EdgeKind, MaxBoundedAdjacencyKinds+1)
	for index := range tooManyKinds {
		tooManyKinds[index] = EdgeCalls
	}
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "raw keys", run: func() error {
			_, err := g.FindOutgoingEdgeIdentitiesBounded(context.Background(), tooManyKeys, []EdgeKind{EdgeCalls}, 1)
			return err
		}},
		{name: "raw kinds", run: func() error {
			_, err := g.FindIncomingEdgeIdentitiesBounded(context.Background(), []string{"target"}, tooManyKinds, 1)
			return err
		}},
		{name: "zero limit on empty input", run: func() error {
			_, err := g.FindOutgoingSiteEdgeIdentitiesBounded(context.Background(), nil, nil, 0)
			return err
		}},
		{name: "max int limit", run: func() error {
			_, err := g.FindOutgoingEdgeIdentitiesBounded(context.Background(), nil, nil, math.MaxInt)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var limitErr *BoundedLocalizationLimitError
			if err := test.run(); !errors.As(err, &limitErr) {
				t.Fatalf("error = %v, want typed bounded limit", err)
			}
		})
	}
}

func TestGraphBoundedAdjacencyPerKeyAndInspectionSentinels(t *testing.T) {
	g := New()
	const source = "dense-source"
	for index := 0; index <= MaxBoundedAdjacencyRowsPerKey; index++ {
		g.AddEdge(&Edge{From: source, To: fmt.Sprintf("target-%03d", index), Kind: EdgeCalls, Line: index})
	}
	projection, err := g.FindOutgoingEdgeIdentitiesBounded(
		context.Background(), []string{source}, []EdgeKind{EdgeCalls}, MaxBoundedAdjacencyRowsPerKey,
	)
	if err != nil {
		t.Fatalf("per-key sentinel: %v", err)
	}
	if !projection.Truncated[source] || len(projection.ByEndpoint[source]) != 0 {
		t.Fatalf("per-key sentinel leaked partial rows: %#v", projection)
	}

	flood := New()
	for index := 0; index < MaxBoundedAdjacencyInspectedEdges; index++ {
		flood.AddEdge(&Edge{From: source, To: fmt.Sprintf("noise-%05d", index), Kind: EdgeReferences, Line: index})
	}
	flood.AddEdge(&Edge{From: source, To: "wanted", Kind: EdgeCalls, Line: MaxBoundedAdjacencyInspectedEdges})
	projection, err = flood.FindOutgoingEdgeIdentitiesBounded(
		context.Background(), []string{source}, []EdgeKind{EdgeCalls}, 1,
	)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "adjacency inspected edge rows" || len(projection.ByEndpoint) != 0 {
		t.Fatalf("inspection overflow = %#v, %v", projection, err)
	}
}

type cancelAdjacencyAfterChecksContext struct {
	context.Context
	remaining int
	checks    int
}

func (ctx *cancelAdjacencyAfterChecksContext) Err() error {
	ctx.checks++
	if ctx.remaining == 0 {
		return context.Canceled
	}
	ctx.remaining--
	return nil
}

func TestGraphBoundedAdjacencyMidScanCancellationReturnsNoPartial(t *testing.T) {
	g := New()
	const source = "cancel-source"
	for index := 0; index < 512; index++ {
		g.AddEdge(&Edge{From: source, To: fmt.Sprintf("target-%03d", index), Kind: EdgeCalls, Line: index})
	}

	// The early checks cover entry, canonicalization, and entering the source.
	// This threshold cancels at a later scan checkpoint, after rows have been
	// accumulated that must not escape.
	ctx := &cancelAdjacencyAfterChecksContext{Context: context.Background(), remaining: 5}
	projection, err := g.FindOutgoingEdgeIdentitiesBounded(ctx, []string{source}, []EdgeKind{EdgeCalls}, MaxBoundedAdjacencyRowsPerKey)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if len(projection.ByEndpoint) != 0 || len(projection.Truncated) != 0 {
		t.Fatalf("canceled projection leaked partial rows: %#v", projection)
	}
	if ctx.checks < 6 {
		t.Fatalf("cancellation checks = %d, want scan progress", ctx.checks)
	}
}

func TestGraphBoundedAdjacencySiteScanChargesEachSourceRowOnce(t *testing.T) {
	g := New()
	const source = "many-sites"
	sites := make([]EdgeSourceSite, 0, MaxBoundedAdjacencyKeys)
	for line := 0; line < MaxBoundedAdjacencyKeys; line++ {
		g.AddEdge(&Edge{From: source, To: fmt.Sprintf("target-%03d", line), Kind: EdgeCalls, Line: line})
		sites = append(sites, EdgeSourceSite{From: source, Line: line})
	}
	for index := MaxBoundedAdjacencyKeys; index < MaxBoundedAdjacencyInspectedEdges; index++ {
		g.AddEdge(&Edge{From: source, To: fmt.Sprintf("noise-%05d", index), Kind: EdgeReferences, Line: index})
	}
	projection, err := g.FindOutgoingSiteEdgeIdentitiesBounded(
		context.Background(), sites, []EdgeKind{EdgeCalls}, 1,
	)
	if err != nil {
		t.Fatalf("same-source site projection: %v", err)
	}
	if len(projection.BySite) != MaxBoundedAdjacencyKeys || len(projection.Truncated) != 0 {
		t.Fatalf("same-source sites = %d complete/%d truncated, want %d/0",
			len(projection.BySite), len(projection.Truncated), MaxBoundedAdjacencyKeys)
	}
}

func TestGraphBoundedAdjacencyTotalIdentityCap(t *testing.T) {
	g := New()
	sources := make([]string, 0, 65)
	for sourceIndex := 0; sourceIndex < 65; sourceIndex++ {
		source := fmt.Sprintf("source-%02d", sourceIndex)
		sources = append(sources, source)
		for edgeIndex := 0; edgeIndex < MaxBoundedAdjacencyRowsPerKey; edgeIndex++ {
			g.AddEdge(&Edge{
				From: source, To: fmt.Sprintf("target-%02d-%03d", sourceIndex, edgeIndex),
				Kind: EdgeCalls, Line: edgeIndex,
			})
		}
	}
	projection, err := g.FindOutgoingEdgeIdentitiesBounded(
		context.Background(), sources[:64], []EdgeKind{EdgeCalls}, MaxBoundedAdjacencyRowsPerKey,
	)
	if err != nil || len(projection.ByEndpoint) != 64 {
		t.Fatalf("exact aggregate cap = %d endpoints, %v", len(projection.ByEndpoint), err)
	}
	projection, err = g.FindOutgoingEdgeIdentitiesBounded(
		context.Background(), sources, []EdgeKind{EdgeCalls}, MaxBoundedAdjacencyRowsPerKey,
	)
	var limitErr *BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "adjacency inspected edge rows" || len(projection.ByEndpoint) != 0 {
		t.Fatalf("aggregate overflow = %#v, %v", projection, err)
	}
}
