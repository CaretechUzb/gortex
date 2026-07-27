package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestCSharpExtractor_GenericInterfaceImplements locks the open/closed
// generic alignment the EF repository pattern depends on: a generic
// interface IRepository<T> is captured by its bare name, and BOTH the
// open implementation (Repository<T> : IRepository<T>) and a closed one
// (UserRepo : IRepository<User>) emit an implements edge to the same
// de-generic'd target "unresolved::IRepository". That short-name match
// is what lets the resolver's resolveTypeRef bind both onto the open
// interface node, so find_implementations(IRepository) returns both —
// across open and closed generic forms.
func TestCSharpExtractor_GenericInterfaceImplements(t *testing.T) {
	src := []byte(`public interface IRepository<T> {}

public class Repository<T> : IRepository<T> {}

public class UserRepo : IRepository<User> {}
`)
	result, err := NewCSharpExtractor().Extract("Repo.cs", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "IRepository", ifaces[0].Name, "generic interface is captured by its bare name")

	impls := edgesOfKind(result.Edges, graph.EdgeImplements)
	openCount := 0
	for _, e := range impls {
		if e.To == "unresolved::IRepository" {
			openCount++
		}
	}
	assert.Equal(t, 2, openCount,
		"both Repository<T> : IRepository<T> and UserRepo : IRepository<User> must target the open interface name")
}
