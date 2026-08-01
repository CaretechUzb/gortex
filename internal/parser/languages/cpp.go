package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/cpp"
)

// qCppAll is a single tree-sitter query alternating over every pattern
// the C++ extractor needs. One tree walk per file replaces the 8
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch
// in Extract can branch on which name is set. Class-method extraction
// still walks the class_specifier body inline — C++ methods can have
// declarators other than bare identifiers (destructor_name,
// field_identifier, qualified_identifier), which the legacy code
// handled via extractFuncName and an explicit body walk; keeping that
// walk inside the class.def dispatch preserves behaviour while still
// collapsing the repeated whole-tree scans into one.
const qCppAll = `
[
  (namespace_definition
    name: (namespace_identifier) @ns.name) @ns.def

  (class_specifier
    name: (type_identifier) @class.name) @class.def

  (struct_specifier
    name: (type_identifier) @struct.name) @struct.def

  (enum_specifier
    name: (type_identifier) @enum.name) @enum.def

  (function_definition
    declarator: (function_declarator
      declarator: (identifier) @func.name)) @func.def

  (function_definition
    declarator: (pointer_declarator
      declarator: (function_declarator
        declarator: (identifier) @func.name))) @func.def

  (function_definition
    declarator: (pointer_declarator
      declarator: (pointer_declarator
        declarator: (function_declarator
          declarator: (identifier) @func.name)))) @func.def

  (function_definition
    declarator: (reference_declarator
      (function_declarator
        declarator: (identifier) @func.name))) @func.def

  (preproc_include
    path: (_) @include.path) @include.def

  (preproc_def
    name: (identifier) @macro.name) @macro.def

  (preproc_function_def
    name: (identifier) @macrofn.name) @macrofn.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (field_expression
      field: (field_identifier) @callm.method)) @callm.expr
]
`

// CppExtractor extracts C++ source files into graph nodes and edges.
type CppExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewCppExtractor() *CppExtractor {
	lang := cpp.GetLanguage()
	return &CppExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qCppAll, lang),
	}
}

func (e *CppExtractor) Language() string     { return "cpp" }
func (e *CppExtractor) Extensions() []string { return []string{".cpp", ".cc", ".cxx", ".hpp"} }

// --- Deferred call buffer ----------------------------------------

// cppDeferredCall buffers a call site discovered during the
// per-match walk so the post-pass can attribute it to the enclosing
// function once funcRanges is built. argTypes carries the C++ ADL
// hint set populated by extractCppCallArgTypes.
type cppDeferredCall struct {
	name     string
	line     int
	isMember bool
	receiver string
	argTypes []string
}

func (e *CppExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "cpp",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	var calls []cppDeferredCall

	// The query and inline class/body walks emit graph values and scalar
	// deferred calls only; no tree-sitter wrapper escapes this pass.
	root.WithScratch(func() {
		parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
			switch {

			case m.Captures["ns.def"] != nil:
				e.emitNamespace(m, filePath, fileID, result)

			case m.Captures["class.def"] != nil:
				e.emitClass(m, filePath, fileID, src, result, seen)

			case m.Captures["struct.def"] != nil:
				e.emitStruct(m, filePath, fileID, src, result, seen)

			case m.Captures["enum.def"] != nil:
				e.emitEnum(m, filePath, fileID, result, seen)

			case m.Captures["func.def"] != nil:
				e.emitFunction(m, filePath, fileID, src, result, seen)

			case m.Captures["include.def"] != nil:
				e.emitInclude(m, filePath, fileID, result)

			case m.Captures["macro.def"] != nil:
				emitCMacro(m.Captures["macro.def"].Node, false, filePath, fileID, "cpp", src, result, seen)

			case m.Captures["macrofn.def"] != nil:
				emitCMacro(m.Captures["macrofn.def"].Node, true, filePath, fileID, "cpp", src, result, seen)

			case m.Captures["callm.expr"] != nil:
				expr := m.Captures["callm.expr"]
				calls = append(calls, cppDeferredCall{
					name:     m.Captures["callm.method"].Text,
					line:     expr.StartLine + 1,
					isMember: true,
					receiver: cppCallReceiverText(expr.Node, src),
					argTypes: extractCppCallArgTypes(expr.Node, src),
				})

			case m.Captures["call.expr"] != nil:
				expr := m.Captures["call.expr"]
				calls = append(calls, cppDeferredCall{
					name:     m.Captures["call.name"].Text,
					line:     expr.StartLine + 1,
					argTypes: extractCppCallArgTypes(expr.Node, src),
				})
			}
		})
	})

	// Resolve call edges against funcRanges.
	funcRanges := buildFuncRanges(result)

	// Emit type-use edges (EdgeTypedAs) for declaration positions the
	// per-match pass leaves edge-less: local variable declarations,
	// function parameters, and return types — each attributed to the
	// enclosing function/method via funcRanges. Member/field type-uses
	// are attributed to the owning class/struct node during the body
	// walk above, so they're already in result.Edges.
	root.WithScratch(func() {
		collectCppTypeUseEdges(root, funcRanges, filePath, src, result)
	})

	// Emit the remaining reference forms a type can appear in beyond a
	// declaration position: construction (new / stack), base-class
	// inheritance, casts, and Capitalized scope/static access. These are
	// EdgeInstantiates (construction) and EdgeReferences with a
	// ref_context subkind (inherit / cast / static_access).
	root.WithScratch(func() {
		emitCppReferenceForms(root, src, filePath, fileID, funcRanges, result)
	})

	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		edge := &graph.Edge{
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			From: callerID,
		}
		if c.isMember {
			edge.To = "unresolved::*." + c.name
		} else {
			edge.To = "unresolved::" + c.name
		}
		if len(c.argTypes) > 0 {
			edge.Meta = map[string]any{
				"scope_arg_types": strings.Join(c.argTypes, ","),
			}
		}
		if c.isMember && c.receiver != "" {
			stampFactoryChainReceiver(edge, c.receiver, resolveChainType(c.receiver, nil, result))
		}
		result.Edges = append(result.Edges, edge)
	}

	captureCFnPointerDispatch(result, root, filePath, src)
	root.WithScratch(func() { captureFnValueCandidates(result, root, filePath, src) })

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *CppExtractor) emitNamespace(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	name := m.Captures["ns.name"].Text
	def := m.Captures["ns.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "cpp",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitClass emits the class node and walks its body inline for methods.
// The inline body walk replaces legacy extractClassMethods and catches
// declarators the outer function_definition query misses
// (field_identifier, destructor_name, qualified_identifier).
func (e *CppExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	className := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	classID := filePath + "::" + className
	if seen[classID] {
		return
	}
	seen[classID] = true
	meta := map[string]any{"type_flavor": "class"}
	if ns := enclosingCppNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if parent := extractCppParentClass(def.Node, src); parent != "" {
		meta["scope_parent"] = parent
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: classID, Kind: graph.KindType, Name: className,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "cpp", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: classID, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	e.walkClassBody(def.Node, src, filePath, fileID, className, classID, seen, result)
}

func (e *CppExtractor) walkClassBody(classNode *sitter.Node, src []byte, filePath, fileID, className, classID string, seen map[string]bool, result *parser.ExtractionResult) {
	var body *sitter.Node
	for i, _nc := 0, int(classNode.NamedChildCount()); i < _nc; i++ {
		child := classNode.NamedChild(i)
		if child.Type() == "field_declaration_list" {
			body = child
			break
		}
	}
	if body == nil {
		return
	}
	typeSeen := make(map[string]bool)
	for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
		child := body.NamedChild(i)
		switch child.Type() {
		case "access_specifier":
			continue
		case "function_definition":
			e.addMethodFromNode(child, src, filePath, fileID, className, classID, seen, result)
		case "field_declaration":
			// Member/field type-use: a `Foo bar;` member references Foo.
			// Attribute to the owning class node (the extractor doesn't
			// materialise a per-field node). Smart-pointer / container
			// fields unwrap to the inner type via the canonicaliser.
			line := int(child.StartPoint().Row) + 1
			if tn := child.ChildByFieldName("type"); tn != nil {
				emitCppTypeUseEdges(classID, tn.Content(src), filePath, line, result, typeSeen)
			}
		case "declaration_list":
			for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
				gc := child.NamedChild(j)
				if gc.Type() == "function_definition" {
					e.addMethodFromNode(gc, src, filePath, fileID, className, classID, seen, result)
				}
			}
		}
	}
}

func (e *CppExtractor) addMethodFromNode(funcNode *sitter.Node, src []byte, filePath, fileID, className, classID string, seen map[string]bool, result *parser.ExtractionResult) {
	methodName := extractFuncName(funcNode, src)
	if methodName == "" {
		return
	}
	startLine := int(funcNode.StartPoint().Row) + 1
	endLine := int(funcNode.EndPoint().Row) + 1

	id := filePath + "::" + className + "." + methodName
	if seen[id] {
		id = filePath + "::" + className + "." + methodName + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	// Mark line so the function_definition dispatcher skips this.
	seen[filePath+"::_method_L"+fmt.Sprint(startLine)] = true

	meta := map[string]any{"receiver": className, "scope_class": className}
	if ns := enclosingCppNamespace(funcNode, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if rt := cppReturnType(funcNode, src); rt != "" {
		meta["return_type"] = rt
	}
	stampCppSignature(meta, funcNode, src)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: methodName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "cpp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})
}

// cppReturnType returns the base return type of a C++ function/method node (its
// `type` field), stripping reference/pointer/const/template decoration to the
// bare type name — the seed for chained-factory receiver inference
// (`Foo::make().x()`).
func cppReturnType(node *sitter.Node, src []byte) string {
	t := node.ChildByFieldName("type")
	if t == nil {
		return ""
	}
	rt := strings.TrimSpace(t.Content(src))
	// Unwrap a smart-pointer / optional return (`unique_ptr<Widget>` → Widget)
	// so a chained factory call (`make_widget()->draw()`) infers the pointee as
	// the receiver, not the wrapper.
	rt = graph.UnwrapCppSmartPointer(rt)
	rt = strings.TrimPrefix(rt, "const ")
	rt = strings.TrimRight(rt, " &*")
	if i := strings.IndexByte(rt, '<'); i >= 0 {
		rt = strings.TrimSpace(rt[:i])
	}
	return rt
}

func (e *CppExtractor) emitStruct(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["struct.name"].Text
	def := m.Captures["struct.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "cpp", Meta: map[string]any{"type_flavor": "struct"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	e.emitCppStructFieldTypeUse(def.Node, id, filePath, src, result)
}

// emitCppStructFieldTypeUse walks a struct_specifier (or class_specifier)
// body's direct field_declaration members and emits EdgeTypedAs from the
// owning type node to each member's referenced type. Structs don't get
// the inline method/field walk classes do, so this is the field-position
// type-use pass for them. Methods nested in a struct are handled
// elsewhere; only data members carry a `type` field here.
func (e *CppExtractor) emitCppStructFieldTypeUse(structNode *sitter.Node, ownerID, filePath string, src []byte, result *parser.ExtractionResult) {
	if structNode == nil {
		return
	}
	var body *sitter.Node
	for i, _nc := 0, int(structNode.NamedChildCount()); i < _nc; i++ {
		child := structNode.NamedChild(i)
		if child.Type() == "field_declaration_list" {
			body = child
			break
		}
	}
	if body == nil {
		return
	}
	typeSeen := make(map[string]bool)
	for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
		child := body.NamedChild(i)
		if child.Type() != "field_declaration" {
			continue
		}
		line := int(child.StartPoint().Row) + 1
		if tn := child.ChildByFieldName("type"); tn != nil {
			emitCppTypeUseEdges(ownerID, tn.Content(src), filePath, line, result, typeSeen)
		}
	}
}

func (e *CppExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "cpp", Meta: map[string]any{"type_flavor": "enum"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitFunction emits a free function. When the same line was already
// claimed by the class-body walk (seen "_method_L<line>"), this is a
// class method with a bare identifier declarator that was emitted
// through addMethodFromNode — skip the duplicate.
func (e *CppExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine := def.StartLine + 1
	lineKey := filePath + "::_method_L" + fmt.Sprint(startLine)
	if seen[lineKey] {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{}
	if ns := enclosingCppNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	// Free-function return type (smart-pointer-unwrapped) seeds chained-factory
	// receiver inference for a bare factory call (`make_widget()->draw()`), the
	// same way addMethodFromNode seeds `Foo::make().x()`.
	if rt := cppReturnType(def.Node, src); rt != "" {
		meta["return_type"] = rt
	}
	stampCppSignature(meta, def.Node, src)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: def.EndLine + 1,
		Language: "cpp", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
}

func (e *CppExtractor) emitInclude(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	pathCap := m.Captures["include.path"]
	raw := strings.TrimSpace(pathCap.Text)
	kind := "system"
	if strings.HasPrefix(raw, `"`) {
		kind = "quoted"
	}
	includePath := strings.Trim(raw, `"<>`)
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + includePath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     pathCap.StartLine + 1,
		Meta:     map[string]any{"include_kind": kind},
	})
}

// --- Helpers --------------------------------------------------------

// extractFuncName walks a function_definition node to find the function name.
// It handles both `identifier` (free functions) and `field_identifier` (methods),
// and peels pointer / reference / parenthesized declarator wrappers so a
// pointer-return method (`robj *Cls::bar() {…}`, `Widget &make() {…}`) is not
// dropped — the function_declarator nests one or two levels below the outer
// declarator in those signatures.
func extractFuncName(funcNode *sitter.Node, src []byte) string {
	fd := cppFunctionDeclarator(funcNode.ChildByFieldName("declarator"))
	if fd == nil {
		return ""
	}
	for j, _nc := 0, int(fd.NamedChildCount()); j < _nc; j++ {
		gc := fd.NamedChild(j)
		switch gc.Type() {
		case "identifier", "field_identifier", "destructor_name", "operator_name":
			return gc.Content(src)
		case "qualified_identifier":
			return lastIdentifier(gc, src)
		}
	}
	return ""
}

// cppFunctionDeclarator descends a declarator through pointer / reference /
// parenthesized wrappers to the function_declarator that carries the name, or
// nil when the declarator does not resolve to a function. Bounds the walk so a
// pathological tree cannot loop.
func cppFunctionDeclarator(decl *sitter.Node) *sitter.Node {
	for i := 0; decl != nil && i < 8; i++ {
		switch decl.Type() {
		case "function_declarator":
			return decl
		case "pointer_declarator", "reference_declarator", "parenthesized_declarator":
			decl = cppInnerDeclarator(decl)
		default:
			return nil
		}
	}
	return nil
}

// cppInnerDeclarator returns the declarator nested inside a wrapper declarator:
// the `declarator` field when the grammar names it (pointer_declarator), else
// the first declarator-shaped named child (reference_declarator and
// parenthesized_declarator carry it positionally).
func cppInnerDeclarator(decl *sitter.Node) *sitter.Node {
	if inner := decl.ChildByFieldName("declarator"); inner != nil {
		return inner
	}
	for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
		c := decl.NamedChild(i)
		switch c.Type() {
		case "function_declarator", "pointer_declarator", "reference_declarator",
			"parenthesized_declarator", "identifier", "field_identifier",
			"qualified_identifier", "destructor_name", "operator_name":
			return c
		}
	}
	return nil
}

// lastIdentifier extracts the last identifier from a qualified_identifier.
func lastIdentifier(node *sitter.Node, src []byte) string {
	name := ""
	for i, _nc := 0, int(node.NamedChildCount()); i < _nc; i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "identifier", "field_identifier", "destructor_name":
			name = child.Content(src)
		}
	}
	return name
}

// enclosingCppNamespace walks node up through the tree-sitter AST
// looking for namespace_definition ancestors and concatenates their
// names with "::" (so `namespace a { namespace b { void foo() {} } }`
// produces "a::b"). Anonymous namespaces are skipped — a function
// inside one still belongs to the surrounding namespace for ADL.
//
// Stamped onto every function / method / type node so the resolver's
// scope-based static resolver can prefer same-namespace candidates
// before falling back to directory-locality.
func enclosingCppNamespace(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	var parts []string
	for p := node.Parent(); p != nil; p = p.Parent() {
		if p.Type() != "namespace_definition" {
			continue
		}
		nameNode := p.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := strings.TrimSpace(nameNode.Content(src))
		if name == "" {
			continue
		}
		parts = append([]string{name}, parts...)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "::")
}

// extractCppParentClass returns the name of the direct base class for
// a C++ class_specifier, or "" if the class has no base. Used by the
// scope-based static resolver to walk the inheritance chain when
// resolving `super`-style calls (C++ doesn't have a literal `super`
// keyword, but Base::method() qualifications follow the same chain).
func extractCppParentClass(classNode *sitter.Node, src []byte) string {
	if classNode == nil {
		return ""
	}
	for i, _nc := 0, int(classNode.NamedChildCount()); i < _nc; i++ {
		child := classNode.NamedChild(i)
		if child.Type() != "base_class_clause" {
			continue
		}
		for j, _nc := 0, int(child.NamedChildCount()); j < _nc; j++ {
			sub := child.NamedChild(j)
			switch sub.Type() {
			case "type_identifier", "qualified_identifier":
				return strings.TrimSpace(sub.Content(src))
			}
		}
	}
	return ""
}

// extractCppCallArgTypes returns the type-name hints harvested from a
// C++ call_expression's argument list, used to seed Argument-Dependent
// Lookup. We restrict the harvest to the cases where the argument
// type is structurally unambiguous from the call site alone:
//
//   - `new Type(...)`  → "Type"
//   - `Type{...}`      → "Type" (compound literal / temporary)
//   - `Type(arg)`      → "Type" (functional cast / explicit ctor)
//
// Anything else (bare variables, method-chain returns, expressions)
// is skipped — ADL is best-effort here, and a partial type list is
// strictly better than guessing. The resolver treats an empty hint
// set as "no ADL evidence" and falls through to the regular cascade.
func extractCppCallArgTypes(callNode *sitter.Node, src []byte) []string {
	if callNode == nil {
		return nil
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	var out []string
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		arg := args.NamedChild(i)
		typeName := cppArgTypeHint(arg, src)
		if typeName == "" {
			// Positional placeholder so the overload ranker keeps argument
			// alignment (an unknown arg is compatible with any param). The ADL
			// namespace pass ignores "?" (it yields no namespace).
			typeName = "?"
		}
		out = append(out, typeName)
	}
	return out
}

func cppArgTypeHint(arg *sitter.Node, src []byte) string {
	if arg == nil {
		return ""
	}
	switch arg.Type() {
	case "number_literal":
		if strings.ContainsAny(arg.Content(src), ".eE") && !strings.HasPrefix(arg.Content(src), "0x") {
			return "double"
		}
		return "int"
	case "string_literal", "raw_string_literal", "concatenated_string":
		return "string"
	case "char_literal":
		return "char"
	case "true", "false":
		return "bool"
	case "null", "nullptr":
		return "null"
	case "new_expression":
		if t := arg.ChildByFieldName("type"); t != nil {
			return strings.TrimSpace(t.Content(src))
		}
	case "compound_literal_expression":
		if t := arg.ChildByFieldName("type"); t != nil {
			return strings.TrimSpace(t.Content(src))
		}
	case "call_expression":
		// Functional-cast `Type(arg)`: the function position is a
		// type_identifier or qualified_identifier whose text is the
		// type itself. Distinguishes from regular method calls by
		// checking that the function position resolves to a type
		// name (proper-cased, single-segment) — heuristic but safe
		// because the resolver treats ADL hints as evidence, not
		// truth.
		if f := arg.ChildByFieldName("function"); f != nil {
			switch f.Type() {
			case "type_identifier", "qualified_identifier":
				return strings.TrimSpace(f.Content(src))
			}
		}
	}
	return ""
}

// cppCallReceiverText returns the receiver expression text of a member call
// `recv.method(...)` / `recv->method(...)` -- the object of the call's
// field_expression -- so a factory chain (`make().with().build()`) can be
// typed by resolveChainType.
func cppCallReceiverText(callNode *sitter.Node, src []byte) string {
	if callNode == nil {
		return ""
	}
	fn := callNode.ChildByFieldName("function")
	if fn == nil || fn.Type() != "field_expression" {
		return ""
	}
	obj := fn.ChildByFieldName("argument")
	if obj == nil && fn.NamedChildCount() > 0 {
		obj = fn.NamedChild(0)
	}
	if obj == nil {
		return ""
	}
	return strings.TrimSpace(obj.Content(src))
}
