package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// odooModelEdgeKinds are the kinds an `odoo-model` placeholder rides:
// _inherit is specialisation, _inherits is composition, and a comodel or
// a <record model=> is a reference.
var odooModelEdgeKinds = []graph.EdgeKind{
	graph.EdgeExtends,
	graph.EdgeComposes,
	graph.EdgeReferences,
}

// bindOdooModels binds every reference to an Odoo model by its `_name`
// string onto the Python class (or classes) that declare it.
//
// Fan-out is expected and correct here in a way it is not for most
// binders: an Odoo model is a name, not a class, and several addons
// routinely extend one `_name` with `_inherit`. Binding to only one of
// them would silently hide the others, so every declaring class is bound.
// The first target keeps the original edge; the rest are materialised as
// sibling edges.
func bindOdooModels(g graph.Store) int {
	byModel := odooModelIndex(g)
	if len(byModel) == 0 {
		return 0
	}

	var plans []odooBindPlan
	var extra []*graph.Edge

	odooCollect(g, odooModelVia, odooModelEdgeKinds, func(e *graph.Edge) {
		if odooIsFanout(e) {
			return
		}
		model := odooMetaString(e, "odoo_model")
		if model == "" {
			return
		}
		placeholder := odooModelStubPrefix + model
		targets := byModel[model]
		if len(targets) == 0 {
			plans = append(plans, odooBindPlan{edge: e, placeholder: placeholder})
			return
		}
		if len(targets) > odooFanoutCap {
			targets = targets[:odooFanoutCap]
		}
		plans = append(plans, odooBindPlan{edge: e, target: targets[0], placeholder: placeholder})
		for _, t := range targets[1:] {
			if t == e.From {
				// A class extending its own model name would be a
				// self-edge, which states nothing.
				continue
			}
			extra = append(extra, odooSiblingEdge(e, t))
		}
	})

	resolved := odooRebind(g, plans, ConfidenceTyped)
	// AddEdge is idempotent on (From, To, Kind), so re-running the pass
	// re-offers the same sibling edges without duplicating them.
	for _, e := range extra {
		g.AddEdge(e)
	}
	return resolved
}

// odooSiblingEdge clones a bound placeholder onto an additional target —
// the extra classes that declare the same `_name`.
func odooSiblingEdge(src *graph.Edge, target string) *graph.Edge {
	meta := map[string]any{}
	for k, v := range src.Meta {
		meta[k] = v
	}
	meta[MetaSynthesizedBy] = SynthOdoo
	meta[MetaProvenance] = ProvenanceFramework
	meta["odoo_fanout"] = true
	return &graph.Edge{
		From: src.From, To: target, Kind: src.Kind,
		FilePath: src.FilePath, Line: src.Line,
		Origin: graph.OriginASTInferred, Confidence: ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(src.Kind, ConfidenceTyped),
		Meta:            meta,
	}
}

// odooModelIndex maps an Odoo model `_name` to every class node declaring
// it. Targets are sorted so a fan-out binds deterministically across runs.
func odooModelIndex(g graph.Store) map[string][]string {
	out := map[string][]string{}
	for n := range g.NodesByKind(graph.KindType) {
		if n == nil || n.Meta == nil {
			continue
		}
		model, _ := n.Meta["odoo_model"].(string)
		if model == "" {
			continue
		}
		out[model] = append(out[model], n.ID)
	}
	for _, ids := range out {
		sort.Strings(ids)
	}
	return out
}
