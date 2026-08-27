package resolver

import "github.com/zzet/gortex/internal/graph"

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
func bindOdooModels(g graph.Store, edges []*graph.Edge, sc *odooSiblingCache, d *odooDecls) int {
	// No early return on an empty index: a full recompute must still run
	// so edges whose declaring class has left the graph are reset to
	// their placeholders and their siblings retired, rather than left
	// pointing at a node that is gone.
	byModel := d.models

	var plans []odooBindPlan
	var extra []*graph.Edge
	observed := map[odooEdgeIdentity]*graph.Edge{}

	for _, e := range edges {
		if odooIsFanout(e) {
			// Recomputing a sibling would collapse the fan-out onto its
			// first target; it is reconciled as a set instead.
			observed[odooEdgeIdentityOf(e)] = e
			continue
		}
		model := odooMetaString(e, "odoo_model")
		if model == "" {
			continue
		}
		placeholder := odooModelStubPrefix + model
		targets := sc.keep(e.From, model, byModel[model])
		if len(targets) == 0 {
			plans = append(plans, odooBindPlan{edge: e, placeholder: placeholder})
			continue
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
	}

	resolved := odooRebind(g, plans, ConfidenceTyped)
	// A sibling is stale once its target stops being one of the classes
	// declaring its model — which covers the deleted class too, since
	// odooModelIndex is built from live nodes only.
	stale := func(e *graph.Edge) bool {
		model := odooMetaString(e, "odoo_model")
		if model == "" {
			return false
		}
		return !sc.declares(e.From, model, byModel[model], e.To)
	}
	odooReconcileFanout(g, observed, extra, odooBoundIdentities(plans), stale)
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
