package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPyTopLevelVar_ModuleAssignmentsBecomeNodes(t *testing.T) {
	src := []byte(`from flask import Flask

VERSION = "1.0"
app = Flask(__name__)
_private = 3


def handler():
    local_only = 1
    return local_only


class Config:
    attr = 5
`)
	e := NewPythonExtractor()
	result, err := e.Extract("settings.py", src)
	require.NoError(t, err)

	names := map[string]bool{}
	for _, n := range nodesOfKind(result.Nodes, graph.KindVariable) {
		names[n.Name] = true
		assert.Equal(t, "settings.py::"+n.Name, n.ID)
	}
	assert.Equal(t, map[string]bool{"VERSION": true, "app": true}, names,
		"only public module-level bindings become variable nodes")

	var defines int
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeDefines && ed.To == "settings.py::app" {
			defines++
		}
	}
	assert.Equal(t, 1, defines, "the file defines the module-level binding")
}

func TestPyTopLevelVar_RealDefinitionKeepsTheSharedID(t *testing.T) {
	// `seen` is shared by the class, function and variable emitters, and
	// they all key on `file::Name`. A variable emitted inline would take
	// the id first and delete the class from the graph outright.
	src := []byte(`Config = load()


class Config:
    pass


Handler = 1


def Handler():
    pass
`)
	e := NewPythonExtractor()
	result, err := e.Extract("shadow.py", src)
	require.NoError(t, err)

	require.Len(t, nodesOfKind(result.Nodes, graph.KindType), 1,
		"the class must survive a same-named module binding above it")
	require.Len(t, nodesOfKind(result.Nodes, graph.KindFunction), 1,
		"the function must survive a same-named module binding above it")
	assert.Empty(t, nodesOfKind(result.Nodes, graph.KindVariable),
		"the binding yields to the real definition rather than duplicating its id")
}

func TestPyTopLevelVar_NestedAssignmentsAreNotModuleLevel(t *testing.T) {
	// A `try:`/`if:` block is a suite, not the module, so an assignment
	// inside one is deliberately out of scope for this pass.
	src := []byte(`try:
    import ujson as json_impl
except ImportError:
    json_impl = None


def f():
    x = 1
`)
	e := NewPythonExtractor()
	result, err := e.Extract("compat.py", src)
	require.NoError(t, err)

	for _, n := range nodesOfKind(result.Nodes, graph.KindVariable) {
		assert.NotEqual(t, "x", n.Name, "function locals are not module bindings")
	}
}
