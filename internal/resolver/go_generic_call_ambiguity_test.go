package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// Go's grammar cannot distinguish an explicitly instantiated generic call
// from two other constructs that share its spelling:
//
//	Zero[int]()     vs  handlers[key]()   — both an index_expression
//	OneArg[int](1)  vs  MyList[int](x)    — both a type_conversion_expression
//
// The extractor widened its patterns to catch the call forms, so it now also
// emits an unresolved stub for the other two. That is only acceptable if the
// stub cannot BIND to something wrong. It cannot, and the reason is a Go rule
// rather than luck: a package-scope name is a func, a var or a type — never
// two of them — so a stub named after a variable or a type finds no function
// candidate and stays unresolved. An unresolved target is a placeholder that
// reverse walks ignore, so nothing downstream sees a phantom call.
func TestResolveGo_IndexAndConversionDoNotBindWrongly(t *testing.T) {
	g := buildMultiLangGraph(t, map[string]string{
		"app/handlers.go": `package app

type MyList[T any] struct{}

var handlers = map[string]func(){}

func Dispatch(key string) {
	handlers[key]()
}

func Convert(xs []int) {
	_ = MyList[int](xs)
}
`,
	})
	New(g).ResolveAll()

	for _, caller := range []string{"app/handlers.go::Dispatch", "app/handlers.go::Convert"} {
		for _, e := range g.GetOutEdges(caller) {
			if e.Kind != graph.EdgeCalls || graph.IsUnresolvedTarget(e.To) {
				continue
			}
			n := g.GetNode(e.To)
			if n == nil {
				continue
			}
			assert.Contains(t, []graph.NodeKind{graph.KindFunction, graph.KindMethod}, n.Kind,
				"%s bound a call to a %s node (%s) — indexing a func-valued map and "+
					"converting to a generic type must never resolve as calls",
				caller, n.Kind, e.To)
		}
	}
}

// The other half of the same trade-off: a real generic call still resolves.
func TestResolveGo_GenericCallResolvesAcrossFiles(t *testing.T) {
	g := buildMultiLangGraph(t, map[string]string{
		"app/lib.go": `package app

func Zero[T any]() T { var z T; return z }
func OneArg[T any](a T) T { return a }
`,
		"app/use.go": `package app

func Run() {
	_ = Zero[int]()
	_ = OneArg[int](1)
}
`,
	})
	New(g).ResolveAll()

	var targets []string
	for _, e := range g.GetOutEdges("app/use.go::Run") {
		if e.Kind == graph.EdgeCalls {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "app/lib.go::Zero",
		"a generic call with no value argument resolves to its declaration")
	assert.Contains(t, targets, "app/lib.go::OneArg",
		"a generic call with exactly one value argument resolves to its declaration")
}

// The package-scope argument above covers a var and a func sharing a
// name. It does NOT cover two cases that are ordinary Go:
//
//   - a func-valued STRUCT FIELD, which lives in the type's namespace and
//     may freely share a name with a method on some other type;
//   - a LOCAL variable shadowing a package-scope function name.
//
// Both spell an index-then-call exactly like a generic instantiation, and
// before the instantiation marker existed the ordinary name tiers bound
// them: `t.handlers[k]()` landed on an unrelated `Other.handlers`, and a
// shadowed `Handler[k]()` landed on the package's `Handler` at the
// ast_resolved tier. Requiring a real type-parameter list on the target
// is what separates the instantiation from both impostors.
func TestResolveGo_IndexedFieldAndLocalShadowDoNotBind(t *testing.T) {
	g := buildMultiLangGraph(t, map[string]string{
		"app/a.go": `package app

type Other struct{}

func (o Other) handlers() {}

func Handler() {}

type T struct {
	handlers map[string]func()
}

func (t T) Dispatch(k string) {
	t.handlers[k]()
}

func LocalShadow(k string) {
	Handler := map[string]func(){}
	Handler[k]()
}
`,
	})
	New(g).ResolveAll()

	for _, caller := range []string{"app/a.go::T.Dispatch", "app/a.go::LocalShadow"} {
		for _, e := range g.GetOutEdges(caller) {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			assert.True(t, graph.IsUnresolvedTarget(e.To),
				"%s: indexing a func value is not a generic call, so it must not bind (got %s)",
				caller, e.To)
		}
	}
}
