package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func csharpNodeByName(t *testing.T, res *parser.ExtractionResult, name string) *graph.Node {
	t.Helper()
	for _, n := range res.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node named %q in extraction result", name)
	return nil
}

// R2 (PR #432 review): type SHAPE — array/nullable suffixes, generic
// arguments, constraints — is part of applicability. The core-name stamps
// keep their existing spelling (every downstream consumer stays valid);
// shape rides in parallel, additive stamps.

func TestCSharpExtractor_ReceiverShape_BuiltinArray(t *testing.T) {
	e := NewCSharpExtractor()
	res, err := e.Extract("Caller.cs", []byte(`namespace App {
    public class Runner {
        public void Run() {
            string[] xs = new string[1];
            xs.Foo();
        }
    }
}`))
	require.NoError(t, err)
	edge := csharpCallEdgeFrom(t, res, "Caller.cs::Runner.Run")
	assert.Equal(t, "string", edge.Meta["receiver_builtin"], "core stamp keeps the element builtin")
	assert.Equal(t, "string[]", edge.Meta["receiver_shape"], "shape stamp keeps the array structure")
}

func TestCSharpExtractor_ReceiverShape_GenericLocal(t *testing.T) {
	e := NewCSharpExtractor()
	res, err := e.Extract("Caller.cs", []byte(`using System.Collections.Generic;
namespace App {
    public class Runner {
        public void Run() {
            List<int> xs = new List<int>();
            xs.Foo();
        }
    }
}`))
	require.NoError(t, err)
	edge := csharpCallEdgeFrom(t, res, "Caller.cs::Runner.Run")
	assert.Equal(t, "List<int>", edge.Meta["receiver_shape"], "generic arguments survive in the shape stamp")
}

func TestCSharpExtractor_ThisParamShape_GenericArgs(t *testing.T) {
	e := NewCSharpExtractor()
	res, err := e.Extract("Ext.cs", []byte(`using System.Collections.Generic;
namespace App {
    public static class E {
        public static int Foo(this List<string> xs) { return 1; }
    }
}`))
	require.NoError(t, err)
	m := csharpNodeByName(t, res, "Foo")
	assert.Equal(t, "List", m.Meta["this_param_type"], "core stamp unchanged")
	assert.Equal(t, "List<string>", m.Meta["this_param_shape"], "shape stamp keeps generic args")
}

func TestCSharpExtractor_ThisParamConstraints(t *testing.T) {
	e := NewCSharpExtractor()
	res, err := e.Extract("Ext.cs", []byte(`namespace App {
    public interface ITagged {}
    public static class E {
        public static int Foo<T>(this T x) where T : ITagged { return 1; }
    }
}`))
	require.NoError(t, err)
	m := csharpNodeByName(t, res, "Foo")
	assert.Equal(t, true, m.Meta["this_param_generic"])
	assert.Equal(t, "ITagged", m.Meta["this_param_constraints"], "constraint core names ride the stamp")
}

func TestCSharpExtractor_MethodTypeParamsStamp(t *testing.T) {
	e := NewCSharpExtractor()
	res, err := e.Extract("Ext.cs", []byte(`using System.Collections.Generic;
namespace App {
    public static class E {
        public static int Foo<T>(this List<T> xs) { return 1; }
    }
}`))
	require.NoError(t, err)
	m := csharpNodeByName(t, res, "Foo")
	assert.Equal(t, "List<T>", m.Meta["this_param_shape"])
	assert.Equal(t, "T", m.Meta["method_type_params"], "the method's own type-parameter names are stamped for shape unification")
}
