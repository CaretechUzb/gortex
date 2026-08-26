package languages

import (
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
