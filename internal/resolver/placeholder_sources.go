package resolver

import (
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// placeholderSourceIndex is the per-pass set of from-IDs that dataflow edges
// (arg_of, value_flow) actually key from unresolved placeholders. Built with
// ONE EdgesByKind stream per pass and consulted before any adjacency probe:
// a cold pass resolves hundreds of thousands of placeholders while only a
// few thousand ever carry dataflow sources, so probing unconditionally would
// be a point-lookup storm (and would violate the compute loop's interleave
// cache contract, which the no-op-yield test pins).
type placeholderSourceIndex struct {
	built bool
	froms map[string]struct{}
}

func (idx *placeholderSourceIndex) ensure(g graph.Store) {
	if idx.built {
		return
	}
	idx.built = true
	for _, kind := range []graph.EdgeKind{graph.EdgeArgOf, graph.EdgeValueFlow} {
		for e := range graph.EdgesLightSeq(g, kind) {
			if e == nil || !strings.Contains(e.From, graph.UnresolvedMarker) {
				continue
			}
			if idx.froms == nil {
				idx.froms = make(map[string]struct{})
			}
			idx.froms[e.From] = struct{}{}
		}
	}
}

// filter keeps only repoints whose source form actually exists in the
// dataflow placeholder set.
func (idx *placeholderSourceIndex) filter(repoints []graph.PlaceholderRepoint) []graph.PlaceholderRepoint {
	if len(idx.froms) == 0 {
		return nil
	}
	kept := repoints[:0]
	for _, rp := range repoints {
		if _, ok := idx.froms[rp.OldFrom]; ok {
			kept = append(kept, rp)
			// A moved source will not match again; keep the set honest so
			// repeated resolutions of the same shared placeholder at other
			// sites still probe (the site match decides), but a fully moved
			// site costs at most one empty probe.
		}
	}
	return kept
}

// reconcilePlaceholderSources re-points dataflow edges keyed FROM an
// unresolved placeholder once a resolution batch rewrites that placeholder's
// reference at the same site — see graph.ReconcilePlaceholderSources for the
// exact-site contract. idx amortizes the placeholder-source discovery to one
// stream per pass; a nil idx (incremental single-file / scoped passes) probes
// candidates directly — batches there are file-sized, so the prefilter's
// whole-graph stream would cost more than the probes it avoids.
// GORTEX_RESOLVE_FROM_RECONCILE=0 disables the reconciliation entirely.
func reconcilePlaceholderSources(g graph.Store, idx *placeholderSourceIndex, reindexes []graph.EdgeReindex) int {
	if len(reindexes) == 0 || os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0" {
		return 0
	}
	repoints := graph.PlaceholderSourceRepoints(reindexes)
	if len(repoints) == 0 {
		return 0
	}
	if idx != nil {
		idx.ensure(g)
		repoints = idx.filter(repoints)
		if len(repoints) == 0 {
			return 0
		}
	}
	return graph.ReconcilePlaceholderSources(g, repoints)
}
