package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitRustFunctionShape emits KindParam / EdgeParamOf / EdgeTypedAs /
// EdgeReturns / KindGenericParam for a Rust function_item or
// function_signature_item.
func emitRustFunctionShape(ownerID string, funcNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if funcNode == nil {
		return
	}
	if params := funcNode.ChildByFieldName("parameters"); params != nil {
		emitRustParamNodes(ownerID, params, src, filePath, declLine, result)
	}
	if rt := rustReturnTypeRaw(funcNode, src); rt != "" {
		emitRustReturnEdges(ownerID, rt, filePath, declLine, result)
	}
	emitRustGenericParamNodes(ownerID, funcNode, src, filePath, declLine, result)
	if body := funcNode.ChildByFieldName("body"); body != nil {
		emitRustAsyncSpawns(ownerID, body, src, filePath, result)
	}
}

// emitRustAsyncSpawns walks a Rust function body and emits
// EdgeSpawns for every `.await` postfix call and every tokio::spawn /
// tokio::task::spawn / tokio::spawn_blocking / async_std::task::spawn
// call. Mode is "async" for .await, "tokio" / "task" for the spawn
// helpers.
func emitRustAsyncSpawns(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	seen := map[string]bool{}
	emit := func(target, mode string, line int) {
		if target == "" {
			return
		}
		key := mode + "\x00" + target
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + target,
			Kind:     graph.EdgeSpawns,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"mode": mode,
			},
		})
	}
	walkRustNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_item", "closure_expression":
			// Don't descend into nested function or closure
			// bodies — their awaits belong to themselves.
			return false
		case "await_expression":
			// `expr.await` — the awaited value is the operand.
			// If it's a call_expression we extract the callee
			// name; otherwise we still emit a spawn edge to
			// "<unknown>" so the count is preserved.
			line := int(n.StartPoint().Row) + 1
			operand := n.NamedChild(0)
			if operand != nil && operand.Type() == "call_expression" {
				if name := rustCallTargetName(operand, src); name != "" {
					emit(name, "async", line)
				}
			} else if operand != nil {
				// `foo.await` — pick last segment of expression.
				if name := rustExprTailName(operand, src); name != "" {
					emit(name, "async", line)
				}
			}
		case "call_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil {
				return true
			}
			if fn.Type() == "scoped_identifier" {
				path := strings.TrimSpace(fn.Content(src))
				switch path {
				case "tokio::spawn", "tokio::task::spawn":
					emit(path, "tokio", int(n.StartPoint().Row)+1)
				case "tokio::spawn_blocking", "tokio::task::spawn_blocking":
					emit(path, "blocking", int(n.StartPoint().Row)+1)
				case "async_std::task::spawn", "smol::spawn":
					emit(path, "task", int(n.StartPoint().Row)+1)
				}
			}
		}
		return true
	})
}

func walkRustNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		walkRustNodes(n.NamedChild(i), visit)
	}
}

func rustCallTargetName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "field_expression":
		if f := fn.ChildByFieldName("field"); f != nil {
			return f.Content(src)
		}
	case "scoped_identifier":
		// crate::foo::Bar() — pick last segment.
		if name := fn.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	}
	return ""
}

func rustExprTailName(expr *sitter.Node, src []byte) string {
	switch expr.Type() {
	case "identifier":
		return expr.Content(src)
	case "field_expression":
		if f := expr.ChildByFieldName("field"); f != nil {
			return f.Content(src)
		}
	}
	return ""
}

func emitRustParamNodes(ownerID string, params *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	pos := 0
	for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
		decl := params.NamedChild(i)
		if decl == nil {
			continue
		}
		switch decl.Type() {
		case "self_parameter":
			// Receiver — not a free param. Skip.
			continue
		case "parameter":
			// (parameter pattern: (identifier) type: (...)) plus
			// optional `mut` modifier on the pattern.
		default:
			continue
		}
		name := rustParamName(decl, src)
		if name == "" || name == "_" {
			continue
		}
		typeRaw := ""
		if t := decl.ChildByFieldName("type"); t != nil {
			typeRaw = strings.TrimSpace(t.Content(src))
		}
		paramID := ownerID + "#param:" + name + "@" + strconv.Itoa(pos)
		meta := map[string]any{"position": pos}
		if typeRaw != "" {
			meta["type"] = typeRaw
		}
		startLine := int(decl.StartPoint().Row) + 1
		if startLine == 0 {
			startLine = declLine
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        paramID,
			Kind:      graph.KindParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   int(decl.EndPoint().Row) + 1,
			Language:  "rust",
			Meta:      meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     paramID,
			To:       ownerID,
			Kind:     graph.EdgeParamOf,
			FilePath: filePath,
			Line:     startLine,
			Origin:   graph.OriginASTResolved,
		})
		for _, canon := range rustCanonicalTypeRefs(typeRaw) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     paramID,
				To:       "unresolved::" + canon,
				Kind:     graph.EdgeTypedAs,
				FilePath: filePath,
				Line:     startLine,
				Origin:   graph.OriginASTInferred,
			})
		}
		pos++
	}
}

func rustParamName(decl *sitter.Node, src []byte) string {
	pat := decl.ChildByFieldName("pattern")
	if pat == nil {
		return ""
	}
	switch pat.Type() {
	case "identifier":
		return pat.Content(src)
	case "mutable_specifier":
		// Older grammar shapes; rare.
	}
	// Walk for identifier child (mut_pattern (identifier)).
	for i, _nc := 0, int(pat.NamedChildCount()); i < _nc; i++ {
		c := pat.NamedChild(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return strings.TrimSpace(pat.Content(src))
}

func rustReturnTypeRaw(funcNode *sitter.Node, src []byte) string {
	if rt := funcNode.ChildByFieldName("return_type"); rt != nil {
		return strings.TrimSpace(rt.Content(src))
	}
	return ""
}

func emitRustReturnEdges(ownerID, returnText, filePath string, line int, result *parser.ExtractionResult) {
	if returnText == "" {
		return
	}
	// A tuple return names one type per slot, so each gets its own edge
	// at its own position.
	for i, t := range rustCanonicalTypeRefs(returnText) {
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + t,
			Kind:     graph.EdgeReturns,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"position": i,
			},
		})
	}
}

// emitRustTypeUseEdges emits an EdgeTypedAs from ownerID to the named
// type referenced by a `let x: Type = ...` binding annotation. typeText
// is the verbatim annotation source; it's canonicalized to its bare
// named form (references / generics / wrappers like Box<T>, Vec<T>,
// Option<T>, Arc<T>, Rc<T>, Result<T, _> stripped to the inner named
// type via canonicalizeRustTypeRef) and primitives are skipped, so the
// edge only fires for workspace-resolvable types. Mirrors the param /
// return function-shape emission so a type used only in a local binding
// is still visible to find_usages without a language server.
func emitRustTypeUseEdges(ownerID, typeText, filePath string, line int, result *parser.ExtractionResult) {
	if ownerID == "" || typeText == "" {
		return
	}
	for _, canon := range rustCanonicalTypeRefs(typeText) {
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + canon,
			Kind:     graph.EdgeTypedAs,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
		})
	}
}

func emitRustGenericParamNodes(ownerID string, funcNode *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	tparams := rustTypeParams(funcNode, src)
	if len(tparams) == 0 {
		return
	}
	for _, tp := range tparams {
		name := tp["name"]
		if name == "" {
			continue
		}
		gpID := ownerID + "#tparam:" + name
		meta := map[string]any{}
		if b := tp["bound"]; b != "" {
			meta["bound"] = b
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        gpID,
			Kind:      graph.KindGenericParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "rust",
			Meta:      meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     gpID,
			To:       ownerID,
			Kind:     graph.EdgeMemberOf,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		})
	}
}

// canonicalizeRustTypeRef strips wrappers Rust idiomatic typing has
// so the resolver lands on the actual struct/enum/trait node:
// Result<X, _> → X, Option<X> → X, Box<X> → X, Vec<X> → X, &X → X,
// &mut X → X.
func canonicalizeRustTypeRef(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Strip leading & / &mut / &'a / &'static.
	for {
		if strings.HasPrefix(t, "&") {
			t = strings.TrimSpace(t[1:])
			continue
		}
		if strings.HasPrefix(t, "mut ") {
			t = strings.TrimSpace(t[4:])
			continue
		}
		// '_ lifetime or 'a lifetime annotation.
		if strings.HasPrefix(t, "'") {
			if idx := strings.IndexAny(t, " \t"); idx > 0 {
				t = strings.TrimSpace(t[idx:])
				continue
			}
		}
		break
	}
	// Strip wrappers.
	for _, wrapper := range []string{"Box", "Arc", "Rc", "Vec", "Option", "Pin", "RefCell", "Cell", "Mutex", "RwLock"} {
		prefix := wrapper + "<"
		if strings.HasPrefix(t, prefix) && strings.HasSuffix(t, ">") {
			inner := t[len(prefix) : len(t)-1]
			return canonicalizeRustTypeRef(inner)
		}
	}
	// Result<X, _> — pick the success type only.
	if strings.HasPrefix(t, "Result<") && strings.HasSuffix(t, ">") {
		inner := t[len("Result<") : len(t)-1]
		// Split at top-level comma, take first.
		depth := 0
		for i := 0; i < len(inner); i++ {
			c := inner[i]
			switch c {
			case '<', '(', '[':
				depth++
			case '>', ')', ']':
				depth--
			case ',':
				if depth == 0 {
					return canonicalizeRustTypeRef(strings.TrimSpace(inner[:i]))
				}
			}
		}
		return canonicalizeRustTypeRef(inner)
	}
	// impl Trait — pick the trait name.
	if strings.HasPrefix(t, "impl ") {
		t = strings.TrimSpace(t[5:])
		if idx := strings.Index(t, "+"); idx > 0 {
			t = strings.TrimSpace(t[:idx])
		}
		return canonicalizeRustTypeRef(t)
	}
	// dyn Trait
	if strings.HasPrefix(t, "dyn ") {
		t = strings.TrimSpace(t[4:])
		return canonicalizeRustTypeRef(t)
	}
	// Raw pointers: *const T / *mut T.
	if strings.HasPrefix(t, "*") {
		t = strings.TrimSpace(t[1:])
		t = strings.TrimPrefix(t, "const ")
		t = strings.TrimPrefix(t, "mut ")
		return canonicalizeRustTypeRef(strings.TrimSpace(t))
	}
	// Slices and arrays: [T] and [T; N] both carry T.
	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		inner := strings.TrimSpace(t[1 : len(t)-1])
		if parts := rustSplitTopLevel(inner, ';'); len(parts) > 0 {
			inner = parts[0]
		}
		return canonicalizeRustTypeRef(inner)
	}
	// Strip generic <...> tail.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Strip parens.
	for strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	// A trait-object or bound list keeps only its first trait:
	// `Error + Send + 'static` names Error.
	if idx := strings.Index(t, "+"); idx > 0 {
		t = strings.TrimSpace(t[:idx])
	}
	// Strip module path: crate::foo::Bar → Bar.
	if idx := strings.LastIndex(t, "::"); idx >= 0 {
		t = t[idx+2:]
	}
	t = strings.TrimSpace(t)
	// Anything still carrying punctuation is not a type name. Emitting
	// it anyway produced targets like "unresolved::u8, u16" and
	// "unresolved::[Item]" — edges that can never resolve and that
	// pollute every unresolved-reference report.
	if !rustSimpleIdentifier(t) {
		return ""
	}
	return t
}

// rustCanonicalTypeRefs expands a type expression into every named type
// it mentions at the top level. A tuple names one type per element, so
// `(Config, Sender)` must not be collapsed to a single garbage target
// the way stripping its parens used to.
func rustCanonicalTypeRefs(t string) []string {
	t = strings.TrimSpace(t)
	// Only a genuine tuple — parens wrapping a top-level comma list.
	if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		inner := strings.TrimSpace(t[1 : len(t)-1])
		if parts := rustSplitTopLevel(inner, ','); len(parts) > 1 {
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				if canon := canonicalizeRustTypeRef(p); canon != "" && !isRustPrimitive(canon) {
					out = append(out, canon)
				}
			}
			return out
		}
	}
	if canon := canonicalizeRustTypeRef(t); canon != "" && !isRustPrimitive(canon) {
		return []string{canon}
	}
	return nil
}

func isRustPrimitive(t string) bool {
	switch t {
	case "", "()", "!",
		"bool", "char", "str", "String",
		"i8", "i16", "i32", "i64", "i128", "isize",
		"u8", "u16", "u32", "u64", "u128", "usize",
		"f32", "f64",
		"Self":
		return true
	}
	return false
}
