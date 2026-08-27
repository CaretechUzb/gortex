package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// detectCSharpORMModel inspects a C# class/record for a [Table]
// attribute (System.ComponentModel.DataAnnotations.Schema — the EF
// Core mapping attribute, shared by Dapper.Contrib) and emits an
// EdgeModelsTable to a synthetic KindTable node when one is found.
//
// Only the attribute form is decided at extraction time: EF's other
// two table-name sources — DbSet<T> property-name convention and
// fluent ToTable(...) configuration — live in files other than the
// entity's, so they are joined by a resolver pass over stamped facts,
// not here. A class with no [Table] attribute emits nothing.
func detectCSharpORMModel(classNode *sitter.Node, src []byte, classID, filePath string) []*graph.Edge {
	if classNode == nil {
		return nil
	}
	tableName := ""
	schema := ""
	for _, ann := range csharpCollectAttributes(classNode, src) {
		if !csharpIsTableAttr(ann.name) {
			continue
		}
		name := csharpAttrPositionalString(ann.args)
		if name == "" {
			continue
		}
		tableName = name
		schema = javaAnnotationStringArg(ann.args, "Schema")
		break
	}
	if tableName == "" {
		return nil
	}
	qualified := tableName
	if schema != "" {
		qualified = schema + "." + tableName
	}
	tableID := ormTableNodeID(qualified)
	meta := map[string]any{
		"orm":        "efcore",
		"binding":    "attribute",
		"table_name": tableName,
		"derivation": "override",
	}
	if schema != "" {
		meta["schema"] = schema
	}
	return []*graph.Edge{
		{
			From:     classID,
			To:       tableID,
			Kind:     graph.EdgeModelsTable,
			FilePath: filePath,
			Line:     int(classNode.StartPoint().Row) + 1,
			Origin:   graph.OriginASTResolved,
			Meta:     meta,
		},
	}
}

// csharpIsTableAttr reports whether an attribute name denotes [Table].
// C# lets the attribute appear qualified (Schema.Table, or the full
// namespace path) and with the explicit Attribute suffix, so compare
// the final dotted segment against both spellings.
func csharpIsTableAttr(name string) bool {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name == "Table" || name == "TableAttribute"
}

// csharpAttrPositionalStringArg matches a leading (optionally
// verbatim) string literal — [Table("name", ...)]'s table name is the
// attribute's first positional argument, unlike JPA's key=value form.
var csharpAttrPositionalStringArg = regexp.MustCompile(`^\s*@?"([^"]*)"`)

// csharpAttrPositionalString extracts the first positional string
// literal from an attribute's verbatim argument text. Non-literal
// firsts (nameof(...), constants) return "" — fail-open, no guess.
func csharpAttrPositionalString(args string) string {
	m := csharpAttrPositionalStringArg.FindStringSubmatch(args)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// emitCSharpORMEdges materialises the KindTable node + EdgeModelsTable
// edges for a C# type, mirroring emitJavaORMEdges: the per-file table
// node dedup happens in one place.
func emitCSharpORMEdges(classNode *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult) {
	for _, e := range detectCSharpORMModel(classNode, src, classID, filePath) {
		if e == nil {
			continue
		}
		if !ormTableNodeAlreadyEmitted(result, e.To) {
			schema, _ := e.Meta["schema"].(string)
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       e.To,
				Kind:     graph.KindTable,
				Name:     e.Meta["table_name"].(string),
				FilePath: filePath,
				Language: "csharp",
				Meta: map[string]any{
					"dialect": "orm",
					"schema":  schema,
					"source":  "csharp-orm",
				},
			})
		}
		result.Edges = append(result.Edges, e)
	}
}
