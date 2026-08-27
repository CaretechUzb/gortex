package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func runCSharpExtractFixtureORM(t *testing.T, filePath, src string) *extractedFixture {
	t.Helper()
	result, err := NewCSharpExtractor().Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return foldFixture(result)
}

func TestCSharpORM_TableAttributeOverride(t *testing.T) {
	src := `using System.ComponentModel.DataAnnotations.Schema;

namespace Probe.Core.Domain;

[Table("stock_crates")]
public class StockCrate
{
    public int Id { get; set; }
}
`
	fix := runCSharpExtractFixtureORM(t, "Models/StockCrate.cs", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1, "StockCrate should produce a models_table edge from [Table]")
	assert.Equal(t, "Models/StockCrate.cs::StockCrate", models[0].From)
	assert.Equal(t, "db::orm::stock_crates", models[0].To)
	assert.Equal(t, "efcore", models[0].Meta["orm"])
	assert.Equal(t, "attribute", models[0].Meta["binding"])
	assert.Equal(t, "stock_crates", models[0].Meta["table_name"])
	assert.Equal(t, "override", models[0].Meta["derivation"])

	tableNode := fix.nodesByID["db::orm::stock_crates"]
	require.NotNil(t, tableNode, "the KindTable node must be materialised alongside the edge")
	assert.Equal(t, graph.KindTable, tableNode.Kind)
	assert.Equal(t, "stock_crates", tableNode.Name)
	assert.Equal(t, "orm", tableNode.Meta["dialect"])
	assert.Equal(t, "csharp-orm", tableNode.Meta["source"])
	assert.Equal(t, "", tableNode.Meta["schema"])
}

func TestCSharpORM_TableAttributeWithSchema(t *testing.T) {
	src := `using System.ComponentModel.DataAnnotations.Schema;

namespace Probe.Core.Domain;

[Table("bin_items", Schema = "audit")]
public class BinItem
{
    public int Id { get; set; }
}
`
	fix := runCSharpExtractFixtureORM(t, "Models/BinItem.cs", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::audit.bin_items", models[0].To)
	assert.Equal(t, "bin_items", models[0].Meta["table_name"])
	assert.Equal(t, "audit", models[0].Meta["schema"])
	assert.Equal(t, "override", models[0].Meta["derivation"])

	tableNode := fix.nodesByID["db::orm::audit.bin_items"]
	require.NotNil(t, tableNode)
	assert.Equal(t, "bin_items", tableNode.Name)
	assert.Equal(t, "audit", tableNode.Meta["schema"])
}

func TestCSharpORM_ConfigClassStampsEntityAndTable(t *testing.T) {
	src := `using Microsoft.EntityFrameworkCore;
using Microsoft.EntityFrameworkCore.Metadata.Builders;

namespace Probe.Data.Config;

public class WidgetConfig : IEntityTypeConfiguration<Widget>
{
    public void Configure(EntityTypeBuilder<Widget> builder)
    {
        builder.ToTable("widgets_v2", "sales");
        builder.HasKey(w => w.Id);
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Config/WidgetConfig.cs", src)
	cfg := fix.nodesByID["Config/WidgetConfig.cs::WidgetConfig"]
	require.NotNil(t, cfg)
	assert.Equal(t, "Widget", cfg.Meta["ef_config_entity"])
	assert.Equal(t, "widgets_v2", cfg.Meta["ef_config_table"])
	assert.Equal(t, "sales", cfg.Meta["ef_config_schema"])
	assert.Equal(t, "table", cfg.Meta["ef_config_relation"])
}

func TestCSharpORM_ConfigClassToViewStampsViewRelation(t *testing.T) {
	src := `namespace Probe.Data.Config;

public class TallyConfig : Microsoft.EntityFrameworkCore.IEntityTypeConfiguration<Domain.CrateTally>
{
    public void Configure(EntityTypeBuilder<Domain.CrateTally> builder)
    {
        builder.ToView("crate_tallies");
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Config/TallyConfig.cs", src)
	cfg := fix.nodesByID["Config/TallyConfig.cs::TallyConfig"]
	require.NotNil(t, cfg)
	assert.Equal(t, "CrateTally", cfg.Meta["ef_config_entity"],
		"qualified iface and qualified entity arg both reduce to final segments")
	assert.Equal(t, "crate_tallies", cfg.Meta["ef_config_table"])
	assert.Equal(t, "view", cfg.Meta["ef_config_relation"])
	_, hasSchema := cfg.Meta["ef_config_schema"]
	assert.False(t, hasSchema, "no schema arg, no schema stamp")
}

func TestCSharpORM_ConfigClassWithoutToTableStampsEntityOnly(t *testing.T) {
	src := `namespace Probe.Data.Config;

public class GadgetConfig : IEntityTypeConfiguration<Gadget>
{
    public void Configure(EntityTypeBuilder<Gadget> builder)
    {
        builder.HasIndex(g => g.Serial);
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Config/GadgetConfig.cs", src)
	cfg := fix.nodesByID["Config/GadgetConfig.cs::GadgetConfig"]
	require.NotNil(t, cfg)
	assert.Equal(t, "Gadget", cfg.Meta["ef_config_entity"])
	_, hasTable := cfg.Meta["ef_config_table"]
	assert.False(t, hasTable, "no ToTable/ToView call, no table stamp")
}

func TestCSharpORM_PlainClassIgnored(t *testing.T) {
	src := `namespace Probe.Core;

public class PlainService
{
    public void Handle() { }
}
`
	fix := runCSharpExtractFixtureORM(t, "Services/PlainService.cs", src)
	assert.Empty(t, fix.edgesByKind[graph.EdgeModelsTable])
}
