package resolver

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/graph"
)

// The frontier pass is checked DIFFERENTIALLY against the repository-scoped one
// it replaces, not against hand-written expectations.
//
// Its failure mode is silent under-binding: an edge the narrower frontier never
// collected keeps whatever binding it had, which is usually the right answer and
// occasionally a reference left pointing at a declaration that no longer exists.
// No count matches on that, and an expectation written by the same person who
// wrote the frontier tends to encode the same blind spot. Running both passes
// from an identical pre-state and requiring identical binding sets does not.

// odooDiffFixture builds one small multi-addon Odoo repository. Both sides of a
// differential get their own copy, so a mutation can be applied to each.
//
// The referencing files (`c` and `e`) deliberately live apart from the
// declaring ones: every interesting case is a change to a DECLARATION that has
// to reach a reference in a file the change never touched.
func odooDiffFixture() *graph.Graph {
	g := graph.New()

	declareModelIn := func(repo, addon, model string) {
		file := repo + "/" + addon + "/models/m.py"
		g.AddNode(&graph.Node{
			ID: file, Kind: graph.KindFile, Name: "m.py",
			FilePath: file, Language: "python", RepoPrefix: repo,
		})
		g.AddNode(&graph.Node{
			ID: file + "::Model_" + addon, Kind: graph.KindType, Name: "Model_" + addon,
			FilePath: file, Language: "python", RepoPrefix: repo,
			Meta: map[string]any{"odoo_model": model},
		})
	}
	declareModel := func(addon, model string) { declareModelIn("local", addon, model) }
	declareModel("a", "his.thing")
	declareModel("b", "his.thing")

	// A SECOND repository declaring the same model, with its own reference to
	// it. The declaration indexes are whole-store by construction, so a key a
	// changed file in `local` declares names this repository's class too — and
	// through it, edges sourced outside the scope. The pass this frontier
	// replaced could not reach them: RepoEdgesByKinds joins the scope on
	// from_id, so it collected source-side only. Nothing else in this file has
	// a key shared across repositories, so without this the widening is
	// untested — in the one area whose recorded incident is 181,077 edges reset
	// to placeholders.
	declareModelIn("other", "z", "his.thing")
	otherRef := "other/z/models/ref.py"
	g.AddNode(&graph.Node{
		ID: otherRef, Kind: graph.KindFile, Name: "ref.py",
		FilePath: otherRef, Language: "python", RepoPrefix: "other",
	})
	g.AddNode(&graph.Node{
		ID: otherRef + "::RefZ", Kind: graph.KindType, Name: "RefZ",
		FilePath: otherRef, Language: "python", RepoPrefix: "other",
	})
	odooStub(g, otherRef+"::RefZ", odooModelStubPrefix+"his.thing", graph.EdgeExtends,
		odooModelVia, map[string]any{"odoo_model": "his.thing"})

	// A reference to his.thing, from a file no scenario ever changes.
	refFile := "local/c/models/ref.py"
	g.AddNode(&graph.Node{
		ID: refFile, Kind: graph.KindFile, Name: "ref.py",
		FilePath: refFile, Language: "python", RepoPrefix: "local",
	})
	g.AddNode(&graph.Node{
		ID: refFile + "::RefC", Kind: graph.KindType, Name: "RefC",
		FilePath: refFile, Language: "python", RepoPrefix: "local",
	})
	odooStub(g, refFile+"::RefC", odooModelStubPrefix+"his.thing", graph.EdgeExtends,
		odooModelVia, map[string]any{"odoo_model": "his.thing"})

	// A reference to a model nothing declares yet — the add-declaration case.
	odooStub(g, refFile+"::RefC", odooModelStubPrefix+"his.later", graph.EdgeReferences,
		odooModelVia, map[string]any{"odoo_model": "his.later"})

	// An XML record and, from a different file, a reference to it.
	recFile := "local/d/views/v.xml"
	g.AddNode(&graph.Node{
		ID: recFile, Kind: graph.KindFile, Name: "v.xml",
		FilePath: recFile, Language: "odoo_xml", RepoPrefix: "local",
	})
	g.AddNode(&graph.Node{
		ID: "local/odoo::record::d.view_x", Kind: graph.KindResource,
		Name: "d.view_x", QualName: "d.view_x",
		FilePath: recFile, Language: "odoo_xml", RepoPrefix: "local",
		Meta: map[string]any{"odoo_xml_id": "d.view_x"},
	})
	srcFile := "local/e/views/w.xml"
	g.AddNode(&graph.Node{
		ID: srcFile, Kind: graph.KindFile, Name: "w.xml",
		FilePath: srcFile, Language: "odoo_xml", RepoPrefix: "local",
	})
	odooStub(g, srcFile, odooXMLIDStubPrefix+"d.view_x", graph.EdgeReferences,
		odooXMLVia, map[string]any{"odoo_xml_id": "d.view_x"})

	return g
}

// odooBindingSet is the pass's whole observable output: which Odoo edge points
// where. Sorted so two runs compare as sets rather than as traversal orders.
func odooBindingSet(g graph.Store) []string {
	seen := map[string]struct{}{}
	for _, kind := range odooFrontierEdgeKinds() {
		for edge := range g.EdgesByKind(kind) {
			if edge == nil || odooEdgeVia(edge) == "" {
				continue
			}
			seen[fmt.Sprintf("%s -[%s]-> %s", edge.From, edge.Kind, edge.To)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for row := range seen {
		out = append(out, row)
	}
	sort.Strings(out)
	return out
}

// odooScopedDifferential checks the file-frontier pass against a FULL cold
// recompute of the same post-change graph, and reports where the
// repository-scoped pass it replaces disagrees with that same ground truth.
//
// The cold pass is the oracle rather than the repository-scoped one, because
// the two narrower passes do not have the same job. A repository scope collects
// source-side only — RepoEdgesByKinds joins the scope on from_id — so a change
// in `local` cannot reach an edge in `other` that binds to a `local`
// declaration, and leaves it stale. The frontier reaches it, because the
// declaration indexes are whole-store and a touched key names every declarer of
// it. Requiring the frontier to match the repository-scoped pass would pin it
// to that staleness; requiring it to match a full recompute is the property
// actually wanted.
func odooScopedDifferential(t *testing.T, mutate func(*graph.Graph), changedFiles []string) []string {
	t.Helper()
	scope := map[string]bool{"local": true}

	cold, wide, narrow := odooDiffFixture(), odooDiffFixture(), odooDiffFixture()
	for _, g := range []*graph.Graph{cold, wide, narrow} {
		ResolveOdooRefs(g)
	}
	require.Equal(t, odooBindingSet(cold), odooBindingSet(narrow),
		"precondition: the fixtures reach the same steady state")

	for _, g := range []*graph.Graph{cold, wide, narrow} {
		mutate(g)
	}

	ResolveOdooRefs(cold)
	ResolveOdooRefsScoped(wide, scope)
	ResolveOdooRefsScopedForFiles(narrow, scope, changedFiles)

	want, got := odooBindingSet(cold), odooBindingSet(narrow)
	assert.Equal(t, want, got,
		"the changed-file frontier must land where a full recompute lands")
	if diff := len(odooBindingSet(wide)) - len(want); diff != 0 {
		t.Logf("note: the repository-scoped pass differs from a full recompute by %d edge(s) "+
			"on this scenario; the frontier does not", diff)
	}
	return got
}

// A declaration the change DELETES is the case a live-node frontier cannot see:
// the node whose in-edges would name the work is exactly what is gone. Without
// the dangling-target sweep the reference stays bound to an id nothing answers
// to, and no count notices.
//
// The reference is re-added after the eviction because Graph.EvictFile also
// drops the evicted nodes' IN-edges, which this state must survive. That is not
// the shape the production store ends up in: the live workspace holds 14 Odoo
// record targets under one repository that no node answers to, and the incident
// recorded on odooReconcileFanout is another — "its sibling survived pointing at
// a node that no longer exists". The eviction cascade is one path to a deleted
// declaration; a reference outliving its target is the state the pass has to
// handle, however it was reached.
func TestOdooFrontier_DeletedDeclarationUnbindsReference(t *testing.T) {
	const record = "local/odoo::record::d.view_x"
	bindings := odooScopedDifferential(t,
		func(g *graph.Graph) {
			g.EvictFile("local/d/views/v.xml")
			odooStub(g, "local/e/views/w.xml", record, graph.EdgeReferences,
				odooXMLVia, map[string]any{"odoo_xml_id": "d.view_x"})
		},
		[]string{"local/d/views/v.xml"},
	)
	assert.Contains(t, bindings,
		"local/e/views/w.xml -[references]-> "+odooXMLIDStubPrefix+"d.view_x",
		"a reference to a deleted record must be reset to its placeholder")
	for _, row := range bindings {
		assert.NotContains(t, row, "-> "+record,
			"no edge may stay bound to a record the change deleted")
	}
}

// A declaration the change ADDS has to reach references that are already in the
// graph, unbound, in files the change never touched. They are reachable because
// an unbound reference points AT the placeholder id, which the key names.
func TestOdooFrontier_AddedDeclarationBindsExistingReference(t *testing.T) {
	added := "local/f/models/m.py"
	bindings := odooScopedDifferential(t,
		func(g *graph.Graph) {
			g.AddNode(&graph.Node{
				ID: added, Kind: graph.KindFile, Name: "m.py",
				FilePath: added, Language: "python", RepoPrefix: "local",
			})
			g.AddNode(&graph.Node{
				ID: added + "::ModelLater", Kind: graph.KindType, Name: "ModelLater",
				FilePath: added, Language: "python", RepoPrefix: "local",
				Meta: map[string]any{"odoo_model": "his.later"},
			})
		},
		[]string{added},
	)
	assert.Contains(t, bindings,
		"local/c/models/ref.py::RefC -[references]-> "+added+"::ModelLater",
		"a reference the new declaration satisfies must bind")
}

// A model that gains a THIRD declaring class widens its fan-out. The reference
// is already bound, so the placeholder does not lead to it — only the "every
// other node declaring this key" half of the frontier does.
func TestOdooFrontier_AddedDeclarationWidensFanout(t *testing.T) {
	added := "local/g/models/m.py"
	bindings := odooScopedDifferential(t,
		func(g *graph.Graph) {
			g.AddNode(&graph.Node{
				ID: added, Kind: graph.KindFile, Name: "m.py",
				FilePath: added, Language: "python", RepoPrefix: "local",
			})
			g.AddNode(&graph.Node{
				ID: added + "::ModelG", Kind: graph.KindType, Name: "ModelG",
				FilePath: added, Language: "python", RepoPrefix: "local",
				Meta: map[string]any{"odoo_model": "his.thing"},
			})
		},
		[]string{added},
	)
	assert.Contains(t, bindings,
		"local/c/models/ref.py::RefC -[extends]-> "+added+"::ModelG",
		"the new declaring class must gain a fan-out sibling")
}

// Removing one of several declaring classes must retire that sibling while
// leaving the survivors bound.
func TestOdooFrontier_RemovedDeclarationNarrowsFanout(t *testing.T) {
	bindings := odooScopedDifferential(t,
		func(g *graph.Graph) { g.EvictFile("local/b/models/m.py") },
		[]string{"local/b/models/m.py"},
	)
	assert.Contains(t, bindings,
		"local/c/models/ref.py::RefC -[extends]-> local/a/models/m.py::Model_a",
		"the surviving declaration must stay bound")
	for _, row := range bindings {
		assert.NotContains(t, row, "local/b/models/m.py::Model_b",
			"the removed declaration must not keep a sibling")
	}
}

// A declaration whose KEY changes leaves its node in place, so the dangling
// sweep never sees it — it is reached because the node lives in a changed file.
func TestOdooFrontier_RenamedDeclarationUnbindsReference(t *testing.T) {
	changed := "local/a/models/m.py"
	odooScopedDifferential(t,
		func(g *graph.Graph) {
			g.EvictFile(changed)
			g.AddNode(&graph.Node{
				ID: changed, Kind: graph.KindFile, Name: "m.py",
				FilePath: changed, Language: "python", RepoPrefix: "local",
			})
			g.AddNode(&graph.Node{
				ID: changed + "::Model_a", Kind: graph.KindType, Name: "Model_a",
				FilePath: changed, Language: "python", RepoPrefix: "local",
				Meta: map[string]any{"odoo_model": "his.renamed"},
			})
		},
		[]string{changed},
	)
}

// A node that STOPS declaring anything is the case the declaration indexes
// cannot report: it survives the re-parse under the same id, so it is not a
// dangling target, and it is in no index under any key, so walking the indexes
// for keys the change touched never names it. Only "whoever points into a
// changed file" reaches the references it leaves behind.
func TestOdooFrontier_DeclarationDroppedFromSurvivingNodeUnbindsReference(t *testing.T) {
	const changed = "local/b/models/m.py"
	bindings := odooScopedDifferential(t,
		func(g *graph.Graph) {
			// Re-added under the same id with no odoo_model, and WITHOUT an
			// evict, so its inbound edges survive — the shape a re-parse leaves
			// when a class stays put and merely stops being an Odoo model.
			g.AddNode(&graph.Node{
				ID: changed + "::Model_b", Kind: graph.KindType, Name: "Model_b",
				FilePath: changed, Language: "python", RepoPrefix: "local",
			})
		},
		[]string{changed},
	)
	for _, row := range bindings {
		assert.NotContains(t, row, "-> "+changed+"::Model_b",
			"a class that stopped declaring the model must lose its inbound binding")
	}
}

// The plainest case, and the one a frontier built only from reverse lookups
// forgets: a changed file gains a NEW reference. Nothing points at it and it
// declares nothing, so it is reachable only because its own source node lives
// in a changed file.
func TestOdooFrontier_NewReferenceInChangedFileBinds(t *testing.T) {
	const changed = "local/c/models/ref.py"
	bindings := odooScopedDifferential(t,
		func(g *graph.Graph) {
			odooStub(g, changed+"::RefC", odooXMLIDStubPrefix+"d.view_x",
				graph.EdgeReferences, odooXMLVia,
				map[string]any{"odoo_xml_id": "d.view_x"})
		},
		[]string{changed},
	)
	assert.Contains(t, bindings,
		"local/c/models/ref.py::RefC -[references]-> local/odoo::record::d.view_x",
		"a reference the re-parse just added must be bound by the same pass")
}

// A caller that lost its file frontier must still get a correct pass. Falling
// through to the repository scope is slow; collecting nothing would be wrong.
func TestOdooFrontier_EmptyFilesFallsBackToRepositoryScope(t *testing.T) {
	wide, narrow := odooDiffFixture(), odooDiffFixture()
	ResolveOdooRefs(wide)
	ResolveOdooRefs(narrow)
	wide.EvictFile("local/d/views/v.xml")
	narrow.EvictFile("local/d/views/v.xml")

	scope := map[string]bool{"local": true}
	ResolveOdooRefsScoped(wide, scope)
	ResolveOdooRefsScopedForFiles(narrow, scope, nil)

	assert.Equal(t, odooBindingSet(wide), odooBindingSet(narrow))
}

// The dangling sweep must not reach out of its own repository: a sibling
// checkout's prefix starts with this one's, so a sweep anchored on the bare
// prefix would drag the sibling's edges in. It must also stay bounded by the
// change — a repository-wide sweep surfaces every target any past change left
// dangling, which is a re-derive's job.
func TestOdooFrontier_DeletionPrefixesAreBoundedByRepoAndChange(t *testing.T) {
	got := odooDeletionIDPrefixes([]string{"local"}, []string{"local/a/models/m.py"})
	assert.Equal(t, []string{"local/odoo::", "local/a/models/m.py"}, got)
	for _, prefix := range got {
		assert.NotEqual(t, "local/", prefix,
			"a bare repository prefix would reach into sibling checkouts and past this change")
	}

	assert.Equal(t, []string{"local/a/models/m.py"},
		odooDeletionIDPrefixes([]string{""}, []string{"local/a/models/m.py"}),
		"an unprefixed single repository has no synthetic namespace to bound")
}

// countingDanglingStore reports whether the backend capability was reached, or
// whether a wrapper hid it and the generic kind-bucket fallback ran instead.
type countingDanglingStore struct {
	graph.Store
	calls int
}

func (s *countingDanglingStore) DanglingEdgeTargets(idPrefixes []string, kinds []graph.EdgeKind) []string {
	s.calls++
	return graph.DanglingEdgeTargets(s.Store, idPrefixes, kinds)
}

// The pass never sees the raw store: framework synthesis hands it a repo-gate
// wrapper. graph.DanglingEdgeTargetReader is not part of graph.Store, so
// embedding promotes nothing, and a wrapper that stays silent about it costs
// the frontier its indexed anti-join — 115s of a 118s collection step on the
// live workspace, against 22ms. This is the second capability to be lost this
// way behind this exact wrapper; the first was the checkout grouping.
func TestFrameworkRepoGateStore_RepublishesDanglingEdgeTargets(t *testing.T) {
	backend := &countingDanglingStore{Store: odooDiffFixture()}
	// A repository that excludes the odoo pass is what makes the gate wrap at
	// all; an unconfigured workspace gets the bare store and never had a
	// capability to lose.
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{
		"local": frameworkgate.New([]string{"fastapi-resolve"}),
	})

	gated := newFrameworkRepoGateStore(backend, gate, SynthOdoo, backend)
	require.NotSame(t, graph.Store(backend), gated, "precondition: the gate must actually wrap")

	reader, ok := gated.(graph.DanglingEdgeTargetReader)
	require.True(t, ok, "the gate wrapper must republish DanglingEdgeTargetReader")
	reader.DanglingEdgeTargets([]string{"local/odoo::"}, odooFrontierEdgeKinds())
	assert.Equal(t, 1, backend.calls, "the call must reach the backend, not a fallback walk")
}

// The frontier reaches edges the repository scope cannot, and that widening is
// intended rather than incidental.
//
// RepoEdgesByKinds joins the scope on from_id, so a repository-scoped pass sees
// only edges SOURCED in the changed repository. An edge in `other` bound to a
// `local` class therefore survives a change that stops that class declaring the
// model — stale, pointing at a declaration that is gone. The frontier finds it
// because the declaration indexes are whole-store: the touched key names every
// declarer, and their in-edges name every reference, wherever they live.
func TestOdooFrontier_RepairsForeignRepoEdgeTheRepositoryScopeLeavesStale(t *testing.T) {
	const changed = "local/b/models/m.py"
	const stale = "other/z/models/ref.py::RefZ -[extends]-> local/b/models/m.py::Model_b"
	dropDeclaration := func(g *graph.Graph) {
		g.AddNode(&graph.Node{
			ID: changed + "::Model_b", Kind: graph.KindType, Name: "Model_b",
			FilePath: changed, Language: "python", RepoPrefix: "local",
		})
	}
	scope := map[string]bool{"local": true}

	wide, narrow := odooDiffFixture(), odooDiffFixture()
	ResolveOdooRefs(wide)
	ResolveOdooRefs(narrow)
	require.Contains(t, odooBindingSet(wide), stale,
		"precondition: the foreign repository binds to local's class")

	dropDeclaration(wide)
	dropDeclaration(narrow)
	ResolveOdooRefsScoped(wide, scope)
	ResolveOdooRefsScopedForFiles(narrow, scope, []string{changed})

	assert.Contains(t, odooBindingSet(wide), stale,
		"the repository scope cannot reach an edge sourced outside it — recorded, not endorsed")
	assert.NotContains(t, odooBindingSet(narrow), stale,
		"the frontier must retire a foreign edge whose declaration this change removed")
}
