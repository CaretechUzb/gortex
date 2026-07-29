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
