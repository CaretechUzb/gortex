package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// odooXMLEdgeKinds are the kinds an `odoo-xml` placeholder rides:
// inherit_id is specialisation, ref= is a reference, and <function> is a
// call into a Python method.
var odooXMLEdgeKinds = []graph.EdgeKind{
	graph.EdgeExtends,
	graph.EdgeReferences,
	graph.EdgeCalls,
}

// bindOdooXMLIDs binds external-ID references (`ref="sale.view_order"`,
// `inherit_id`, menu actions) onto the record they name, and binds
// `<function model= name=>` onto the Python method it invokes.
//
// An external ID no `<record>` declares may still be one the ORM mints
// for its own registry rows (`model_sale_order`, `field_x__y`,
// `module_sale`); those bind through the implicit index as a fallback, so
// a declared record always wins over the synthesized reading of a name.
func bindOdooXMLIDs(g graph.Store, edges []*graph.Edge, sc *odooSiblingCache) int {
	// No early return on empty indexes: a full recompute must still run
	// so edges whose target has left the graph are reset to their
	// placeholders and their siblings retired.
	byXMLID := odooXMLIDIndex(g)
	byMethod := odooModelMethodIndex(g)

	var plans []odooBindPlan
	var extra []*graph.Edge
	observed := map[odooEdgeIdentity]*graph.Edge{}
	// Built on first use: the implicit index costs three node scans and
	// most corpora reference no implicit external ID at all.
	var implicit *odooImplicitXMLIDs

	for _, e := range edges {
		// A materialised fan-out sibling has no placeholder of its own;
		// recomputing it would collapse the fan-out onto its first
		// target, so it is reconciled as a set instead.
		if odooIsFanout(e) {
			observed[odooEdgeIdentityOf(e)] = e
			continue
		}
		// A <function model= name=> edge carries both keys, so the
		// method form is checked first.
		if method := odooMetaString(e, "odoo_method"); method != "" {
			model := odooMetaString(e, "odoo_model")
			if model == "" {
				continue
			}
			key := model + "." + method
			plans = append(plans, odooBindPlan{
				edge:        e,
				target:      byMethod.lookup(sc, e.From, key),
				placeholder: odooMethodStubPrefix + key,
			})
			continue
		}
		xmlID := odooMetaString(e, "odoo_xml_id")
		if xmlID == "" {
			continue
		}
		target := odooLookupXMLID(sc, byXMLID, e.From, xmlID)
		var siblings []string
		if target == "" && odooIsImplicitXMLID(xmlID) {
			if implicit == nil {
				implicit = buildOdooImplicitXMLIDs(g)
			}
			if targets := sc.keep(e.From, xmlID, implicit.lookup(xmlID)); len(targets) > 0 {
				if len(targets) > odooFanoutCap {
					targets = targets[:odooFanoutCap]
				}
				target, siblings = targets[0], targets[1:]
			}
		}
		plans = append(plans, odooBindPlan{
			edge:        e,
			target:      target,
			placeholder: odooXMLIDStubPrefix + xmlID,
		})
		for _, t := range siblings {
			if t == e.From {
				// A class referencing its own model would be a
				// self-edge, which states nothing.
				continue
			}
			extra = append(extra, odooSiblingEdge(e, t))
		}
	}

	resolved := odooRebind(g, plans, ConfidenceTyped)
	// Only the implicit index ever fans out here — a declared external ID
	// is unique — so a sibling is stale once the implicit reading of its
	// own external ID stops naming its target.
	stale := func(e *graph.Edge) bool {
		xmlID := odooMetaString(e, "odoo_xml_id")
		if xmlID == "" || !odooIsImplicitXMLID(xmlID) {
			return false
		}
		if implicit == nil {
			implicit = buildOdooImplicitXMLIDs(g)
		}
		return !sc.declares(e.From, xmlID, implicit.lookup(xmlID), e.To)
	}
	odooReconcileFanout(g, observed, extra, odooBoundIdentities(plans), stale)
	return resolved
}

// odooXMLIDIndex maps an external ID to the record / template / menu node
// declaring it. A duplicate external ID is an Odoo error, so the first
// (deterministically lowest) ID wins rather than fanning out.
//
// Records are indexed under BOTH their qualified and bare forms. The
// module prefix is derived from the file path, and a repository that IS a
// single addon has no `<addon>/` segment in its repo-relative paths — so
// the same view can be declared bare and referenced qualified (or the
// reverse) purely because of repository layout. Indexing both forms makes
// the binding layout-independent; odooLookupXMLID still prefers an exact
// match, so a genuine cross-module reference is never mis-bound to a
// same-named record in another addon when the exact one exists.
func odooXMLIDIndex(g graph.Store) odooIndex {
	out := odooIndex{}
	put := out.put
	for n := range g.NodesByKind(graph.KindResource) {
		if n == nil || n.Meta == nil {
			continue
		}
		xmlID, _ := n.Meta["odoo_xml_id"].(string)
		if xmlID == "" {
			continue
		}
		put(xmlID, n.ID)
		put(odooBareXMLID(xmlID), n.ID)
	}
	return out
}

// odooBareXMLID drops a module prefix: `sale.view_order` -> `view_order`.
func odooBareXMLID(xmlID string) string {
	if i := strings.LastIndex(xmlID, "."); i >= 0 {
		return xmlID[i+1:]
	}
	return ""
}

// odooLookupXMLID resolves an external ID, preferring the exact form and
// falling back to the bare name for the layout reasons above.
func odooLookupXMLID(sc *odooSiblingCache, idx odooIndex, fromID, xmlID string) string {
	if target := idx.lookup(sc, fromID, xmlID); target != "" {
		return target
	}
	return idx.lookup(sc, fromID, odooBareXMLID(xmlID))
}

// odooModelMethodIndex maps "<model._name>.<method>" to the Python method
// node implementing it, so `<function model="sale.order" name="_do">`
// lands on real code.
//
// The join runs class → method rather than the reverse because a method
// node knows only its class, while the `_name` lives on the class.
func odooModelMethodIndex(g graph.Store) odooIndex {
	classModel := map[string]string{}
	for n := range g.NodesByKind(graph.KindType) {
		if n == nil || n.Meta == nil {
			continue
		}
		if model, _ := n.Meta["odoo_model"].(string); model != "" {
			classModel[n.ID] = model
		}
	}
	if len(classModel) == 0 {
		return nil
	}
	out := odooIndex{}
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Name == "" {
			continue
		}
		// Python method node IDs are shaped "<classID>.<method>".
		idx := strings.LastIndex(n.ID, ".")
		if idx <= 0 {
			continue
		}
		model := classModel[n.ID[:idx]]
		if model == "" {
			continue
		}
		out.put(model+"."+n.Name, n.ID)
	}
	return out
}
