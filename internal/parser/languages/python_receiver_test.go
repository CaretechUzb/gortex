package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// pyCallReceiverTypes maps `unresolved::*.<method>` → receiver_type for
// every method-call edge that carries one.
func pyCallReceiverTypes(edges []*graph.Edge) map[string]string {
	out := map[string]string{}
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls || e.Meta == nil {
			continue
		}
		rt, ok := e.Meta["receiver_type"].(string)
		if !ok || rt == "" {
			continue
		}
		out[e.To] = rt
	}
	return out
}

func TestPyReceiver_SelfAndClsBindToEnclosingClass(t *testing.T) {
	src := []byte(`class Console:
    def render(self):
        pass

    def show(self):
        self.render()

    @classmethod
    def build(cls):
        cls.render()


def free_function(self):
    # self outside a class names nothing bindable.
    self.render()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("console.py", src)
	require.NoError(t, err)

	recv := pyCallReceiverTypes(result.Edges)
	assert.Equal(t, "Console", recv["unresolved::*.render"],
		"self./cls. calls must bind to the class they are written in")

	// The module-level `self.render()` has no enclosing class, so it must
	// not invent one — but it must still produce a call edge.
	var renderCalls int
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::*.render" {
			renderCalls++
		}
	}
	assert.Equal(t, 3, renderCalls, "every self/cls call site emits an edge")
}

func TestPyReceiver_AnnotatedParametersBind(t *testing.T) {
	src := []byte(`from typing import Optional


class Widget:
    pass


def draw(target: Widget, fallback: Optional[Widget], maybe: Widget | None, quoted: "Widget", n: int = 0):
    target.paint()
    fallback.paint()
    maybe.paint()
    quoted.paint()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("draw.py", src)
	require.NoError(t, err)

	assert.Equal(t, "Widget", pyCallReceiverTypes(result.Edges)["unresolved::*.paint"],
		"annotated parameters, Optional[...], PEP 604 `| None` and quoted forward refs all name Widget")
}

func TestPyReceiver_ContainerAnnotationsAreNotBound(t *testing.T) {
	// The element type of a container is NOT the type of the variable —
	// binding `xs.append(...)` to Widget would attach the call to an
	// unrelated class.
	src := []byte(`class Widget:
    pass


def collect(xs: list[Widget], m: dict[str, Widget], cb: Callable[[Widget], None], both: Widget | Console):
    xs.append(1)
    m.update({})
    cb.call()
    both.paint()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("collect.py", src)
	require.NoError(t, err)

	recv := pyCallReceiverTypes(result.Edges)
	for _, target := range []string{
		"unresolved::*.append", "unresolved::*.update",
		"unresolved::*.call", "unresolved::*.paint",
	} {
		assert.Empty(t, recv[target],
			"container elements and ambiguous unions must not be stamped as a receiver type (%s)", target)
	}
}

func TestPyReceiver_BindingsDoNotLeakAcrossFunctions(t *testing.T) {
	src := []byte(`class Console:
    pass


def owns_it():
    m = Console()
    m.render()


def borrows_the_name():
    m.render()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("scope.py", src)
	require.NoError(t, err)

	var typed, untyped int
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeCalls || ed.To != "unresolved::*.render" {
			continue
		}
		if ed.Meta != nil && ed.Meta["receiver_type"] == "Console" {
			typed++
		} else {
			untyped++
		}
	}
	assert.Equal(t, 1, typed, "only the function that assigned `m` gets the type")
	assert.Equal(t, 1, untyped, "another function's local must not type this call")
}

func TestPyReceiver_ModuleScopeIsVisibleInsideFunctions(t *testing.T) {
	src := []byte(`class Console:
    pass


console = Console()


def render_it():
    console.render()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("modscope.py", src)
	require.NoError(t, err)

	assert.Equal(t, "Console", pyCallReceiverTypes(result.Edges)["unresolved::*.render"],
		"module-level bindings are globals and stay visible inside functions")
}

func TestPyReceiver_ConflictingAssignmentsYieldNoType(t *testing.T) {
	// A flow-insensitive environment cannot say which assignment reaches
	// the call, and a wrong receiver_type binds at a high-confidence tier.
	src := []byte(`class Console:
    pass


class Widget:
    pass


def ambiguous(flag):
    thing = Console()
    if flag:
        thing = Widget()
    thing.render()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("conflict.py", src)
	require.NoError(t, err)

	assert.Empty(t, pyCallReceiverTypes(result.Edges)["unresolved::*.render"],
		"a name assigned two different classes in one scope must yield no receiver type")
}

func TestPyReceiver_AnnotationOutranksConstructorInference(t *testing.T) {
	src := []byte(`class Console:
    pass


class Widget:
    pass


def declared(thing: Widget):
    thing = Console()
    thing.render()
`)
	e := NewPythonExtractor()
	result, err := e.Extract("declared.py", src)
	require.NoError(t, err)

	assert.Equal(t, "Widget", pyCallReceiverTypes(result.Edges)["unresolved::*.render"],
		"a written annotation is a declaration and outranks an inferred constructor type")
}

func TestPyReceiverTypeFromAnnotation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Widget", "Widget"},
		{"models.Model", "Model"},
		{"Optional[Widget]", "Widget"},
		{"typing.Optional[Widget]", "Widget"},
		{"Union[Widget, None]", "Widget"},
		{"Widget | None", "Widget"},
		{"None | Widget", "Widget"},
		{"Annotated[Widget, Field(...)]", "Widget"},
		{"Final[Widget]", "Widget"},
		{`"Widget"`, "Widget"},
		{"'Widget'", "Widget"},
		{"Repo[User]", "Repo"},
		// Nested `|` inside brackets is not a top-level union — this shape
		// once recursed until the stack overflowed.
		{"Optional[int | str]", ""},
		{"Widget | Console", ""},
		{"list[Widget]", ""},
		{"dict[str, Widget]", ""},
		{"Sequence[Widget]", ""},
		{"Callable[[Widget], None]", ""},
		{"Type[Widget]", ""},
		{"int", ""},
		{"str", ""},
		{"Any", ""},
		{"None", ""},
		{"object", ""},
		{"self", ""},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, pyReceiverTypeFromAnnotation(c.in), "annotation %q", c.in)
	}
}

func TestPyReceiver_VariadicParamsAreNotBound(t *testing.T) {
	// `*args: Widget` makes args a TUPLE of Widget, not a Widget.
	src := []byte(`class Widget:
    pass


def spread(*args: Widget, **kwargs: Widget):
    args.count(1)
    kwargs.get("x")
`)
	e := NewPythonExtractor()
	result, err := e.Extract("variadic.py", src)
	require.NoError(t, err)

	recv := pyCallReceiverTypes(result.Edges)
	assert.Empty(t, recv["unresolved::*.count"])
	assert.Empty(t, recv["unresolved::*.get"])
}
