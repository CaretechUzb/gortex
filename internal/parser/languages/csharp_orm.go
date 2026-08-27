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

// csharpEFConfigIfaceArg pulls the single type argument out of an
// IEntityTypeConfiguration<T> base-list entry. Text-level on purpose:
// the base list is one short line, and the extractor needs only T's
// name — qualification is stripped after the match.
var csharpEFConfigIfaceArg = regexp.MustCompile(`IEntityTypeConfiguration\s*<\s*([^<>,]+?)\s*>`)

// stampCSharpEFConfig detects an EF Core entity-configuration class —
// one implementing IEntityTypeConfiguration<T> — and stamps the facts
// a resolver pass needs to join fluent table mapping to the entity:
//
//	ef_config_entity    T's final name segment (always, when detected)
//	ef_config_table     first ToTable/ToView string arg (when present)
//	ef_config_relation  "table" or "view" (with ef_config_table)
//	ef_config_schema    second string arg (when present)
//
// The entity class, the config class, and the DbContext registration
// live in different files, so the extractor only records what this
// file proves; the cross-file join happens at resolve time.
func stampCSharpEFConfig(decl *sitter.Node, src []byte, meta map[string]any) {
	if decl == nil {
		return
	}
	var baseList *sitter.Node
	for i, _nc := 0, int(decl.ChildCount()); i < _nc; i++ {
		if c := decl.Child(i); c != nil && c.Type() == "base_list" {
			baseList = c
			break
		}
	}
	if baseList == nil {
		return
	}
	m := csharpEFConfigIfaceArg.FindStringSubmatch(baseList.Content(src))
	if len(m) < 2 {
		return
	}
	entity := strings.TrimSpace(m[1])
	if i := strings.LastIndexByte(entity, '.'); i >= 0 {
		entity = entity[i+1:]
	}
	entity = strings.TrimPrefix(entity, "@")
	if entity == "" {
		return
	}
	meta["ef_config_entity"] = entity
	table, schema, relation := csharpEFConfigTableCall(decl, src)
	if table == "" {
		return
	}
	meta["ef_config_table"] = table
	meta["ef_config_relation"] = relation
	if schema != "" {
		meta["ef_config_schema"] = schema
	}
}

// csharpEFConfigTableCall finds the first builder.ToTable(...) or
// builder.ToView(...) invocation in the declaration's subtree and
// returns its literal name/schema arguments. The first argument must
// itself be a string literal — the lambda-only ToTable overloads
// configure without naming, so a non-literal first argument returns
// nothing rather than fishing a string out of a nested lambda.
func csharpEFConfigTableCall(decl *sitter.Node, src []byte) (table, schema, relation string) {
	walkAST(decl, func(n *sitter.Node) bool {
		if table != "" {
			return false
		}
		t, s, r, ok := csharpEFTableViewArgs(n, src)
		if !ok {
			return true
		}
		table, schema, relation = t, s, r
		return false
	})
	return table, schema, relation
}

// csharpEFTableViewArgs recognises an invocation node as a
// ToTable/ToView call with a literal name and returns its arguments.
// The first argument must itself be a string literal — the
// lambda-only overloads configure without naming, so a non-literal
// first argument is not a table fact.
func csharpEFTableViewArgs(n *sitter.Node, src []byte) (table, schema, relation string, ok bool) {
	if n.Type() != "invocation_expression" {
		return "", "", "", false
	}
	fn := n.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_access_expression" {
		return "", "", "", false
	}
	name := ""
	if nm := fn.ChildByFieldName("name"); nm != nil {
		name = nm.Content(src)
	}
	if name != "ToTable" && name != "ToView" {
		return "", "", "", false
	}
	args := n.ChildByFieldName("arguments")
	if args == nil {
		return "", "", "", false
	}
	var lits []string
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		lit := csharpAttrPositionalString(a.Content(src))
		if i == 0 && lit == "" {
			return "", "", "", false
		}
		lits = append(lits, lit)
	}
	if len(lits) == 0 || lits[0] == "" {
		return "", "", "", false
	}
	table = lits[0]
	if len(lits) > 1 {
		schema = lits[1]
	}
	relation = "table"
	if name == "ToView" {
		relation = "view"
	}
	return table, schema, relation, true
}

// csharpEFEntityGenericArg matches the Entity<T> generic-name link of
// a modelBuilder fluent chain.
var csharpEFEntityGenericArg = regexp.MustCompile(`^Entity\s*<\s*([^<>,]+?)\s*>$`)

// csharpEFEntityFromChain walks a ToTable/ToView call's receiver
// chain (`modelBuilder.Entity<T>().HasKey(...).ToTable(...)`) down to
// the Entity<T> link and returns T's final name segment, or "" when
// the chain never names an entity.
func csharpEFEntityFromChain(fn *sitter.Node, src []byte) string {
	expr := fn.ChildByFieldName("expression")
	for expr != nil {
		switch expr.Type() {
		case "invocation_expression":
			expr = expr.ChildByFieldName("function")
		case "member_access_expression":
			if nm := expr.ChildByFieldName("name"); nm != nil && nm.Type() == "generic_name" {
				if m := csharpEFEntityGenericArg.FindStringSubmatch(nm.Content(src)); len(m) >= 2 {
					entity := strings.TrimSpace(m[1])
					if i := strings.LastIndexByte(entity, '.'); i >= 0 {
						entity = entity[i+1:]
					}
					return strings.TrimPrefix(entity, "@")
				}
			}
			expr = expr.ChildByFieldName("expression")
		default:
			return ""
		}
	}
	return ""
}

// stampCSharpEFFluent records the OnModelCreating inline fluent
// mapping facts on the file node as Meta["ef_fluent"], one
// "entity|table|schema|relation" entry per Entity<T> chain that ends
// in a literal ToTable/ToView. Only OnModelCreating bodies are
// scanned: that is where EF looks, and a helper method reached from
// there is a cross-file/cross-method chase the extractor stays out
// of. The resolver joins entries to entity class nodes by name, same
// as the config-class stamps.
func stampCSharpEFFluent(root *sitter.Node, src []byte, fileNode *graph.Node) {
	var entries []string
	seen := map[string]bool{}
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "method_declaration" {
			return
		}
		if nm := n.ChildByFieldName("name"); nm == nil || nm.Content(src) != "OnModelCreating" {
			return
		}
		body := n.ChildByFieldName("body")
		if body == nil {
			return
		}
		walkAST(body, func(inv *sitter.Node) bool {
			table, schema, relation, ok := csharpEFTableViewArgs(inv, src)
			if !ok {
				return true
			}
			entity := csharpEFEntityFromChain(inv.ChildByFieldName("function"), src)
			if entity == "" {
				return true
			}
			entry := entity + "|" + table + "|" + schema + "|" + relation
			if !seen[entry] {
				seen[entry] = true
				entries = append(entries, entry)
			}
			return true
		})
	})
	if len(entries) == 0 {
		return
	}
	if fileNode.Meta == nil {
		fileNode.Meta = map[string]any{}
	}
	fileNode.Meta["ef_fluent"] = entries
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
