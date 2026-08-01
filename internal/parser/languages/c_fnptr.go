package languages

import (
	"strconv"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// C/C++ function-pointer dispatch binding. A command-table / ops-struct /
// vtable pattern registers concrete functions into a struct's
// function-pointer fields (`static struct cmd cmds[] = {{"add", cmd_add}}`)
// and dispatches them indirectly (`cmds[i].fn(argc, argv)`). The static
// call graph cannot connect the dispatch to the concrete function because
// the field holds a runtime pointer. This pass records the (struct, field)
// registrations and the dispatch sites; the resolver pairs them by slot and
// fans out a call edge to every registered function.

// fnPtrDispatchViaTag marks a dispatch placeholder; fnPtrRegViaTag marks a
// registration carrier. Both must match the resolver's constants.
const (
	fnPtrDispatchViaTag = "fn-pointer-dispatch"
	fnPtrRegViaTag      = "fn-pointer-reg"
)

// captureCFnPointerDispatch runs the extractor passes for the C/C++
// function-pointer dispatch synthesizer. Shared by the C and C++ extractors.
func captureCFnPointerDispatch(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	// Pass 1: fn-pointer typedefs.
	var typedefs map[string]bool
	root.WithScratch(func() { typedefs = cFnPtrTypedefs(root, src) })
	// Pass 2: struct fn-pointer fields (ordered fields + fn-ptr set).
	var fields map[string]*structFields
	root.WithScratch(func() { fields = cStructFnPtrFields(root, src, typedefs) })
	if len(fields) == 0 {
		return
	}
	// Variable → struct type (file scope), for dispatch receiver inference.
	var varTypes map[string]string
	root.WithScratch(func() { varTypes = cVarStructTypes(root, src) })

	// Pass 3: registrations (initializers + assignments).
	root.WithScratch(func() {
		cEmitRegistrations(result, root, filePath, src, fields, varTypes)
	})
	// Pass 4: dispatch sites.
	root.WithScratch(func() {
		cEmitDispatch(result, root, filePath, src, fields, varTypes)
	})
}

// structFields holds a struct's ordered field names and its fn-pointer set.
type structFields struct {
	order []string
	fnptr map[string]bool
}

// cFnPtrTypedefs collects the names of `typedef RET (*NAME)(...)` fn-pointer
// typedefs.
func cFnPtrTypedefs(root *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	cFnPtrWalk(root, func(n *sitter.Node) {
		if n.Type() != "type_definition" {
			return
		}
		decl := n.ChildByFieldName("declarator")
		if decl == nil || decl.Type() != "function_declarator" {
			return
		}
		inner := decl.ChildByFieldName("declarator")
		if inner == nil || inner.Type() != "parenthesized_declarator" {
			return
		}
		if name := cTypeIdentifierIn(inner, src); name != "" {
			out[name] = true
		}
	})
	return out
}

// cStructFnPtrFields maps a struct type name to its ordered fields and the
// subset that are function pointers.
func cStructFnPtrFields(root *sitter.Node, src []byte, typedefs map[string]bool) map[string]*structFields {
	out := map[string]*structFields{}
	cFnPtrWalk(root, func(n *sitter.Node) {
		if n.Type() != "struct_specifier" && n.Type() != "union_specifier" {
			return
		}
		nameNode := n.ChildByFieldName("name")
		body := n.ChildByFieldName("body")
		if nameNode == nil || body == nil {
			return
		}
		structName := nameNode.Content(src)
		sf := &structFields{fnptr: map[string]bool{}}
		for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
			fd := body.NamedChild(i)
			if fd == nil || fd.Type() != "field_declaration" {
				continue
			}
			name := cFieldName(fd, src)
			if name == "" {
				continue
			}
			sf.order = append(sf.order, name)
			if cFieldIsFnPtr(fd, src, typedefs) {
				sf.fnptr[name] = true
			}
		}
		if len(sf.fnptr) > 0 {
			out[structName] = sf
		}
	})
	return out
}

// cVarStructTypes maps a variable name to its struct type from declarations
// and parameters (`struct cmd cmds[]`, `struct cmd *c`).
func cVarStructTypes(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	cFnPtrWalk(root, func(n *sitter.Node) {
		switch n.Type() {
		case "declaration", "parameter_declaration", "field_declaration":
		default:
			return
		}
		st := cStructTypeOf(n, src)
		if st == "" {
			return
		}
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			c := n.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "init_declarator", "array_declarator", "pointer_declarator", "identifier":
				if name := cDeclName(c, src); name != "" {
					out[name] = st
				}
			}
		}
	})
	return out
}

// cEmitRegistrations records concrete functions bound to (struct, field)
// slots, from struct initializers and `x.field = fn` assignments.
func cEmitRegistrations(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte, fields map[string]*structFields, varTypes map[string]string) {
	cFnPtrWalk(root, func(n *sitter.Node) {
		switch n.Type() {
		case "declaration":
			st := cStructTypeOf(n, src)
			sf := fields[st]
			if sf == nil {
				return
			}
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				id := n.NamedChild(i)
				if id == nil || id.Type() != "init_declarator" {
					continue
				}
				val := id.ChildByFieldName("value")
				if val == nil || val.Type() != "initializer_list" {
					continue
				}
				declTy := id.ChildByFieldName("declarator")
				if declTy != nil && declTy.Type() == "array_declarator" {
					// Array of structs: each element is a struct initializer.
					for j, _nc := 0, int(val.NamedChildCount()); j < _nc; j++ {
						if el := val.NamedChild(j); el != nil && el.Type() == "initializer_list" {
							cEmitStructInit(result, filePath, src, st, sf, el)
						}
					}
				} else {
					cEmitStructInit(result, filePath, src, st, sf, val)
				}
			}
		case "assignment_expression":
			cEmitFieldAssignment(result, filePath, src, fields, varTypes, n)
		}
	})
}

// cEmitStructInit emits registrations for one struct initializer, mapping
// positional values by field order and designated values by field name.
func cEmitStructInit(result *parser.ExtractionResult, filePath string, src []byte, st string, sf *structFields, init *sitter.Node) {
	pos := 0
	for i, _nc := 0, int(init.NamedChildCount()); i < _nc; i++ {
		el := init.NamedChild(i)
		if el == nil {
			continue
		}
		if el.Type() == "initializer_pair" {
			field := cDesignatorField(el, src)
			val := el.ChildByFieldName("value")
			if field != "" && sf.fnptr[field] {
				cEmitReg(result, filePath, src, st, field, val, init)
			}
			continue
		}
		// Positional element → the field at this position.
		if pos < len(sf.order) {
			field := sf.order[pos]
			if sf.fnptr[field] {
				cEmitReg(result, filePath, src, st, field, el, init)
			}
		}
		pos++
	}
}

// cEmitFieldAssignment handles `recv.field = fn` / `recv->field = fn` and
// the field-copy `a.field = b.field`.
func cEmitFieldAssignment(result *parser.ExtractionResult, filePath string, src []byte, fields map[string]*structFields, varTypes map[string]string, assign *sitter.Node) {
	left := assign.ChildByFieldName("left")
	right := assign.ChildByFieldName("right")
	if left == nil || right == nil || left.Type() != "field_expression" {
		return
	}
	st, field := cFieldExprSlot(left, src, varTypes, fields)
	if st == "" || field == "" {
		return
	}
	if right.Type() == "field_expression" {
		// Field copy: a.field ← b.field.
		fromSt, fromField := cFieldExprSlot(right, src, varTypes, fields)
		if fromSt == "" || fromField == "" {
			return
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: filePath, To: "unresolved::*." + fromField, Kind: graph.EdgeReferences,
			FilePath: filePath, Line: int(assign.StartPoint().Row) + 1,
			Meta: map[string]any{
				"via": fnPtrRegViaTag, "fnptr_struct": st, "fnptr_field": field,
				"fnptr_copy_struct": fromSt, "fnptr_copy_field": fromField,
			},
		})
		return
	}
	cEmitReg(result, filePath, src, st, field, right, assign)
}

// cEmitReg emits a registration carrier edge for a concrete function value
// bound to a (struct, field) slot.
func cEmitReg(result *parser.ExtractionResult, filePath string, src []byte, st, field string, val, site *sitter.Node) {
	fn := cFnValueName(val, src)
	if fn == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: filePath, To: "unresolved::*." + fn, Kind: graph.EdgeReferences,
		FilePath: filePath, Line: int(site.StartPoint().Row) + 1,
		Meta: map[string]any{"via": fnPtrRegViaTag, "fnptr_struct": st, "fnptr_field": field, "fnptr_fn": fn},
	})
}

// cEmitDispatch stamps a placeholder per indirect dispatch through a known
// fn-pointer field.
func cEmitDispatch(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte, fields map[string]*structFields, varTypes map[string]string) {
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	cFnPtrWalk(root, func(call *sitter.Node) {
		if call.Type() != "call_expression" {
			return
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "field_expression" {
			return
		}
		st, field := cFieldExprSlot(fn, src, varTypes, fields)
		if st == "" || field == "" {
			return
		}
		line := int(call.StartPoint().Row) + 1
		from := findEnclosingFunc(funcRanges, line)
		if from == "" {
			return
		}
		k := from + "\x00" + st + "\x00" + field + "\x00" + strconv.Itoa(line)
		if seen[k] {
			return
		}
		seen[k] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: from, To: "unresolved::*." + field, Kind: graph.EdgeCalls,
			FilePath: filePath, Line: line,
			Meta: map[string]any{"via": fnPtrDispatchViaTag, "fnptr_struct": st, "fnptr_field": field},
		})
	})
}

// cFieldExprSlot resolves a field_expression (`recv.field` / `recv->field` /
// `table[i].field`) to a (struct type, field) slot when field is a known
// fn-pointer field of recv's struct type.
func cFieldExprSlot(fe *sitter.Node, src []byte, varTypes map[string]string, fields map[string]*structFields) (string, string) {
	fieldNode := fe.ChildByFieldName("field")
	if fieldNode == nil {
		return "", ""
	}
	field := fieldNode.Content(src)
	base := cBaseVar(fe.ChildByFieldName("argument"), src)
	if base == "" {
		return "", ""
	}
	st := varTypes[base]
	if st == "" {
		return "", ""
	}
	sf := fields[st]
	if sf == nil || !sf.fnptr[field] {
		return "", ""
	}
	return st, field
}

// cBaseVar returns the base variable name of a receiver expression:
// `cmds` for `cmds[i]`, `c` for `c`, unwrapping `(*c)` and subscripts.
func cBaseVar(recv *sitter.Node, src []byte) string {
	for recv != nil {
		switch recv.Type() {
		case "identifier":
			return recv.Content(src)
		case "subscript_expression":
			arg := recv.ChildByFieldName("argument")
			if arg == nil && recv.NamedChildCount() > 0 {
				arg = recv.NamedChild(0)
			}
			recv = arg
		case "parenthesized_expression", "pointer_expression":
			if recv.NamedChildCount() == 0 {
				return ""
			}
			recv = recv.NamedChild(0)
		case "field_expression":
			recv = recv.ChildByFieldName("argument")
		default:
			return ""
		}
	}
	return ""
}

// cFnValueName returns the function name of a value identifier (`cmd_add`),
// unwrapping a leading `&`.
func cFnValueName(val *sitter.Node, src []byte) string {
	if val == nil {
		return ""
	}
	if val.Type() == "pointer_expression" && val.NamedChildCount() > 0 {
		val = val.NamedChild(0)
	}
	if val.Type() == "identifier" {
		return val.Content(src)
	}
	return ""
}

// cDesignatorField returns the field name of an `initializer_pair`'s
// `.field =` designator.
func cDesignatorField(pair *sitter.Node, src []byte) string {
	for i, _nc := 0, int(pair.NamedChildCount()); i < _nc; i++ {
		d := pair.NamedChild(i)
		if d != nil && d.Type() == "field_designator" && d.NamedChildCount() > 0 {
			return d.NamedChild(0).Content(src)
		}
	}
	return ""
}

// cStructTypeOf returns the `struct X` type name declared by a declaration /
// parameter / field node, or "".
func cStructTypeOf(n *sitter.Node, src []byte) string {
	t := n.ChildByFieldName("type")
	if t == nil {
		for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
			if c := n.NamedChild(i); c != nil && (c.Type() == "struct_specifier" || c.Type() == "union_specifier") {
				t = c
				break
			}
		}
	}
	if t == nil {
		return ""
	}
	if t.Type() == "struct_specifier" || t.Type() == "union_specifier" {
		if nm := t.ChildByFieldName("name"); nm != nil {
			return nm.Content(src)
		}
	}
	return ""
}

// cFieldName returns the declared field name of a field_declaration.
func cFieldName(fd *sitter.Node, src []byte) string {
	d := fd.ChildByFieldName("declarator")
	if d == nil {
		return ""
	}
	return cDeclName(d, src)
}

// cFieldIsFnPtr reports whether a field_declaration declares a function
// pointer — inline `RET (*f)(...)` or a fn-pointer typedef'd type.
func cFieldIsFnPtr(fd *sitter.Node, src []byte, typedefs map[string]bool) bool {
	if d := fd.ChildByFieldName("declarator"); d != nil && d.Type() == "function_declarator" {
		return true
	}
	if t := fd.ChildByFieldName("type"); t != nil && t.Type() == "type_identifier" && typedefs[t.Content(src)] {
		return true
	}
	return false
}

// cDeclName walks a declarator chain to its innermost identifier name.
func cDeclName(decl *sitter.Node, src []byte) string {
	for decl != nil {
		switch decl.Type() {
		case "identifier", "field_identifier":
			return decl.Content(src)
		case "function_declarator", "array_declarator", "pointer_declarator", "init_declarator":
			decl = decl.ChildByFieldName("declarator")
		case "parenthesized_declarator":
			if decl.NamedChildCount() == 0 {
				return ""
			}
			decl = decl.NamedChild(0)
		default:
			return ""
		}
	}
	return ""
}

// cTypeIdentifierIn returns the first type_identifier descendant of n.
func cTypeIdentifierIn(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	if n.Type() == "type_identifier" {
		return n.Content(src)
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		if x := cTypeIdentifierIn(n.NamedChild(i), src); x != "" {
			return x
		}
	}
	return ""
}

// cFnPtrWalk visits n and all its named descendants.
func cFnPtrWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		cFnPtrWalk(n.NamedChild(i), fn)
	}
}
