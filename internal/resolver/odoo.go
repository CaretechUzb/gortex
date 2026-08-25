package resolver

import "github.com/zzet/gortex/internal/graph"

// Odoo binding.
//
// Odoo is registered as ONE framework named "odoo" — not split into
// odoo-model / odoo-xml / odoo-js — so a single index.frameworks.allow
// entry governs the whole framework and it can never end up half-indexed
// with models bound but templates dangling.
//
// The three binding families are therefore ordered sub-steps of one pass
// rather than three registry entries. The order is load-bearing and is
// enforced here, by call sequence, rather than by registry adjacency:
//
//  1. models — a <record model="sale.order"> can only bind once the
//     Python model classes are indexed by their `_name`;
//  2. XML IDs — view inheritance and menu/action chains;
//  3. JS — an OWL component's `static template` binds to a QWeb template
//     node, which step 2 is what mints.
//
// Every placeholder these binders consume was pre-tagged by the extractor
// with Meta["via"], so none of them needs the claiming-resolver tail.

// Odoo placeholder prefixes, matching what the extractors emit. The
// languages package does not import resolver, so the two sides agree by
// value — the same arrangement celery and mybatis use.
const (
	odooModelStubPrefix    = unresolvedPrefix + "odoo::model::"
	odooXMLIDStubPrefix    = unresolvedPrefix + "odoo::xmlid::"
	odooMethodStubPrefix   = unresolvedPrefix + "odoo::method::"
	odooTemplateStubPrefix = unresolvedPrefix + "odoo::template::"
	odooJSModuleStubPrefix = unresolvedPrefix + "odoo::jsmodule::"
	odooJSMethodStubPrefix = unresolvedPrefix + "odoo::jsmethod::"
)

// Odoo `via` tags. These are internal index keys that let each sub-binder
// scan only its own placeholders; they are NOT framework names and never
// appear in the exclusion inventory or in synthesized_by, which is always
// "odoo".
const (
	odooModelVia = "odoo-model"
	odooXMLVia   = "odoo-xml"
	odooJSVia    = "odoo-js"
)

// odooFanoutCap bounds how many targets one placeholder may bind. A model
// legitimately fans out — several addons extend one `_name` — but an
// unbounded fan-out on a pathological corpus would be a graph bomb.
const odooFanoutCap = 200

// ResolveOdooRefs binds every Odoo placeholder in the graph. It is the
// single registered synthesizer for the odoo framework; see the package
// comment above for why the three families run in this fixed order.
func ResolveOdooRefs(g graph.Store) int {
	if g == nil {
		return 0
	}
	n := bindOdooModels(g)
	n += bindOdooXMLIDs(g)
	n += bindOdooJS(g)
	return n
}

// odooBindPlan is one edge queued for rebinding. placeholder is the
// target to restore when target is empty, which is what makes the pass a
// full recompute rather than a one-way binding.
type odooBindPlan struct {
	edge        *graph.Edge
	target      string
	placeholder string
}

// odooRebind applies a batch of rebindings, stamping Odoo provenance and
// keeping the graph's edge buckets consistent.
//
// Like the other framework binders this is a FULL RECOMPUTE, and each
// plan therefore carries both the resolved target and the placeholder to
// fall back to. Every pass recomputes an edge's destination from its own
// Meta rather than from its current target, so re-running the pass is a
// no-op, and an edge whose target has since left the graph is reset to
// its placeholder and loses its resolution metadata instead of silently
// pointing at a node that no longer exists.
func odooRebind(g graph.Store, plans []odooBindPlan, confidence float64) int {
	var batch []graph.EdgeReindex
	resolved := 0
	for _, p := range plans {
		e := p.edge
		if e == nil {
			continue
		}
		want, bound := p.target, p.target != ""
		if !bound {
			want = p.placeholder
		}
		if want == "" {
			continue
		}
		if bound {
			resolved++
		}
		if e.To == want {
			continue
		}
		oldTo := e.To
		e.To = want
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		if bound {
			e.Origin = graph.OriginASTInferred
			e.Confidence = confidence
			e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, confidence)
			e.Meta[MetaSynthesizedBy] = SynthOdoo
			e.Meta[MetaProvenance] = ProvenanceFramework
		} else {
			// Re-orphaned since the last pass: drop the resolution-tier
			// metadata so the edge reads as a plain placeholder again.
			e.Origin = ""
			e.Confidence = 0
			e.ConfidenceLabel = ""
			delete(e.Meta, MetaSynthesizedBy)
			delete(e.Meta, MetaProvenance)
		}
		batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return resolved
}

// odooMetaString reads a string value off an edge's Meta.
func odooMetaString(e *graph.Edge, key string) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	v, _ := e.Meta[key].(string)
	return v
}

// odooIsFanout reports whether an edge is one of the extra targets
// bindOdooModels materialised for a multiply-declared model. Those are
// derived edges: recomputing them would collapse the fan-out back onto
// its first target.
func odooIsFanout(e *graph.Edge) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	v, _ := e.Meta["odoo_fanout"].(bool)
	return v
}

// odooEdgeVia reports the edge's Odoo via tag.
func odooEdgeVia(e *graph.Edge) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	v, _ := e.Meta["via"].(string)
	return v
}

// odooCollect walks the kinds an Odoo placeholder can ride and yields the
// edges carrying the given via tag. Odoo placeholders do NOT ride
// EdgeCalls alone — they use extends / composes / references / imports /
// renders_child / overrides — which is also why the framework registry
// gates this pass on a node marker rather than on the call-edge census.
func odooCollect(g graph.Store, via string, kinds []graph.EdgeKind, fn func(*graph.Edge)) {
	for _, kind := range kinds {
		for e := range g.EdgesByKind(kind) {
			if e == nil || odooEdgeVia(e) != via {
				continue
			}
			fn(e)
		}
	}
}

// odooNodeMarker reports whether a node belongs to the Odoo framework, in
// any of its three halves. It feeds the registry's node preflight: the
// pass runs iff the graph contains anything Odoo at all.
func odooNodeMarker(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if n.Language == "odoo_xml" {
		return true
	}
	if n.Meta == nil {
		return false
	}
	for _, key := range []string{"odoo_model", "odoo_xml_id", "odoo_js_module", "odoo_module"} {
		if v, ok := n.Meta[key]; ok && v != nil && v != "" {
			return true
		}
	}
	return false
}
