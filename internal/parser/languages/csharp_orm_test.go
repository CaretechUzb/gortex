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
