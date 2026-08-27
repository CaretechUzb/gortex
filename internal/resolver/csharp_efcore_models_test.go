package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// efModelsTableEdges collects every models_table edge in the store.
func efModelsTableEdges(g graph.Store) []*graph.Edge {
	var out []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeModelsTable) {
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

// TestResolveCSharpEFCoreModels_DbSetConventionBindsPropertyName pins
// EF's actual convention: the table name is the DbSet PROPERTY name,
// verbatim — not a pluralization of the entity class name.
func TestResolveCSharpEFCoreModels_DbSetConventionBindsPropertyName(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Domain/Widget.cs": `namespace App.Domain;

public class Widget
{
    public int Id { get; set; }
}
`,
		"Data/ProbeContext.cs": `using Microsoft.EntityFrameworkCore;

namespace App.Data;

public class ProbeContext : DbContext
{
    public DbSet<Widget> StockWidgets { get; set; }
}
`,
	})
	n := ResolveCSharpEFCoreModels(g)
	assert.Equal(t, 1, n)

	models := efModelsTableEdges(g)
	require.Len(t, models, 1)
	e := models[0]
	assert.Equal(t, "Domain/Widget.cs::Widget", e.From)
	assert.Equal(t, "db::orm::StockWidgets", e.To,
		"table = DbSet property name verbatim, never a pluralized class name")
	assert.Equal(t, "efcore", e.Meta["orm"])
	assert.Equal(t, "dbset", e.Meta["binding"])
	assert.Equal(t, "StockWidgets", e.Meta["table_name"])
	assert.Equal(t, "convention", e.Meta["derivation"])

	tableNode := g.GetNode("db::orm::StockWidgets")
	require.NotNil(t, tableNode, "the KindTable node is minted with the edge")
	assert.Equal(t, graph.KindTable, tableNode.Kind)
	assert.Equal(t, "orm", tableNode.Meta["dialect"])
	assert.Equal(t, "csharp-orm", tableNode.Meta["source"])

	assert.Equal(t, 0, ResolveCSharpEFCoreModels(g), "second run is a no-op")
	assert.Len(t, efModelsTableEdges(g), 1, "idempotent: no duplicate edges")
}

// TestResolveCSharpEFCoreModels_AmbiguousEntityNameSkips: the join is
// by unique class name — two classes sharing the entity's name means
// the pass refuses to guess and emits nothing.
func TestResolveCSharpEFCoreModels_AmbiguousEntityNameSkips(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Domain/Widget.cs": `namespace App.Domain;

public class Widget
{
    public int Id { get; set; }
}
`,
		"Legacy/Widget.cs": `namespace App.Legacy;

public class Widget
{
    public string Tag { get; set; }
}
`,
		"Data/ProbeContext.cs": `using Microsoft.EntityFrameworkCore;

namespace App.Data;

public class ProbeContext : DbContext
{
    public DbSet<Widget> Widgets { get; set; }
}
`,
	})
	assert.Equal(t, 0, ResolveCSharpEFCoreModels(g))
	assert.Empty(t, efModelsTableEdges(g))
}
