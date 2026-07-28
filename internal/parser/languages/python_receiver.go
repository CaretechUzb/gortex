package languages

import (
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// pyAssign is one name→type binding observed while walking a file. The
// line is kept so the binding can be attributed to a scope once function
// ranges are known — the walk sees matches in document order, before any
// function node exists to own them.
type pyAssign struct {
	name     string
	typeName string
	line     int
	// explicit marks a binding that came from a written annotation
	// (`x: Foo`, `def f(x: Foo)`) rather than one inferred from a
	// constructor call (`x = Foo()`). A declaration outranks an
	// inference when the two disagree.
	explicit bool
}

// pyScopedTypeEnv holds one typeEnv per owning function, keyed by the
// function's node ID; the empty key is module scope.
type pyScopedTypeEnv map[string]typeEnv

// buildPyScopedTypeEnv buckets every binding into the scope that owns it
// and drops any name bound to two different types within one scope.
//
// Scoping is a precision fix. With one file-wide map, a `m = Foo()` in
// any function typed every unrelated `m.bar()` in every other function of
// the same file — the receiver_type stamped on those edges was simply
// wrong. The rule applied here is Python's own: a call sees its own
// function's bindings, then module-level bindings, and never another
// function's locals. Nested functions resolve to their outermost
// enclosing function (findEnclosingFunc returns the first, i.e. outermost,
// matching range), so a closure and the function that encloses it share
// one scope and a captured variable still resolves.
//
// The conflict rule is the one the PHP receiver work settled on: a name
// assigned two different classes in the same scope yields NOTHING. A
// flow-insensitive environment cannot say which assignment reaches the
// call, and a wrong receiver_type binds the edge at a high-confidence
// tier — strictly worse than leaving the call untyped.
func buildPyScopedTypeEnv(assigns []pyAssign, funcRanges []funcRange) pyScopedTypeEnv {
	type binding struct {
		typeName string
		explicit bool
		conflict bool
	}
	scoped := map[string]map[string]*binding{}
	for _, a := range assigns {
		if a.name == "" || a.typeName == "" {
			continue
		}
		owner := findEnclosingFunc(funcRanges, a.line)
		byName, ok := scoped[owner]
		if !ok {
			byName = map[string]*binding{}
			scoped[owner] = byName
		}
		cur, ok := byName[a.name]
		if !ok {
			byName[a.name] = &binding{typeName: a.typeName, explicit: a.explicit}
			continue
		}
		if cur.typeName == a.typeName {
			continue
		}
		switch {
		case a.explicit && !cur.explicit:
			// A written annotation replaces an inferred type outright.
			cur.typeName, cur.explicit, cur.conflict = a.typeName, true, false
		case !a.explicit && cur.explicit:
			// Keep the declaration; an inference does not override it.
		default:
			// Two declarations, or two inferences, that disagree.
			cur.conflict = true
		}
	}
	out := pyScopedTypeEnv{}
	for owner, byName := range scoped {
		env := typeEnv{}
		for name, b := range byName {
			if b.conflict {
				continue
			}
			env[name] = b.typeName
		}
		if len(env) > 0 {
			out[owner] = env
		}
	}
	return out
}

// lookup resolves a name for a call owned by ownerID: the owning
// function's own bindings first, then module scope. Another function's
// locals are never consulted.
func (s pyScopedTypeEnv) lookup(ownerID, name string) (string, bool) {
	if env, ok := s[ownerID]; ok {
		if t, ok := env[name]; ok {
			return t, true
		}
	}
	if ownerID != "" {
		if t, ok := s[""][name]; ok {
			return t, true
		}
	}
	return "", false
}

// flatten returns the bindings visible to ownerID as one typeEnv, for
// helpers that take a flat map (resolveChainType).
func (s pyScopedTypeEnv) flatten(ownerID string) typeEnv {
	env := typeEnv{}
	for k, v := range s[""] {
		env[k] = v
	}
	if ownerID != "" {
		for k, v := range s[ownerID] {
			env[k] = v
		}
	}
	return env
}

// pyAppendParamAssign records an annotated parameter as an explicit
// binding for its enclosing function. The parameter's own line is used
// so the binding lands in the function that declares it.
//
// Variadic parameters never reach here: `*args: int` and `**kw: int` wrap
// their name in a list_splat_pattern / dictionary_splat_pattern, which the
// query's `(identifier)` child does not match. That exclusion is load
// bearing — the value of `*args` is a tuple, not an instance of the
// annotated type, so binding it would be wrong.
func pyAppendParamAssign(assigns *[]pyAssign, nameCap, typeCap *parser.CapturedNode) {
	if nameCap == nil || typeCap == nil {
		return
	}
	typeName := pyReceiverTypeFromAnnotation(typeCap.Text)
	if typeName == "" || nameCap.Text == "" {
		return
	}
	*assigns = append(*assigns, pyAssign{
		name:     nameCap.Text,
		typeName: typeName,
		line:     nameCap.StartLine + 1,
		explicit: true,
	})
}

// pyEnclosingClassName walks up to the nearest enclosing class_definition
// and returns its name, or "" at module scope. This is what makes
// `self.method()` bindable: `self` names the class the call is written
// in, which is known exactly, with no inference involved. It is by far
// the most common typed receiver in idiomatic Python — 26% of all
// attribute calls in a 4,400-file corpus.
func pyEnclosingClassName(n *sitter.Node, src []byte) string {
	for cur := n; cur != nil; cur = cur.Parent() {
		if cur.Type() != "class_definition" {
			continue
		}
		if nameNode := cur.ChildByFieldName("name"); nameNode != nil {
			return nameNode.Content(src)
		}
		return ""
	}
	return ""
}

// pyContainerTypes are annotations whose value is a CONTAINER, not an
// instance of whatever sits inside the brackets. `xs: list[Foo]` must not
// type `xs` as Foo: `xs.append(...)` is a list operation, and binding it
// to Foo would attach the call to an unrelated class. Unwrapping these
// is the difference between a useful hint and a fabricated one.
var pyContainerTypes = map[string]bool{
	"list": true, "List": true, "dict": true, "Dict": true,
	"set": true, "Set": true, "frozenset": true, "FrozenSet": true,
	"tuple": true, "Tuple": true, "Sequence": true, "MutableSequence": true,
	"Mapping": true, "MutableMapping": true, "Iterable": true,
	"Iterator": true, "Generator": true, "AsyncIterable": true,
	"AsyncIterator": true, "AsyncGenerator": true, "Awaitable": true,
	"Coroutine": true, "Callable": true, "Deque": true, "DefaultDict": true,
	"Counter": true, "OrderedDict": true, "ChainMap": true,
	"Type": true, "type": true,
}

// pyNonBindingTypes never name a concrete class a receiver could be.
var pyNonBindingTypes = map[string]bool{
	"": true, "Any": true, "None": true, "object": true, "Self": true,
	"AnyStr": true, "NoReturn": true, "Never": true, "Ellipsis": true,
	"int": true, "float": true, "str": true, "bool": true, "bytes": true,
	"bytearray": true, "complex": true, "memoryview": true, "range": true,
}

// pyReceiverTypeFromAnnotation reduces a type annotation to the class an
// value of that type is an instance of, or "" when no single class can be
// named.
//
// It exists because normalizePyTypeName simply truncates at the first
// bracket, which turns `Optional[Foo]` into the meaningless wrapper name
// `Optional` and `list[Foo]` into `list`. For a receiver hint that is not
// good enough in either direction: the wrapper cases lose a type that is
// perfectly recoverable, and the container cases would hand back the
// element type, which is actively wrong. This unwraps the wrappers that
// preserve the instance type and REFUSES the ones that do not.
func pyReceiverTypeFromAnnotation(t string) string {
	t = strings.TrimSpace(t)
	// Forward references are written as string literals: `x: "Foo"`.
	if len(t) >= 2 && (t[0] == '"' || t[0] == '\'') && t[len(t)-1] == t[0] {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	if t == "" {
		return ""
	}
	// PEP 604 unions: `Foo | None` is a Foo; `Foo | Bar` names no single
	// class. Only a TOP-LEVEL `|` makes this a union — in
	// `Optional[int | str]` the bar is nested inside the brackets, and
	// treating it as a union here would hand the whole string back
	// unchanged and recurse forever.
	if parts := splitPyTopLevel(t, '|'); len(parts) > 1 {
		return pyOnlyNonNone(parts)
	}
	open := strings.IndexByte(t, '[')
	if open < 0 {
		return pyBareReceiverType(t)
	}
	if !strings.HasSuffix(t, "]") {
		return ""
	}
	head := pyUnqualified(strings.TrimSpace(t[:open]))
	args := splitPyTopLevel(t[open+1:len(t)-1], ',')
	switch head {
	case "Optional", "Final", "ClassVar", "Required", "NotRequired", "InitVar":
		if len(args) == 1 {
			return pyReceiverTypeFromAnnotation(args[0])
		}
		return ""
	case "Union":
		return pyOnlyNonNone(args)
	case "Annotated":
		// `Annotated[Foo, metadata...]` — the type is the first argument.
		if len(args) >= 1 {
			return pyReceiverTypeFromAnnotation(args[0])
		}
		return ""
	}
	if pyContainerTypes[head] {
		return ""
	}
	// A generic instance (`Repo[User]`) is still an instance of `Repo`.
	return pyBareReceiverType(head)
}

// pyOnlyNonNone returns the single non-None member of a union, or "" when
// the union holds none or more than one distinct type.
func pyOnlyNonNone(parts []string) string {
	only := ""
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "None" {
			continue
		}
		if only != "" && only != p {
			return ""
		}
		only = p
	}
	if only == "" {
		return ""
	}
	return pyReceiverTypeFromAnnotation(only)
}

// pyBareReceiverType accepts an unsubscripted annotation as a class name.
func pyBareReceiverType(t string) string {
	t = pyUnqualified(strings.TrimSpace(t))
	if pyNonBindingTypes[t] || pyContainerTypes[t] {
		return ""
	}
	// A class name starts upper-case by universal Python convention; the
	// existing constructor-call inference applies the same test. A
	// lower-case annotation is a builtin or a type alias we cannot bind.
	if !unicode.IsUpper(rune(t[0])) {
		return ""
	}
	return t
}

// pyUnqualified strips a module qualifier: `models.Model` → `Model`,
// `typing.Optional` → `Optional`.
func pyUnqualified(t string) string {
	if i := strings.LastIndexByte(t, '.'); i >= 0 {
		return strings.TrimSpace(t[i+1:])
	}
	return t
}

// splitPyTopLevel splits on sep, ignoring separators nested inside
// brackets, parentheses or braces — `Dict[str, int] | None` splits into
// two parts on '|', not three.
func splitPyTopLevel(s string, sep byte) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '(', '{':
			depth++
		case ']', ')', '}':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
