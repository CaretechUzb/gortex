package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestCSharpExtractor_ParamsGrammarShapes(t *testing.T) {
	src := []byte(`namespace App {
    public class ParamsCases {
        public void Objects(params object[] values) { }
        public void Ints(params int[] values) { }
        public void Nullable(params object?[] values) { }
        public void Span(params ReadOnlySpan<int> values) { }
        public void Generic(params List<int> values) { }
    }
}
`)
	result, err := NewCSharpExtractor().Extract("Params.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}
	for _, method := range []string{"Objects", "Ints", "Nullable", "Span", "Generic"} {
		ownerID := "Params.cs::ParamsCases." + method
		owner := byID[ownerID]
		require.NotNil(t, owner, method)
		assert.Equal(t, 1, owner.Meta["param_count"], method)
		assert.Equal(t, 0, owner.Meta["param_required"], method)
		assert.Equal(t, true, owner.Meta["param_variadic"], method)

		param := byID[ownerID+"#param:values@0"]
		require.NotNil(t, param, method)
		assert.Equal(t, 0, param.Meta["position"], method)
		assert.Equal(t, true, param.Meta["variadic"], method)
	}
}

func TestCSharpExtractor_DiscardEmitsParamNode(t *testing.T) {
	src := []byte(`namespace App {
    public class Handler {
        public void Handle(int _, string name) { }
    }
}
`)
	result, err := NewCSharpExtractor().Extract("Handler.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}
	discard := byID["Handler.cs::Handler.Handle#param:_@0"]
	require.NotNil(t, discard)
	assert.Equal(t, "int", discard.Meta["type"])
	assert.Equal(t, 0, discard.Meta["position"])
}
