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

func TestCSharpORM_ConfigOwnedTypeLambdaToTableNotStamped(t *testing.T) {
	src := `namespace Probe.Data.Config;

public class CrateSlotConfig : IEntityTypeConfiguration<CrateSlot>
{
    public void Configure(EntityTypeBuilder<CrateSlot> builder)
    {
        builder.OwnsOne(c => c.Slot, s => s.ToTable("slot_rows"));
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Config/CrateSlotConfig.cs", src)
	cfg := fix.nodesByID["Config/CrateSlotConfig.cs::CrateSlotConfig"]
	require.NotNil(t, cfg)
	assert.Equal(t, "CrateSlot", cfg.Meta["ef_config_entity"])
	_, hasTable := cfg.Meta["ef_config_table"]
	assert.False(t, hasTable,
		"an owned-type ToTable inside a lambda names the OWNED type's table, not T's — must not stamp")
}

func TestCSharpORM_ConfigChainedOwnsOneToTableNotStamped(t *testing.T) {
	src := `namespace Probe.Data.Config;

public class ParcelConfig : IEntityTypeConfiguration<Parcel>
{
    public void Configure(EntityTypeBuilder<Parcel> builder)
    {
        builder.OwnsOne(p => p.Stub).ToTable("stub_rows");
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Config/ParcelConfig.cs", src)
	cfg := fix.nodesByID["Config/ParcelConfig.cs::ParcelConfig"]
	require.NotNil(t, cfg)
	assert.Equal(t, "Parcel", cfg.Meta["ef_config_entity"])
	_, hasTable := cfg.Meta["ef_config_table"]
	assert.False(t, hasTable,
		"ToTable on an OwnsOne return names the owned type's table — the receiver is not the entity builder")
}

func TestCSharpORM_ConfigDualInterfaceRefusesEntirely(t *testing.T) {
	src := `namespace Probe.Data.Config;

public class PairConfig : IEntityTypeConfiguration<Drum>, IEntityTypeConfiguration<Spool>
{
    public void Configure(EntityTypeBuilder<Drum> builder)
    {
        builder.HasKey(d => d.Id);
    }

    public void Configure(EntityTypeBuilder<Spool> builder)
    {
        builder.ToTable("spool_rows");
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Config/PairConfig.cs", src)
	cfg := fix.nodesByID["Config/PairConfig.cs::PairConfig"]
	require.NotNil(t, cfg)
	_, hasEntity := cfg.Meta["ef_config_entity"]
	assert.False(t, hasEntity,
		"two IEntityTypeConfiguration<T> arguments: a single-entity stamp cannot say whose ToTable it found — refuse")
}

func TestCSharpORM_ConfigLambdaOnlyToTableNotStamped(t *testing.T) {
	src := `namespace Probe.Data.Config;

public class VaultConfig : IEntityTypeConfiguration<Vault>
{
    public void Configure(EntityTypeBuilder<Vault> builder)
    {
        builder.ToTable(tb => tb.HasComment("keyless vault"));
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Config/VaultConfig.cs", src)
	cfg := fix.nodesByID["Config/VaultConfig.cs::VaultConfig"]
	require.NotNil(t, cfg)
	assert.Equal(t, "Vault", cfg.Meta["ef_config_entity"])
	_, hasTable := cfg.Meta["ef_config_table"]
	assert.False(t, hasTable, "lambda-only ToTable overload names no table — the string inside the lambda is a comment")
}

func TestCSharpORM_InlineChainedOwnsOneNotStamped(t *testing.T) {
	src := `namespace Probe.Data;

public class ShipContext : DbContext
{
    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<Parcel>().OwnsOne(p => p.Stub).ToTable("stub_split");
        modelBuilder.Entity<Parcel>().ToTable("parcels_main");
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Data/ShipContext.cs", src)
	fileNode := fix.nodesByID["Data/ShipContext.cs"]
	require.NotNil(t, fileNode)
	assert.Equal(t, []string{"Parcel|parcels_main||table|8"}, fileNode.Meta["ef_fluent"],
		"the OwnsOne chain's ToTable names the OWNED type's table — the walk must stop at subject-changing links")
}

func TestCSharpORM_OnModelCreatingFluentFileStamp(t *testing.T) {
	src := `using Microsoft.EntityFrameworkCore;

namespace Probe.Data;

public class ProbeContext : DbContext
{
    public DbSet<Widget> Widgets { get; set; }

    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<Widget>().ToTable("widget_master", "core");
        modelBuilder.Entity<Domain.Gadget>().HasKey(g => g.Id);
        modelBuilder.Entity<Domain.Gadget>().ToView("gadget_view");
        modelBuilder.Entity<Sprocket>().HasIndex(s => s.Serial);
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Data/ProbeContext.cs", src)
	fileNode := fix.nodesByID["Data/ProbeContext.cs"]
	require.NotNil(t, fileNode)
	assert.Equal(t, []string{
		"Widget|widget_master|core|table|11",
		"Gadget|gadget_view||view|13",
	}, fileNode.Meta["ef_fluent"],
		"one entry per Entity<T> chain ending in a literal ToTable/ToView, line-stamped; chains without one stamp nothing")
}

func TestCSharpORM_FluentOutsideOnModelCreatingNotStamped(t *testing.T) {
	src := `namespace Probe.Data;

public class MapHelper
{
    public void Wire(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<Widget>().ToTable("widgets_elsewhere");
    }
}
`
	fix := runCSharpExtractFixtureORM(t, "Data/MapHelper.cs", src)
	fileNode := fix.nodesByID["Data/MapHelper.cs"]
	require.NotNil(t, fileNode)
	_, has := fileNode.Meta["ef_fluent"]
	assert.False(t, has, "only OnModelCreating bodies are scanned in v1")
}

func TestCSharpORM_NameofTableArgRefused(t *testing.T) {
	src := `using System.ComponentModel.DataAnnotations.Schema;

namespace Probe.Core.Domain;

[Table(nameof(SlateBin))]
public class SlateBin
{
    public int Id { get; set; }
}
`
	fix := runCSharpExtractFixtureORM(t, "Models/SlateBin.cs", src)
	assert.Empty(t, fix.edgesByKind[graph.EdgeModelsTable],
		"a non-literal table name (nameof, constants) stamps nothing — fail-open, no guess")
}

func TestCSharpORM_VerbatimStringAndAttributeSuffix(t *testing.T) {
	src := `using System.ComponentModel.DataAnnotations.Schema;

namespace Probe.Core.Domain;

[TableAttribute(@"vault_rows")]
public class VaultRow
{
    public int Id { get; set; }
}
`
	fix := runCSharpExtractFixtureORM(t, "Models/VaultRow.cs", src)
	models := fix.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1, "the explicit Attribute suffix and a verbatim string are both ordinary spellings")
	assert.Equal(t, "db::orm::vault_rows", models[0].To)
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
