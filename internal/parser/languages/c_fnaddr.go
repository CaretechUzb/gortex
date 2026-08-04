package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// cFnAddressMaxPerFile fuses the per-file candidate count. A generated parser
// (a tree-sitter lexer, a bytecode dispatch table) presents tens of thousands
// of distinct value-position identifiers; unbounded, each becomes a
// speculative placeholder edge and the set explodes. A hand-written dispatch
// table sits far below this cap, so the fuse trips only on machine-generated
// input. Candidates are taken in walk order, so the cut is deterministic.
const cFnAddressMaxPerFile = 1024

// C cross-file function-address references.
//
// A function used as a *value* rather than called — a command-table macro
// argument (`{MAKE_CMD("get", ..., getCommand, ...)}`), a function-pointer
// comparison (`c->cmd->proc != execCommand`), a function-pointer assignment, an
// aggregate initializer element, or `&fn` — is a genuine reference the static
// call graph misses. C has a flat extern namespace, so the referenced function
// almost always lives in another translation unit; captureFnValueCandidates
// pre-filters to same-file functions and therefore drops these entirely.
//
// This pass captures the bare identifier in those value positions when the name
// is NOT declared in the current file (so it can only bind cross-module) and is
// not a parameter / local. It emits the same fn-value candidate the resolver
// gate already understands, marked ungated so ResolveFnValueCallbacks binds it
// to a uniquely-named, non-file-local function anywhere in the repo. Unlike the
// shared pass it attributes a file-scope reference (a command table lives
// outside any function) to the file node, so a table entry becomes a usage.
//
// Flood control matters most on generated C (a tree-sitter lexer, a bytecode
// dispatch table), where naive capture explodes. Bounds keep the pass to the
// small set of free identifiers a gate lookup can turn into a real cross-TU
// function edge:
//
//   - value-position-only, and never a name the file declares (functions go to
//     the gated pass; variables / constants / types resolve locally) or a
//     parameter / local / uninitialised declaration / in-file enum member;
//   - one candidate per (enclosing symbol or file, name): a function address
//     binds by name repo-wide, so the same free identifier on N lines of a
//     generated ==/!= lexer is one reference, not N;
//   - a per-file fuse (cFnAddressMaxPerFile) bounds a pathological generated
//     file; a hand-written dispatch table sits far below it.
func captureCFnAddressRefs(result *parser.ExtractionResult, root *sitter.Node, filePath, fileID string, src []byte) {
	if root == nil || result == nil {
		return
	}
	sameFileFunc := map[string]bool{}
	localDecl := map[string]bool{}
	for _, n := range result.Nodes {
		if n == nil || n.FilePath != filePath {
			continue
		}
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod:
			sameFileFunc[n.Name] = true
		case graph.KindVariable, graph.KindConstant, graph.KindType:
			localDecl[n.Name] = true
		}
	}
	var shadowed map[string]bool
	root.WithScratch(func() { shadowed = cCollectLocalNames(root, src) })

	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	var cands []FnValueCandidate
	root.WithScratch(func() {
		walkNodes(root, func(n *sitter.Node) {
			if len(cands) >= cFnAddressMaxPerFile {
				return // per-file fuse: bound a pathological generated file
			}
			if n.Type() != "identifier" {
				return
			}
			form, ok := cFnAddressPosition(n)
			if !ok {
				return
			}
			name := n.Content(src)
			if name == "" || isCFnAddressNonTarget(name) || isCValueRefNoise(name) {
				return
			}
			// A name the file itself declares (function / variable / constant /
			// type) or that is a parameter / local can never be a cross-TU
			// function address — resolve it locally or not at all.
			if sameFileFunc[name] || localDecl[name] || shadowed[name] {
				return
			}
			// Defensive callee guard: positions above exclude the callee, but a
			// macro-call argument that is itself a call (`f(g())`) must not treat
			// the inner callee as a value.
			if byteAfterIdentStartsCall(src, int(n.EndByte())) {
				return
			}
			line := int(n.StartPoint().Row) + 1
			from := findEnclosingFunc(funcRanges, line)
			if from == "" {
				from = fileID // file-scope reference (command / dispatch table)
			}
			// One candidate per (enclosing symbol or file, name): a function
			// address binds by name repo-wide, so the same free identifier on N
			// lines of a generated ==/!= lexer collapses to a single reference.
			// First occurrence by walk order wins; mirrors the shared
			// captureFnValueCandidates key shape.
			key := from + "\x00" + name
			if seen[key] {
				return
			}
			seen[key] = true
			cands = append(cands, FnValueCandidate{
				FromID: from, Name: name, FilePath: filePath, Line: line,
				Form: form, Lang: "c", Ungated: true,
			})
		})
	})
	EmitFnValueCandidates(result, cands)
}

// cFnAddressPosition reports whether an identifier node sits in a C value
// position that can hold a function address, and the fn_ref_form to tag ("" for
// a plain value, "address_of" for `&fn`). Positions cover the generated-table
// shape — a macro / call argument (`MAKE_CMD(..., getCommand, ...)`), an
// aggregate initializer element (`{ ..., getCommand }`), a designated
// initializer value (`{ .proc = getCommand }`) — and the in-function
// function-pointer idioms: a `==` / `!=` comparison (`c->cmd->proc !=
// execCommand`), an assignment or declaration-initializer right-hand side
// (`c->proc = execCommand`, `cmdProc p = execCommand`), a return operand, and
// `&fn`.
func cFnAddressPosition(n *sitter.Node) (string, bool) {
	p := n.Parent()
	if p == nil {
		return "", false
	}
	switch p.Type() {
	case "argument_list", "initializer_list", "return_statement":
		return "", true
	case "initializer_pair":
		if isFieldChild(p, "value", n) {
			return "", true
		}
	case "assignment_expression":
		if isFieldChild(p, "right", n) {
			return "", true
		}
	case "init_declarator":
		if isFieldChild(p, "value", n) {
			return "", true
		}
	case "binary_expression":
		// A function pointer is compared for identity, never ordered — only
		// `==` / `!=` operands are candidate function addresses.
		if op := p.ChildByFieldName("operator"); op != nil {
			if t := op.Type(); t == "==" || t == "!=" {
				return "", true
			}
		}
	case "pointer_expression":
		// `&fn` address-of. tree-sitter-c models it as a pointer_expression
		// with a '&' operator; a '*' operator is a dereference, not a value.
		if op := p.ChildByFieldName("operator"); op != nil && op.Type() == "&" {
			return "address_of", true
		}
	}
	return "", false
}

// isFieldChild reports whether n is exactly the `field`-named child of p (by
// byte span), so an identifier is recognised only in the intended slot.
func isFieldChild(p *sitter.Node, field string, n *sitter.Node) bool {
	c := p.ChildByFieldName(field)
	return c != nil && c.StartByte() == n.StartByte() && c.EndByte() == n.EndByte()
}

// cCollectLocalNames gathers the file's parameter, local, uninitialised-
// declaration, and in-file enum-member names — every declared name that can
// never denote a cross-TU function address and so must be dropped from the
// candidate set. It spans four declaration shapes:
//
//   - parameters and initialised locals (`f(int x)`, `Handler h = g`) — the
//     dominant flood source, since a call passes locals / params by value into
//     the very positions this pass scans;
//   - uninitialised declarations (`int32_t lookahead;`) — a generated lexer
//     compares such a local on ==/!= across tens of thousands of lines, and the
//     identifier is only ever that local, never a function address;
//   - in-file enum members (`enum { sym_a, sym_b }`) — a generated parser
//     declares its token / symbol enum in-file, then floods designated-
//     initializer tables with the members, none of which is a function.
//
// A same-file function prototype is deliberately NOT collected: a declaration
// whose declarator is (or wraps) a function_declarator names a function, left
// bindable for the resolver gate, not shadowed here.
func cCollectLocalNames(root *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "parameter_declaration":
			if d := n.ChildByFieldName("declarator"); d != nil {
				if name := cDeclName(d, src); name != "" {
					out[name] = true
				}
			}
		case "init_declarator":
			if name := cDeclName(n.ChildByFieldName("declarator"), src); name != "" {
				out[name] = true
			}
		case "declaration":
			// An uninitialised declaration's declarators sit directly under it
			// (an initialised one is wrapped in init_declarator, handled above).
			// A declaration may carry several (`int a, *b, c[3];`), so every
			// declarator-field child is unwrapped to its variable name.
			for i := 0; i < int(n.ChildCount()); i++ {
				if n.FieldNameForChild(i) != "declarator" {
					continue
				}
				if name := cUninitDeclName(n.Child(i), src); name != "" {
					out[name] = true
				}
			}
		case "enumerator":
			if d := n.ChildByFieldName("name"); d != nil {
				if name := d.Content(src); name != "" {
					out[name] = true
				}
			}
		}
	})
	return out
}

// cUninitDeclName unwraps the pointer / array / parenthesized wrappers of an
// uninitialised declaration's declarator to its variable name. It returns "" for
// a function_declarator — a prototype names a function, which must stay a
// bindable candidate rather than a shadowing local — and for any other
// unexpected shape. Unlike cDeclName it never descends through a
// function_declarator, so `int *foo(int);` (a pointer-returning prototype)
// contributes no name.
func cUninitDeclName(decl *sitter.Node, src []byte) string {
	for decl != nil {
		switch decl.Type() {
		case "identifier", "field_identifier":
			return decl.Content(src)
		case "pointer_declarator", "array_declarator":
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

// isCFnAddressNonTarget reports whether a name is a C literal / keyword /
// builtin constant that can never be a function address, so the capture skips it
// before the candidate is emitted (the resolver gate would drop it anyway).
func isCFnAddressNonTarget(name string) bool {
	switch name {
	case "NULL", "sizeof", "offsetof", "va_arg", "va_start", "va_end",
		"true", "false", "nil", "Nil":
		return true
	}
	return false
}

// isCValueRefNoise reports whether a captured value-position identifier is
// grep-level noise that must never become a cross-file function reference. A
// generated command-table row interleaves the handler with flag macros
// (CMD_WRITE), enum constants (OBJ_STRING), short arity counts, and keywords
// that only reach identifier position through ERROR recovery on a malformed
// fragment. Three cheap shape rules drop them before they flood the resolver
// gate, keeping the pass to the small set of free identifiers that can turn
// into a real cross-TU function edge:
//
//   - shorter than 4 characters — too short to be a distinct handler name and
//     the dominant collision source under the gate's repo-wide unique-name bind;
//   - ALL_CAPS — the macro / enum-constant convention; a handler name is mixed-
//     or lower-case, never all-uppercase;
//   - a C keyword — never a value, only reachable in this position via a
//     recovered parse.
//
// A real handler (getCommand, strlenCommand, pingCommand) is mixed-case and
// well over four characters, so none of the rules touch it.
func isCValueRefNoise(name string) bool {
	if len(name) < 4 {
		return true
	}
	if isAllCapsToken(name) {
		return true
	}
	return cValueRefKeyword[name]
}

// isAllCapsToken reports whether name follows the ALL_CAPS macro / constant
// convention: it contains at least one letter and no lowercase letter. A
// mixed-case handler (getCommand) has a lowercase letter and is never treated
// as a macro; a digits-and-underscores-only token (never a valid identifier)
// is excluded by the letter requirement.
func isAllCapsToken(name string) bool {
	hasLetter := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
			return false
		case c >= 'A' && c <= 'Z':
			hasLetter = true
		}
	}
	return hasLetter
}

// cValueRefKeyword is the set of C keywords long enough to survive the
// length rule that could reach an identifier value position only through ERROR
// recovery on a malformed generated fragment.
var cValueRefKeyword = map[string]bool{
	"auto": true, "break": true, "case": true, "char": true, "const": true,
	"continue": true, "default": true, "double": true, "else": true,
	"enum": true, "extern": true, "float": true, "goto": true, "long": true,
	"register": true, "restrict": true, "return": true, "short": true,
	"signed": true, "sizeof": true, "static": true, "struct": true,
	"switch": true, "typedef": true, "union": true, "unsigned": true,
	"void": true, "volatile": true, "while": true, "inline": true,
	"_Bool": true, "_Complex": true,
}
