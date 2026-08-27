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

// TestResolveCSharpEFCoreModels_FluentConfigRewiresAttributeEdge: the
// precedence is fluent > attribute — a config class's ToTable rewires
// the extractor's attribute edge in place, go_orm's override model.
func TestResolveCSharpEFCoreModels_FluentConfigRewiresAttributeEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Domain/Widget.cs": `using System.ComponentModel.DataAnnotations.Schema;

namespace App.Domain;

[Table("attr_widgets")]
public class Widget
{
    public int Id { get; set; }
}
`,
		"Config/WidgetConfig.cs": `using Microsoft.EntityFrameworkCore;
using Microsoft.EntityFrameworkCore.Metadata.Builders;

namespace App.Config;

public class WidgetConfig : IEntityTypeConfiguration<Widget>
{
    public void Configure(EntityTypeBuilder<Widget> builder)
    {
        builder.ToTable("fluent_widgets", "sales");
    }
}
`,
	})
	n := ResolveCSharpEFCoreModels(g)
	assert.Equal(t, 1, n, "the rewire counts as resolution work")

	models := efModelsTableEdges(g)
	require.Len(t, models, 1, "rewired in place — never a second edge")
	e := models[0]
	assert.Equal(t, "Domain/Widget.cs::Widget", e.From)
	assert.Equal(t, "db::orm::sales.fluent_widgets", e.To)
	assert.Equal(t, "fluent", e.Meta["binding"])
	assert.Equal(t, "fluent_widgets", e.Meta["table_name"])
	assert.Equal(t, "sales", e.Meta["schema"])
	assert.Equal(t, "override", e.Meta["derivation"])
	require.NotNil(t, g.GetNode("db::orm::sales.fluent_widgets"))

	assert.Equal(t, 0, ResolveCSharpEFCoreModels(g), "second run is a no-op")
	assert.Len(t, efModelsTableEdges(g), 1)
}

// TestResolveCSharpEFCoreModels_InlineFluentBeatsDbSet: an
// OnModelCreating Entity<T>().ToTable wins over the DbSet property
// convention for the same entity — one edge, fluent-bound; a ToView
// carries relation=view.
func TestResolveCSharpEFCoreModels_InlineFluentBeatsDbSet(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Domain/Gadget.cs": `namespace App.Domain;

public class Gadget
{
    public int Id { get; set; }
}
`,
		"Domain/CrateTally.cs": `namespace App.Domain;

public class CrateTally
{
    public int Total { get; set; }
}
`,
		"Data/ProbeContext.cs": `using Microsoft.EntityFrameworkCore;

namespace App.Data;

public class ProbeContext : DbContext
{
    public DbSet<Gadget> Gadgets { get; set; }
    public DbSet<CrateTally> Tallies { get; set; }

    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<Gadget>().ToTable("gadget_rows");
        modelBuilder.Entity<CrateTally>().ToView("crate_tallies");
    }
}
`,
	})
	n := ResolveCSharpEFCoreModels(g)
	assert.Equal(t, 2, n)

	byFrom := map[string]*graph.Edge{}
	for _, e := range efModelsTableEdges(g) {
		require.NotContains(t, byFrom, e.From, "one edge per entity")
		byFrom[e.From] = e
	}
	require.Len(t, byFrom, 2)

	ge := byFrom["Domain/Gadget.cs::Gadget"]
	require.NotNil(t, ge)
	assert.Equal(t, "db::orm::gadget_rows", ge.To, "fluent beats the DbSet convention name")
	assert.Equal(t, "fluent", ge.Meta["binding"])
	assert.Equal(t, "override", ge.Meta["derivation"])

	te := byFrom["Domain/CrateTally.cs::CrateTally"]
	require.NotNil(t, te)
	assert.Equal(t, "db::orm::crate_tallies", te.To)
	assert.Equal(t, "view", te.Meta["relation"])
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
