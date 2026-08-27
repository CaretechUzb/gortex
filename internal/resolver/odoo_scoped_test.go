package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// odooRepoRecord seeds one addon's file, record node and reference edge
// under a repository prefix, so a scoped run has something to exclude.
func odooRepoRecord(g *graph.Graph, repo, xmlID string) *graph.Edge {
	file := repo + "/views/v.xml"
	g.AddNode(&graph.Node{
		ID: file, Kind: graph.KindFile, Name: "v.xml",
		FilePath: file, Language: "odoo_xml",
	})
	node := repo + "/odoo::record::" + xmlID
	g.AddNode(&graph.Node{
		ID: node, Kind: graph.KindResource, Name: xmlID, QualName: xmlID,
		FilePath: file, Language: "odoo_xml",
		Meta: map[string]any{"odoo_xml_id": xmlID},
	})
	return odooStub(g, file, odooXMLIDStubPrefix+xmlID, graph.EdgeReferences,
		odooXMLVia, map[string]any{"odoo_xml_id": xmlID})
}

// Indexing a second addon must not un-bind the first.
//
// Every Odoo family is a full recompute: an edge whose target is absent
// from the index is RESET to its placeholder, which is how a reference to
// a deleted record un-binds itself. Registered without a scoped entry
// point, the pass was handed a store seeded from the changed repository
// alone — so it built its indexes from that repository and applied the
// verdict to every Odoo edge in the workspace. On the real corpus that
// reset 181,077 odoo edges to placeholders while the records they named
// were still in the graph.
func TestRunFrameworkSynthesizers_ScopedRunKeepsOtherReposBound(t *testing.T) {
	g := graph.New()
	kept := odooRepoRecord(g, "odoo", "sale.view_order")
	changed := odooRepoRecord(g, "addons", "crm.view_lead")

	RunFrameworkSynthesizers(g)
	require.Equal(t, "odoo/odoo::record::sale.view_order", kept.To,
		"precondition: a full run binds both repositories")
	require.Equal(t, "addons/odoo::record::crm.view_lead", changed.To)

	RunFrameworkSynthesizersScopedForFiles(
		g, map[string]bool{"addons": true},
		[]string{"addons/views/v.xml"}, false,
	)

	assert.Equal(t, "odoo/odoo::record::sale.view_order", kept.To,
		"a run scoped to addons must leave odoo's bound edge alone")
	assert.Equal(t, "addons/odoo::record::crm.view_lead", changed.To,
		"the in-scope edge must stay bound too")
}

// partialIndexStore hides some nodes from the kind scans an index is built
// from while still answering for them by id — the shape a scoped or
// per-repository view presents when a pass runs over a store that holds
// more than that view.
type partialIndexStore struct {
	graph.Store
	hidden map[string]bool
}

func (s *partialIndexStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	inner := s.Store.NodesByKind(kind)
	return func(yield func(*graph.Node) bool) {
		inner(func(n *graph.Node) bool {
			if n != nil && s.hidden[n.ID] {
				return true
			}
			return yield(n)
		})
	}
}

// A recompute may un-bind only because the target is gone — never because
// the index it was built from could not see it.
//
// The two are indistinguishable from inside the pass: both leave the
// lookup empty. Resetting on that alone meant one pass over a partial view
// reset a whole repository's edges to placeholders while the records they
// named were still in the graph, and no later pass put them back, because
// the recompute reached the same empty verdict every time. Measured on the
// real workspace this left 96,651 odoo edges unresolved whose targets a
// full-view pass binds immediately.
func TestResolveOdooRefs_PartialIndexDoesNotUnbindLiveTargets(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "odoo/sale/views/v.xml", Kind: graph.KindFile,
		FilePath: "odoo/sale/views/v.xml", Language: "odoo_xml",
	})
	record := "odoo/odoo::record::sale.view_order"
	g.AddNode(&graph.Node{
		ID: record, Kind: graph.KindResource, Name: "view_order",
		QualName: "sale.view_order", FilePath: "odoo/sale/views/v.xml",
		Language: "odoo_xml",
		Meta:     map[string]any{"odoo_xml_id": "sale.view_order"},
	})
	e := odooStub(g, "odoo/sale/views/v.xml",
		odooXMLIDStubPrefix+"sale.view_order", graph.EdgeReferences, odooXMLVia,
		map[string]any{"odoo_xml_id": "sale.view_order"})

	ResolveOdooRefs(g)
	require.Equal(t, record, e.To, "precondition: a full view binds the reference")

	// The next pass runs over a view that cannot see the record, though
	// the record is still in the store.
	ResolveOdooRefs(&partialIndexStore{Store: g, hidden: map[string]bool{record: true}})

	assert.Equal(t, record, e.To,
		"a pass that cannot see the target must not un-bind an edge whose target still exists")
}

// The guard must not block a legitimate un-bind. A target that still
// exists but no longer answers to the edge's key has to release it back to
// a placeholder — otherwise "never un-bind a live target" would freeze
// every binding the moment it was made.
func TestResolveOdooRefs_RenamedTargetStillUnbinds(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "odoo/sale/views/v.xml", Kind: graph.KindFile,
		FilePath: "odoo/sale/views/v.xml", Language: "odoo_xml",
	})
	record := "odoo/odoo::record::sale.view_order"
	g.AddNode(&graph.Node{
		ID: record, Kind: graph.KindResource, Name: "view_order",
		QualName: "sale.view_order", FilePath: "odoo/sale/views/v.xml",
		Language: "odoo_xml",
		Meta:     map[string]any{"odoo_xml_id": "sale.view_order"},
	})
	e := odooStub(g, "odoo/other/views/w.xml",
		odooXMLIDStubPrefix+"sale.view_order", graph.EdgeReferences, odooXMLVia,
		map[string]any{"odoo_xml_id": "sale.view_order"})

	ResolveOdooRefs(g)
	require.Equal(t, record, e.To)

	// The record is re-declared under a different external ID, so the
	// reference names something that no longer exists.
	g.AddNode(&graph.Node{
		ID: record, Kind: graph.KindResource, Name: "view_order",
		QualName: "sale.view_quotation", FilePath: "odoo/sale/views/v.xml",
		Language: "odoo_xml",
		Meta:     map[string]any{"odoo_xml_id": "sale.view_quotation"},
	})
	ResolveOdooRefs(g)

	assert.Equal(t, odooXMLIDStubPrefix+"sale.view_order", e.To,
		"a reference whose record was renamed must fall back to its placeholder")
}
