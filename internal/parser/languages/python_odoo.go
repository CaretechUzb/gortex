package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Odoo models.
//
// An Odoo model is identified by a STRING, not by its class name: two
// unrelated classes in two addons both carrying `_name = "sale.order"`
// are one model, and a class carrying only `_inherit = "sale.order"`
// extends that same model in place. Indexed as plain Python, none of
// that is visible — the classes look unrelated and the relational graph
// hiding in `fields.Many2one("res.partner")` call arguments is invisible
// too.
//
// This pass tags the class node with its Odoo identity and emits the
// links that identity implies, leaving placeholder targets for the
// resolver to bind once every addon's models are in the graph.

// odooPlaceholderPrefix is the unresolved-target prefix for a reference
// to an Odoo model by its `_name` string.
const odooPlaceholderPrefix = "unresolved::odoo::model::"

// odooModelVia tags the placeholder edges this pass emits, so the
// resolver scans only its own candidates.
const odooModelVia = "odoo-model"

// odooModelBases maps an Odoo model base class, written with its module
// qualifier, to the model kind it declares. The qualifier is required:
// a bare `Model` is far too common a name to key on.
var odooModelBases = map[string]string{
	"models.Model":          "model",
	"models.TransientModel": "transient",
	"models.AbstractModel":  "abstract",
	// Pre-v10 OpenERP spelling, still present in long-lived addons.
	"osv.osv":        "model",
	"osv.osv_memory": "transient",
	"osv.Model":      "model",
}

// odooFieldTypes is Odoo's field constructor set. A `fields.X(...)`
// assignment in a class body is the second half of the two-factor model
// test below.
var odooFieldTypes = map[string]bool{
	"Char": true, "Text": true, "Html": true, "Boolean": true,
	"Integer": true, "Float": true, "Monetary": true, "Date": true,
	"Datetime": true, "Binary": true, "Image": true, "Selection": true,
	"Reference": true, "Json": true, "Properties": true,
	"Many2one": true, "One2many": true, "Many2many": true,
	"Many2oneReference": true,
}

// odooRelationalFields are the field types whose first argument (or
// comodel_name kwarg) names another model — the ER graph.
var odooRelationalFields = map[string]bool{
	"Many2one": true, "One2many": true, "Many2many": true,
}

// detectOdooModel tags an Odoo model class and emits its identity edges.
//
// Detection is class-local and two-factor: the class must inherit from a
// QUALIFIED Odoo base (models.Model and friends) AND declare at least one
// Odoo attribute or field. Requiring both guards against a shadowed
// `Model` symbol in a non-Odoo codebase, and keeps the test lexical — no
// file-level import sniff and no cross-file state, so it behaves
// identically on a full index and an incremental one.
func detectOdooModel(classNode *sitter.Node, src []byte, classID, className, filePath string, result *parser.ExtractionResult) {
	if classNode == nil || result == nil {
		return
	}
	body := classNode.ChildByFieldName("body")
	if body == nil {
		return
	}
	modelKind := odooModelKind(classNode, src)
	if modelKind == "" {
		return
	}
	attrs := odooClassAttrs(body, src)
	fields := odooClassFields(body, src)
	if len(attrs) == 0 && len(fields) == 0 {
		// Factor two absent: an Odoo base name without a single Odoo
		// attribute or field is far more likely a same-named class in
		// an unrelated codebase than a real model.
		return
	}

	name := odooModelName(attrs)
	if name == "" {
		return
	}
	classNodeRef := odooFindNode(result, classID)
	if classNodeRef == nil {
		return
	}
	startLine := int(classNode.StartPoint().Row) + 1

	if classNodeRef.Meta == nil {
		classNodeRef.Meta = map[string]any{}
	}
	classNodeRef.Meta["odoo_model"] = name
	classNodeRef.Meta["odoo_model_kind"] = modelKind
	if d := attrs["_description"]; d != "" {
		classNodeRef.Meta["odoo_description"] = d
	}

	inherits := odooInheritNames(body, src)
	if len(inherits) > 0 {
		classNodeRef.Meta["odoo_inherit"] = inherits
	}
	delegates := odooInheritsMap(body, src)
	if len(delegates) > 0 {
		classNodeRef.Meta["odoo_inherits"] = delegates
	}

	// An abstract model is a mixin: it has no table of its own.
	if modelKind != "abstract" {
		odooEmitTableEdge(result, classID, name, filePath, startLine)
	}

	// `_inherit` is specialisation, whether it extends the model in
	// place (same _name) or prototypes a new one.
	for _, parent := range inherits {
		if parent == name && attrs["_name"] == "" {
			// A pure in-place extension names itself; a self-edge
			// would say nothing. The resolver binds the sibling
			// classes together through the shared model name.
			continue
		}
		result.Edges = append(result.Edges, odooPlaceholderEdge(
			classID, parent, graph.EdgeExtends, filePath, startLine,
			map[string]any{"odoo_link": "inherit"}))
	}

	// `_inherits` is delegation — the classic embedding relationship.
	for parent, viaField := range delegates {
		result.Edges = append(result.Edges, odooPlaceholderEdge(
			classID, parent, graph.EdgeComposes, filePath, startLine,
			map[string]any{"odoo_link": "delegate", "odoo_delegate_field": viaField}))
	}

	odooEmitFields(result, classID, filePath, fields)
}

// odooEmitTableEdge binds the model to its physical table. Odoo derives
// the table name from `_name` by replacing dots with underscores, so this
// is a fact, not a guess — and it makes every Odoo model visible to the
// existing analyze kind=models / unreferenced_tables queries for free.
func odooEmitTableEdge(result *parser.ExtractionResult, classID, modelName, filePath string, line int) {
	tableName := strings.ReplaceAll(modelName, ".", "_")
	tableID := ormTableNodeID(tableName)
	if !ormTableNodeAlreadyEmitted(result, tableID) {
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:       tableID,
			Kind:     graph.KindTable,
			Name:     tableName,
			FilePath: filePath,
			Language: "python",
			Meta: map[string]any{
				"dialect": "orm",
				"schema":  "",
				"source":  "python-odoo",
			},
		})
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: classID, To: tableID, Kind: graph.EdgeModelsTable,
		FilePath: filePath, Line: line, Origin: graph.OriginASTResolved,
		Meta: map[string]any{
			"orm":        "odoo",
			"binding":    "subclass",
			"table_name": tableName,
			"model_name": modelName,
			"derivation": "convention",
		},
	})
}

// odooField is one `name = fields.Type(...)` declaration.
type odooField struct {
	name     string
	fieldTyp string
	comodel  string
	related  string
	compute  string
	required bool
	line     int
}

// odooEmitFields mints a node per declared field plus the relational
// edges between them. Python emits no class-body field nodes of its own
// (emitTopLevelVar only handles module scope), so these are minted here
// rather than tagged.
func odooEmitFields(result *parser.ExtractionResult, classID, filePath string, fields []odooField) {
	for _, f := range fields {
		fieldID := classID + "#field:" + f.name
		meta := map[string]any{
			"odoo_field_type": f.fieldTyp,
			"visibility":      VisibilityByUnderscore(f.name),
		}
		if f.comodel != "" {
			meta["odoo_comodel"] = f.comodel
		}
		if f.related != "" {
			meta["odoo_related"] = f.related
		}
		if f.compute != "" {
			meta["odoo_compute"] = f.compute
		}
		if f.required {
			meta["odoo_required"] = true
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: fieldID, Kind: graph.KindField, Name: f.name,
			FilePath: filePath, StartLine: f.line, EndLine: f.line,
			Language: "python", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fieldID, To: classID, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: f.line, Origin: graph.OriginASTResolved,
		})
		// The relational target is the ER graph: this is what turns a
		// pile of Odoo classes into a navigable data model.
		if f.comodel != "" {
			result.Edges = append(result.Edges, odooPlaceholderEdge(
				fieldID, f.comodel, graph.EdgeReferences, filePath, f.line,
				map[string]any{"odoo_link": "comodel", "odoo_field_type": f.fieldTyp}))
		}
		// A computed field names a method on the same class, which
		// resolves locally with no placeholder.
		if f.compute != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From: fieldID, To: classID + "." + f.compute, Kind: graph.EdgeCalls,
				FilePath: filePath, Line: f.line, Origin: graph.OriginASTResolved,
				Meta: map[string]any{"via": odooModelVia, "odoo_link": "compute"},
			})
		}
	}
}

func odooPlaceholderEdge(from, model string, kind graph.EdgeKind, filePath string, line int, extra map[string]any) *graph.Edge {
	meta := map[string]any{"via": odooModelVia, "odoo_model": model}
	for k, v := range extra {
		meta[k] = v
	}
	return &graph.Edge{
		From: from, To: odooPlaceholderPrefix + model, Kind: kind,
		FilePath: filePath, Line: line, Origin: graph.OriginASTInferred, Meta: meta,
	}
}

// odooModelKind reports the model kind declared by the class's qualified
// base list, or "" when no Odoo base is present.
func odooModelKind(classNode *sitter.Node, src []byte) string {
	supers := classNode.ChildByFieldName("superclasses")
	if supers == nil {
		return ""
	}
	for i, nc := 0, int(supers.NamedChildCount()); i < nc; i++ {
		arg := supers.NamedChild(i)
		if arg == nil {
			continue
		}
		// pyBaseClassName returns (bare, qualified, generic); Odoo
		// detection needs the qualified form, since a bare `Model` is
		// too common a name to key on.
		_, qualified, _ := pyBaseClassName(arg, src)
		if kind, ok := odooModelBases[strings.TrimSpace(qualified)]; ok {
			return kind
		}
	}
	return ""
}

// odooModelName is the model's identity: `_name` when declared, else the
// first `_inherit` (an in-place extension of an existing model).
func odooModelName(attrs map[string]string) string {
	if n := attrs["_name"]; n != "" {
		return n
	}
	return attrs["_inherit"]
}

// odooClassAttrs reads the simple string-valued Odoo class attributes.
func odooClassAttrs(body *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	odooEachAssignment(body, src, func(target string, right *sitter.Node) {
		switch target {
		case "_name", "_description", "_table", "_order", "_rec_name":
			if v := odooStringValue(right, src); v != "" {
				out[target] = v
			}
		case "_inherit":
			// _inherit is a string or a list of strings; record the
			// first so a class with only _inherit still has an identity.
			if names := odooStringOrList(right, src); len(names) > 0 {
				out["_inherit"] = names[0]
			}
		}
	})
	return out
}

// odooInheritNames returns every `_inherit` entry, string or list form.
func odooInheritNames(body *sitter.Node, src []byte) []string {
	var out []string
	odooEachAssignment(body, src, func(target string, right *sitter.Node) {
		if target == "_inherit" {
			out = append(out, odooStringOrList(right, src)...)
		}
	})
	return out
}

// odooInheritsMap returns the `_inherits` delegation map: parent model →
// the Many2one field that stores the delegate's id.
func odooInheritsMap(body *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	odooEachAssignment(body, src, func(target string, right *sitter.Node) {
		if target != "_inherits" || right == nil || right.Type() != "dictionary" {
			return
		}
		for i, nc := 0, int(right.NamedChildCount()); i < nc; i++ {
			pair := right.NamedChild(i)
			if pair == nil || pair.Type() != "pair" {
				continue
			}
			model := odooStringValue(pair.ChildByFieldName("key"), src)
			field := odooStringValue(pair.ChildByFieldName("value"), src)
			if model != "" {
				out[model] = field
			}
		}
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// odooClassFields reads every `name = fields.Type(...)` declaration.
func odooClassFields(body *sitter.Node, src []byte) []odooField {
	var out []odooField
	odooEachAssignment(body, src, func(target string, right *sitter.Node) {
		if right == nil || right.Type() != "call" || target == "" {
			return
		}
		fn := right.ChildByFieldName("function")
		if fn == nil || fn.Type() != "attribute" {
			return
		}
		text := strings.TrimSpace(fn.Content(src))
		dot := strings.LastIndex(text, ".")
		if dot < 0 {
			return
		}
		receiver, typ := text[:dot], text[dot+1:]
		// Keyed on the `fields.` receiver so an unrelated
		// `something.Char(...)` call cannot be mistaken for a field.
		if receiver != "fields" || !odooFieldTypes[typ] {
			return
		}
		f := odooField{
			name:     target,
			fieldTyp: typ,
			line:     int(right.StartPoint().Row) + 1,
		}
		odooReadFieldArgs(right, src, typ, &f)
		out = append(out, f)
	})
	return out
}

// odooReadFieldArgs pulls the comodel, related, compute and required
// facts out of a field constructor call.
func odooReadFieldArgs(call *sitter.Node, src []byte, typ string, f *odooField) {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	positional := 0
	for i, nc := 0, int(args.NamedChildCount()); i < nc; i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		if arg.Type() != "keyword_argument" {
			// Relational fields take the comodel as their first
			// positional argument: Many2one("res.partner", ...).
			if positional == 0 && odooRelationalFields[typ] {
				if v := odooStringValue(arg, src); v != "" {
					f.comodel = v
				}
			}
			positional++
			continue
		}
		key := ""
		if k := arg.ChildByFieldName("name"); k != nil {
			key = strings.TrimSpace(k.Content(src))
		}
		val := arg.ChildByFieldName("value")
		switch key {
		case "comodel_name":
			if v := odooStringValue(val, src); v != "" {
				f.comodel = v
			}
		case "related":
			f.related = odooStringValue(val, src)
		case "compute":
			f.compute = odooStringValue(val, src)
		case "required":
			f.required = val != nil && strings.TrimSpace(val.Content(src)) == "True"
		}
	}
}

// odooEachAssignment visits every direct `target = value` assignment in a
// class body. Direct children only: a nested class or a method body is a
// different scope and its assignments are not this model's attributes.
func odooEachAssignment(body *sitter.Node, src []byte, fn func(target string, right *sitter.Node)) {
	for i, nc := 0, int(body.NamedChildCount()); i < nc; i++ {
		stmt := body.NamedChild(i)
		if stmt == nil {
			continue
		}
		if stmt.Type() != "expression_statement" {
			continue
		}
		for j, jc := 0, int(stmt.NamedChildCount()); j < jc; j++ {
			assign := stmt.NamedChild(j)
			if assign == nil || assign.Type() != "assignment" {
				continue
			}
			left := assign.ChildByFieldName("left")
			if left == nil || left.Type() != "identifier" {
				continue
			}
			fn(strings.TrimSpace(left.Content(src)), assign.ChildByFieldName("right"))
		}
	}
}

// odooStringValue returns the text of a string literal node, or "".
func odooStringValue(n *sitter.Node, src []byte) string {
	if n == nil || n.Type() != "string" {
		return ""
	}
	return pyStringLiteralValue(n.Content(src))
}

// odooStringOrList accepts either a bare string or a list of strings —
// `_inherit` is written both ways.
func odooStringOrList(n *sitter.Node, src []byte) []string {
	if n == nil {
		return nil
	}
	if n.Type() == "string" {
		if v := pyStringLiteralValue(n.Content(src)); v != "" {
			return []string{v}
		}
		return nil
	}
	if n.Type() != "list" && n.Type() != "tuple" {
		return nil
	}
	var out []string
	for i, nc := 0, int(n.NamedChildCount()); i < nc; i++ {
		if v := odooStringValue(n.NamedChild(i), src); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// odooFindNode locates an already-emitted node by ID so this pass can tag
// the class rather than duplicate it.
func odooFindNode(result *parser.ExtractionResult, id string) *graph.Node {
	for _, n := range result.Nodes {
		if n != nil && n.ID == id {
			return n
		}
	}
	return nil
}
