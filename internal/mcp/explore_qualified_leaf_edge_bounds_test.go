package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

type qualifiedLeafEdgeProjectionReader struct {
	*exactNameAnchorCountingReader
	edgeCalls  int
	endpoints  []graph.TypedEdgeEndpoint
	edgeLimit  int
	projection func(context.Context, []graph.TypedEdgeEndpoint, int) (map[graph.TypedEdgeEndpoint]struct{}, error)
}

func (reader *qualifiedLeafEdgeProjectionReader) FindExistingEdgeEndpoints(
	ctx context.Context,
	endpoints []graph.TypedEdgeEndpoint,
	limit int,
) (map[graph.TypedEdgeEndpoint]struct{}, error) {
	reader.edgeCalls++
	reader.endpoints = append([]graph.TypedEdgeEndpoint(nil), endpoints...)
	reader.edgeLimit = limit
	if reader.projection != nil {
		return reader.projection(ctx, endpoints, limit)
	}
	return map[graph.TypedEdgeEndpoint]struct{}{}, nil
}

func (*qualifiedLeafEdgeProjectionReader) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	panic("qualified-leaf proof must not materialize legacy outgoing adjacency")
}

type qualifiedLeafLegacyPanicReader struct {
	*exactNameAnchorCountingReader
}

func (*qualifiedLeafLegacyPanicReader) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	panic("qualified-leaf proof must fail closed without the bounded edge capability")
}

func TestExploreQualifiedLeafUsesBoundedExactEdgeProof(t *testing.T) {
	anchor, owner, _ := qualifiedLeafFixture()
	requested := &graph.Node{
		ID: owner.FilePath + "::buildContent#requested", Name: "buildContent",
		Kind: graph.KindMethod, FilePath: owner.FilePath, StartLine: 40,
	}
	ambiguous := &graph.Node{
		ID: owner.FilePath + "::buildContent#ambiguous", Name: "buildContent",
		Kind: graph.KindMethod, FilePath: owner.FilePath, StartLine: 50,
	}
	base := graph.New()
	for _, node := range []*graph.Node{owner, requested, ambiguous} {
		base.AddNode(node)
	}
	proof := graph.TypedEdgeEndpoint{From: requested.ID, To: owner.ID, Kind: graph.EdgeMemberOf}
	reader := &qualifiedLeafEdgeProjectionReader{
		exactNameAnchorCountingReader: &exactNameAnchorCountingReader{Reader: base},
		projection: func(_ context.Context, endpoints []graph.TypedEdgeEndpoint, limit int) (map[graph.TypedEdgeEndpoint]struct{}, error) {
			return map[graph.TypedEdgeEndpoint]struct{}{proof: {}}, nil
		},
	}
	server := &Server{graph: base}
	got := server.exploreQualifiedLeafCandidate(
		context.Background(), reader, anchor, []*rerank.Candidate{{Node: owner}},
		query.QueryOptions{}, map[string]struct{}{}, map[string]struct{}{},
	)
	if got == nil || got.Node == nil || got.Node.ID != requested.ID {
		t.Fatalf("qualified leaf = %#v, want exact member_of proof for %q", got, requested.ID)
	}
	if reader.edgeCalls != 1 || reader.edgeLimit != exploreQualifiedLeafEndpointLimit {
		t.Fatalf("edge projection calls/limit = %d/%d, want 1/%d", reader.edgeCalls, reader.edgeLimit, exploreQualifiedLeafEndpointLimit)
	}
	if len(reader.endpoints) != 2 {
		t.Fatalf("edge predicates = %#v, want the two exact member_of alternatives", reader.endpoints)
	}
	seen := make(map[graph.TypedEdgeEndpoint]struct{}, len(reader.endpoints))
	for _, endpoint := range reader.endpoints {
		seen[endpoint] = struct{}{}
		if endpoint.Kind != graph.EdgeMemberOf || endpoint.To != owner.ID {
			t.Fatalf("unexpected edge predicate: %#v", endpoint)
		}
	}
	if _, ok := seen[proof]; !ok {
		t.Fatalf("edge predicates = %#v, missing requested proof %#v", reader.endpoints, proof)
	}
}

func TestExploreQualifiedLeafEdgeProofFailsClosed(t *testing.T) {
	anchor, owner, _ := qualifiedLeafFixture()
	bare := &graph.Node{
		ID: owner.FilePath + "::buildContent", Name: "buildContent",
		Kind: graph.KindMethod, FilePath: owner.FilePath, StartLine: 40,
	}
	base := graph.New()
	base.AddNode(owner)
	base.AddNode(bare)
	server := &Server{graph: base}
	ordinary := []*rerank.Candidate{{Node: owner}}

	t.Run("unsupported", func(t *testing.T) {
		reader := &qualifiedLeafLegacyPanicReader{
			exactNameAnchorCountingReader: &exactNameAnchorCountingReader{Reader: base},
		}
		if got := server.exploreQualifiedLeafCandidate(
			context.Background(), reader, anchor, ordinary, query.QueryOptions{},
			map[string]struct{}{}, map[string]struct{}{},
		); got != nil {
			t.Fatalf("qualified leaf = %#v, want missing bounded capability to fail closed", got)
		}
	})

	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "error", err: errors.New("projection failed")},
		{name: "canceled", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &qualifiedLeafEdgeProjectionReader{
				exactNameAnchorCountingReader: &exactNameAnchorCountingReader{Reader: base},
				projection: func(context.Context, []graph.TypedEdgeEndpoint, int) (map[graph.TypedEdgeEndpoint]struct{}, error) {
					return map[graph.TypedEdgeEndpoint]struct{}{
						{From: bare.ID, To: owner.ID, Kind: graph.EdgeMemberOf}: {},
					}, test.err
				},
			}
			if got := server.exploreQualifiedLeafCandidate(
				context.Background(), reader, anchor, ordinary, query.QueryOptions{},
				map[string]struct{}{}, map[string]struct{}{},
			); got != nil {
				t.Fatalf("qualified leaf = %#v, want %v to discard partial proof", got, test.err)
			}
			if reader.edgeCalls != 1 {
				t.Fatalf("edge projection calls = %d, want one bounded attempt", reader.edgeCalls)
			}
		})
	}

	t.Run("request canceled after projection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &qualifiedLeafEdgeProjectionReader{
			exactNameAnchorCountingReader: &exactNameAnchorCountingReader{Reader: base},
			projection: func(context.Context, []graph.TypedEdgeEndpoint, int) (map[graph.TypedEdgeEndpoint]struct{}, error) {
				cancel()
				return map[graph.TypedEdgeEndpoint]struct{}{
					{From: bare.ID, To: owner.ID, Kind: graph.EdgeMemberOf}: {},
				}, nil
			},
		}
		if got := server.exploreQualifiedLeafCandidate(
			ctx, reader, anchor, ordinary, query.QueryOptions{},
			map[string]struct{}{}, map[string]struct{}{},
		); got != nil {
			t.Fatalf("qualified leaf = %#v, want post-projection cancellation to discard proof", got)
		}
	})
}

func TestExploreQualifiedLeafEdgeProofUsesExactPredicateCap(t *testing.T) {
	if exploreQualifiedLeafEndpointLimit != graph.MaxBoundedEdgeExistencePredicates {
		t.Fatalf("MCP/store predicate caps drifted: %d != %d", exploreQualifiedLeafEndpointLimit, graph.MaxBoundedEdgeExistencePredicates)
	}
	anchor, _, _ := qualifiedLeafFixture()
	const path = "src/HipChatHandler.php"
	base := graph.New()
	owners := make(map[string]struct{}, exploreExactNameAnchorOwnerScan)
	for index := 0; index < exploreExactNameAnchorOwnerScan; index++ {
		node := &graph.Node{
			ID: fmt.Sprintf("%s::HipChatHandler#%02d", path, index), Name: "HipChatHandler",
			Kind: graph.KindType, FilePath: path, StartLine: index + 1,
		}
		base.AddNode(node)
		owners[node.ID] = struct{}{}
	}
	leaves := make(map[string]struct{}, exploreSyntacticAnchorFetch)
	for index := 0; index < exploreSyntacticAnchorFetch; index++ {
		node := &graph.Node{
			ID: fmt.Sprintf("%s::leaf#%02d", path, index), Name: "buildContent",
			Kind: graph.KindMethod, FilePath: path, StartLine: 100 + index,
		}
		base.AddNode(node)
		leaves[node.ID] = struct{}{}
	}
	ranked := &graph.Node{ID: path + "::ranked", Name: "ranked", Kind: graph.KindFunction, FilePath: path}
	base.AddNode(ranked)
	reader := &qualifiedLeafEdgeProjectionReader{
		exactNameAnchorCountingReader: &exactNameAnchorCountingReader{Reader: base},
	}
	server := &Server{graph: base}
	if got := server.exploreQualifiedLeafCandidate(
		context.Background(), reader, anchor, []*rerank.Candidate{{Node: ranked}},
		query.QueryOptions{}, map[string]struct{}{}, map[string]struct{}{},
	); got != nil {
		t.Fatalf("qualified leaf = %#v, want ten ambiguous unproven leaves", got)
	}
	if reader.edgeCalls != 1 || reader.edgeLimit != exploreQualifiedLeafEndpointLimit ||
		len(reader.endpoints) != exploreQualifiedLeafEndpointLimit {
		t.Fatalf("edge calls/limit/predicates = %d/%d/%d, want 1/%d/%d",
			reader.edgeCalls, reader.edgeLimit, len(reader.endpoints),
			exploreQualifiedLeafEndpointLimit, exploreQualifiedLeafEndpointLimit)
	}
	seen := make(map[graph.TypedEdgeEndpoint]struct{}, len(reader.endpoints))
	for _, endpoint := range reader.endpoints {
		if endpoint.Kind != graph.EdgeMemberOf {
			t.Fatalf("edge predicate kind = %q, want member_of", endpoint.Kind)
		}
		if _, ok := leaves[endpoint.From]; !ok {
			t.Fatalf("edge predicate source = %q, want one of the ten leaves", endpoint.From)
		}
		if _, ok := owners[endpoint.To]; !ok {
			t.Fatalf("edge predicate target = %q, want one of the 32 owners", endpoint.To)
		}
		if _, duplicate := seen[endpoint]; duplicate {
			t.Fatalf("duplicate edge predicate: %#v", endpoint)
		}
		seen[endpoint] = struct{}{}
	}
}
