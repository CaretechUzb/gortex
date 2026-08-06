package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// C# property / field / constant usage edges.
//
// The extractor recorded calls but never accesses: `p.Title`,
// `this.Note`, `Plaque.Cap` and `p?.Title` produced no edge of any
// kind, so find_usages on a C# property or field returned its
// declaration and nothing else — the member looked unused no matter how
// often the code read or wrote it. Mirrors the PHP property-access
// emission: each access is an EdgeReads (EdgeWrites in assignment
// position) to `unresolved::*.<member>`, the shape resolveFieldRef
// already consumes.
//
// One rule PHP does not need: dotted namespace qualification parses as
// nested member_access_expression here (`System.Threading.Tasks.Task`),
// so an access with NO receiver evidence emits only at the outermost
// link of its chain — a typed inner link (`p.Title` in `p.Title.Length`)
// still emits, while the namespace-prefix links stay silent.

// csharpDeferredAccess buffers one member-access site until the
// type-env build has run, mirroring csharpDeferredCall — receiver
// typing needs the same tenv the call path uses.
type csharpDeferredAccess struct {
	name        string
	node        *sitter.Node
	line        int
	conditional bool
}

// csharpAccessInCallPosition reports whether the access node is the
// function of an invocation — `p.Chime()` belongs to the call query,
// never to the access emitter.
func csharpAccessInCallPosition(node *sitter.Node) bool {
	parent := node.Parent()
	if parent == nil || parent.Type() != "invocation_expression" {
		return false
	}
	fn := parent.ChildByFieldName("function")
	return fn != nil && fn.Equal(node)
}

// csharpAccessIsInnerLink reports whether the access is the receiver of
// an enclosing member access (`p.Title` inside `p.Title.Length`).
func csharpAccessIsInnerLink(node *sitter.Node) bool {
	parent := node.Parent()
	if parent == nil || parent.Type() != "member_access_expression" {
		return false
	}
	expr := parent.ChildByFieldName("expression")
	return expr != nil && expr.Equal(node)
}

// csharpAccessIsWrite reports whether an access sits in a position that
// stores into the member: an assignment (simple or compound)
// left-hand side, or the operand of `++` / `--`. Element accesses are
// climbed first: `p.Items[0] = v` mutates Items, so it counts as a
// write of Items; an access used as the index is a read.
func csharpAccessIsWrite(node *sitter.Node) bool {
	for {
		parent := node.Parent()
		if parent == nil || parent.Type() != "element_access_expression" {
			break
		}
		expr := parent.ChildByFieldName("expression")
		if expr == nil || !expr.Equal(node) {
			return false
		}
		node = parent
	}
	parent := node.Parent()
	if parent == nil {
		return false
	}
	switch parent.Type() {
	case "assignment_expression":
		left := parent.ChildByFieldName("left")
		return left != nil && left.Equal(node)
	case "prefix_unary_expression", "postfix_unary_expression":
		op := parent.Child(0)
		if parent.Type() == "postfix_unary_expression" {
			op = parent.Child(int(parent.ChildCount()) - 1)
		}
		if op == nil {
			return false
		}
		t := op.Type()
		return t == "++" || t == "--"
	}
	return false
}

// csharpAccessKeyword returns "this"/"base" when the access is
// qualified by one of the anonymous keyword tokens (which no named
// field capture ever sees — the same grammar wrinkle the call patterns
// handle with literal-token alternations), "" otherwise.
func csharpAccessKeyword(node *sitter.Node) string {
	if node == nil || node.ChildCount() == 0 {
		return ""
	}
	switch node.Child(0).Type() {
	case "this":
		return "this"
	case "base":
		return "base"
	}
	return ""
}

// csharpAccessReceiverType resolves the receiver type evidence for one
// access, through the same ladder the deferred-call path uses: keyword
// qualifiers from the enclosing declaration, identifiers from the
// method's tenv (builtins stay unstamped, matching call policy), bare
// known types for static accesses, and chains through resolveChainType.
func csharpAccessReceiverType(a csharpDeferredAccess, src []byte, owner string,
	tenv typeEnv, builtins map[string]string, result *parser.ExtractionResult) string {
	if kw := csharpAccessKeyword(a.node); kw != "" {
		return csharpQualifiedReceiverType(a.node, src, kw)
	}
	var recv *sitter.Node
	if a.conditional {
		recv = a.node.ChildByFieldName("condition")
	} else {
		recv = a.node.ChildByFieldName("expression")
	}
	if recv == nil {
		return ""
	}
	if recv.Type() == "identifier" {
		name := recv.Content(src)
		if builtins[name] != "" {
			return ""
		}
		if t, ok := tenv[name]; ok {
			return t
		}
		if isKnownType(name, result) {
			return name
		}
		return ""
	}
	text := strings.TrimSpace(recv.Content(src))
	if text == "" {
		return ""
	}
	return resolveChainType(text, tenv, result)
}

// emitCSharpMemberAccesses turns the buffered access sites into
// EdgeReads/EdgeWrites once the type environments exist.
func emitCSharpMemberAccesses(accesses []csharpDeferredAccess, src []byte,
	filePath string, funcRanges *csharpFuncLookup,
	tenvByOwner map[string]typeEnv, builtinsByOwner map[string]map[string]string,
	result *parser.ExtractionResult) {
	for _, a := range accesses {
		if a.node == nil || csharpAccessInCallPosition(a.node) {
			continue
		}
		callerID := funcRanges.enclosing(a.line)
		if callerID == "" {
			continue
		}
		recvType := csharpAccessReceiverType(a, src, callerID,
			tenvByOwner[callerID], builtinsByOwner[callerID], result)
		if recvType == "" && !a.conditional && csharpAccessIsInnerLink(a.node) {
			// Evidence-less inner link — almost always a namespace or
			// static-qualification prefix; emitting it would shower the
			// graph with reads of `Threading`/`Tasks`.
			continue
		}
		kind := graph.EdgeReads
		if !a.conditional && csharpAccessIsWrite(a.node) {
			kind = graph.EdgeWrites
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + a.name,
			Kind: kind, FilePath: filePath, Line: a.line,
		}
		if recvType != "" {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}
}
