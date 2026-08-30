package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A same-file partial type spells its interface paths across TWO
// declarations that share one node ID. The duplicate-declaration branch
// returned before base-list emission, so the second fragment's paths
// never reached the graph - the surviving fragment's stamp then read as
// the type's unique closure and filtered the whole family downstream
// (round-5 finding 5).
//
// Both declarations spelling `partial` is the gate: arity twins and
// other short-ID collisions must never blindly merge bases.
func TestCSharpBaseList_SameFilePartialKeepsBothFragments(t *testing.T) {
	src := []byte(`namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public interface ICrateBox : IBox<Crate> { }
    public partial class Dual : IBox<Widget> { public void Put(Widget item) { } }
    public partial class Dual : ICrateBox { public void Put(Crate item) { } }
}
`)
	res, err := NewCSharpExtractor().Extract("P.cs", src)
	require.NoError(t, err)

	targets := map[string]bool{}
	for _, e := range csharpBaseEdges(res, "P.cs::Dual") {
		targets[e.To] = true
	}
	assert.True(t, targets["unresolved::IBox"],
		"the first fragment's path must be there, got %v", targets)
	assert.True(t, targets["unresolved::ICrateBox"],
		"the SECOND fragment's path must survive node deduplication, got %v", targets)
}

// The guard: a non-partial short-ID collision (an arity twin) keeps the
// old behavior - the dropped declaration's bases stay dropped, because
// nothing proves they belong to the surviving node's type.
func TestCSharpBaseList_ArityTwinCollisionDoesNotMergeBases(t *testing.T) {
	src := []byte(`namespace App {
    public interface ITagged { }
    public class Result { }
    public class Result<T> : ITagged { }
}
`)
	res, err := NewCSharpExtractor().Extract("T.cs", src)
	require.NoError(t, err)

	assert.Empty(t, csharpBaseEdges(res, "T.cs::Result"),
		"the twin's base list must not be grafted onto the surviving bare Result")
}
