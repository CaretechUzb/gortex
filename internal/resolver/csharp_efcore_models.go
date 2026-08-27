package resolver

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveCSharpEFCoreModels joins the EF Core mapping facts the C#
// extractor stamps into models_table edges. The extractor decides only
// the in-file form — a [Table] attribute on the entity itself; the two
// cross-file forms land here:
//
//   - DbSet<T> convention: EF names the table after the DbSet PROPERTY
//     (verbatim — not a pluralization of the class name), and the
//     property lives on the DbContext, not the entity.
//
//   - Fluent configuration: an IEntityTypeConfiguration<T> class's
//     ToTable/ToView (stamped as ef_config_* class meta) or an
//     OnModelCreating Entity<T>() chain (stamped as ef_fluent file
//     meta) names the table from yet another file.
//
// Precedence is fluent > attribute > dbset: a fluent fact REWIRES an
// existing attribute edge in place (go_orm's override model) rather
// than adding a second edge, and an entity a fluent fact claimed never
// falls through to the DbSet convention. Within the fluent tier an
// inline OnModelCreating entry beats a config class (EF applies
// OnModelCreating statements after ApplyConfiguration); two
// same-tier facts that disagree drop the entity entirely.
//
// Facts join to entity classes by unique class name within the same
// boundary — an ambiguous name (two classes called Widget) skips
// rather than guesses. An entity that already carries a models_table
// edge at its final target is left alone, which is also what makes
// the pass idempotent.
//
// Returns the number of edges synthesized or rewired.
func ResolveCSharpEFCoreModels(g graph.Store) int {
	if g == nil {
		return 0
	}
	classesByName := map[string][]*graph.Node{}
	var dbsets []csharpDbSetFact
	var fluents []csharpEFFluentFact
	for _, n := range nodesByKindsOrAll(g, graph.KindType, graph.KindField, graph.KindFile) {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindFile {
			fluents = append(fluents, csharpEFInlineFactsFromFile(n)...)
			continue
		}
		if !strings.EqualFold(n.Language, "csharp") {
			continue
		}
		switch n.Kind {
		case graph.KindType:
			classesByName[n.Name] = append(classesByName[n.Name], n)
			if f, ok := csharpEFConfigFactFromNode(n); ok {
				fluents = append(fluents, f)
			}
		case graph.KindField:
			if f, ok := csharpDbSetFactFromNode(n); ok {
				dbsets = append(dbsets, f)
			}
		}
	}
	if len(dbsets) == 0 && len(fluents) == 0 {
		return 0
	}

	// Entities that already model a table: the attribute binding was
	// decided at extraction, and a prior run of this pass already
	// landed. Fluent facts may rewire these; the DbSet leg never
	// touches them.
	mapped := map[string]bool{}
	edgesByFrom := map[string][]*graph.Edge{}
	for e := range g.EdgesByKind(graph.EdgeModelsTable) {
		if e != nil {
			mapped[e.From] = true
			edgesByFrom[e.From] = append(edgesByFrom[e.From], e)
		}
	}

	resolved := 0

	// Fluent tier first, so its claims shadow the DbSet convention.
	var reindex []graph.EdgeReindex
	for _, f := range csharpEFMergeFluentFacts(fluents) {
		cands := sameBoundaryCandidates(g, f.siteID, classesByName[f.entity])
		if len(cands) != 1 {
			continue
		}
		cls := cands[0]
		tableID := csharpEFTableNodeID(f.table, f.schema)
		meta := map[string]any{
			"orm":        "efcore",
			"binding":    "fluent",
			"table_name": f.table,
			"derivation": "override",
		}
		if f.schema != "" {
			meta["schema"] = f.schema
		}
		if f.relation != "" {
			meta["relation"] = f.relation
		}
		if es := edgesByFrom[cls.ID]; len(es) > 0 {
			e := es[0]
			if e.To == tableID {
				continue
			}
			csharpEFEnsureTableNode(g, tableID, f.table, f.schema, f.filePath)
			oldTo := e.To
			e.To = tableID
			e.Origin = graph.OriginASTInferred
			e.Confidence = ConfidenceTyped
			e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeModelsTable, ConfidenceTyped)
			e.Meta = meta
			StampSynthesizedTyped(e, SynthCSharpEFCoreModels)
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			resolved++
			continue
		}
		if mapped[cls.ID] {
			continue
		}
		mapped[cls.ID] = true
		csharpEFEnsureTableNode(g, tableID, f.table, f.schema, f.filePath)
		e := &graph.Edge{
			From: cls.ID, To: tableID, Kind: graph.EdgeModelsTable,
			FilePath:        f.filePath,
			Line:            f.line,
			Origin:          graph.OriginASTInferred,
			Confidence:      ConfidenceTyped,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeModelsTable, ConfidenceTyped),
			Meta:            meta,
		}
		StampSynthesizedTyped(e, SynthCSharpEFCoreModels)
		g.AddEdge(e)
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}

	for _, f := range dbsets {
		cands := sameBoundaryCandidates(g, f.siteID, classesByName[f.entity])
		if len(cands) != 1 {
			continue
		}
		cls := cands[0]
		if mapped[cls.ID] {
			continue
		}
		mapped[cls.ID] = true
		tableID := csharpEFTableNodeID(f.table, "")
		csharpEFEnsureTableNode(g, tableID, f.table, "", f.filePath)
		e := &graph.Edge{
			From: cls.ID, To: tableID, Kind: graph.EdgeModelsTable,
			FilePath:        f.filePath,
			Line:            f.line,
			Origin:          graph.OriginASTInferred,
			Confidence:      ConfidenceTyped,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeModelsTable, ConfidenceTyped),
			Meta: map[string]any{
				"orm":        "efcore",
				"binding":    "dbset",
				"table_name": f.table,
				"derivation": "convention",
			},
		}
		StampSynthesizedTyped(e, SynthCSharpEFCoreModels)
		g.AddEdge(e)
		resolved++
	}
	return resolved
}

// csharpEFFluentFact is one fluent table naming: from a config class
// (inline=false) or an OnModelCreating entry (inline=true).
type csharpEFFluentFact struct {
	entity   string
	table    string
	schema   string
	relation string
	inline   bool
	siteID   string
	filePath string
	line     int
}

// csharpEFMergeFluentFacts reduces the fluent facts to at most one per
// entity name: inline OnModelCreating entries beat config classes, and
// two same-tier facts that disagree on the mapping drop the entity —
// refusing beats guessing.
func csharpEFMergeFluentFacts(fluents []csharpEFFluentFact) []csharpEFFluentFact {
	merged := map[string]csharpEFFluentFact{}
	dropped := map[string]bool{}
	var order []string
	for _, f := range fluents {
		cur, ok := merged[f.entity]
		if !ok {
			merged[f.entity] = f
			order = append(order, f.entity)
			continue
		}
		if f.inline != cur.inline {
			if f.inline {
				merged[f.entity] = f
			}
			continue
		}
		if f.table != cur.table || f.schema != cur.schema || f.relation != cur.relation {
			dropped[f.entity] = true
		}
	}
	var out []csharpEFFluentFact
	for _, name := range order {
		if !dropped[name] {
			out = append(out, merged[name])
		}
	}
	return out
}

// csharpEFConfigFactFromNode reads the ef_config_* stamps off an
// IEntityTypeConfiguration<T> class node. A config class without a
// literal ToTable/ToView carries no table fact.
func csharpEFConfigFactFromNode(n *graph.Node) (csharpEFFluentFact, bool) {
	if n.Meta == nil {
		return csharpEFFluentFact{}, false
	}
	entity, _ := n.Meta["ef_config_entity"].(string)
	table, _ := n.Meta["ef_config_table"].(string)
	if entity == "" || table == "" {
		return csharpEFFluentFact{}, false
	}
	schema, _ := n.Meta["ef_config_schema"].(string)
	relation, _ := n.Meta["ef_config_relation"].(string)
	return csharpEFFluentFact{
		entity: entity, table: table, schema: schema, relation: relation,
		siteID: n.ID, filePath: n.FilePath, line: n.StartLine,
	}, true
}

// csharpEFInlineFactsFromFile parses the ef_fluent
// "entity|table|schema|relation" entries off a file node. Meta lists
// round-trip through persistence as []any, so both shapes decode.
func csharpEFInlineFactsFromFile(n *graph.Node) []csharpEFFluentFact {
	if n.Meta == nil {
		return nil
	}
	var entries []string
	switch v := n.Meta["ef_fluent"].(type) {
	case []string:
		entries = v
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				entries = append(entries, s)
			}
		}
	}
	var out []csharpEFFluentFact
	for _, entry := range entries {
		parts := strings.SplitN(entry, "|", 4)
		if len(parts) != 4 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, csharpEFFluentFact{
			entity: parts[0], table: parts[1], schema: parts[2], relation: parts[3],
			inline: true,
			siteID: n.ID, filePath: n.FilePath, line: 1,
		})
	}
	return out
}

// csharpEFTableNodeID mirrors the db::<dialect>::<schema>.<table>
// convention the other ORM passes share.
func csharpEFTableNodeID(table, schema string) string {
	if schema != "" {
		return "db::orm::" + schema + "." + table
	}
	return "db::orm::" + table
}

// csharpEFEnsureTableNode mints the KindTable node when the store does
// not already hold it.
func csharpEFEnsureTableNode(g graph.Store, tableID, table, schema, filePath string) {
	if g.GetNode(tableID) != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID:       tableID,
		Kind:     graph.KindTable,
		Name:     table,
		FilePath: filePath,
		Language: "csharp",
		Meta: map[string]any{
			"dialect": "orm",
			"schema":  schema,
			"source":  "csharp-orm",
		},
	})
}

// csharpDbSetFact is one DbSet<T> property: the entity it names, the
// table EF derives (the property name, verbatim), and the property's
// site as edge evidence.
type csharpDbSetFact struct {
	entity   string
	table    string
	siteID   string
	filePath string
	line     int
}

// csharpDbSetType matches a (possibly qualified) DbSet<T> property
// type and captures T.
var csharpDbSetType = regexp.MustCompile(`^(?:[A-Za-z_][\w.]*\.)?DbSet<(.+)>\??$`)

// csharpDbSetFactFromNode recognises a C# property node whose type is
// DbSet<T>. A nested-generic argument (DbSet<Pair<A,B>>) is not an
// entity registration and returns no fact.
func csharpDbSetFactFromNode(n *graph.Node) (csharpDbSetFact, bool) {
	if n.Meta == nil {
		return csharpDbSetFact{}, false
	}
	if k, _ := n.Meta["kind"].(string); k != "property" {
		return csharpDbSetFact{}, false
	}
	ft, _ := n.Meta["field_type"].(string)
	m := csharpDbSetType.FindStringSubmatch(strings.TrimSpace(ft))
	if len(m) < 2 {
		return csharpDbSetFact{}, false
	}
	entity := strings.TrimSpace(m[1])
	if strings.ContainsAny(entity, "<>,") {
		return csharpDbSetFact{}, false
	}
	if i := strings.LastIndexByte(entity, '.'); i >= 0 {
		entity = entity[i+1:]
	}
	entity = strings.TrimPrefix(entity, "@")
	if entity == "" || n.Name == "" {
		return csharpDbSetFact{}, false
	}
	return csharpDbSetFact{
		entity:   entity,
		table:    n.Name,
		siteID:   n.ID,
		filePath: n.FilePath,
		line:     n.StartLine,
	}, true
}
