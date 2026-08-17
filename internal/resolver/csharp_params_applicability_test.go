package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestCSharpNarrowByApplicability_VariadicTailAcceptsBeyondDeclaredCount(t *testing.T) {
	call := &graph.Edge{Meta: map[string]any{"arg_count": 4}}
	fixed := &graph.Node{ID: "x.cs::Log.Fixed", Meta: map[string]any{
		"param_count": 3,
		"receiver":    "LogExt",
	}}
	variadic := &graph.Node{ID: "x.cs::Log.Variadic", Meta: map[string]any{
		"param_count":    3,
		"param_required": 2,
		"param_variadic": true,
		"receiver":       "LogExt",
	}}

	got := csharpNarrowByApplicability(call, []*graph.Node{fixed, variadic})
	require.Equal(t, []*graph.Node{variadic}, got)
}
