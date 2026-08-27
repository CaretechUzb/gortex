package languages

import (
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// OdooXMLExtractor indexes Odoo data / view / QWeb XML — the declarative
// half of an Odoo addon.
//
// Odoo keeps its UI and configuration in XML records keyed by an
// "external ID" (`<record id="view_sale_order_form" model="ir.ui.view">`)
// and cross-references them by name (`ref="view_sale_order_form"`). The
// `model=` attribute names a Python model class by its `_name` string, and
// `inherit_id` builds a view-inheritance chain. Indexed as generic XML,
// none of those links exist: the file is a bag of tags and every
// connection between a view and the model it renders is lost.
//
// Odoo XML shares the plain `.xml` extension with MyBatis mappers, Spring
// bean definitions and every other XML document, so like those the
// extractor is gated on content: IsOdooXML recognises the document, and
// Extract returns just the file node for anything else. The registry
// routes a `.xml` file here only when the content sniff matches.
type OdooXMLExtractor struct{}

// NewOdooXMLExtractor constructs an OdooXMLExtractor.
func NewOdooXMLExtractor() *OdooXMLExtractor { return &OdooXMLExtractor{} }

func (e *OdooXMLExtractor) Language() string     { return "odoo_xml" }
func (e *OdooXMLExtractor) Extensions() []string { return []string{".xml"} }

// Odoo placeholder prefixes. Two `via` tags rather than one because two
// different indexes resolve them — one keyed by model `_name`, one by
// external ID — and keeping them apart lets each resolver scan only its
// own candidates. Neither is a framework name: both belong to the single
// `odoo` framework.
const (
	odooXMLIDPlaceholder  = "unresolved::odoo::xmlid::"
	odooMethodPlaceholder = "unresolved::odoo::method::"
	odooXMLVia            = "odoo-xml"
)

// odooRefEvalRE matches the `ref('module.name')` form used inside eval=
// attributes, where a plain ref= attribute will not do.
var odooRefEvalRE = regexp.MustCompile(`ref\(\s*["']([\w.]+)["']\s*\)`)

// odooScope is one open record/template/menu element and the nesting
// depth it was opened at, so it can be popped by its own end tag rather
// than by whichever element happens to close next.
type odooScope struct {
	depth int
	node  string
}

// IsOdooXML reports whether src is an Odoo XML document.
//
// Two shapes qualify. The common one is the `<odoo>` root (or the pre-v10
// `<openerp>`). The second is a standalone QWeb asset template file under
// static/src/xml, which has a bare `<templates>` root carrying `t-name`
// attributes and no `<odoo>` wrapper at all — a real and common shape that
// a root-element-only probe would miss entirely.
//
// Only the document head is scanned, so the probe stays cheap on large
// data files.
func IsOdooXML(src []byte) bool {
	head := src
	const headCap = 8 * 1024
	if len(head) > headCap {
		head = head[:headCap]
	}
	lower := bytes.ToLower(head)
	if bytes.Contains(lower, []byte("<odoo")) || bytes.Contains(lower, []byte("<openerp")) {
		return true
	}
	if bytes.Contains(lower, []byte("<templates")) && bytes.Contains(lower, []byte("t-name")) {
		return true
	}
	return false
}

// Extract parses an Odoo XML file. A document that is not Odoo XML yields
// only the file node, so a misrouted plain-XML file degrades gracefully.
// A malformed document yields whatever was decoded before the error —
// never a hard failure.
func (e *OdooXMLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	fileNode := &graph.Node{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     path.Base(filePath),
		FilePath: filePath,
		Language: "odoo_xml",
	}
	result.Nodes = append(result.Nodes, fileNode)

	if !IsOdooXML(src) {
		return result, nil
	}

	module := odooModuleFromPath(filePath)
	if module != "" {
		fileNode.Meta = map[string]any{"odoo_module": module}
	}

	lineStarts := lineStartOffsets(src)
	dec := xml.NewDecoder(bytes.NewReader(src))
	dec.Strict = false

	// scopes is the stack of record/template/menu nodes an attribute-level
	// reference attributes to; without it a `ref=` deep inside a record
	// would have nothing to hang off.
	//
	// It has to be a STACK rather than one current node. Odoo nests these
	// freely — a `<template>` wrapping a `<t t-name>`, a `<record>` next to
	// a sibling `<record>` — and a scalar that is only ever overwritten
	// never returns to the enclosing node when the inner one closes, so
	// every later reference in the file is attributed to whichever node was
	// opened last rather than to the one that actually contains it.
	var scopes []odooScope
	depth := 0
	current := func() string {
		if len(scopes) == 0 {
			return ""
		}
		return scopes[len(scopes)-1].node
	}
	seen := map[string]bool{}
	// pendingModelField tracks an open `<field name="model">`, whose value
	// is element TEXT rather than an attribute. This is the canonical way
	// an Odoo view names the model it renders, so handling only the `ref=`
	// attribute form would miss almost every real view.
	pendingModelField, pendingLine := "", 0

	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			break // EOF or malformed — keep what was decoded
		}
		switch t := tok.(type) {
		case xml.CharData:
			if pendingModelField == "" || current() == "" {
				continue
			}
			if model := strings.TrimSpace(string(t)); model != "" {
				result.Edges = append(result.Edges, odooModelRef(
					current(), model, graph.EdgeReferences, filePath, pendingLine,
					"field_"+pendingModelField))
			}
			pendingModelField = ""
			continue
		case xml.EndElement:
			if strings.EqualFold(t.Name.Local, "field") {
				pendingModelField = ""
			}
			// Pop the scope this element opened, if it opened one. A
			// self-closing tag yields both a start and an end token, so
			// the depth counter stays balanced either way.
			if len(scopes) > 0 && scopes[len(scopes)-1].depth == depth {
				scopes = scopes[:len(scopes)-1]
			}
			depth--
			continue
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		depth++
		local := strings.ToLower(se.Name.Local)
		line := lineForOffset(lineStarts, int(clampOffset(dec.InputOffset(), len(src))))
		if local == "field" {
			switch odooAttr(se, "name") {
			case "model", "res_model":
				pendingModelField, pendingLine = odooAttr(se, "name"), line
			default:
				pendingModelField = ""
			}
		}

		// A node-minting element opens a scope that stays current until its
		// own end tag. An element that mints nothing (an empty `id=`, a
		// `<t>` with no t-name) is deliberately still pushed where it owns
		// the scope, so the enclosing node is restored on close rather than
		// leaking into the rest of the document.
		push := func(node string) {
			scopes = append(scopes, odooScope{depth: depth, node: node})
		}
		switch local {
		case "record":
			push(e.emitRecord(result, fileNode, seen, filePath, module, se, line))
		case "template":
			push(e.emitTemplate(result, fileNode, seen, filePath, module, se, line, "template"))
		case "t":
			// A QWeb template inside <templates>, keyed by t-name.
			if name := odooAttr(se, "t-name"); name != "" {
				push(e.emitQWeb(result, fileNode, seen, filePath, module, name, line))
			}
		case "menuitem":
			push(e.emitMenu(result, fileNode, seen, filePath, module, se, line))
		case "field":
			e.emitFieldRefs(result, current(), filePath, module, se, line)
		case "function":
			e.emitFunctionCall(result, current(), filePath, se, line)
		}

		// ref= and eval="ref(...)" appear on many element kinds; handle
		// them uniformly rather than per element.
		e.emitGenericRefs(result, current(), filePath, module, se, line, local)
	}
	return result, nil
}

// emitRecord mints the node for a `<record id= model=>` and links it to
// the Python model it configures.
func (e *OdooXMLExtractor) emitRecord(result *parser.ExtractionResult, fileNode *graph.Node, seen map[string]bool, filePath, module string, se xml.StartElement, line int) string {
	id := odooAttr(se, "id")
	model := odooAttr(se, "model")
	if id == "" {
		return ""
	}
	xmlID := odooQualifyXMLID(module, id)
	nodeID := "odoo::record::" + xmlID
	if !seen[nodeID] {
		seen[nodeID] = true
		meta := map[string]any{
			"odoo_xml_id":   xmlID,
			"resource_kind": "odoo_record",
		}
		if model != "" {
			meta["odoo_model"] = model
		}
		if module != "" {
			meta["odoo_module"] = module
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: nodeID, Kind: graph.KindResource, Name: id, QualName: xmlID,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: "odoo_xml", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: nodeID, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: line,
		})
	}
	// The record configures a Python model, named by its `_name` string.
	if model != "" {
		result.Edges = append(result.Edges, odooModelRef(nodeID, model, graph.EdgeReferences, filePath, line, "record_model"))
	}
	return nodeID
}

// emitTemplate mints a node for `<template id=>`, an ir.ui.view in
// shorthand form.
func (e *OdooXMLExtractor) emitTemplate(result *parser.ExtractionResult, fileNode *graph.Node, seen map[string]bool, filePath, module string, se xml.StartElement, line int, kind string) string {
	id := odooAttr(se, "id")
	if id == "" {
		return ""
	}
	nodeID := e.mintTemplateNode(result, fileNode, seen, filePath, module, odooQualifyXMLID(module, id), id, line, kind)
	// `inherit_id` on a template is real view inheritance.
	if parent := odooAttr(se, "inherit_id"); parent != "" {
		result.Edges = append(result.Edges, odooXMLIDRef(nodeID, odooQualifyXMLID(module, parent), graph.EdgeExtends, filePath, line, "inherit_id"))
	}
	return nodeID
}

// emitQWeb mints a node for a `<t t-name=>` QWeb template. These carry an
// already-qualified name, so the module prefix is not re-applied.
func (e *OdooXMLExtractor) emitQWeb(result *parser.ExtractionResult, fileNode *graph.Node, seen map[string]bool, filePath, module, name string, line int) string {
	return e.mintTemplateNode(result, fileNode, seen, filePath, module, name, name, line, "qweb")
}

func (e *OdooXMLExtractor) mintTemplateNode(result *parser.ExtractionResult, fileNode *graph.Node, seen map[string]bool, filePath, module, xmlID, name string, line int, kind string) string {
	nodeID := "odoo::template::" + xmlID
	if seen[nodeID] {
		return nodeID
	}
	seen[nodeID] = true
	meta := map[string]any{
		"odoo_xml_id":    xmlID,
		"resource_kind":  "odoo_template",
		"odoo_template":  xmlID,
		"odoo_view_kind": kind,
	}
	if module != "" {
		meta["odoo_module"] = module
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: nodeID, Kind: graph.KindResource, Name: name, QualName: xmlID,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: "odoo_xml", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: nodeID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: line,
	})
	return nodeID
}

// emitMenu mints a node for `<menuitem>` and links it to the action it
// opens and the parent menu it hangs under.
func (e *OdooXMLExtractor) emitMenu(result *parser.ExtractionResult, fileNode *graph.Node, seen map[string]bool, filePath, module string, se xml.StartElement, line int) string {
	id := odooAttr(se, "id")
	if id == "" {
		return ""
	}
	xmlID := odooQualifyXMLID(module, id)
	nodeID := "odoo::menu::" + xmlID
	if !seen[nodeID] {
		seen[nodeID] = true
		meta := map[string]any{"odoo_xml_id": xmlID, "resource_kind": "odoo_menu"}
		if module != "" {
			meta["odoo_module"] = module
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: nodeID, Kind: graph.KindResource, Name: id, QualName: xmlID,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: "odoo_xml", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: nodeID, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: line,
		})
	}
	for _, attr := range []string{"action", "parent"} {
		if v := odooAttr(se, attr); v != "" {
			result.Edges = append(result.Edges, odooXMLIDRef(nodeID, odooQualifyXMLID(module, v), graph.EdgeReferences, filePath, line, "menu_"+attr))
		}
	}
	return nodeID
}

// emitFieldRefs handles the `<field>` element, whose meaning depends on
// its name attribute: `model`/`res_model` name a Python model, and
// `inherit_id` is view inheritance.
func (e *OdooXMLExtractor) emitFieldRefs(result *parser.ExtractionResult, current, filePath, module string, se xml.StartElement, line int) {
	if current == "" {
		return
	}
	name := odooAttr(se, "name")
	ref := odooAttr(se, "ref")
	switch name {
	case "model", "res_model":
		// The value is usually the element's text body rather than an
		// attribute; the generic ref handler covers the ref= form, and
		// the text form is picked up by the caller's char-data pass.
		if ref != "" {
			result.Edges = append(result.Edges, odooModelRef(current, ref, graph.EdgeReferences, filePath, line, "field_"+name))
		}
	case "inherit_id":
		if ref != "" {
			result.Edges = append(result.Edges, odooXMLIDRef(current, odooQualifyXMLID(module, ref), graph.EdgeExtends, filePath, line, "inherit_id"))
		}
	}
}

// emitFunctionCall handles `<function model="x" name="y">`, which invokes
// a Python method at data-load time.
func (e *OdooXMLExtractor) emitFunctionCall(result *parser.ExtractionResult, current, filePath string, se xml.StartElement, line int) {
	model, name := odooAttr(se, "model"), odooAttr(se, "name")
	if current == "" || model == "" || name == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: current, To: odooMethodPlaceholder + model + "." + name,
		Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		Origin: graph.OriginASTInferred,
		Meta: map[string]any{
			"via": odooXMLVia, "odoo_link": "function",
			"odoo_model": model, "odoo_method": name,
		},
	})
}

// emitGenericRefs handles `ref=` and `eval="ref('…')"` wherever they
// appear. `inherit_id` is skipped because emitFieldRefs already models it
// as inheritance rather than a plain reference.
func (e *OdooXMLExtractor) emitGenericRefs(result *parser.ExtractionResult, current, filePath, module string, se xml.StartElement, line int, local string) {
	if current == "" {
		return
	}
	if local == "field" && odooAttr(se, "name") == "inherit_id" {
		return
	}
	if local == "menuitem" || local == "record" || local == "template" {
		// Their own emitters already modelled the meaningful refs.
		return
	}
	if ref := odooAttr(se, "ref"); ref != "" {
		result.Edges = append(result.Edges, odooXMLIDRef(current, odooQualifyXMLID(module, ref), graph.EdgeReferences, filePath, line, "ref"))
	}
	if eval := odooAttr(se, "eval"); eval != "" {
		for _, m := range odooRefEvalRE.FindAllStringSubmatch(eval, -1) {
			result.Edges = append(result.Edges, odooXMLIDRef(current, odooQualifyXMLID(module, m[1]), graph.EdgeReferences, filePath, line, "eval_ref"))
		}
	}
}

func odooModelRef(from, model string, kind graph.EdgeKind, filePath string, line int, link string) *graph.Edge {
	return &graph.Edge{
		From: from, To: odooPlaceholderPrefix + model, Kind: kind,
		FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
		Meta: map[string]any{"via": odooModelVia, "odoo_model": model, "odoo_link": link},
	}
}

func odooXMLIDRef(from, xmlID string, kind graph.EdgeKind, filePath string, line int, link string) *graph.Edge {
	return &graph.Edge{
		From: from, To: odooXMLIDPlaceholder + xmlID, Kind: kind,
		FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
		Meta: map[string]any{"via": odooXMLVia, "odoo_xml_id": xmlID, "odoo_link": link},
	}
}

// odooAttr returns an attribute value by local name (namespace-agnostic).
func odooAttr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if strings.EqualFold(a.Name.Local, local) {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

// odooQualifyXMLID applies the implicit module prefix. Inside module
// `sale`, `ref="view_x"` means `sale.view_x`; an already-dotted reference
// is left alone. Without this the same view would carry two different
// identities depending on which file referenced it.
func odooQualifyXMLID(module, id string) string {
	id = strings.TrimSpace(id)
	if id == "" || module == "" || strings.Contains(id, ".") {
		return id
	}
	return module + "." + id
}

// odooModuleFromPath derives the addon name from the file's path. An Odoo
// module IS its directory name, so the path is authoritative and needs no
// cross-file state — which keeps incremental re-indexing exact.
//
// The manifest-relative form cannot be used here: the extractor sees one
// file at a time and has no filesystem access, so the addon is recovered
// from the conventional `.../addons/<module>/...` layout, falling back to
// the directory above the well-known data subdirectories.
func odooModuleFromPath(filePath string) string {
	parts := strings.Split(path.Clean(filePath), "/")
	for i, p := range parts {
		if (p == "addons" || p == "addons_extra" || p == "custom_addons") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	// Fall back to the directory above views/data/security/static, the
	// conventional places Odoo XML lives inside an addon.
	//
	// The scan must reach parts[0]: a repository tracked AT the addons
	// root — the layout docs/multi-repo.md documents, where paths read
	// `sale/views/sale_views.xml` — puts the module name in the first
	// segment. Stopping at parts[1] returned "" for every such file, so
	// records were minted with bare unqualified external IDs while the
	// manifest still named the module, and odooXMLIDIndex's bare-form
	// fallback then collapsed same-named views across addons onto one
	// node.
	for i := len(parts) - 2; i >= 0; i-- {
		switch parts[i] {
		case "views", "data", "security", "report", "wizard", "demo", "static", "src", "xml":
			continue
		default:
			return parts[i]
		}
	}
	return ""
}
