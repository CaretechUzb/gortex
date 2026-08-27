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

// TestCSharpEFMergeFluentFacts_OrderIndependent: the merge's verdict
// for an entity must not depend on fact arrival order — facts come off
// a map-ordered node scan, so every permutation of the same set must
// produce the same result.
func TestCSharpEFMergeFluentFacts_OrderIndependent(t *testing.T) {
	configA := csharpEFFluentFact{entity: "Widget", table: "w1", siteID: "a.cs::AConfig"}
	configB := csharpEFFluentFact{entity: "Widget", table: "w2", siteID: "b.cs::BConfig"}
	inline := csharpEFFluentFact{entity: "Widget", table: "w3", inline: true, siteID: "ctx.cs"}

	perms := [][]csharpEFFluentFact{
		{configA, configB, inline},
		{configA, inline, configB},
		{inline, configA, configB},
		{inline, configB, configA},
		{configB, inline, configA},
		{configB, configA, inline},
	}
	for i, p := range perms {
		out := csharpEFMergeFluentFacts(p)
		require.Len(t, out, 1, "perm %d: inline tier wins outright — config disagreement below it is irrelevant", i)
		assert.Equal(t, "w3", out[0].table, "perm %d", i)
		assert.True(t, out[0].inline, "perm %d", i)
	}

	// No inline tier: the config disagreement drops the entity, both orders.
	for i, p := range [][]csharpEFFluentFact{{configA, configB}, {configB, configA}} {
		assert.Empty(t, csharpEFMergeFluentFacts(p), "conflict perm %d", i)
	}

	// Agreeing same-tier facts: kept, and the retained site is the
	// deterministic (lowest-siteID) one regardless of order.
	configA2 := csharpEFFluentFact{entity: "Widget", table: "w1", siteID: "z.cs::ZConfig"}
	agreeA := csharpEFFluentFact{entity: "Widget", table: "w1", siteID: "a.cs::AConfig"}
	for i, p := range [][]csharpEFFluentFact{{configA2, agreeA}, {agreeA, configA2}} {
		out := csharpEFMergeFluentFacts(p)
		require.Len(t, out, 1, "agree perm %d", i)
		assert.Equal(t, "a.cs::AConfig", out[0].siteID, "agree perm %d: lowest siteID wins deterministically", i)
	}
}

// TestResolveCSharpEFCoreModels_InlineBeatsConfigClass: within the
// fluent tier, an OnModelCreating inline entry beats a config class —
// EF applies OnModelCreating statements after ApplyConfiguration.
func TestResolveCSharpEFCoreModels_InlineBeatsConfigClass(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Domain/Gadget.cs": `namespace App.Domain;

public class Gadget
{
    public int Id { get; set; }
}
`,
		"Config/GadgetConfig.cs": `using Microsoft.EntityFrameworkCore;
using Microsoft.EntityFrameworkCore.Metadata.Builders;

namespace App.Config;

public class GadgetConfig : IEntityTypeConfiguration<Gadget>
{
    public void Configure(EntityTypeBuilder<Gadget> builder)
    {
        builder.ToTable("cfg_gadgets");
    }
}
`,
		"Data/ProbeContext.cs": `using Microsoft.EntityFrameworkCore;

namespace App.Data;

public class ProbeContext : DbContext
{
    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<Gadget>().ToTable("inline_gadgets");
    }
}
`,
	})
	assert.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	models := efModelsTableEdges(g)
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::inline_gadgets", models[0].To)
}

// TestResolveCSharpEFCoreModels_EfFluentAnySliceDecodes: meta lists
// round-trip through persistence as []any — the pass must decode both
// shapes, not just the in-process []string.
func TestResolveCSharpEFCoreModels_EfFluentAnySliceDecodes(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Domain/Gadget.cs": `namespace App.Domain;

public class Gadget
{
    public int Id { get; set; }
}
`,
		"Data/ProbeContext.cs": `using Microsoft.EntityFrameworkCore;

namespace App.Data;

public class ProbeContext : DbContext
{
    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<Gadget>().ToTable("gadget_rows");
    }
}
`,
	})
	fileNode := g.GetNode("Data/ProbeContext.cs")
	require.NotNil(t, fileNode)
	entries, ok := fileNode.Meta["ef_fluent"].([]string)
	require.True(t, ok, "extractor stamps []string in-process")
	anyEntries := make([]any, len(entries))
	for i, e := range entries {
		anyEntries[i] = e
	}
	fileNode.Meta["ef_fluent"] = anyEntries

	assert.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	models := efModelsTableEdges(g)
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::gadget_rows", models[0].To)
	assert.Greater(t, models[0].Line, 1, "edge evidence carries the mapping statement's line, not the file top")
}

// TestResolveCSharpEFCoreModels_RepoPrefixedStoreSpelling: in a
// workspace store, ingest prefixes every extraction ID with the repo —
// including the extractor-minted db::orm:: table nodes. Edges the
// RESOLVER mints must use the same spelling, or one logical table
// splits into a bare and a prefixed identity depending on which
// binding path named it.
func TestResolveCSharpEFCoreModels_RepoPrefixedStoreSpelling(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "probe/Domain\\Widget.cs::Widget", Kind: graph.KindType, Name: "Widget",
		FilePath: "probe/Domain\\Widget.cs", Language: "csharp", RepoPrefix: "probe",
	})
	g.AddNode(&graph.Node{
		ID: "probe/Data\\Ctx.cs::ProbeContext", Kind: graph.KindType, Name: "ProbeContext",
		FilePath: "probe/Data\\Ctx.cs", Language: "csharp", RepoPrefix: "probe",
	})
	g.AddNode(&graph.Node{
		ID: "probe/Data\\Ctx.cs::ProbeContext.StockWidgets", Kind: graph.KindField, Name: "StockWidgets",
		FilePath: "probe/Data\\Ctx.cs", Language: "csharp", RepoPrefix: "probe", StartLine: 7,
		Meta: map[string]any{"kind": "property", "field_type": "DbSet<Widget>", "receiver": "ProbeContext"},
	})

	assert.Equal(t, 1, ResolveCSharpEFCoreModels(g))
	models := efModelsTableEdges(g)
	require.Len(t, models, 1)
	assert.Equal(t, "probe/db::orm::StockWidgets", models[0].To,
		"resolver-minted table IDs carry the entity's repo prefix, matching the ingest spelling")
	tableNode := g.GetNode("probe/db::orm::StockWidgets")
	require.NotNil(t, tableNode)
	assert.Equal(t, "probe", tableNode.RepoPrefix)
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
