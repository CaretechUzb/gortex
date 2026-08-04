package semantic

import "context"

// enrichmentAdmissionNodesKey carries a conservative work-size hint from the
// manager to compiler-backed providers. It is deliberately context-scoped: a
// shared provider serves many repositories concurrently, so mutable provider
// state cannot safely describe the repository currently entering admission.
type enrichmentAdmissionNodesKey struct{}

// WithEnrichmentAdmissionNodes attaches the number of enrichable nodes for the
// repository being dispatched. Providers may use it only for scheduling; it
// must not change enrichment results. Negative counts mean no census evidence
// and leave the context unchanged.
func WithEnrichmentAdmissionNodes(ctx context.Context, nodes int) context.Context {
	if nodes < 0 {
		return ctx
	}
	return context.WithValue(ctx, enrichmentAdmissionNodesKey{}, nodes)
}

// EnrichmentAdmissionNodes returns the manager-supplied repository work-size
// hint. ok is false for direct provider calls that bypass Manager; heavyweight
// providers should treat that unknown case conservatively.
func EnrichmentAdmissionNodes(ctx context.Context) (nodes int, ok bool) {
	if ctx == nil {
		return 0, false
	}
	nodes, ok = ctx.Value(enrichmentAdmissionNodesKey{}).(int)
	return nodes, ok
}
