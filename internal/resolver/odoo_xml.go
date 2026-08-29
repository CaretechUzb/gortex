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
func bindOdooXMLIDs(g graph.Store, edges []*graph.Edge, sc *odooSiblingCache, d *odooDecls) int {
	// No early return on empty indexes: a full recompute must still run
	// so edges whose target has left the graph are reset to their
	// placeholders and their siblings retired.
	byXMLID := d.xmlIDs
	byMethod := d.modelMethods

	var plans []odooBindPlan
	var extra []*graph.Edge
	observed := map[odooEdgeIdentity]*graph.Edge{}
	// The implicit index used to be built on first use, because it cost
	// three more whole-store node scans. It now rides the same two walks
	// as every other declaration index, so it is simply here.
	implicit := d.implicit

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
		return !sc.declares(e.From, xmlID, implicit.lookup(xmlID), e.To)
	}
	odooReconcileFanout(g, observed, extra, odooBoundIdentities(plans), stale)
	return resolved
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
