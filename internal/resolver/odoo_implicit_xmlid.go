package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Implicit Odoo external IDs.
//
// Not every external ID has a `<record>` behind it. The ORM mints one for
// each of its own registry rows when a module is installed:
//
//	ir.model         -> <module>.model_<model with dots as underscores>
//	ir.model.fields  -> <module>.field_<model underscored>__<field>
//	ir.module.module -> <module>.module_<addon>
//
// Nothing declares them, yet they are how the whole access-control layer
// of an addon is written — `ir.model.access.csv`, `<record model="ir.rule">`
// and `<record model="ir.model.data">` reference models and fields
// exclusively through these names. Without them a corpus binds its views
// and menus but leaves every security rule dangling.
//
// They resolve as a FALLBACK, after the declared-record index has missed,
// so a real (if unusual) `<record id="model_foo">` still wins over the
// synthesized reading of the same name.

// Implicit external-ID prefixes. The three are mutually exclusive — none
// is a prefix of another — so one prefix test picks the family.
const (
	odooImplicitModelPrefix  = "model_"
	odooImplicitFieldPrefix  = "field_"
	odooImplicitModulePrefix = "module_"
)

// odooFieldIDSep separates a field node's declaring class from its name.
// The languages package does not import resolver, so the two sides agree
// by value, exactly as the placeholder prefixes do.
const odooFieldIDSep = "#field:"

// odooModuleIDMarker identifies an Odoo addon's KindModule node. Module
// IDs follow the `module::<ecosystem>:<name>@<version>` convention, and
// gating on the ecosystem keeps a same-named npm or pip package from
// being mistaken for the addon.
const odooModuleIDMarker = "module::odoo:"

// odooImplicitXMLIDs indexes the three implicit external-ID families.
type odooImplicitXMLIDs struct {
	// models fans out: an Odoo model is a name, and several addons
	// routinely declare the same `_name`. A security rule written
	// against `model_sale_order` governs all of them, so binding to
	// only the first would hide the rest.
	models  map[string][]string
	fields  map[string]string
	modules map[string]string
}

// odooImplicitXMLIDKey turns an Odoo model `_name` into the identifier
// form its implicit external IDs are built from: `sale.order` ->
// `sale_order`. The reverse mapping is ambiguous (`sale_order_line` could
// be `sale.order.line` or `sale.order_line`), which is why the index is
// built forwards from the models that actually exist rather than by
// guessing dots back into a name.
func odooImplicitXMLIDKey(model string) string {
	return strings.ReplaceAll(model, ".", "_")
}

// odooImplicitLocalID strips the module prefix from an external ID.
// Unlike odooBareXMLID it keeps an already-bare ID, because a same-module
// reference is written without one (`ref="model_sale_order"`).
func odooImplicitLocalID(xmlID string) string {
	if i := strings.LastIndex(xmlID, "."); i >= 0 {
		return xmlID[i+1:]
	}
	return xmlID
}

// odooIsImplicitXMLID reports whether an external ID has one of the three
// ORM-minted shapes. It is a pure string test so the (three-scan) index
// is built only once a corpus actually references an implicit ID.
func odooIsImplicitXMLID(xmlID string) bool {
	local := odooImplicitLocalID(xmlID)
	return strings.HasPrefix(local, odooImplicitModelPrefix) ||
		strings.HasPrefix(local, odooImplicitFieldPrefix) ||
		strings.HasPrefix(local, odooImplicitModulePrefix)
}

// buildOdooImplicitXMLIDs indexes every model, field and addon the graph
// knows under the external ID the ORM would mint for it.
func buildOdooImplicitXMLIDs(g graph.Store) *odooImplicitXMLIDs {
	idx := &odooImplicitXMLIDs{
		models:  map[string][]string{},
		fields:  map[string]string{},
		modules: map[string]string{},
	}
	if g == nil {
		return idx
	}

	// classModel is built in the same walk the model index needs, so the
	// field join below costs one extra scan rather than two.
	classModel := map[string]string{}
	for n := range g.NodesByKind(graph.KindType) {
		if n == nil || n.Meta == nil {
			continue
		}
		model, _ := n.Meta["odoo_model"].(string)
		if model == "" {
			continue
		}
		classModel[n.ID] = model
		key := odooImplicitXMLIDKey(model)
		idx.models[key] = append(idx.models[key], n.ID)
	}
	for _, ids := range idx.models {
		sort.Strings(ids)
	}

	if len(classModel) > 0 {
		for n := range g.NodesByKind(graph.KindField) {
			if n == nil || n.Name == "" {
				continue
			}
			cut := strings.LastIndex(n.ID, odooFieldIDSep)
			if cut <= 0 {
				continue
			}
			model := classModel[n.ID[:cut]]
			if model == "" {
				continue
			}
			key := odooImplicitXMLIDKey(model) + "__" + n.Name
			if prev, ok := idx.fields[key]; !ok || n.ID < prev {
				idx.fields[key] = n.ID
			}
		}
	}

	for n := range g.NodesByKind(graph.KindModule) {
		if n == nil || n.Name == "" || !strings.Contains(n.ID, odooModuleIDMarker) {
			continue
		}
		if prev, ok := idx.modules[n.Name]; !ok || n.ID < prev {
			idx.modules[n.Name] = n.ID
		}
	}
	return idx
}

// lookup returns every node the implicit external ID denotes. Only the
// model family can return more than one.
func (idx *odooImplicitXMLIDs) lookup(xmlID string) []string {
	if idx == nil {
		return nil
	}
	local := odooImplicitLocalID(xmlID)
	switch {
	case strings.HasPrefix(local, odooImplicitFieldPrefix):
		if id := idx.fields[strings.TrimPrefix(local, odooImplicitFieldPrefix)]; id != "" {
			return []string{id}
		}
	case strings.HasPrefix(local, odooImplicitModulePrefix):
		if id := idx.modules[strings.TrimPrefix(local, odooImplicitModulePrefix)]; id != "" {
			return []string{id}
		}
	case strings.HasPrefix(local, odooImplicitModelPrefix):
		return idx.models[strings.TrimPrefix(local, odooImplicitModelPrefix)]
	}
	return nil
}
