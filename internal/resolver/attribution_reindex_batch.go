package resolver

import "github.com/zzet/gortex/internal/graph"

// attributionReindexBatchSize bounds the number of full edge payloads retained
// while attribution passes rewrite graph targets. It is deliberately below the
// SQLite store's transaction chunk so one resolver batch remains one write.
const attributionReindexBatchSize = 2048

// attributionReindexCollector preserves rewrite order while keeping both the
// caller-local buffer and the incremental cross-file queue bounded.
type attributionReindexCollector struct {
	resolver *Resolver
	batch    []graph.EdgeReindex
}

func newAttributionReindexCollector(r *Resolver) *attributionReindexCollector {
	return &attributionReindexCollector{resolver: r}
}

func (c *attributionReindexCollector) add(edge *graph.Edge, oldTo string) {
	if edge == nil || oldTo == "" {
		return
	}
	c.batch = append(c.batch, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
	if len(c.batch) == attributionReindexBatchSize {
		c.flush()
	}
}

func (c *attributionReindexCollector) flush() {
	if len(c.batch) == 0 {
		return
	}
	c.resolver.persistAttributionReindexes(c.batch)
	// Drop the backing array as well as its length: EdgeReindex contains a full
	// *Edge, so retaining capacity would keep already-written payloads alive.
	c.batch = nil
}

// reindexAttributionEdgesBatched scans only the requested edge kinds and
// rewrites them in bounded batches. Backends with a native batch scanner close
// each read cursor before the callback, allowing ReindexEdges to safely re-enter
// a single-connection store.
func (r *Resolver) reindexAttributionEdgesBatched(
	kinds []graph.EdgeKind,
	rewrite func(*graph.Edge) string,
) {
	collector := newAttributionReindexCollector(r)
	graph.ScanEdgesByKindsBatched(r.graph, kinds, attributionReindexBatchSize, func(edges []*graph.Edge) bool {
		for _, edge := range edges {
			collector.add(edge, rewrite(edge))
		}
		return true
	})
	collector.flush()
}
