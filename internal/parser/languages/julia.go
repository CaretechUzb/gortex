package languages

import (
	"strings"

	juliaforest "github.com/alexaandru/go-sitter-forest/julia"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// JuliaExtractor extracts Julia source through the tree-sitter-julia
// grammar (vendored via alexaandru/go-sitter-forest), replacing the
// original line-regex extractor.
//
// Covered definition forms:
//   - `function f(...) ... end`, including `where`-parametrised
//     signatures, declared return types (`function f(x)::Int`), the
//     empty generic declaration `function f end`, and qualified /
//     operator callees (`function Base.show`, `function Base.:+`)
//   - short-form definitions `f(x) = body`, `f(x)::T = body` and
//     `f(x)::T where T = body`, including nested closures inside `begin`
//     blocks
//   - `macro m(...) ... end`
//   - `struct` / `mutable struct` / `abstract type` / `primitive type`
//     with parametric names (`struct Pair{T,S}`) and supertypes
//     (`<: Living` → EdgeExtends), plus struct fields (KindField)
//   - `module` / `baremodule` — KindType node whose Meta carries the
//     module's `export` list; definitions inside get EdgeMemberOf
//   - `const X = ...` constants (KindVariable)
//
// Imports: `using M`, `using M: a, b`, `import M`, `import M as Alias`,
// dotted and relative import paths (`A.B`, `.Local`, `..Up`), and
// `include("file.jl")` — all as EdgeImports to
// `unresolved::import::<module>`, never to a selected name. A selective
// list also emits one edge per binding to
// `unresolved::import::<module>::<name>`, and a rename rides on
// Edge.Alias (and on the module edge's Meta, which is the persisted
// half). A module alias additionally rewrites qualified callees, so
// `import Foo as F` + `F.process(x)` calls `Foo.process`.
//
// Calls: call_expression / broadcast_call_expression / macrocall
// callees (identifier or qualified field_expression) attribute to the
// enclosing function-like definition as EdgeCalls to
// `unresolved::[Mod.]name`. Unicode identifiers (θ, σ̂), bang names
// (`foo!`), and broadcast (`f.(x)`) are native grammar forms.
//
// Docstrings — a string literal directly preceding a definition, or the
// first statement of a function/module body — attach as Meta["doc"].
type JuliaExtractor struct {
	lang *sitter.Language
}

func NewJuliaExtractor() *JuliaExtractor {
	return &JuliaExtractor{lang: sitter.NewLanguage(juliaforest.GetLanguage())}
}

func (e *JuliaExtractor) Language() string     { return "julia" }
func (e *JuliaExtractor) Extensions() []string { return []string{".jl"} }

// juliaScope carries the enclosing context down the walk: the innermost
// module (for EdgeMemberOf and export attachment) and the innermost
// function-like definition (for call attribution).
type juliaScope struct {
	moduleID     string
	functionID   string
	functionName string
	functionRecv string
}

type juliaWalkState struct {
	filePath string
	fileNode *graph.Node
	result   *parser.ExtractionResult
	seen     map[string]bool
	nodes    map[string]*graph.Node
	// importAliases maps a local module alias to the module it renames
	// (`import Foo as F` → F→Foo), so a qualified call can name the
	// module the resolver can find rather than a file-local nickname.
	importAliases map[string]string
}

func (e *JuliaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "julia",
	}
	result.Nodes = append(result.Nodes, fileNode)

	st := &juliaWalkState{
		filePath:      filePath,
		fileNode:      fileNode,
		result:        result,
		seen:          make(map[string]bool),
		nodes:         map[string]*graph.Node{filePath: fileNode},
		importAliases: map[string]string{},
	}
	// Aliases are collected up front: the emitting walk is a single
	// forward pass, and Julia does not require `import ... as ...` to
	// precede the code that uses the alias.
	juliaCollectImportAliases(root, src, st.importAliases)
	e.walk(root, src, juliaScope{}, st)
	return result, nil
}

// walk iterates a node's named children, dispatching definition / import
// / call handlers and recursing with updated scope. pendingDoc is the
// last sibling string literal (Julia docstring convention).
func (e *JuliaExtractor) walk(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	pendingDoc := ""
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "string_literal":
			pendingDoc = juliaDocText(c, src)

		case "module_definition":
			e.handleModule(c, src, scope, st, pendingDoc)
			pendingDoc = ""

		case "struct_definition", "abstract_definition", "primitive_definition":
			e.handleType(c, src, scope, st, pendingDoc)
			pendingDoc = ""

		case "function_definition", "macro_definition":
			e.handleFunction(c, src, scope, st, pendingDoc)
			pendingDoc = ""

		case "const_statement":
			pendingDoc = ""
			for a := range c.NamedChildren() {
				if a.Type() == "assignment" {
					e.handleAssignment(a, src, scope, st, true)
				}
			}

		case "assignment":
			e.handleAssignment(c, src, scope, st, false)
			pendingDoc = ""

		case "using_statement", "import_statement":
			e.handleImport(c, src, st)
			pendingDoc = ""

		case "export_statement":
			e.handleExport(c, src, scope, st)
			pendingDoc = ""

		case "call_expression", "broadcast_call_expression":
			e.handleCall(c, src, scope, st)
			e.walk(c, src, scope, st)
			pendingDoc = ""

		case "macrocall_expression":
			e.handleMacroCall(c, src, scope, st)
			e.walk(c, src, scope, st)
			pendingDoc = ""

		default:
			pendingDoc = ""
			e.walk(c, src, scope, st)
		}
	}
}

// handleModule emits the module as a KindType node (legacy mapping;
// graph.KindModule is reserved for ecosystem packages) and walks its
// body with the module scope pushed.
func (e *JuliaExtractor) handleModule(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, doc string) {
	name := ""
	if nn := n.ChildByFieldName("name"); nn != nil {
		name = nn.Content(src)
	}
	inner := scope
	if name != "" {
		id := st.filePath + "::" + name
		if !st.seen[id] {
			st.seen[id] = true
			node := &graph.Node{
				ID: id, Kind: graph.KindType, Name: name,
				FilePath:  st.filePath,
				StartLine: int(n.StartPoint().Row) + 1,
				EndLine:   int(n.EndPoint().Row) + 1,
				Language:  "julia",
			}
			if doc != "" {
				node.Meta = map[string]any{"doc": doc}
			}
			st.result.Nodes = append(st.result.Nodes, node)
			st.nodes[id] = node
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
				FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
			})
		}
		inner.moduleID = id
	}
	e.walk(n, src, inner, st)
}

// juliaTypeHeadInfo decodes a `type_head` child: the declared name
// (inside identifier / parametrized_type_expression / the lhs of the
// `<:` binary_expression), its type parameters, and the supertype text.
func juliaTypeHeadInfo(head *sitter.Node, src []byte) (name, super string, params []string) {
	if head == nil || head.Type() != "type_head" {
		return "", "", nil
	}
	c := head.NamedChild(0)
	if c == nil {
		return "", "", nil
	}
	if c.Type() == "binary_expression" {
		// Named children are [lhs, operator, rhs] — the operator is a
		// named node in this grammar, so address by position, not index 1.
		lhs := c.NamedChild(0)
		rhs := c.NamedChild(int(c.NamedChildCount()) - 1)
		if lhs != nil {
			name, params = juliaNameAndParams(lhs, src)
		}
		if rhs != nil && rhs.Type() != "operator" {
			super = rhs.Content(src)
		}
		return name, super, params
	}
	name, params = juliaNameAndParams(c, src)
	return name, "", params
}

// juliaNameAndParams extracts name + type parameters from an identifier
// or a parametrized_type_expression (`Pair{T,S}`).
func juliaNameAndParams(n *sitter.Node, src []byte) (string, []string) {
	if n == nil {
		return "", nil
	}
	switch n.Type() {
	case "identifier":
		return n.Content(src), nil
	case "parametrized_type_expression":
		var params []string
		name := ""
		for c := range n.NamedChildren() {
			switch c.Type() {
			case "identifier":
				if name == "" {
					name = c.Content(src)
				}
			case "curly_expression":
				for g := range c.NamedChildren() {
					if g.Type() == "identifier" {
						params = append(params, g.Content(src))
					}
				}
			}
		}
		return name, params
	}
	return "", nil
}

// handleType covers struct / mutable struct / abstract type /
// primitive type: KindType node, EdgeExtends for the supertype (bare
// name target + full path in Meta, matching the python extractor
// convention), KindField nodes for struct members, and EdgeMemberOf for
// definitions nested inside.
func (e *JuliaExtractor) handleType(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, doc string) {
	var head *sitter.Node
	for c := range n.NamedChildren() {
		if c.Type() == "type_head" {
			head = c
			break
		}
	}
	name, super, _ := juliaTypeHeadInfo(head, src)
	if name == "" {
		e.walk(n, src, scope, st)
		return
	}

	id := st.filePath + "::" + name
	if !st.seen[id] {
		st.seen[id] = true
		node := &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath:  st.filePath,
			StartLine: int(n.StartPoint().Row) + 1,
			EndLine:   int(n.EndPoint().Row) + 1,
			Language:  "julia",
		}
		if doc != "" {
			node.Meta = map[string]any{"doc": doc}
		}
		st.result.Nodes = append(st.result.Nodes, node)
		st.nodes[id] = node
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
		})
		if super != "" {
			bare := super
			if idx := strings.IndexAny(bare, "{"); idx > 0 {
				bare = bare[:idx]
			}
			edge := &graph.Edge{
				From: id, To: "unresolved::" + bare, Kind: graph.EdgeExtends,
				FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
			}
			if super != bare {
				edge.Meta = map[string]any{"base_path": super}
			}
			st.result.Edges = append(st.result.Edges, edge)
		}
		if scope.moduleID != "" {
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: id, To: scope.moduleID, Kind: graph.EdgeMemberOf,
				FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
			})
		}
	}

	// Struct fields: typed_expression (`x::T`) or bare identifier
	// members at the top level of the struct body.
	if n.Type() == "struct_definition" {
		for c := range n.NamedChildren() {
			if c.Type() == "type_head" {
				continue
			}
			var fieldName string
			switch c.Type() {
			case "typed_expression":
				if lhs := c.NamedChild(0); lhs != nil && lhs.Type() == "identifier" {
					fieldName = lhs.Content(src)
				}
			case "identifier":
				fieldName = c.Content(src)
			}
			if fieldName == "" {
				continue
			}
			fid := id + "." + fieldName
			if st.seen[fid] {
				continue
			}
			st.seen[fid] = true
			st.result.Nodes = append(st.result.Nodes, &graph.Node{
				ID: fid, Kind: graph.KindField, Name: fieldName,
				FilePath:  st.filePath,
				StartLine: int(c.StartPoint().Row) + 1,
				EndLine:   int(c.EndPoint().Row) + 1,
				Language:  "julia",
			})
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: fid, To: id, Kind: graph.EdgeMemberOf,
				FilePath: st.filePath, Line: int(c.StartPoint().Row) + 1,
			})
		}
	}

	e.walk(n, src, scope, st)
}

// juliaCalleeName decodes a call callee: bare identifier or qualified
// field_expression (`Base.show`, `Base.:+`). Returns name, receiver.
func juliaCalleeName(n *sitter.Node, src []byte) (name, receiver string) {
	if n == nil {
		return "", ""
	}
	switch n.Type() {
	case "identifier":
		return n.Content(src), ""
	case "field_expression":
		full := n.Content(src)
		idx := strings.LastIndex(full, ".")
		if idx <= 0 {
			return strings.TrimPrefix(full, ":"), ""
		}
		receiver = full[:idx]
		name = strings.TrimPrefix(full[idx+1:], ":") // Base.:+ → +
		return name, receiver
	case "quote_expression": // bare operator callee, e.g. `:+`
		if inner := n.NamedChild(0); inner != nil {
			return inner.Content(src), ""
		}
	}
	return "", ""
}

// juliaSignatureCall peels the wrappers a definition head can carry until
// it reaches the call_expression that names the definition. Three wrappers
// occur, and they nest in either order:
//
//	signature          the long form's head, `function f(x) ... end`
//	where_expression   `f(x) where T`          → where(call)
//	typed_expression   `f(x)::Int`             → typed(call)
//	                   `f(x)::T where T`       → where(typed(call))
//
// so the peel is a loop rather than a fixed descent. Anything else stops
// it: an assignment whose left-hand side is `x::Int` peels the
// typed_expression and finds a bare identifier, which is a typed variable,
// not a definition.
func juliaSignatureCall(n *sitter.Node) *sitter.Node {
	for n != nil {
		switch n.Type() {
		case "call_expression":
			return n
		case "signature", "where_expression", "typed_expression":
			n = n.NamedChild(0)
		default:
			return nil
		}
	}
	return nil
}

// handleFunction covers `function f(...) end` and `macro m(...) end`.
// The grammar has no named fields here: the first named child is the
// `signature` wrapping the callee call_expression (optionally inside a
// where_expression). Qualified callees become KindMethod with
// Meta["receiver"] + EdgeMemberOf, mirroring the luau extractor.
func (e *JuliaExtractor) handleFunction(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, doc string) {
	head := n.NamedChild(0)
	name, receiver := "", ""
	if call := juliaSignatureCall(head); call != nil {
		name, receiver = juliaCalleeName(call.NamedChild(0), src)
	} else if head != nil && head.Type() == "signature" {
		// `function f end` declares an empty generic function: there is
		// no argument list, so the signature holds the callee directly.
		name, receiver = juliaCalleeName(head.NamedChild(0), src)
	}

	inner := scope
	if name != "" {
		kind := graph.KindFunction
		id := st.filePath + "::" + name
		if receiver != "" {
			kind = graph.KindMethod
			id = st.filePath + "::" + receiver + "." + name
		}
		if !st.seen[id] {
			st.seen[id] = true
			node := &graph.Node{
				ID: id, Kind: kind, Name: name,
				FilePath:  st.filePath,
				StartLine: int(n.StartPoint().Row) + 1,
				EndLine:   int(n.EndPoint().Row) + 1,
				Language:  "julia",
			}
			meta := map[string]any{}
			if receiver != "" {
				meta["receiver"] = receiver
			}
			if n.Type() == "macro_definition" {
				meta["macro"] = true
			}
			if doc != "" {
				meta["doc"] = doc
			}
			if len(meta) > 0 {
				node.Meta = meta
			}
			st.result.Nodes = append(st.result.Nodes, node)
			st.nodes[id] = node
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
				FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
			})
			if receiver != "" {
				st.result.Edges = append(st.result.Edges, &graph.Edge{
					From: id, To: st.filePath + "::" + receiver, Kind: graph.EdgeMemberOf,
					FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
				})
			}
			if scope.moduleID != "" {
				st.result.Edges = append(st.result.Edges, &graph.Edge{
					From: id, To: scope.moduleID, Kind: graph.EdgeMemberOf,
					FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
				})
			}
		}
		inner.functionID = id
		inner.functionName = name
		inner.functionRecv = receiver
	}
	// Body docstring: first body statement that is a string literal.
	bodyDoc := ""
	if name != "" && doc == "" {
		if second := n.NamedChild(1); second != nil && second.Type() == "string_literal" {
			bodyDoc = juliaDocText(second, src)
		}
	}
	if bodyDoc != "" {
		if node, ok := st.nodes[inner.functionID]; ok && node.Meta == nil {
			node.Meta = map[string]any{"doc": bodyDoc}
		}
	}
	e.walk(n, src, inner, st)
}

// handleAssignment: `f(x) = body` (and `f(x) where T = body`) are
// short-form function definitions — the LHS is a call_expression,
// directly or under a where_expression. `const X = ...` arrives with
// isConst and a plain identifier LHS.
func (e *JuliaExtractor) handleAssignment(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState, isConst bool) {
	lhs := n.NamedChild(0)

	// Short-form definition? The left-hand side carries the same
	// wrappers a long-form signature does — `f(x) where T`,
	// `f(x)::Int`, `f(x)::T where T` — so peel with the shared helper
	// instead of unwrapping one fixed level.
	if sig := juliaSignatureCall(lhs); sig != nil {
		if name, receiver := juliaCalleeName(sig.NamedChild(0), src); name != "" {
			e.emitShortFunction(n, sig, name, receiver, src, scope, st)
			return
		}
	}

	if isConst && lhs != nil && lhs.Type() == "identifier" {
		name := lhs.Content(src)
		id := st.filePath + "::" + name
		if !st.seen[id] {
			st.seen[id] = true
			st.result.Nodes = append(st.result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindVariable, Name: name,
				FilePath:  st.filePath,
				StartLine: int(n.StartPoint().Row) + 1,
				EndLine:   int(n.EndPoint().Row) + 1,
				Language:  "julia",
			})
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
				FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
			})
		}
	}

	e.walk(n, src, scope, st)
}

func (e *JuliaExtractor) emitShortFunction(n, sig *sitter.Node, name, receiver string, src []byte, scope juliaScope, st *juliaWalkState) {
	kind := graph.KindFunction
	id := st.filePath + "::" + name
	if receiver != "" {
		kind = graph.KindMethod
		id = st.filePath + "::" + receiver + "." + name
	}
	if !st.seen[id] {
		st.seen[id] = true
		node := &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath:  st.filePath,
			StartLine: int(n.StartPoint().Row) + 1,
			EndLine:   int(n.EndPoint().Row) + 1,
			Language:  "julia",
		}
		if receiver != "" {
			node.Meta = map[string]any{"receiver": receiver}
		}
		st.result.Nodes = append(st.result.Nodes, node)
		st.nodes[id] = node
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: st.fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
		})
		if receiver != "" {
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: id, To: st.filePath + "::" + receiver, Kind: graph.EdgeMemberOf,
				FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
			})
		}
		if scope.moduleID != "" {
			st.result.Edges = append(st.result.Edges, &graph.Edge{
				From: id, To: scope.moduleID, Kind: graph.EdgeMemberOf,
				FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
			})
		}
	}
	inner := scope
	inner.functionID = id
	inner.functionName = name
	inner.functionRecv = receiver
	e.walk(n, src, inner, st)
}

// juliaImportModule returns the module a `selected_import` /
// `import_alias` names, plus the index of the first child after it. A
// dotted or relative path (`A.B`, `.Local`, `..Up`) is an `import_path`
// node; a single-segment module is a bare `identifier`. Everything after
// it is the selected bindings or the alias.
func juliaImportModule(n *sitter.Node, src []byte) (module string, next int) {
	first := n.NamedChild(0)
	if first == nil {
		return "", 0
	}
	switch first.Type() {
	case "import_path", "identifier":
		return first.Content(src), 1
	}
	return "", 0
}

// juliaAliasParts decodes an `import_alias` — `Foo as F`, `C.D as CD` at
// statement level, `bar as baz` inside a selected list — into the
// upstream name and the local alias.
func juliaAliasParts(n *sitter.Node, src []byte) (orig, alias string) {
	orig, next := juliaImportModule(n, src)
	if orig == "" {
		return "", ""
	}
	for i, count := next, int(n.NamedChildCount()); i < count; i++ {
		s := n.NamedChild(i)
		if s == nil {
			continue
		}
		if s.Type() == "identifier" || s.Type() == "operator" {
			alias = s.Content(src)
		}
	}
	return orig, alias
}

// juliaCollectImportAliases records every `import M as A` / `using M as A`
// module rename in the file, so handleCall can rewrite a qualified callee
// onto the module it actually names. Only MODULE aliases are collected: a
// renamed binding inside a selected list (`import Foo: bar as baz`)
// renames a function, and rewriting a bare call through it would fight
// any local shadowing the extractor cannot see.
func juliaCollectImportAliases(n *sitter.Node, src []byte, out map[string]string) {
	if n.Type() == "using_statement" || n.Type() == "import_statement" {
		for c := range n.NamedChildren() {
			if c.Type() != "import_alias" {
				continue
			}
			if module, alias := juliaAliasParts(c, src); module != "" && alias != "" && alias != module {
				out[alias] = module
			}
		}
		return
	}
	for c := range n.NamedChildren() {
		juliaCollectImportAliases(c, src, out)
	}
}

// handleImport covers `using M`, `using M: a, b`, `import M`,
// `import M as Alias`, and dotted / relative import paths. The import
// target is always the MODULE; a selective list additionally emits one
// binding-aware edge per selected name (the JS/TS per-binding
// convention), so "who imports `mean` from Statistics" is a traversable
// question rather than a Meta key nothing reads.
func (e *JuliaExtractor) handleImport(n *sitter.Node, src []byte, st *juliaWalkState) {
	line := int(n.StartPoint().Row) + 1
	emit := func(target, alias string, meta map[string]any) {
		if target == "" {
			return
		}
		if len(meta) == 0 {
			meta = nil
		}
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: st.fileNode.ID, To: "unresolved::import::" + target,
			Kind:     graph.EdgeImports,
			FilePath: st.filePath, Line: line,
			// Edge.Alias is the graph's canonical spelling for a renamed
			// binding; Meta keeps the same fact on the durable side,
			// since the SQLite edges table has no alias column.
			Alias: alias,
			Meta:  meta,
		})
	}
	// One edge per selected binding, targeting the binding rather than
	// the module — the representation JS/TS already emits for
	// `import { a, b as c } from "mod"`.
	emitBinding := func(module, orig, alias string) {
		if module == "" || orig == "" {
			return
		}
		st.result.Edges = append(st.result.Edges, &graph.Edge{
			From: st.fileNode.ID, To: "unresolved::import::" + module + "::" + orig,
			Kind:     graph.EdgeImports,
			FilePath: st.filePath, Line: line,
			Alias: alias,
		})
	}
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "identifier", "import_path":
			emit(c.Content(src), "", nil)
		case "selected_import":
			// `using A.B: x, y` — the module is the FIRST child and is
			// an import_path whenever the path is dotted or relative.
			// Scanning for identifiers instead skipped it and promoted
			// the first selected name to the import target.
			module, next := juliaImportModule(c, src)
			var names []string
			type binding struct{ orig, alias string }
			var bindings []binding
			for i, count := next, int(c.NamedChildCount()); i < count; i++ {
				s := c.NamedChild(i)
				if s == nil {
					continue
				}
				switch s.Type() {
				case "identifier", "operator":
					// `using Base: +, -` selects operators, which are
					// `operator` nodes rather than identifiers.
					names = append(names, s.Content(src))
					bindings = append(bindings, binding{orig: s.Content(src)})
				case "import_alias":
					// `import Foo: bar as baz` renames one binding.
					orig, alias := juliaAliasParts(s, src)
					if orig == "" {
						continue
					}
					names = append(names, orig)
					if alias == orig {
						alias = ""
					}
					bindings = append(bindings, binding{orig: orig, alias: alias})
				}
			}
			meta := map[string]any{}
			if len(names) > 0 {
				meta["names"] = names
			}
			emit(module, "", meta)
			for _, b := range bindings {
				emitBinding(module, b.orig, b.alias)
			}
		case "import_alias":
			path, alias := juliaAliasParts(c, src)
			if alias == path {
				alias = ""
			}
			meta := map[string]any{}
			if alias != "" {
				meta["alias"] = alias
			}
			emit(path, alias, meta)
		}
	}
}

// handleExport records the module's public surface on the enclosing
// module node's Meta (Julia export lists are only meaningful inside
// modules).
func (e *JuliaExtractor) handleExport(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	if scope.moduleID == "" {
		return
	}
	node, ok := st.nodes[scope.moduleID]
	if !ok {
		return
	}
	names := []string{}
	for c := range n.NamedChildren() {
		if c.Type() == "identifier" {
			names = append(names, c.Content(src))
		}
	}
	if len(names) == 0 {
		return
	}
	if node.Meta == nil {
		node.Meta = map[string]any{}
	}
	prev, _ := node.Meta["exports"].([]string)
	node.Meta["exports"] = append(prev, names...)
}

// handleCall emits EdgeCalls from the enclosing function to the callee
// (bare or qualified). `include("f.jl")` becomes an import edge instead,
// preserving the legacy extractor's contract.
func (e *JuliaExtractor) handleCall(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	callee := n.NamedChild(0)
	name, receiver := juliaCalleeName(callee, src)
	if name == "" {
		return
	}
	if name == "include" && receiver == "" {
		if args := n.NamedChild(1); args != nil && args.Type() == "argument_list" {
			if lit := args.NamedChild(0); lit != nil && lit.Type() == "string_literal" {
				st.result.Edges = append(st.result.Edges, &graph.Edge{
					From:     st.fileNode.ID,
					To:       "unresolved::import::" + juliaUnquote(lit.Content(src)),
					Kind:     graph.EdgeImports,
					FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
				})
			}
		}
		return
	}
	if scope.functionID == "" {
		return
	}
	if scope.functionName == name && scope.functionRecv == receiver {
		return // direct recursion
	}
	target := name
	if receiver != "" {
		// `import Foo as F` then `F.process(x)`: name the module, not
		// the file-local nickname, so the call target is something
		// another file's `module Foo` can be matched against.
		if module, ok := st.importAliases[receiver]; ok {
			receiver = module
		}
		target = receiver + "." + name
	}
	meta := map[string]any{}
	if n.Type() == "broadcast_call_expression" {
		meta["broadcast"] = true
	}
	edge := &graph.Edge{
		From: scope.functionID, To: "unresolved::" + target,
		Kind:     graph.EdgeCalls,
		FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
	}
	if len(meta) > 0 {
		edge.Meta = meta
	}
	st.result.Edges = append(st.result.Edges, edge)
}

// handleMacroCall attributes `@macroname ...` invocations to the
// enclosing function as calls to the bare macro name.
func (e *JuliaExtractor) handleMacroCall(n *sitter.Node, src []byte, scope juliaScope, st *juliaWalkState) {
	if scope.functionID == "" {
		return
	}
	for c := range n.NamedChildren() {
		if c.Type() != "macro_identifier" {
			continue
		}
		for m := range c.NamedChildren() {
			if m.Type() == "identifier" {
				st.result.Edges = append(st.result.Edges, &graph.Edge{
					From: scope.functionID, To: "unresolved::" + m.Content(src),
					Kind:     graph.EdgeCalls,
					Meta:     map[string]any{"macro": true},
					FilePath: st.filePath, Line: int(n.StartPoint().Row) + 1,
				})
			}
		}
	}
}

// juliaDocText normalises a docstring string literal: strips quotes and
// collapses to the first paragraph.
func juliaDocText(n *sitter.Node, src []byte) string {
	s := juliaUnquote(n.Content(src))
	if i := strings.Index(s, "\n\n"); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func juliaUnquote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "\"\"\"")
	s = strings.TrimSuffix(s, "\"\"\"")
	s = strings.TrimPrefix(s, "\"")
	s = strings.TrimSuffix(s, "\"")
	return strings.TrimSpace(s)
}

var _ parser.Extractor = (*JuliaExtractor)(nil)
