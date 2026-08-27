package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// One walk per projection, not one scan per index.
//
// Every Odoo binder opens by building the declaration indexes it looks
// through: `_name` to declaring class, external ID to record, QWeb template
// to markup, JS specifier to file, "<model>.<method>" to method, plus the
// implicit `model_` / `field_` / `module_` external IDs the ORM mints.
// Built one function at a time, that was eleven whole-store NodesByKind
// scans per pass over four kinds — KindType four times, KindMethod twice,
// KindResource twice, KindFile / KindField / KindModule once each — and
// NodesByKind materialises complete nodes, so each scan decoded doc
// comments, signatures, section text and the search projections it had no
// use for.
//
// Measured on a 855k-node workspace, that was ~253s per pass, and it was
// paid in full even by a re-derive whose edge frontier was four files:
// the indexes are whole-store by construction, because a reference in one
// repository binds to a declaration in another, so no repository scope
// narrows them. The only lever is how much of each row crosses the storage
// boundary, and how many times.
//
// So the indexes are built together instead: one FrameworkDeclNodesSeq walk
// over the three meta-bearing kinds, then one NodeIDNamesByKindsSeq walk per
// identity-only kind. Two column sets, four queries, no complete node
// decoded anywhere.
//
// The implicit external-ID index used to be built lazily, on the first
// reference that looked like one, because it cost three more scans. It now
// rides the same two walks, so the laziness bought nothing and is gone —
// which also removes the case where the retirement predicate built it a
// second time.

// odooDecls holds every declaration index the three Odoo binders read.
// Built once per ResolveOdooRefsScoped call and not mutated afterwards —
// odooSiblingCache memoizes filtered views of these maps and would serve
// stale answers if they moved underneath it.
type odooDecls struct {
	// models maps an Odoo `_name` to every class node declaring it,
	// sorted so a fan-out binds deterministically across runs.
	models map[string][]string
	// xmlIDs maps a declared external ID, and its module-less bare form,
	// to the record / view / menu / template node declaring it.
	xmlIDs odooIndex
	// templates maps a QWeb template name, and its bare form, to markup.
	templates odooIndex
	// jsModules maps every specifier shape an Odoo JS import can take to
	// the file node answering it.
	jsModules odooIndex
	// modelMethods maps "<model>.<method>" to the Python method node.
	modelMethods odooIndex
	// jsMethods maps "<ClassName>.<method>" to the method a patch(...)
	// call overrides.
	jsMethods odooIndex
	// implicit holds the external IDs the ORM mints rather than declares.
	implicit *odooImplicitXMLIDs

	// classModel and classNames are the class-side halves of the two
	// method joins. A method node knows only its owning class; the
	// `_name` and the JS class name live on the class, so they are
	// collected in the KindType walk and consumed in the KindMethod one.
	classModel map[string]string
	classNames map[string]string
}

// buildOdooDecls builds every Odoo declaration index in two projected walks.
func buildOdooDecls(g graph.Store) *odooDecls {
	d := &odooDecls{
		models:       map[string][]string{},
		xmlIDs:       odooIndex{},
		templates:    odooIndex{},
		jsModules:    odooIndex{},
		modelMethods: odooIndex{},
		jsMethods:    odooIndex{},
		implicit: &odooImplicitXMLIDs{
			models:  map[string][]string{},
			fields:  map[string]string{},
			modules: map[string]string{},
		},
		classModel: map[string]string{},
		classNames: map[string]string{},
	}
	if g == nil {
		return d
	}

	// Walk one: the kinds whose declaration lives in node metadata.
	for n := range graph.FrameworkDeclNodesSeq(g,
		graph.KindType, graph.KindResource, graph.KindFile) {
		switch n.Kind {
		case graph.KindType:
			d.absorbType(n)
		case graph.KindResource:
			d.absorbResource(n)
		case graph.KindFile:
			d.absorbFile(n)
		}
	}
	for _, ids := range d.models {
		sort.Strings(ids)
	}
	for _, ids := range d.implicit.models {
		sort.Strings(ids)
	}

	// Walk two: the kinds joined by node ID alone. Methods and fields are
	// skipped outright when walk one found no class to join them against,
	// which is every workspace holding no Odoo code.
	kinds := []graph.NodeKind{graph.KindModule}
	if len(d.classModel) > 0 || len(d.classNames) > 0 {
		kinds = append(kinds, graph.KindMethod)
	}
	if len(d.classModel) > 0 {
		kinds = append(kinds, graph.KindField)
	}
	for row := range graph.FrameworkDeclIdentitiesSeq(g, kinds...) {
		switch row.Kind {
		case graph.KindMethod:
			d.absorbMethod(row)
		case graph.KindField:
			d.absorbField(row)
		case graph.KindModule:
			d.absorbModule(row)
		}
	}
	return d
}

// absorbType feeds the model index, the implicit model IDs, and both
// class-side halves of the method joins.
func (d *odooDecls) absorbType(n graph.FrameworkDeclNode) {
	// The JS class name is read off the node itself, not its metadata, so
	// it is collected before the Odoo-metadata gate below.
	if n.Name != "" && (n.Language == "javascript" || n.Language == "typescript") {
		if prev, ok := d.classNames[n.ID]; !ok || n.Name < prev {
			d.classNames[n.ID] = n.Name
		}
	}
	if n.Meta == nil {
		return
	}
	model, _ := n.Meta["odoo_model"].(string)
	if model == "" {
		return
	}
	d.models[model] = append(d.models[model], n.ID)
	d.classModel[n.ID] = model
	key := odooImplicitXMLIDKey(model)
	d.implicit.models[key] = append(d.implicit.models[key], n.ID)
}

// absorbResource feeds the external-ID and QWeb template indexes. One
// resource node can carry both: a <template> is a record with an external
// ID and a template name.
func (d *odooDecls) absorbResource(n graph.FrameworkDeclNode) {
	if n.Meta == nil {
		return
	}
	if xmlID, _ := n.Meta["odoo_xml_id"].(string); xmlID != "" {
		d.xmlIDs.put(xmlID, n.ID)
		d.xmlIDs.put(odooBareXMLID(xmlID), n.ID)
	}
	if tmpl, _ := n.Meta["odoo_template"].(string); tmpl != "" {
		d.templates.put(tmpl, n.ID)
		d.templates.put(odooBareXMLID(tmpl), n.ID)
	}
}

// absorbFile feeds the JS module index under every specifier shape an
// Odoo import can take: the legacy dotted name, the addon-relative key,
// and the escape form for specifiers that climb out of static/src.
func (d *odooDecls) absorbFile(n graph.FrameworkDeclNode) {
	if n.Meta == nil {
		return
	}
	legacy, _ := n.Meta["odoo_js_legacy_name"].(string)
	d.jsModules.put(legacy, n.ID)
	addon, _ := n.Meta["odoo_js_addon"].(string)
	if addon == "" {
		return
	}
	if rest, ok := odooJSModuleRelPath(n.FilePath, addon); ok {
		d.jsModules.put(addon+"/"+rest, n.ID)
	}
	if rest, ok := odooJSStaticRelPath(n.FilePath, addon); ok {
		d.jsModules.put(addon+"/static/"+rest, n.ID)
	}
}

// absorbMethod runs both method joins off one row. A Python method node ID
// is shaped "<classID>.<method>", and so is a JS one, so the owner is cut
// the same way for both; which map answers decides which index it feeds.
func (d *odooDecls) absorbMethod(row graph.FrameworkDeclIdentity) {
	if row.Name == "" {
		return
	}
	cut := strings.LastIndex(row.ID, ".")
	if cut <= 0 {
		return
	}
	owner := row.ID[:cut]
	if model := d.classModel[owner]; model != "" {
		d.modelMethods.put(model+"."+row.Name, row.ID)
	}
	if className := d.classNames[owner]; className != "" {
		d.jsMethods.put(className+"."+row.Name, row.ID)
	}
}

// absorbField feeds the implicit `field_<model>__<name>` external IDs.
func (d *odooDecls) absorbField(row graph.FrameworkDeclIdentity) {
	if row.Name == "" {
		return
	}
	cut := strings.LastIndex(row.ID, odooFieldIDSep)
	if cut <= 0 {
		return
	}
	model := d.classModel[row.ID[:cut]]
	if model == "" {
		return
	}
	key := odooImplicitXMLIDKey(model) + "__" + row.Name
	if prev, ok := d.implicit.fields[key]; !ok || row.ID < prev {
		d.implicit.fields[key] = row.ID
	}
}

// absorbModule feeds the implicit `module_<addon>` external IDs.
func (d *odooDecls) absorbModule(row graph.FrameworkDeclIdentity) {
	if row.Name == "" || !strings.Contains(row.ID, odooModuleIDMarker) {
		return
	}
	if prev, ok := d.implicit.modules[row.Name]; !ok || row.ID < prev {
		d.implicit.modules[row.Name] = row.ID
	}
}
