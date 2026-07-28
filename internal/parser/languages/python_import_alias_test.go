package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func pyEdgeTargets(edges []*graph.Edge, kind graph.EdgeKind) []string {
	var out []string
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e.To)
		}
	}
	return out
}

func TestPyImports_AliasedImportIsNotDropped(t *testing.T) {
	// `import numpy as np` puts the module name in an `aliased_import`
	// node, so a query anchored on `(dotted_name)` never matched it — and
	// that is how the entire numpy/pandas idiom is written.
	src := []byte(`import numpy as np


def go():
    np.array([1])
`)
	e := NewPythonExtractor()
	result, err := e.Extract("calc.py", src)
	require.NoError(t, err)

	imports := nodesOfKind(result.Nodes, graph.KindImport)
	require.Len(t, imports, 1)
	assert.Equal(t, "numpy", imports[0].Meta["path"])
	assert.Equal(t, "np", imports[0].Meta["alias"])

	assert.Contains(t, pyEdgeTargets(result.Edges, graph.EdgeImports),
		"unresolved::import::numpy")
	assert.Contains(t, pyEdgeTargets(result.Edges, graph.EdgeCalls),
		"unresolved::extern::numpy::array",
		"the aliased receiver must attribute the call to its module")
}

func TestPyImports_EveryNameInAMultiImportEmitsANode(t *testing.T) {
	src := []byte(`import os.path, numpy as np, json
`)
	e := NewPythonExtractor()
	result, err := e.Extract("multi.py", src)
	require.NoError(t, err)

	paths := map[string]bool{}
	for _, n := range nodesOfKind(result.Nodes, graph.KindImport) {
		paths[n.Meta["path"].(string)] = true
	}
	assert.Equal(t, map[string]bool{"os.path": true, "numpy": true, "json": true}, paths,
		"one statement can bind several modules; each needs its own import node")
}

func TestPyImports_PlainImportStillBindsItsRootName(t *testing.T) {
	src := []byte(`import os.path


def go():
    os.path.join("a", "b")
`)
	e := NewPythonExtractor()
	result, err := e.Extract("paths.py", src)
	require.NoError(t, err)

	assert.Contains(t, pyEdgeTargets(result.Edges, graph.EdgeCalls),
		"unresolved::extern::os.path::join",
		"`import os.path` binds `os`, so os.path.join resolves through it")
}
