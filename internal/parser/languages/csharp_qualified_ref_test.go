package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestCSharpExtractor_QualifiedTypeRefKeepsFQN: a qualified spelling
// names its namespace in the source. The bare name stays the target;
// the dotted spelling rides Meta["target_fqn"] for the resolver.
func TestCSharpExtractor_QualifiedTypeRefKeepsFQN(t *testing.T) {
	src := []byte(`public class Wiring {
    public void Configure(MapperConfig cfg) {
        cfg.AddProfile<Shared.Reporting.SalesProfile>();
        cfg.AddProfile<LocalProfile>();
        var x = new Data.Models.Order();
    }
}
`)
	res, err := NewCSharpExtractor().Extract("w.cs", src)
	require.NoError(t, err)

	var qualified, bare, created *graph.Edge
	for _, e := range res.Edges {
		switch e.To {
		case "unresolved::SalesProfile":
			if e.Kind == graph.EdgeReferences {
				qualified = e
			}
		case "unresolved::LocalProfile":
			if e.Kind == graph.EdgeReferences {
				bare = e
			}
		case "unresolved::Order":
			if e.Kind == graph.EdgeInstantiates {
				created = e
			}
		}
	}

	require.NotNil(t, qualified, "qualified generic arg must emit a reference")
	assert.Equal(t, "Shared.Reporting.SalesProfile", qualified.Meta["target_fqn"])

	require.NotNil(t, bare, "bare generic arg must emit a reference")
	assert.Nil(t, bare.Meta["target_fqn"], "bare spellings carry no qualifier")

	require.NotNil(t, created, "qualified new must emit an instantiates edge")
	assert.Equal(t, "Data.Models.Order", created.Meta["target_fqn"])
}

// TestCSharpExtractor_QualifiedBaseListKeepsFQN: base-list spellings
// resolve through resolveTypeRef, which consumes the same qualifier
// evidence — extends and implements must carry it too.
func TestCSharpExtractor_QualifiedBaseListKeepsFQN(t *testing.T) {
	src := []byte(`public class Repo : Data.Access.RepoBase, Core.Abstractions.IStore
{
}
`)
	res, err := NewCSharpExtractor().Extract("r.cs", src)
	require.NoError(t, err)

	var ext, impl *graph.Edge
	for _, e := range res.Edges {
		switch {
		case e.Kind == graph.EdgeExtends && e.To == "unresolved::RepoBase":
			ext = e
		case e.Kind == graph.EdgeImplements && e.To == "unresolved::IStore":
			impl = e
		}
	}
	require.NotNil(t, ext, "qualified base class must emit extends")
	assert.Equal(t, "Data.Access.RepoBase", ext.Meta["target_fqn"])
	require.NotNil(t, impl, "qualified interface must emit implements")
	assert.Equal(t, "Core.Abstractions.IStore", impl.Meta["target_fqn"])
}
