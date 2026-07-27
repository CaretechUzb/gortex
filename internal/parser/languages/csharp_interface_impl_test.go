package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestCSharpExtractor_ExplicitInterfaceImpl locks the explicit interface
// implementation shape `void IFoo.Bar() {}`: the grammar keeps the name a
// bare identifier ("Bar") with the IFoo. qualifier as a separate
// explicit_interface_specifier child, so the method is extracted and
// owned by the *class* (C.Bar) — distinct from the interface's own
// declaration node (IFoo.Bar), not dropped and not fused with it.
func TestCSharpExtractor_ExplicitInterfaceImpl(t *testing.T) {
	src := []byte(`public interface IFoo {
    void Bar();
}

public class C : IFoo {
    void IFoo.Bar() { }
}
`)
	result, err := NewCSharpExtractor().Extract("X.cs", src)
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, m := range nodesOfKind(result.Nodes, graph.KindMethod) {
		ids[m.ID] = true
	}
	assert.True(t, ids["X.cs::C.Bar"],
		"explicit interface impl `void IFoo.Bar()` must be a method owned by class C")
	assert.True(t, ids["X.cs::IFoo.Bar"],
		"the interface's own declaration keeps its member node alongside the impl")
}
