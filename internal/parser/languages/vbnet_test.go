package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// vbFind returns the first node with the given name, or nil.
func vbFind(nodes []*graph.Node, name string) *graph.Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func vbHasEdge(edges []*graph.Edge, kind graph.EdgeKind, from, to string) bool {
	for _, e := range edges {
		if e.Kind == kind && (from == "" || e.From == from) && e.To == to {
			return true
		}
	}
	return false
}

func TestVBNetExtractor_Basics(t *testing.T) {
	src := []byte(`Imports System.Data
Imports Alias = System.Text

Namespace Ltk.Demo

    Public Class OrderService
        Inherits ServiceBase
        Implements IOrderService, IDisposable

        Public Property OrderCount As Integer

        Public Sub New()
            _count = 0
        End Sub

        Public Function Total(ByVal id As Integer) As Decimal
            Dim repo As New OrderRepository
            Return repo.Sum(id)
        End Function

        Private Sub Log(ByVal msg As String)
            Trace.WriteLine(msg)
        End Sub
    End Class

End Namespace
`)
	e := NewVBNetExtractor()
	require.Equal(t, "vbnet", e.Language())
	require.Equal(t, []string{".vb"}, e.Extensions())

	res, err := e.Extract("Order.vb", src)
	require.NoError(t, err)

	// Namespace is a package node.
	ns := vbFind(res.Nodes, "Ltk.Demo")
	require.NotNil(t, ns, "namespace node")
	assert.Equal(t, graph.KindPackage, ns.Kind)

	// Class carries flavor, visibility and enclosing namespace.
	cls := vbFind(res.Nodes, "OrderService")
	require.NotNil(t, cls, "class node")
	assert.Equal(t, graph.KindType, cls.Kind)
	assert.Equal(t, "class", cls.Meta["type_flavor"])
	assert.Equal(t, VisibilityPublic, cls.Meta["visibility"])
	assert.Equal(t, "Ltk.Demo", cls.Meta["scope_ns"])
	// End Class is line 26 of the fixture; the range must be real, not a point.
	assert.Greater(t, cls.EndLine, cls.StartLine)

	// Members are methods owned by the class, not free functions.
	total := vbFind(res.Nodes, "Total")
	require.NotNil(t, total, "Total method")
	assert.Equal(t, graph.KindMethod, total.Kind)
	assert.Equal(t, "OrderService", total.Meta["receiver"])
	assert.Greater(t, total.EndLine, total.StartLine)
	assert.Equal(t, "Order.vb::OrderService.Total", total.ID)

	logM := vbFind(res.Nodes, "Log")
	require.NotNil(t, logM, "Log method")
	assert.Equal(t, VisibilityPrivate, logM.Meta["visibility"])

	// Constructor is keyed on <init> rather than "New".
	ctor := vbFind(res.Nodes, "OrderService.<init>")
	require.NotNil(t, ctor, "constructor node")
	assert.Equal(t, graph.KindMethod, ctor.Kind)
	assert.Equal(t, "Order.vb::OrderService.<init>", ctor.ID)

	// Property becomes a field member.
	prop := vbFind(res.Nodes, "OrderCount")
	require.NotNil(t, prop, "property node")
	assert.Equal(t, graph.KindField, prop.Kind)

	// Structural edges.
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeMemberOf, total.ID, cls.ID),
		"Total MEMBER_OF OrderService")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeDefines, "Order.vb", cls.ID),
		"file DEFINES OrderService")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImports, "Order.vb", "unresolved::import::System.Data"),
		"Imports System.Data")
	// An aliased import records the target namespace, not the alias.
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImports, "Order.vb", "unresolved::import::System.Text"),
		"aliased Imports resolves to the namespace")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeReferences, cls.ID, "unresolved::ServiceBase"),
		"Inherits ServiceBase")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImplements, cls.ID, "unresolved::IOrderService"),
		"Implements IOrderService")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImplements, cls.ID, "unresolved::IDisposable"),
		"second interface on the same Implements clause")

	// Qualified call attributed to its enclosing member.
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeCalls, total.ID, "unresolved::Sum"),
		"repo.Sum() attributed to Total")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeCalls, logM.ID, "unresolved::WriteLine"),
		"Trace.WriteLine() attributed to Log")
}

// VB is case-insensitive: keywords and terminators must match in any casing.
func TestVBNetExtractor_CaseInsensitive(t *testing.T) {
	src := []byte(`MODULE Helpers
    PUBLIC FUNCTION Widen(ByVal s As String) As String
        Return s
    END FUNCTION
END MODULE
`)
	res, err := NewVBNetExtractor().Extract("h.vb", src)
	require.NoError(t, err)

	mod := vbFind(res.Nodes, "Helpers")
	require.NotNil(t, mod)
	assert.Equal(t, graph.KindModule, mod.Kind)

	fn := vbFind(res.Nodes, "Widen")
	require.NotNil(t, fn)
	assert.Equal(t, graph.KindMethod, fn.Kind)
	// END FUNCTION in caps must still close the block.
	assert.Equal(t, 4, fn.EndLine)
}

// Overloads and same-named members of different types must not collapse onto
// one node — the failure mode a flat filePath::name ID scheme produces.
func TestVBNetExtractor_OverloadsStayDistinct(t *testing.T) {
	src := []byte(`Public Class A
    Public Sub Run()
    End Sub
    Public Sub Run(ByVal n As Integer)
    End Sub
End Class

Public Class B
    Public Sub Run()
    End Sub
End Class
`)
	res, err := NewVBNetExtractor().Extract("d.vb", src)
	require.NoError(t, err)

	var runs int
	ids := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Name == "Run" {
			runs++
			ids[n.ID] = true
		}
	}
	assert.Equal(t, 3, runs, "two A.Run overloads plus B.Run")
	assert.Len(t, ids, 3, "every Run has a distinct ID")

	var aReceiver, bReceiver int
	for _, n := range res.Nodes {
		if n.Name != "Run" {
			continue
		}
		switch n.Meta["receiver"] {
		case "A":
			aReceiver++
		case "B":
			bReceiver++
		}
	}
	assert.Equal(t, 2, aReceiver)
	assert.Equal(t, 1, bReceiver)
}

// A Sub at file scope is a free function; the same Sub inside a type is a
// method. VB's script-style files rely on the former.
func TestVBNetExtractor_FileScopeIsFreeFunction(t *testing.T) {
	src := []byte(`Public Sub Main()
    Console.WriteLine("hi")
End Sub
`)
	res, err := NewVBNetExtractor().Extract("m.vb", src)
	require.NoError(t, err)

	main := vbFind(res.Nodes, "Main")
	require.NotNil(t, main)
	assert.Equal(t, graph.KindFunction, main.Kind)
	assert.Nil(t, main.Meta["receiver"])
	assert.Equal(t, "m.vb::Main", main.ID)
}

// Bodyless declarations: interface members, MustOverride members and
// auto-properties have no End keyword, so the extent is the declaration line.
func TestVBNetExtractor_BodylessDeclarations(t *testing.T) {
	src := []byte(`Public Interface IShape
    Function Area() As Double
    Property Name As String
End Interface
`)
	res, err := NewVBNetExtractor().Extract("i.vb", src)
	require.NoError(t, err)

	iface := vbFind(res.Nodes, "IShape")
	require.NotNil(t, iface)
	assert.Equal(t, graph.KindInterface, iface.Kind)

	area := vbFind(res.Nodes, "Area")
	require.NotNil(t, area)
	assert.Equal(t, graph.KindMethod, area.Kind)
	// No `End Function`: start and end collapse to the declaration line.
	assert.Equal(t, area.StartLine, area.EndLine)
}

// Delegate is a type; Declare is an external P/Invoke entry point. Neither may
// be emitted as an ordinary Sub/Function despite matching that modifier run.
func TestVBNetExtractor_DelegateAndDeclare(t *testing.T) {
	src := []byte(`Public Delegate Sub Notify(ByVal msg As String)
Public Declare Function GetTickCount Lib "kernel32" () As Long
`)
	res, err := NewVBNetExtractor().Extract("p.vb", src)
	require.NoError(t, err)

	notify := vbFind(res.Nodes, "Notify")
	require.NotNil(t, notify)
	assert.Equal(t, graph.KindType, notify.Kind)
	assert.Equal(t, "delegate", notify.Meta["type_flavor"])

	tick := vbFind(res.Nodes, "GetTickCount")
	require.NotNil(t, tick)
	assert.Equal(t, graph.KindFunction, tick.Kind)
	assert.Equal(t, true, tick.Meta["external"])

	// Exactly one node per declaration — the Declare must not also surface via
	// the Function pattern.
	var count int
	for _, n := range res.Nodes {
		if n.Name == "GetTickCount" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

// A declaration-free file — the shape of classic-ASP-style VB script — yields
// the file node alone and must not panic.
func TestVBNetExtractor_NoDeclarations(t *testing.T) {
	src := []byte(`Dim x As Integer = 1
Response.Write("hello")
If x > 0 Then
    x = x + 1
End If
`)
	res, err := NewVBNetExtractor().Extract("s.vb", src)
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	// With no enclosing callable there is nothing to attribute a call to.
	for _, e := range res.Edges {
		assert.NotEqual(t, graph.EdgeCalls, e.Kind)
	}
}

func TestVBNetExtractor_EmptyInput(t *testing.T) {
	res, err := NewVBNetExtractor().Extract("e.vb", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}
