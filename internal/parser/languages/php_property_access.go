package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// PHP property and class-constant usage edges.
//
// The extractor recorded calls but never accesses: `$this->handlers`,
// `$obj->name`, `Foo::$registry` and `Foo::VERSION` produced no edge of any
// kind, so find_usages on a PHP property or class constant returned its
// declaration and nothing else — the member looked unused no matter how often
// the code read or wrote it.
//
// Each access emits an EdgeReads (EdgeWrites in assignment position) to
// `unresolved::*.<member>`, which is the shape resolveFieldRef already
// consumes: with a receiver_type it binds to that type's field exactly, and
// without one it falls back through the same locality cascade every other
// language uses.

// emitPHPMemberAccess records one property / static-property / class-constant
// access. Dynamic members (`$obj->$name`, `Foo::$$var`, `{$expr}`) name no
// symbol and are skipped rather than emitted as an unresolvable target.
func (e *PHPExtractor) emitPHPMemberAccess(
	node *sitter.Node, src []byte,
	filePath, callerID string,
	result *parser.ExtractionResult,
	env phpReceiverEnv,
) {
	var member, receiver string
	switch node.Type() {
	case "member_access_expression", "nullsafe_member_access_expression":
		nameNode := node.ChildByFieldName("name")
		if nameNode == nil || nameNode.Type() != "name" {
			return // `$obj->$prop` / `$obj->{$expr}`
		}
		member = nameNode.Content(src)
		receiver = env.phpReceiverTypeFor(node.ChildByFieldName("object"), src)

	case "scoped_property_access_expression":
		nameNode := node.ChildByFieldName("name")
		if nameNode == nil || nameNode.Type() != "variable_name" {
			return // `Foo::$$dynamic`
		}
		member = strings.TrimPrefix(strings.TrimSpace(nameNode.Content(src)), "$")
		receiver = env.phpScopeReceiverType(node.ChildByFieldName("scope"), src)

	case "class_constant_access_expression":
		name := phpClassConstantName(node, src)
		// `Foo::class` is the class-name literal, not a constant member; the
		// type reference it carries is already emitted by
		// emitPHPReferenceForms.
		if name == "" || name == "class" {
			return
		}
		member = name
		receiver = env.phpScopeReceiverType(phpClassConstantScope(node), src)
	}

	if member == "" {
		return
	}
	kind := graph.EdgeReads
	if phpAccessIsWrite(node) {
		kind = graph.EdgeWrites
	}
	edge := &graph.Edge{
		From: callerID, To: "unresolved::*." + member,
		Kind: kind, FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	}
	if receiver != "" {
		edge.Meta = map[string]any{"receiver_type": receiver}
	}
	result.Edges = append(result.Edges, edge)
}

// phpClassConstantScope returns the scope child of a class_constant_access
// expression. The grammar exposes no field names on this node, so the scope is
// its first named child and the constant name its second.
func phpClassConstantScope(node *sitter.Node) *sitter.Node {
	if node == nil || node.NamedChildCount() == 0 {
		return nil
	}
	return node.NamedChild(0)
}

// phpClassConstantName returns the constant identifier of `Foo::BAR`, or "" for
// `Foo::class` (whose `class` keyword is an anonymous token) and for a dynamic
// constant fetch.
func phpClassConstantName(node *sitter.Node, src []byte) string {
	if node == nil || node.NamedChildCount() < 2 {
		return ""
	}
	name := node.NamedChild(1)
	if name == nil || name.Type() != "name" {
		return ""
	}
	return name.Content(src)
}

// phpAccessIsWrite reports whether an access sits in a position that stores
// into the member: an assignment / augmented-assignment / reference-assignment
// left-hand side, the operand of `++` / `--`, or an argument of `unset()`.
//
// Subscripts are climbed first: `$this->handlers[] = $h` and
// `$this->map['k'] = $v` mutate the property, so they count as writes of
// `handlers` / `map`. Only the container operand is climbed — a member access
// used as the *index* (`$a[$this->key] = 1`) is a read of `key`.
func phpAccessIsWrite(node *sitter.Node) bool {
	for {
		parent := node.Parent()
		if parent == nil || parent.Type() != "subscript_expression" {
			break
		}
		if parent.NamedChildCount() == 0 || !parent.NamedChild(0).Equal(node) {
			return false
		}
		node = parent
	}
	parent := node.Parent()
	if parent == nil {
		return false
	}
	switch parent.Type() {
	case "assignment_expression", "augmented_assignment_expression", "reference_assignment_expression":
		left := parent.ChildByFieldName("left")
		return left != nil && left.Equal(node)
	case "update_expression":
		arg := parent.ChildByFieldName("argument")
		return arg != nil && arg.Equal(node)
	case "unset_statement":
		return true
	}
	return false
}
