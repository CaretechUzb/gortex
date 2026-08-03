package indexer

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestApplyCloneSignaturesSkipsOffsetsWithoutCallables(t *testing.T) {
	src := []byte(strings.Repeat("generated data row\n", 4096))
	result := &parser.ExtractionResult{Nodes: []*graph.Node{
		nil,
		{Kind: graph.KindFile},
		{Kind: graph.KindVariable},
	}}

	allocs := testing.AllocsPerRun(20, func() {
		applyCloneSignatures(src, result)
	})
	if allocs != 0 {
		t.Fatalf("applyCloneSignatures allocated %.1f times without callable nodes; want 0", allocs)
	}
}

func TestApplyCloneSignaturesPreservesCallableShingles(t *testing.T) {
	src := []byte(`func example(items []int) int {
	total := 0
	for index, item := range items {
		if item > 0 {
			total += item * index
		} else {
			total -= index
		}
	}
	if total < 0 {
		total = -total
	}
	return total
}`)

	for _, kind := range []graph.NodeKind{graph.KindFunction, graph.KindMethod} {
		t.Run(string(kind), func(t *testing.T) {
			node := &graph.Node{Kind: kind, StartLine: 1, EndLine: 13}
			result := &parser.ExtractionResult{Nodes: []*graph.Node{
				{Kind: graph.KindFile},
				node,
			}}

			applyCloneSignatures(src, result)

			shingles, ok := node.Meta[cloneShinglesMetaKey].([]uint64)
			if !ok || len(shingles) == 0 {
				t.Fatal("callable node did not receive clone shingles")
			}
			if tokens, ok := node.Meta[cloneTokensMetaKey].(int); !ok || tokens == 0 {
				t.Fatal("callable node did not receive a clone token count")
			}
		})
	}
}

func BenchmarkApplyCloneSignaturesNoCallables(b *testing.B) {
	src := []byte(strings.Repeat("generated data row\n", 1<<17))
	result := &parser.ExtractionResult{Nodes: []*graph.Node{
		nil,
		{Kind: graph.KindFile},
		{Kind: graph.KindVariable},
	}}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		applyCloneSignatures(src, result)
	}
}
