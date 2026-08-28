package languages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestJuliaExtractor_Module(t *testing.T) {
	src := []byte(`module Geometry

using LinearAlgebra
import Statistics

struct Circle
    radius::Float64
end

function area(c::Circle)
    pi * c.radius^2
end

diameter(c::Circle) = 2 * c.radius

end # module
`)
	e := NewJuliaExtractor()
	require.Equal(t, "julia", e.Language())

	res, err := e.Extract("geom.jl", src)
	require.NoError(t, err)

	var gotModule, gotStruct, gotArea, gotDiameter bool
	for _, n := range res.Nodes {
		switch n.Name {
		case "Geometry":
			gotModule = true
		case "Circle":
			gotStruct = true
		case "area":
			gotArea = true
		case "diameter":
			gotDiameter = true
		}
	}
	var gotUsing, gotImport bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::LinearAlgebra" {
			gotUsing = true
		}
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::Statistics" {
			gotImport = true
		}
	}
	assert.True(t, gotModule)
	assert.True(t, gotStruct)
	assert.True(t, gotArea)
	assert.True(t, gotDiameter)
	assert.True(t, gotUsing)
	assert.True(t, gotImport)
}

func TestJuliaExtractor_Include(t *testing.T) {
	src := []byte(`include("helpers.jl")
`)
	res, err := NewJuliaExtractor().Extract("m.jl", src)
	require.NoError(t, err)

	var got bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::helpers.jl" {
			got = true
		}
	}
	assert.True(t, got)
}

func TestJuliaExtractor_EmptyInput(t *testing.T) {
	res, err := NewJuliaExtractor().Extract("e.jl", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

// Definitions the regex extractor missed: where-parametrised
// signatures, unicode / bang names, operator methods.
func TestJuliaExtractor_DefinitionForms(t *testing.T) {
	src := []byte(`function typed(a::Int, b::T) where T <: Number
    return a + b
end

typedshort(x::Vector{T}) where T = sum(x)

θ(σ̂) = σ̂ + 1

foo!(x) = push!(x, 1)

function Base.:+(a::Point, b::Point)
    return a
end

macro mymacro(ex)
    esc(ex)
end
`)
	res, err := NewJuliaExtractor().Extract("forms.jl", src)
	require.NoError(t, err)

	kinds := map[string]graph.NodeKind{}
	for _, n := range res.Nodes {
		if n.ID == "forms.jl::"+n.Name || n.ID == "forms.jl::Base.+" {
			kinds[n.Name] = n.Kind
		}
	}
	assert.Equal(t, graph.KindFunction, kinds["typed"])
	assert.Equal(t, graph.KindFunction, kinds["typedshort"])
	assert.Equal(t, graph.KindFunction, kinds["θ"])
	assert.Equal(t, graph.KindFunction, kinds["foo!"])
	assert.Equal(t, graph.KindMethod, kinds["+"])

	// Receiver metadata on the operator method.
	var recvMeta bool
	for _, n := range res.Nodes {
		if n.Name == "+" && n.Kind == graph.KindMethod {
			if m, ok := n.Meta["receiver"].(string); ok && m == "Base" {
				recvMeta = true
			}
		}
	}
	assert.True(t, recvMeta, "Base.:+ should carry receiver metadata")

	// Macro flag.
	var macroFlag bool
	for _, n := range res.Nodes {
		if n.Name == "mymacro" {
			if flag, ok := n.Meta["macro"].(bool); ok && flag {
				macroFlag = true
			}
		}
	}
	assert.True(t, macroFlag)
}

// A declared return type nests the callee one level deeper — the grammar
// parses `f(x)::Int` as typed_expression(call_expression) and
// `f(x)::T where T` as where_expression(typed_expression(call_expression)) —
// so a peeler that knew only `where_expression` lost the definition AND
// every call in its body. The base regex extractor still emitted the long
// form, which makes that half a straight regression.
func TestJuliaExtractor_ReturnTypeAnnotatedDefinitions(t *testing.T) {
	src := []byte(`function long_form(x)::Int
    helper(x)
end

short_plain(x)::Int = helper(x)

short_where(x)::T where T = helper(x)

function bare_where(x) where T
    helper(x)
end

function empty_generic end
`)
	res, err := NewJuliaExtractor().Extract("ret.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}
	calls := map[string]bool{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			calls[ed.From+" -> "+ed.To] = true
		}
	}

	for _, name := range []string{"long_form", "short_plain", "short_where", "bare_where"} {
		n, ok := nodes["ret.jl::"+name]
		require.True(t, ok, "missing definition %s", name)
		assert.Equal(t, graph.KindFunction, n.Kind)
		assert.True(t, calls["ret.jl::"+name+" -> unresolved::helper"],
			"%s should attribute its body call", name)
	}

	// `function f end` declares an empty generic function: the signature
	// holds the name directly, with no argument list to peel to.
	n, ok := nodes["ret.jl::empty_generic"]
	require.True(t, ok, "empty generic declaration should still be a definition")
	assert.Equal(t, graph.KindFunction, n.Kind)
}

func TestJuliaExtractor_TypesAndFields(t *testing.T) {
	src := []byte(`module Shapes

export area, Circle

abstract type AbstractShape end
abstract type Animal{T} <: Living end

struct Point
    x::Float64
    y
end

mutable struct Counter <: AbstractShape
    n::Int
end

struct Pair{T, S}
    a::T
    b::S
end

end
`)
	res, err := NewJuliaExtractor().Extract("shapes.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}

	// All type forms present.
	for _, name := range []string{"AbstractShape", "Animal", "Point", "Counter", "Pair"} {
		n, ok := nodes["shapes.jl::"+name]
		require.True(t, ok, "missing type %s", name)
		assert.Equal(t, graph.KindType, n.Kind)
	}

	// Struct fields with member_of edges into the struct.
	for _, fid := range []string{
		"shapes.jl::Point.x", "shapes.jl::Point.y",
		"shapes.jl::Counter.n", "shapes.jl::Pair.a", "shapes.jl::Pair.b",
	} {
		n, ok := nodes[fid]
		require.True(t, ok, "missing field %s", fid)
		assert.Equal(t, graph.KindField, n.Kind)
	}
	var pointMemberOf int
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeMemberOf && ed.To == "shapes.jl::Point" {
			pointMemberOf++
		}
	}
	assert.GreaterOrEqual(t, pointMemberOf, 2, "fields should member_of Point")

	// Supertype edges: Animal → Living, Counter → AbstractShape.
	var extendsAnimal, extendsCounter bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeExtends && ed.From == "shapes.jl::Animal" && ed.To == "unresolved::Living" {
			extendsAnimal = true
		}
		if ed.Kind == graph.EdgeExtends && ed.From == "shapes.jl::Counter" && ed.To == "unresolved::AbstractShape" {
			extendsCounter = true
		}
	}
	assert.True(t, extendsAnimal, "Animal <: Living edge missing")
	assert.True(t, extendsCounter, "Counter <: AbstractShape edge missing")

	// Export list recorded on the module node.
	mod := nodes["shapes.jl::Shapes"]
	require.NotNil(t, mod)
	exports, ok := mod.Meta["exports"].([]string)
	require.True(t, ok, "module Meta exports missing")
	assert.ElementsMatch(t, []string{"area", "Circle"}, exports)
}

// juliaOwners maps each node to the set of member_of targets it carries.
// A method or constructor inside a module has TWO owners — its type and
// its module — so a last-write-wins map would silently pick one.
func juliaOwners(edges []*graph.Edge) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, ed := range edges {
		if ed.Kind != graph.EdgeMemberOf {
			continue
		}
		if out[ed.From] == nil {
			out[ed.From] = map[string]bool{}
		}
		out[ed.From][ed.To] = true
	}
	return out
}

// One file, two modules, one name. Node ids stay flat (the house
// convention — folding the module in would break the owner derivation
// that cuts a method id at its last dot), so both definitions have to
// separate through the shared line-suffix helper and carry their module
// on Meta["scope_mod"]. Before this, the second definition was dropped
// AND its body's calls were re-parented onto the first, so the surviving
// node reported calls it does not make.
func TestJuliaExtractor_SameNameAcrossModulesBothSurvive(t *testing.T) {
	src := []byte(`module A
f() = from_a()

struct S
    a::Int
end
end

module B
f() = from_b()

struct S
    b::Int
end
end
`)
	res, err := NewJuliaExtractor().Extract("dup.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}

	first, second := nodes["dup.jl::f"], nodes["dup.jl::f_L10"]
	require.NotNil(t, first, "the first f must keep the clean id")
	require.NotNil(t, second, "the second f must survive under a disambiguated id")
	assert.Equal(t, "A", first.Meta["scope_mod"])
	assert.Equal(t, "B", second.Meta["scope_mod"])

	calls := map[string]string{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			calls[ed.To] = ed.From
		}
	}
	owners := juliaOwners(res.Edges)
	assert.Equal(t, "dup.jl::f", calls["unresolved::from_a"])
	assert.Equal(t, "dup.jl::f_L10", calls["unresolved::from_b"],
		"each definition must own the calls in its own body")
	assert.True(t, owners["dup.jl::f"]["dup.jl::A"])
	assert.True(t, owners["dup.jl::f_L10"]["dup.jl::B"])

	// Same for the two structs, and each field must point at its own.
	require.NotNil(t, nodes["dup.jl::S"])
	require.NotNil(t, nodes["dup.jl::S_L12"])
	require.NotNil(t, nodes["dup.jl::S.a"])
	require.NotNil(t, nodes["dup.jl::S_L12.b"])
	// Two methods of one name written on ONE physical line cannot be
	// separated by a line suffix. Only one node is minted, but the
	// second body's calls must still be attributed rather than dropped.
	oneLine, err := NewJuliaExtractor().Extract("one.jl", []byte("g(x) = h(x); g(y) = k(y)\n"))
	require.NoError(t, err)
	oneLineCalls := map[string]bool{}
	for _, ed := range oneLine.Edges {
		if ed.Kind == graph.EdgeCalls {
			oneLineCalls[ed.From+" -> "+ed.To] = true
		}
	}
	assert.True(t, oneLineCalls["one.jl::g -> unresolved::h"])
	assert.True(t, oneLineCalls["one.jl::g -> unresolved::k"],
		"a same-line twin's calls must not be dropped")

	assert.True(t, owners["dup.jl::S.a"]["dup.jl::S"])
	assert.True(t, owners["dup.jl::S_L12.b"]["dup.jl::S_L12"],
		"a field must belong to its own struct, not to whichever came first")
}

// Julia has no constructor keyword: a callable named after a type IS that
// type's constructor. Sharing the type's id meant the type node swallowed
// every constructor and — worse — adopted the calls in their bodies, so
// `struct Box` reported calling `make_box`. Constructors take the
// cross-language `<Type>.<init>` spelling instead.
func TestJuliaExtractor_ConstructorsAreDistinctFromTheirType(t *testing.T) {
	src := []byte(`struct Box
    x::Int
    Box(x) = new(check(x))
    function Box()
        new(0)
    end
end

Box(x, y) = make_box(x, y)

function Box(x, y, z)
    make_box3(x, y, z)
end
`)
	res, err := NewJuliaExtractor().Extract("ctor.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}
	boxType := nodes["ctor.jl::Box"]
	require.NotNil(t, boxType, "the struct itself must still be a type node")
	assert.Equal(t, graph.KindType, boxType.Kind)

	// Four constructors: inner short, inner long, outer short, outer long.
	var ctors []*graph.Node
	for _, n := range res.Nodes {
		if n.Kind == graph.KindMethod && strings.HasPrefix(n.ID, "ctor.jl::Box.<init>") {
			ctors = append(ctors, n)
		}
	}
	require.Len(t, ctors, 4, "every constructor form needs its own node")
	for _, n := range ctors {
		assert.Equal(t, "Box.<init>", n.Name)
		assert.Equal(t, "Box", n.Meta["receiver"])
	}

	owners := juliaOwners(res.Edges)
	callers := map[string]string{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			callers[ed.To] = ed.From
		}
	}
	for _, n := range ctors {
		assert.True(t, owners[n.ID]["ctor.jl::Box"], "%s must belong to Box", n.ID)
	}

	// Constructor bodies must attribute to the constructor, never to the
	// type node.
	for _, target := range []string{"unresolved::check", "unresolved::make_box", "unresolved::make_box3"} {
		from := callers[target]
		require.NotEmpty(t, from, "missing call to %s", target)
		assert.NotEqual(t, "ctor.jl::Box", from,
			"a constructor body's calls must not be attributed to the type node")
		assert.True(t, strings.HasPrefix(from, "ctor.jl::Box.<init>"),
			"%s should be called by a constructor, got %s", target, from)
	}
}

// Julia does not require a struct to precede the constructors that build
// it, and a macro sharing a type's name is not a constructor at all —
// `macro Tag` defines `@Tag`, a name in a disjoint namespace.
func TestJuliaExtractor_ConstructorRecognitionEdges(t *testing.T) {
	src := []byte(`Box(x) = make_box(x)

struct Box
    x::Int
end

Box(x, y) = make_box2(x, y)

struct Tag
    v::Int
end

macro Tag(x)
    x
end
`)
	res, err := NewJuliaExtractor().Extract("fw.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}
	owners := juliaOwners(res.Edges)

	// The TYPE keeps the canonical id even though a constructor was
	// written above it.
	box := nodes["fw.jl::Box"]
	require.NotNil(t, box, "the struct must keep the canonical id")
	assert.Equal(t, graph.KindType, box.Kind)
	require.NotNil(t, nodes["fw.jl::Box.x"], "and so must its field")

	var ctors int
	for _, n := range res.Nodes {
		if n.Kind == graph.KindMethod && strings.HasPrefix(n.ID, "fw.jl::Box.<init>") {
			ctors++
			assert.True(t, owners[n.ID]["fw.jl::Box"], "%s must belong to Box", n.ID)
		}
	}
	assert.Equal(t, 2, ctors, "the constructor above the struct counts too")

	// Delegation to another method of the same constructor is the most
	// idiomatic constructor body Julia has, and it is NOT recursion —
	// multiple dispatch sends it to a different method.
	deleg, err := NewJuliaExtractor().Extract("dg.jl", []byte(`struct Box
    x::Int
    Box(x) = new(check(x))
end

Box() = Box(0)
`))
	require.NoError(t, err)
	var delegated bool
	for _, ed := range deleg.Edges {
		if ed.Kind == graph.EdgeCalls && strings.HasPrefix(ed.From, "dg.jl::Box.<init>") &&
			ed.To == "unresolved::Box" {
			delegated = true
		}
	}
	assert.True(t, delegated, "a delegating constructor must keep its call edge")

	// Exactly one: a definition's own signature is a call_expression in
	// this grammar, and counting it as a call site would make every
	// definition appear to call itself — invisible while the recursion
	// guard swallowed it, visible the moment a constructor stops
	// applying that guard. A call in a DEFAULT ARGUMENT is still a call
	// and must survive.
	sig, err := NewJuliaExtractor().Extract("sig.jl", []byte(`struct Rep
    n::Int
    tag::Bool
end

Rep(n) = Rep(n, false)

function build(x = fallback())
    use(x)
end
`))
	require.NoError(t, err)
	var selfCalls int
	sigCalls := map[string]bool{}
	for _, ed := range sig.Edges {
		if ed.Kind != graph.EdgeCalls {
			continue
		}
		sigCalls[ed.From+" -> "+ed.To] = true
		if ed.To == "unresolved::Rep" {
			selfCalls++
		}
	}
	assert.Equal(t, 1, selfCalls,
		"the delegation is one call; the signature is not a second one")
	assert.True(t, sigCalls["sig.jl::build -> unresolved::fallback"],
		"a call in a default argument value must still be recorded")
	assert.True(t, sigCalls["sig.jl::build -> unresolved::use"])

	// The macro is a macro, not Tag's constructor.
	_, tagCtor := nodes["fw.jl::Tag.<init>"]
	assert.False(t, tagCtor, "a macro must never be read as a constructor")
	var macroNode *graph.Node
	for _, n := range res.Nodes {
		if n.Name == "Tag" && n.Meta["macro"] == true {
			macroNode = n
		}
	}
	require.NotNil(t, macroNode, "the macro must survive as its own node")
	assert.Equal(t, graph.KindFunction, macroNode.Kind)
	assert.False(t, owners[macroNode.ID]["fw.jl::Tag"])
}

// A nested module contributes its full dotted path, and a constructor
// binds to the type declared in its OWN module.
//
// The declaration order is the point: the outer struct comes first and
// its constructor last, so a flat last-write-wins name table would hold
// the INNER module's Node by the time the outer constructor is reached
// and would bind it to the wrong type. Only keying by module gets this
// right.
func TestJuliaExtractor_NestedModuleScope(t *testing.T) {
	src := []byte(`module Outer

struct Node
    w::Int
end

module Inner
struct Node
    v::Int
end
Node(v) = build_inner(v)
end

Node(w) = build_outer(w)

end
`)
	res, err := NewJuliaExtractor().Extract("nest.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}
	require.NotNil(t, nodes["nest.jl::Inner"])
	assert.Equal(t, "Outer", nodes["nest.jl::Inner"].Meta["scope_mod"])
	require.NotNil(t, nodes["nest.jl::Node"], "the outer struct keeps the clean id")
	assert.Equal(t, "Outer", nodes["nest.jl::Node"].Meta["scope_mod"])
	require.NotNil(t, nodes["nest.jl::Node_L8"], "the inner struct is disambiguated")
	assert.Equal(t, "Outer.Inner", nodes["nest.jl::Node_L8"].Meta["scope_mod"])

	owners := juliaOwners(res.Edges)
	callers := map[string]string{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			callers[ed.To] = ed.From
		}
	}
	inner := callers["unresolved::build_inner"]
	outer := callers["unresolved::build_outer"]
	require.NotEmpty(t, inner)
	require.NotEmpty(t, outer)
	assert.True(t, owners[inner]["nest.jl::Node_L8"],
		"the inner module's constructor must belong to the inner module's type")
	assert.True(t, owners[outer]["nest.jl::Node"],
		"the outer module's constructor must belong to the outer module's type")
	assert.False(t, owners[outer]["nest.jl::Node_L8"],
		"it must NOT bind to the same-named type declared later in a submodule")
}

func TestJuliaExtractor_Imports(t *testing.T) {
	src := []byte(`module M
using LinearAlgebra
using Statistics: mean, std
import Base: show
import Foo as F
import A.B.C
end
`)
	res, err := NewJuliaExtractor().Extract("imports.jl", src)
	require.NoError(t, err)

	targets := map[string]map[string]any{}
	for _, ed := range res.Edges {
		if ed.Kind != graph.EdgeImports {
			continue
		}
		meta := map[string]any{}
		for k, v := range ed.Meta {
			meta[k] = v
		}
		targets[ed.To] = meta
	}

	require.Contains(t, targets, "unresolved::import::LinearAlgebra")
	require.Contains(t, targets, "unresolved::import::Statistics")
	names, ok := targets["unresolved::import::Statistics"]["names"].([]string)
	require.True(t, ok, "selected import names missing")
	assert.ElementsMatch(t, []string{"mean", "std"}, names)

	require.Contains(t, targets, "unresolved::import::Base")
	names, ok = targets["unresolved::import::Base"]["names"].([]string)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"show"}, names)

	require.Contains(t, targets, "unresolved::import::Foo")
	alias, ok := targets["unresolved::import::Foo"]["alias"].(string)
	require.True(t, ok)
	assert.Equal(t, "F", alias)

	require.Contains(t, targets, "unresolved::import::A.B.C")
}

// The cross-product the isolated per-feature tests missed: a dotted or
// relative module path COMBINED with a selective list or an alias. In
// those shapes the module is an `import_path` child, so the
// identifier-only scan skipped it and promoted the first selected name
// (or the alias) to the import target — `using A.B: x, y` imported "x".
// The regex extractor this replaced still recorded `A.B`.
func TestJuliaExtractor_CombinedImportPaths(t *testing.T) {
	src := []byte(`using A.B: x, y
import C.D as CD
using .Local: z
import .Local as L
import ..Up: q
using Base: +, -
import Foo: bar as baz
`)
	res, err := NewJuliaExtractor().Extract("imp.jl", src)
	require.NoError(t, err)

	type imp struct {
		names []string
		alias string
	}
	byTarget := map[string][]imp{}
	for _, ed := range res.Edges {
		if ed.Kind != graph.EdgeImports {
			continue
		}
		var got imp
		if v, ok := ed.Meta["names"].([]string); ok {
			got.names = v
		}
		if v, ok := ed.Meta["alias"].(string); ok {
			got.alias = v
		}
		byTarget[ed.To] = append(byTarget[ed.To], got)
	}

	require.Contains(t, byTarget, "unresolved::import::A.B", "dotted + selective must keep the module")
	assert.ElementsMatch(t, []string{"x", "y"}, byTarget["unresolved::import::A.B"][0].names)

	require.Contains(t, byTarget, "unresolved::import::C.D", "dotted + alias must keep the module")
	assert.Equal(t, "CD", byTarget["unresolved::import::C.D"][0].alias)

	require.Contains(t, byTarget, "unresolved::import::..Up", "a relative path keeps its leading dots")
	assert.ElementsMatch(t, []string{"q"}, byTarget["unresolved::import::..Up"][0].names)

	// One relative module reached both ways in the same file.
	local := byTarget["unresolved::import::.Local"]
	require.Len(t, local, 2, "relative + selective and relative + alias are two imports of .Local")
	var sawNames, sawAlias bool
	for _, got := range local {
		if len(got.names) == 1 && got.names[0] == "z" {
			sawNames = true
		}
		if got.alias == "L" {
			sawAlias = true
		}
	}
	assert.True(t, sawNames, "relative + selective must record the selected name")
	assert.True(t, sawAlias, "relative + alias must record the alias")

	// Operators are `operator` nodes, not identifiers, so an
	// identifier-only scan recorded an empty selection for them.
	require.Contains(t, byTarget, "unresolved::import::Base")
	assert.ElementsMatch(t, []string{"+", "-"}, byTarget["unresolved::import::Base"][0].names)

	// A renamed binding inside a selected list still selects the
	// UPSTREAM name.
	require.Contains(t, byTarget, "unresolved::import::Foo")
	assert.ElementsMatch(t, []string{"bar"}, byTarget["unresolved::import::Foo"][0].names)
}

// A selected macro is a `macro_identifier` node and a selected operator an
// `operator` node, so an identifier-only scan drops both — including from
// the per-binding edges the module edge's `names` meta exists to back.
// `using Base: @time` and `import Base: + as plus` are ordinary Julia.
func TestJuliaExtractor_MacroAndOperatorSelections(t *testing.T) {
	src := []byte(`using Base: @time, isempty
import Base.Threads: @spawn
import Base: + as plus, -
`)
	res, err := NewJuliaExtractor().Extract("sel.jl", src)
	require.NoError(t, err)

	names := map[string][]string{}
	bindings := map[string]string{}
	for _, ed := range res.Edges {
		if ed.Kind != graph.EdgeImports {
			continue
		}
		if v, ok := ed.Meta["names"].([]string); ok {
			names[ed.To] = append(names[ed.To], v...)
		}
		bindings[ed.To] = ed.Alias
	}

	// Two statements select from Base, so the names accumulate.
	assert.ElementsMatch(t, []string{"@time", "isempty", "+", "-"},
		names["unresolved::import::Base"])
	require.Contains(t, bindings, "unresolved::import::Base::@time",
		"a selected macro needs its own binding edge")
	require.Contains(t, bindings, "unresolved::import::Base.Threads::@spawn")
	assert.ElementsMatch(t, []string{"@spawn"}, names["unresolved::import::Base.Threads"])

	require.Contains(t, bindings, "unresolved::import::Base::+",
		"a renamed operator selection must not vanish")
	assert.Equal(t, "plus", bindings["unresolved::import::Base::+"])
	require.Contains(t, bindings, "unresolved::import::Base::-")

	// A rename must survive the store, which drops Edge.Alias — so it
	// rides on Meta too.
	for _, ed := range res.Edges {
		if ed.To == "unresolved::import::Base::+" {
			assert.Equal(t, "plus", ed.Meta["alias"],
				"the local name needs a persisted half, not only Edge.Alias")
		}
	}

	// One statement must not dwarf a file's graph: past the cap only the
	// module edge is kept, and the selected names stay in its Meta.
	var wide strings.Builder
	wide.WriteString("using Base: ")
	for i := 0; i < juliaImportBindingCap+1; i++ {
		if i > 0 {
			wide.WriteString(", ")
		}
		fmt.Fprintf(&wide, "n%d", i)
	}
	wide.WriteString("\n")
	big, err := NewJuliaExtractor().Extract("big.jl", []byte(wide.String()))
	require.NoError(t, err)
	var bigImports int
	var bigNames []string
	for _, ed := range big.Edges {
		if ed.Kind != graph.EdgeImports {
			continue
		}
		bigImports++
		if v, ok := ed.Meta["names"].([]string); ok {
			bigNames = v
		}
	}
	assert.Equal(t, 1, bigImports, "past the cap only the module edge is emitted")
	assert.Len(t, bigNames, juliaImportBindingCap+1,
		"and the selected names are still recorded on it")
}

// An export list can export a macro (`export @m`) and an operator
// (`export ⊗`) — a macro_identifier node and an operator node, the same
// distinction import selections already decode — so an identifier-only
// scan dropped them from the module's recorded public surface.
func TestJuliaExtractor_MacroAndOperatorExports(t *testing.T) {
	src := []byte(`module Ops
export apply, @m, ⊗
end
`)
	res, err := NewJuliaExtractor().Extract("exports.jl", src)
	require.NoError(t, err)

	mod := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		mod[n.ID] = n
	}
	m := mod["exports.jl::Ops"]
	require.NotNil(t, m)
	exports, ok := m.Meta["exports"].([]string)
	require.True(t, ok, "module Meta exports missing")
	assert.ElementsMatch(t, []string{"apply", "@m", "⊗"}, exports,
		"a module's exported macros and operators are part of its public surface")
}

func TestJuliaExtractor_Calls(t *testing.T) {
	src := []byte(`module Calls

function outer(xs)
    s = sum(xs)
    m = MyMod.helper(xs)
    b = sin.(xs)
    @time helper(xs)
    return θ(s)
end

inner() = begin
    nested() = 1
    nested()
end

end
`)
	res, err := NewJuliaExtractor().Extract("calls.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}
	require.Contains(t, nodes, "calls.jl::outer")
	require.Contains(t, nodes, "calls.jl::inner")
	require.Contains(t, nodes, "calls.jl::nested", "closure inside begin block should be extracted")

	type call struct {
		from, to string
		meta     map[string]any
	}
	var calls []call
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			meta := map[string]any{}
			for k, v := range ed.Meta {
				meta[k] = v
			}
			calls = append(calls, call{ed.From, ed.To, meta})
		}
	}
	find := func(from, to string) *call {
		for i := range calls {
			if calls[i].from == from && calls[i].to == to {
				return &calls[i]
			}
		}
		return nil
	}

	require.NotNil(t, find("calls.jl::outer", "unresolved::sum"), "bare call")
	require.NotNil(t, find("calls.jl::outer", "unresolved::MyMod.helper"), "qualified call")
	bc := find("calls.jl::outer", "unresolved::sin")
	require.NotNil(t, bc, "broadcast call")
	if flag, ok := bc.meta["broadcast"].(bool); !ok || !flag {
		t.Errorf("broadcast call missing broadcast meta: %+v", bc.meta)
	}
	require.NotNil(t, find("calls.jl::outer", "unresolved::helper"), "call inside macro invocation")
	require.NotNil(t, find("calls.jl::outer", "unresolved::time"), "macro invocation edge")
	require.NotNil(t, find("calls.jl::outer", "unresolved::θ"), "unicode callee")
	require.NotNil(t, find("calls.jl::inner", "unresolved::nested"), "closure call attributed to closure")

	// Direct recursion must not emit an edge from nested to itself.
	for _, c := range calls {
		if c.from == "calls.jl::nested" && c.to == "unresolved::nested" {
			t.Errorf("self-recursion edge emitted")
		}
	}
}

// A chained callee's base is a call, not a name: `get(cfg).run(x)` used
// to reach the graph as `unresolved::get(cfg).run` — arguments and all —
// because the callee was decoded by splitting its source text on the
// last dot, and a chain broken across lines even carried the line break
// into the target. Decoding the field_expression's children instead
// degrades the callee to its method name, the only part a resolver
// could ever match, while a genuinely dotted base (A.B.c) keeps its
// full qualification.
func TestJuliaExtractor_ChainedCalleeDegradesToMethodName(t *testing.T) {
	src := []byte(`launch(cfg) = get(cfg).run(x)

wrapped(x) = foo(x,
    1,
).bar(y)

deep(x) = A.B.c(x)
`)
	res, err := NewJuliaExtractor().Extract("chain.jl", src)
	require.NoError(t, err)

	calls := map[string]bool{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			calls[ed.From+" -> "+ed.To] = true
		}
		require.NotContains(t, ed.To, "\n", "a target must never carry a line break")
		require.NotContains(t, ed.To, "(", "a target must never carry argument text")
	}
	require.True(t, calls["chain.jl::launch -> unresolved::run"],
		"a chained callee degrades to its method name, not its argument text")
	require.True(t, calls["chain.jl::wrapped -> unresolved::bar"],
		"a chain broken across lines must not leak the break into the target")
	require.True(t, calls["chain.jl::deep -> unresolved::A.B.c"],
		"a dotted base keeps its full qualification")
}

// `Base.:(==)(a, b)` used to normalise its callee to `(==)` — the
// parenthesised quote survived the text trim — while `Base.:+` trimmed
// to `+`. Both spellings name the same operator, so both must decode to
// the operator's own name; a bare `(==)(a, b)` callee, which wears only
// the parentheses, does too.
func TestJuliaExtractor_QuotedOperatorCallee(t *testing.T) {
	src := []byte(`same(a, b) = Base.:(==)(a, b)
plus(a, b) = Base.:+(a, b)
plain(a, b) = (==)(a, b)
`)
	res, err := NewJuliaExtractor().Extract("op.jl", src)
	require.NoError(t, err)

	calls := map[string]bool{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			calls[ed.From+" -> "+ed.To] = true
		}
	}
	require.True(t, calls["op.jl::same -> unresolved::Base.=="],
		"`:(==)` must normalise to the operator name like `:+` does")
	require.True(t, calls["op.jl::plus -> unresolved::Base.+"],
		"`:+` keeps its existing normalisation")
	require.True(t, calls["op.jl::plain -> unresolved::=="],
		"a bare parenthesised operator callee decodes too")
}

// Constructing a parametric type is one of the most common calls in real
// Julia, and its callee is a parametrized_type_expression, not an
// identifier — a decoder with no case for it dropped the edge, so
// `Vector{Int}(xs)` vanished from the call graph. The edge names the
// constructor the way Julia prints it, parameters included, and a
// qualified head keeps its module.
func TestJuliaExtractor_ParametrizedConstructorCallee(t *testing.T) {
	src := []byte(`build(xs) = Vector{Int}(xs)
qualified(xs) = Base.Vector{Int}(xs)
table(xs) = Dict{String,Int}(xs)
`)
	res, err := NewJuliaExtractor().Extract("param.jl", src)
	require.NoError(t, err)

	calls := map[string]bool{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			calls[ed.From+" -> "+ed.To] = true
		}
		require.NotContains(t, ed.To, "\n", "a target must never carry a line break")
	}
	require.True(t, calls["param.jl::build -> unresolved::Vector{Int}"],
		"a parametric constructor call is a call edge")
	require.True(t, calls["param.jl::qualified -> unresolved::Base.Vector{Int}"],
		"a qualified parametric head keeps its module")
	require.True(t, calls["param.jl::table -> unresolved::Dict{String,Int}"],
		"multi-parameter constructors carry their parameters")
}

// A module-qualified macro call nests its macro_identifier under a
// field_expression (Base.@time), so a scan that matches only a direct
// macro_identifier child recorded the inner helper call but never the
// macro's own edge. The qualified form must record both, with the
// module as receiver — the same spelling qualified call callees use —
// and the import-alias rewrite applies to it just as it does to calls.
func TestJuliaExtractor_QualifiedMacroCall(t *testing.T) {
	src := []byte(`module M
import Foo as F

function work(xs)
    Base.@time helper(xs)
    F.@spawn helper(xs)
end
end
`)
	res, err := NewJuliaExtractor().Extract("qmac.jl", src)
	require.NoError(t, err)

	type call struct {
		to   string
		meta map[string]any
	}
	calls := map[string][]call{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			meta := map[string]any{}
			for k, v := range ed.Meta {
				meta[k] = v
			}
			calls[ed.From] = append(calls[ed.From], call{ed.To, meta})
		}
	}
	var sawBase, sawAliased, sawHelper bool
	for _, c := range calls["qmac.jl::work"] {
		switch c.to {
		case "unresolved::Base.time":
			sawBase = c.meta["macro"] == true
		case "unresolved::Foo.spawn":
			sawAliased = c.meta["macro"] == true
		case "unresolved::helper":
			sawHelper = true
		}
	}
	assert.True(t, sawBase, "Base.@time needs its macro edge, receiver included")
	assert.True(t, sawAliased, "an aliased module qualifies the macro edge, as it does for calls")
	assert.True(t, sawHelper, "the inner call of a qualified macro keeps its own edge")
}

// A docstring above a macro call switches walkMacroArgs into its
// doc-carrying loop, which dispatched definition arguments to their
// handlers but walked every other argument with walk() — and walk()
// visits a node's children, never the node itself. A call_expression
// argument therefore never reached handleCall; since ordinary call
// edges need an enclosing function, which a documented module-level
// macro call never has, the one observable loss is the include() that
// loads a file on every worker.
func TestJuliaExtractor_DocumentedMacroArgumentsKeepCalls(t *testing.T) {
	src := []byte(`module M
"""load helpers on every worker"""
@everywhere include("helpers.jl")
end
`)
	res, err := NewJuliaExtractor().Extract("everywhere.jl", src)
	require.NoError(t, err)

	var got bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::helpers.jl" {
			got = true
		}
	}
	assert.True(t, got, "an include() in a documented macro argument keeps its import edge")

	// The undocumented form must keep working identically.
	plain, err := NewJuliaExtractor().Extract("plain.jl", []byte("module M\n@everywhere include(\"more.jl\")\nend\n"))
	require.NoError(t, err)
	var plainGot bool
	for _, ed := range plain.Edges {
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::more.jl" {
			plainGot = true
		}
	}
	assert.True(t, plainGot, "the undocumented form is unchanged")
}

func TestJuliaExtractor_ConstAndDocstrings(t *testing.T) {
	src := []byte(`"""
Circle radius helpers.
"""
function radius(c)
    c.r
end

const DEFAULT = 42
`)
	res, err := NewJuliaExtractor().Extract("doc.jl", src)
	require.NoError(t, err)

	nodes := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		nodes[n.ID] = n
	}

	r := nodes["doc.jl::radius"]
	require.NotNil(t, r)
	doc, ok := r.Meta["doc"].(string)
	require.True(t, ok, "preceding docstring should attach")
	assert.Equal(t, "Circle radius helpers.", doc)

	c := nodes["doc.jl::DEFAULT"]
	require.NotNil(t, c)
	assert.Equal(t, graph.KindVariable, c.Kind)
}

// Julia attaches a docstring to the object IMMEDIATELY below it: its
// parser allows exactly one newline between the two, and the manual says
// twice that no blank line or comment may intervene. A string at the top
// of a function BODY is not documentation at all — a function body is
// parsed with the ordinary expression production — so a helper that
// returns a string used to document itself with its own return value.
func TestJuliaExtractor_DocstringAttachment(t *testing.T) {
	src := []byte(`"""
    radius(c)

Return the radius of ` + "`c`" + `.
"""
long(c) = c.r

"""short doc"""
short(x) = x

"""const doc"""
const TUNED = 1

"""type doc"""
struct Documented
    a
end

"""trailing comment doc""" # still adjacent
adjacent(x) = x

"""detached by a blank line"""

blank(x) = x

"""detached by a comment"""
# an own line
commented(x) = x

function returns_a_string()
    "not documentation, just the return value"
end

function outer()
    "not documentation either"
    function nested()
        1
    end
end
`)
	res, err := NewJuliaExtractor().Extract("attach.jl", src)
	require.NoError(t, err)

	docs := map[string]string{}
	for _, n := range res.Nodes {
		if d, ok := n.Meta["doc"].(string); ok {
			docs[n.ID] = d
		}
	}

	// Julia's own convention opens a docstring with the signature,
	// indented, then a blank line, then the prose — so the summary is the
	// FIRST PROSE paragraph, not literally the first one.
	assert.Equal(t, "Return the radius of `c`.", docs["attach.jl::long"],
		"the indented signature block is not the documentation")
	assert.Equal(t, "short doc", docs["attach.jl::short"],
		"a short-form definition takes a docstring like any other")
	assert.Equal(t, "const doc", docs["attach.jl::TUNED"])
	assert.Equal(t, "type doc", docs["attach.jl::Documented"])
	assert.Equal(t, "trailing comment doc", docs["attach.jl::adjacent"],
		"a comment on the docstring's own line does not detach it")

	// A Windows-authored file separates paragraphs with "\r\n\r\n",
	// which holds no "\n\n" — the signature block has to be recognised
	// there too, or it becomes the documentation.
	crlf, err := NewJuliaExtractor().Extract("crlf.jl",
		[]byte("\"\"\"\r\n    api(x)\r\n\r\nProse here.\r\n\"\"\"\r\napi(x) = x\r\n"))
	require.NoError(t, err)
	var crlfDoc string
	for _, n := range crlf.Nodes {
		if n.ID == "crlf.jl::api" {
			crlfDoc, _ = n.Meta["doc"].(string)
		}
	}
	assert.Equal(t, "Prose here.", crlfDoc, "CRLF paragraphs must split like LF ones")

	// A docstring written inside an indented body has EVERY line
	// indented, so an absolute "is this paragraph indented" test calls
	// all of them signature blocks and eats the docstring. The signature
	// block is indented relative to the prose, which only shows after the
	// literal's own common indent is removed.
	nested, err := NewJuliaExtractor().Extract("nested.jl", []byte(`module Mod
    """
        render(s)

    Renders a shape.

    Returns a string.
    """
    function render(s)
        s
    end
end
`))
	require.NoError(t, err)
	var nestedDoc string
	for _, n := range nested.Nodes {
		if n.ID == "nested.jl::render" {
			nestedDoc, _ = n.Meta["doc"].(string)
		}
	}
	assert.Equal(t, "Renders a shape.", nestedDoc,
		"an indented docstring keeps its first prose paragraph")

	// A docstring that is nothing but a signature still documents better
	// than nothing.
	only, err := NewJuliaExtractor().Extract("only.jl", []byte("\"\"\"\n    sig(x)\n\"\"\"\nsig(x) = x\n"))
	require.NoError(t, err)
	var onlyDoc string
	for _, n := range only.Nodes {
		if n.ID == "only.jl::sig" {
			onlyDoc, _ = n.Meta["doc"].(string)
		}
	}
	assert.Equal(t, "sig(x)", onlyDoc, "a signature-only docstring must not vanish")

	// A block comment spans rows, and those rows must be discounted from
	// the adjacency distance rather than breaking it — the single-line
	// case above spans zero rows and so cannot pin the arithmetic.
	// A macro-wrapped definition takes the docstring above the WRAPPER,
	// and a `quote` block inside a macro body is code, not a doc context.
	more, err := NewJuliaExtractor().Extract("more.jl", []byte(`"""block doc""" #=
spanning
three rows
=#
blocked(x) = x

"""kwdef doc"""
Base.@kwdef struct Wrapped
    n::Int = 1
end

macro gen(ex)
    quote
        "not documentation"
        function generated()
            1
        end
    end
end
`))
	require.NoError(t, err)
	moreDocs := map[string]string{}
	for _, n := range more.Nodes {
		if d, ok := n.Meta["doc"].(string); ok {
			moreDocs[n.ID] = d
		}
	}
	assert.Equal(t, "block doc", moreDocs["more.jl::blocked"],
		"a block comment opening on the docstring's line does not detach it")
	assert.Equal(t, "kwdef doc", moreDocs["more.jl::Wrapped"],
		"a macro-wrapped struct takes the docstring above the macro call")
	_, generatedDoc := moreDocs["more.jl::generated"]
	assert.False(t, generatedDoc,
		"a string inside a quote block in a macro body is code, not documentation")

	for _, id := range []string{
		"attach.jl::blank", "attach.jl::commented",
		"attach.jl::returns_a_string", "attach.jl::nested",
	} {
		_, ok := docs[id]
		assert.False(t, ok, "%s must not be documented, got %q", id, docs[id])
	}
}
