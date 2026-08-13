package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Out-of-line definitions — `void ns::Cls::run() {…}` written in a .cpp whose
// class lives in a header — carry a qualified_identifier declarator. The
// function_definition patterns only admitted a bare identifier, so the whole
// definition matched nothing: no method node, and every call in its body was
// dropped with it (a deferred call needs an enclosing function range to attach
// to). In an idiomatic translation unit that is most of the file.

// emitOutOfLineMember emits a definition whose declarator names its own scope.
// The trailing segment of the qualifier decides the shape: a type owner yields
// a method (`Cls::run` → `file.cpp::Cls.run`, member_of the class when it is
// declared in this file), and an all-namespace qualifier yields a free function
// carrying the qualifier as scope_ns (`ns::helper`). Reports whether the node
// was handled, so the caller can fall through to its regular emission path.
func (e *CppExtractor) emitOutOfLineMember(fnNode *sitter.Node, startLine, endLine int, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) bool {
	decl := cppQualifiedDeclarator(fnNode)
	if decl == nil {
		return false
	}
	scopes, name := cppQualifiedParts(decl, src)
	if name == "" || len(scopes) == 0 {
		return false
	}

	owner := scopes[len(scopes)-1]
	nsParts := scopes[:len(scopes)-1]
	if !cppScopeNamesType(owner, filePath, seen) {
		// Every segment is a namespace — `void ns::helper() {}` declares a
		// namespace-scope function, not a member.
		owner, nsParts = "", scopes
	}

	id := filePath + "::" + name
	if owner != "" {
		id = filePath + "::" + owner + "." + name
	}
	if seen[id] {
		id += "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return true
	}
	seen[id] = true

	meta := map[string]any{}
	if ns := cppJoinNamespace(enclosingCppNamespace(fnNode, src), nsParts); ns != "" {
		meta["scope_ns"] = ns
	}
	if rt := cppReturnType(fnNode, src); rt != "" {
		meta["return_type"] = rt
	}
	stampCppSignature(meta, fnNode, src)

	kind := graph.KindFunction
	if owner != "" {
		kind = graph.KindMethod
		meta["receiver"] = owner
		meta["scope_class"] = owner
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "cpp", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	// The owning class is usually in a header, and this extractor never invents
	// a node for a type it has not seen — link the member only when the class
	// was emitted from this same file.
	if owner != "" && seen[cppTypeMarker(filePath, owner)] {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: filePath + "::" + owner, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: startLine,
		})
	}
	return true
}

// cppQualifiedDeclarator returns the qualified_identifier a function definition
// declares itself with, or nil when the declarator is a plain name. Mirrors the
// declarator dispatch in extractFuncName so the same pointer / reference /
// parenthesized wrappers are peeled first.
func cppQualifiedDeclarator(fnNode *sitter.Node) *sitter.Node {
	if fnNode == nil {
		return nil
	}
	fd := cppFunctionDeclarator(fnNode.ChildByFieldName("declarator"))
	if fd == nil {
		return nil
	}
	for i, _nc := 0, int(fd.NamedChildCount()); i < _nc; i++ {
		switch c := fd.NamedChild(i); c.Type() {
		case "qualified_identifier":
			return c
		case "identifier", "field_identifier", "destructor_name", "operator_name":
			return nil
		}
	}
	return nil
}

// cppQualifiedParts splits a qualified_identifier into its scope segments and
// its trailing name: `ns::Cls::run` → ["ns", "Cls"], "run". tree-sitter-cpp
// nests another qualified_identifier in the name field for every extra ::
// segment, so the walk is structural rather than depth-fixed; a templated tail
// (`Cls::run<int>`) adds one template_function wrapper, and a templated scope
// (`Holder<T>::put`) carries the segment inside a template_type. An unknown
// shape yields no name rather than an invented one.
//
// Both sides of a :: name need this walk — the callee of `a::b::c()` and the
// declarator of `void a::b::c() {}` are the same shape — so it is the one
// place either is decoded. Callers that only want the name discard the scopes.
func cppQualifiedParts(node *sitter.Node, src []byte) ([]string, string) {
	var scopes []string
	for depth := 0; node != nil && depth < 64; depth++ {
		switch node.Type() {
		case "qualified_identifier":
			if s := node.ChildByFieldName("scope"); s != nil {
				if seg := cppScopeSegment(s, src); seg != "" {
					scopes = append(scopes, seg)
				}
			}
			node = node.ChildByFieldName("name")
		case "template_function", "template_method":
			node = node.ChildByFieldName("name")
		case "identifier", "field_identifier", "destructor_name", "operator_name":
			return scopes, node.Content(src)
		default:
			return scopes, ""
		}
	}
	return scopes, ""
}

// cppScopeSegment renders one :: segment of a qualifier, dropping the template
// arguments a generic owner carries (`Holder<T>` → `Holder`) so the segment
// matches the type node's name.
func cppScopeSegment(node *sitter.Node, src []byte) string {
	if node.Type() == "template_type" {
		if inner := node.ChildByFieldName("name"); inner != nil {
			return strings.TrimSpace(inner.Content(src))
		}
	}
	return strings.TrimSpace(node.Content(src))
}

// cppScopeNamesType reports whether a qualifier segment names a type rather
// than a namespace. A class or struct emitted from this file is proof; nothing
// else is, because the declaring header is a different translation unit — so
// the same Capitalized-name heuristic the reference-form pass uses decides.
func cppScopeNamesType(seg, filePath string, seen map[string]bool) bool {
	if seen[cppTypeMarker(filePath, seg)] {
		return true
	}
	if seen[cppNamespaceMarker(filePath, seg)] {
		return false
	}
	return isCapitalizedCppType(seg)
}

// cppTypeMarker / cppNamespaceMarker key the seen-set entries that record what
// this file declared, following the "_method_L<line>" marker convention the
// class-body walk already uses to talk to the function_definition dispatch.
func cppTypeMarker(filePath, name string) string { return filePath + "::_type_" + name }

func cppNamespaceMarker(filePath, name string) string { return filePath + "::_ns_" + name }

// cppJoinNamespace concatenates the lexically enclosing namespace with the
// namespace segments the declarator spells out — `namespace a { void b::f(){} }`
// puts f in a::b.
func cppJoinNamespace(enclosing string, parts []string) string {
	all := parts
	if enclosing != "" {
		all = append([]string{enclosing}, parts...)
	}
	return strings.Join(all, "::")
}
