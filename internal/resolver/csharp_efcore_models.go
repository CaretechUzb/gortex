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
// Facts join to entity classes by unique class name within the same
// boundary — an ambiguous name (two classes called Widget) skips
// rather than guesses. An entity that already carries a models_table
// edge (attribute-bound at extraction, or a previous run of this
// pass) is left alone, which is also what makes the pass idempotent.
//
// Returns the number of edges synthesized.
func ResolveCSharpEFCoreModels(g graph.Store) int {
	if g == nil {
		return 0
	}
	classesByName := map[string][]*graph.Node{}
	var dbsets []csharpDbSetFact
	for _, n := range nodesByKindsOrAll(g, graph.KindType, graph.KindField) {
		if n == nil || !strings.EqualFold(n.Language, "csharp") {
			continue
		}
		switch n.Kind {
		case graph.KindType:
			classesByName[n.Name] = append(classesByName[n.Name], n)
		case graph.KindField:
			if f, ok := csharpDbSetFactFromNode(n); ok {
				dbsets = append(dbsets, f)
			}
		}
	}
	if len(dbsets) == 0 {
		return 0
	}

	// Entities that already model a table keep their edge: the
	// attribute binding was decided at extraction with the entity's own
	// file as evidence, and a prior run of this pass already landed.
	mapped := map[string]bool{}
	for e := range g.EdgesByKind(graph.EdgeModelsTable) {
		if e != nil {
			mapped[e.From] = true
		}
	}

	resolved := 0
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
		tableID := "db::orm::" + f.table
		if g.GetNode(tableID) == nil {
			g.AddNode(&graph.Node{
				ID:       tableID,
				Kind:     graph.KindTable,
				Name:     f.table,
				FilePath: f.filePath,
				Language: "csharp",
				Meta: map[string]any{
					"dialect": "orm",
					"schema":  "",
					"source":  "csharp-orm",
				},
			})
		}
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
