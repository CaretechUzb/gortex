package entrypoints

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestDetect_DotNetApiControllerAndAction: an [ApiController] type and an
// [HttpGet] action have no in-app callers but are framework-invoked, so
// both must be flagged; a plain private helper in the same file must not.
func TestDetect_DotNetApiControllerAndAction(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "Ctrl.cs", Kind: graph.KindFile, FilePath: "Ctrl.cs"},
		{ID: "Ctrl.cs::UsersController", Kind: graph.KindType, Name: "UsersController"},
		{ID: "Ctrl.cs::UsersController.Get", Kind: graph.KindMethod, Name: "Get"},
		{ID: "Ctrl.cs::UsersController.helper", Kind: graph.KindMethod, Name: "helper"},
	}
	edges := []*graph.Edge{
		{From: "Ctrl.cs::UsersController", To: "annotation::csharp::ApiController", Kind: graph.EdgeAnnotated},
		{From: "Ctrl.cs::UsersController.Get", To: "annotation::csharp::HttpGet", Kind: graph.EdgeAnnotated},
	}

	require.Equal(t, 2, Detect("src/Controllers/UsersController.cs", "csharp", nodes, edges))
	assert.Equal(t, "aspnet:controller", nodes[1].Meta[MetaEntryKind])
	assert.Equal(t, "aspnet:handler", nodes[2].Meta[MetaEntryKind])
	assert.Nil(t, nodes[3].Meta, "a private helper is not a framework entry point")
	assert.Nil(t, nodes[0].Meta, "the file node is not stamped — dead helpers in it must still surface")
}

// TestDetect_DotNetControllerByBaseType: a controller that extends
// ControllerBase without an [ApiController]/[Controller] attribute is
// still an entry point.
func TestDetect_DotNetControllerByBaseType(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "H.cs::HomeController", Kind: graph.KindType, Name: "HomeController"},
	}
	edges := []*graph.Edge{
		{From: "H.cs::HomeController", To: "unresolved::ControllerBase", Kind: graph.EdgeExtends},
	}

	require.Equal(t, 1, Detect("src/HomeController.cs", "csharp", nodes, edges))
	assert.Equal(t, "aspnet:controller", nodes[0].Meta[MetaEntryKind])
}

// TestDetect_DotNetTestMethodAndAttributeSuffix: an xUnit [Fact] is an
// entry point, and the optional C# `Attribute` suffix ([ControllerAttribute])
// must match the same as the bare name.
func TestDetect_DotNetTestMethodAndAttributeSuffix(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "T.cs::Tests.It_works", Kind: graph.KindMethod, Name: "It_works"},
		{ID: "T.cs::Api", Kind: graph.KindType, Name: "Api"},
	}
	edges := []*graph.Edge{
		{From: "T.cs::Tests.It_works", To: "annotation::csharp::Fact", Kind: graph.EdgeAnnotated},
		{From: "T.cs::Api", To: "annotation::csharp::ControllerAttribute", Kind: graph.EdgeAnnotated},
	}

	require.Equal(t, 2, Detect("src/Tests.cs", "csharp", nodes, edges))
	assert.Equal(t, "xunit:test", nodes[0].Meta[MetaEntryKind])
	assert.Equal(t, "aspnet:controller", nodes[1].Meta[MetaEntryKind])
}

// TestDetect_DotNetPlainTypeNotEntry: a plain class with no entry-point
// attribute or base type must not be flagged.
func TestDetect_DotNetPlainTypeNotEntry(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "P.cs::Plain", Kind: graph.KindType, Name: "Plain"},
		{ID: "P.cs::Plain.Do", Kind: graph.KindMethod, Name: "Do"},
	}
	require.Equal(t, 0, Detect("src/Plain.cs", "csharp", nodes, nil))
	assert.Nil(t, nodes[0].Meta)
	assert.Nil(t, nodes[1].Meta)
}
