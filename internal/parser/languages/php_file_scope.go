package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// File-scope PHP.
//
// extractCallSites is reached only from extractFunction / extractMethod, so
// every statement that is not inside a named function or method was invisible:
// a script's whole body, a bootstrap file, a Laravel `routes/web.php`, a
// WordPress theme template, a fixture that returns a closure. Those files
// produced a file node and nothing else — 35 of symfony/console's 361 files
// yielded zero symbols and zero call edges.
//
// walkPHPFileScope re-walks the tree attributing those statements to the file
// node, stopping wherever a declaration owns its own subtree so nothing is
// counted twice.

// phpDeclarationBoundary reports whether a node's subtree is extracted by its
// own declaration handler, and so must not be walked again at file scope.
func phpDeclarationBoundary(t string) bool {
	switch t {
	case "function_definition", "method_declaration", "class_declaration",
		"interface_declaration", "trait_declaration", "enum_declaration",
		"anonymous_class":
		return true
	}
	return false
}

// walkPHPFileScope attributes calls and member accesses in top-level
// statements to the file node. A closure written at file scope is walked as
// part of it — the closure is not a symbol, so its body belongs to the file
// that defines it.
func (e *PHPExtractor) walkPHPFileScope(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
	env phpReceiverEnv,
) {
	if node == nil || phpDeclarationBoundary(node.Type()) {
		return
	}
	switch node.Type() {
	case "anonymous_function", "anonymous_function_creation_expression", "arrow_function":
		env = env.childScope(node, src)
	}
	e.emitPHPCallSiteEdges(node, src, filePath, fileNode.ID, result, env)
	for i, n := 0, int(node.NamedChildCount()); i < n; i++ {
		e.walkPHPFileScope(node.NamedChild(i), src, filePath, fileNode, result, env)
	}
}

// extractPHPFileScope seeds the file-scope walk. The environment is built from
// the top-level statements themselves, so `$app = new Application(); $app->run();`
// in a bootstrap script types its receiver exactly as it would inside a
// function.
func (e *PHPExtractor) extractPHPFileScope(
	root *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	if root == nil {
		return
	}
	env := phpReceiverEnv{vars: typeEnv{}}
	collectPHPFileScopeTypes(root, src, env.vars)
	e.walkPHPFileScope(root, src, filePath, fileNode, result, env)
}

// collectPHPFileScopeTypes records `$v = new Foo` bindings made in top-level
// statements, skipping any subtree a declaration owns.
func collectPHPFileScopeTypes(node *sitter.Node, src []byte, vars typeEnv) {
	if node == nil || phpDeclarationBoundary(node.Type()) {
		return
	}
	if node.Type() == "assignment_expression" {
		left := node.ChildByFieldName("left")
		right := node.ChildByFieldName("right")
		if left != nil && right != nil && left.Type() == "variable_name" {
			if cls := phpNewExpressionClass(right, src); cls != "" {
				name := strings.TrimPrefix(strings.TrimSpace(left.Content(src)), "$")
				if prev, ok := vars[name]; ok && prev != cls {
					delete(vars, name)
				} else {
					vars[name] = cls
				}
			}
		}
	}
	for i, n := 0, int(node.NamedChildCount()); i < n; i++ {
		collectPHPFileScopeTypes(node.NamedChild(i), src, vars)
	}
}

// extractAnonymousClass mints a type node for `new class (...) extends B
// implements I { ... }` and extracts its members. Without a node the class's
// methods are not symbols at all, so calls into them resolve nowhere and calls
// out of them are attributed to whatever function happened to enclose the
// expression.
//
// The name is line-qualified (`class@anonymous:12`) because PHP allows several
// anonymous classes in one file and the members' `receiver` meta must tell them
// apart for receiver-directed resolution to stay precise.
func (e *PHPExtractor) extractAnonymousClass(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	startLine := int(node.StartPoint().Row) + 1
	name := fmt.Sprintf("class@anonymous:%d", startLine)
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	meta := map[string]any{
		"visibility":  VisibilityPublic,
		"type_flavor": "class",
		"anonymous":   true,
	}
	if parent := extractPhpParentClass(node, src); parent != "" {
		meta["scope_parent"] = parent
	}
	if ifaces := phpTypeClauseNames(node, src, "class_interface_clause"); len(ifaces) > 0 {
		meta["scope_interfaces"] = strings.Join(ifaces, ",")
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "php",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	if body := node.ChildByFieldName("body"); body != nil {
		e.extractPhpMembers(body, src, filePath, fileNode, result, seen, name, id)
	}
}
