package tstypes

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestPreloadWorkingSetIgnoresSharedMethodName pins the page preload's working
// set to the PAGE — its own files plus the types a receiver can actually
// resolve to — rather than to the repository's most popular identifier.
//
// The corpus shape is Odoo's: thousands of classes each declaring the same
// method name (create / write / _compute_*). Before names carried roles, one
// such name in a page's call facts pulled EVERY same-named method node in the
// repository into the per-page applier, and those candidates then seeded the
// frontier walk, which re-expanded their owner types and members — on every
// 32-file page, of every one of the four apply phases. That is what made the
// whole-repo pass quadratic in file count (measured on the addons-shaped
// benchmark: 4.5s at 2k files, 64s at 8k, 165s at 12k, log-log slope ~2).
//
// A called member never needs name candidates: methodOn walks the RESOLVED
// receiver's member_of edges. This test fails on the pre-role code because the
// node set scales with the number of same-named classes.
func TestPreloadWorkingSetIgnoresSharedMethodName(t *testing.T) {
	measure := func(t *testing.T, classes int) (int, *applier) {
		t.Helper()
		g := graph.New()
		nodes := make([]*graph.Node, 0, classes*2+1)
		edges := make([]*graph.Edge, 0, classes)
		caller := &graph.Node{
			ID: "repo/use.py::caller", Kind: graph.KindFunction, Name: "caller",
			FilePath: "repo/use.py", Language: "python", RepoPrefix: "repo",
		}
		nodes = append(nodes, caller)
		for i := 0; i < classes; i++ {
			file := fmt.Sprintf("repo/mod%d/models.py", i)
			class := &graph.Node{
				ID: fmt.Sprintf("%s::Worker%d", file, i), Kind: graph.KindType,
				Name: fmt.Sprintf("Worker%d", i), FilePath: file,
				Language: "python", RepoPrefix: "repo",
			}
			method := &graph.Node{
				ID: class.ID + ".shared", Kind: graph.KindMethod, Name: "shared",
				FilePath: file, Language: "python", RepoPrefix: "repo",
			}
			nodes = append(nodes, class, method)
			edges = append(edges, &graph.Edge{
				From: method.ID, To: class.ID, Kind: graph.EdgeMemberOf,
			})
		}
		g.AddBatch(nodes, edges)

		ap := newApplier(g, PythonSpec(), "test-types")
		ap.preload([]*fileFacts{{
			file: "repo/use.py", repoPrefix: "repo",
			calls: []callFact{{line: 2, method: "shared", recvType: "Worker0"}},
		}})
		return len(ap.nodesByID), ap
	}

	small, _ := measure(t, 4)
	large, ap := measure(t, 512)
	if large != small {
		t.Fatalf("per-page working set = %d nodes at 512 same-named classes vs %d at 4; "+
			"a shared method name must not scale the page's node set", large, small)
	}
	// And the bound is the page itself: its one file node, the one receiver
	// type its facts name, and that type's own member.
	if small != 3 {
		t.Fatalf("working set = %d nodes, want exactly 3 "+
			"{repo/use.py::caller, Worker0, Worker0.shared}", small)
	}
	// Starving the name index must not cost resolution: the member still
	// grounds through the receiver type's member_of edges.
	receiver := ap.node("repo/mod0/models.py::Worker0")
	if receiver == nil {
		t.Fatal("the receiver type named by the page's facts was not hydrated")
	}
	target := ap.methodOn(receiver, "shared", -1, 0)
	if target == nil || target.ID != "repo/mod0/models.py::Worker0.shared" {
		t.Fatalf("methodOn(Worker0, shared) = %#v, want repo/mod0/models.py::Worker0.shared", target)
	}
}

// TestPreloadWorkingSetIgnoresHubInboundDegree pins the page preload's EDGE
// working set to the page, the way the test above pins its node set.
//
// Inbound degree is the one graph statistic with no local bound. The corpus
// shape is Odoo's again: one base model class that every addon extends and
// every addon calls into, so the hub's inbound degree is a function of the
// REPOSITORY. Every reader of applier.inEdges filters to member_of /
// param_of — preloadApplicationFrontier, relevantEndpointIDs, methodOn,
// typeHasRealMember, paramArity — so the inbound `calls`, `references` and
// `extends` edges were fetched, grouped, cached and retained without a single
// consumer that could read them.
//
// Measured on `addons` (11,970 Python files) before the kind narrowing: one
// 32-file page loaded 860,270 inbound edges to consume 6,809 of them (0.8%),
// one hub method contributing 10,255, at ~12.6 s of random B-tree page reads
// per page — which is what kept the whole-repo pass from ever finishing inside
// its budget. This test fails on the pre-narrowing code because the retained
// edge set scales with the number of siblings on the hub.
func TestPreloadWorkingSetIgnoresHubInboundDegree(t *testing.T) {
	measure := func(t *testing.T, siblings int, hideCapability bool) (int, *applier) {
		t.Helper()
		g := graph.New()
		// The hub: a base class with one member, extended and called by the
		// whole repository.
		base := &graph.Node{
			ID: "repo/base.py::Base", Kind: graph.KindType, Name: "Base",
			FilePath: "repo/base.py", Language: "python", RepoPrefix: "repo",
		}
		member := &graph.Node{
			ID: "repo/base.py::Base.create", Kind: graph.KindMethod, Name: "create",
			FilePath: "repo/base.py", Language: "python", RepoPrefix: "repo",
		}
		// The page's own file: one more subclass of the hub, calling its member.
		page := &graph.Node{
			ID: "repo/page.py::Page", Kind: graph.KindType, Name: "Page",
			FilePath: "repo/page.py", Language: "python", RepoPrefix: "repo",
		}
		nodes := []*graph.Node{base, member, page}
		edges := []*graph.Edge{
			{From: member.ID, To: base.ID, Kind: graph.EdgeMemberOf},
			{From: page.ID, To: base.ID, Kind: graph.EdgeExtends},
		}
		for i := 0; i < siblings; i++ {
			file := fmt.Sprintf("repo/mod%d/models.py", i)
			sub := &graph.Node{
				ID: fmt.Sprintf("%s::Sub%d", file, i), Kind: graph.KindType,
				Name: fmt.Sprintf("Sub%d", i), FilePath: file,
				Language: "python", RepoPrefix: "repo",
			}
			caller := &graph.Node{
				ID: fmt.Sprintf("%s::caller%d", file, i), Kind: graph.KindFunction,
				Name: fmt.Sprintf("caller%d", i), FilePath: file,
				Language: "python", RepoPrefix: "repo",
			}
			nodes = append(nodes, sub, caller)
			edges = append(edges,
				&graph.Edge{From: sub.ID, To: base.ID, Kind: graph.EdgeExtends},
				&graph.Edge{From: caller.ID, To: member.ID, Kind: graph.EdgeCalls},
				&graph.Edge{From: caller.ID, To: base.ID, Kind: graph.EdgeReferences},
			)
		}
		g.AddBatch(nodes, edges)

		var store graph.Store = g
		if hideCapability {
			// A backend without the pushdown must still get a bounded
			// working set: the narrowing is the engine's invariant, not the
			// store's favour. The embedded-interface wrapper hides
			// *Graph's InEdgesByKindFinder method set from the type
			// assertion, forcing the Go-side fallback.
			store = plainStore{g}
		}
		ap := newApplier(store, PythonSpec(), "test-types")
		ap.preload([]*fileFacts{{
			file: "repo/page.py", repoPrefix: "repo",
			supers: []superFact{{typeName: "Page", superName: "Base", line: 1}},
			calls:  []callFact{{line: 3, method: "create", recvType: "Page"}},
		}})
		retained := 0
		for _, group := range ap.inByID {
			retained += len(group)
		}
		return retained, ap
	}

	small, _ := measure(t, 4, false)
	large, ap := measure(t, 512, false)
	if large != small {
		t.Fatalf("per-page inbound working set = %d edges at 512 hub siblings vs %d at 4; "+
			"a hub's inbound degree must not scale the page's edge set", large, small)
	}
	// Same bound without the store-side pushdown. A backend that cannot
	// narrow the READ must still not leave the rows RESIDENT, or the
	// invariant silently becomes SQLite-only.
	fallbackSmall, _ := measure(t, 4, true)
	fallbackLarge, _ := measure(t, 512, true)
	if fallbackLarge != fallbackSmall || fallbackLarge != large {
		t.Fatalf("without the InEdgesByKindFinder pushdown the working set = %d edges at 512 "+
			"siblings vs %d at 4 (pushdown path: %d); the Go-side fallback must bound it identically",
			fallbackLarge, fallbackSmall, large)
	}
	// The bound is the structure the page can traverse: the hub's one member.
	if got := len(ap.inByID["repo/base.py::Base"]); got != 1 {
		t.Fatalf("hub retained %d inbound edges, want exactly 1 (its member_of); "+
			"inbound calls/references/extends have no reader", got)
	}
	// Narrowing the fetch must not cost resolution: the member still grounds
	// through the receiver's inheritance chain and the hub's member_of edges.
	receiver := ap.node("repo/page.py::Page")
	if receiver == nil {
		t.Fatal("the receiver type named by the page's facts was not hydrated")
	}
	if got := ap.methodOn(receiver, "create", -1, 0); got == nil || got.ID != "repo/base.py::Base.create" {
		t.Fatalf("methodOn(Page, create) = %#v, want repo/base.py::Base.create", got)
	}
}

// plainStore hides every optional capability its embedded Store implements
// beyond graph.Store itself: the embedded INTERFACE promotes only that method
// set, so a type assertion for a narrower capability fails even though the
// concrete value behind it satisfies one.
type plainStore struct{ graph.Store }

// TestCandidateKindsAreRoleScoped pins which node kinds each collected role
// admits — the contract that lets a page skip a popular name entirely.
func TestCandidateKindsAreRoleScoped(t *testing.T) {
	py := newApplier(graph.New(), PythonSpec(), "test-types")
	if kinds := py.candidateKinds(roleMember); len(kinds) != 0 {
		t.Fatalf("roleMember kinds for a spec without extension functions = %v, want none: "+
			"a called member resolves structurally, never by name", kinds)
	}
	if kinds := py.candidateKinds(roleType); !kinds[graph.KindType] || kinds[graph.KindMethod] {
		t.Fatalf("roleType kinds = %v, want receiver/supertype kinds only", kinds)
	}
	if kinds := py.candidateKinds(roleCallee); !kinds[graph.KindFunction] || !kinds[graph.KindMethod] {
		t.Fatalf("roleCallee kinds = %v, want function and method "+
			"(callableReturnType reads a repo-unique callee's return_type)", kinds)
	}

	// A spec with extension functions is the one case a member name must buy
	// candidates for: a cross-file extension carries member_of to a same-file
	// phantom of its receiver, so it is reachable only by its own name.
	kt := newApplier(graph.New(), KotlinSpec(), "test-types")
	if kinds := kt.candidateKinds(roleMember); !kinds[graph.KindMethod] {
		t.Fatalf("roleMember kinds for a spec WITH extension functions = %v, want method", kinds)
	}
}

// nameQueryRecorder records the names the batched candidate lookup was asked
// for, so a test can assert on WHICH names reached the store rather than only
// how many calls it took (the batch collapses a page's names into one call).
type nameQueryRecorder struct {
	graph.Store
	asked []string
}

func (s *nameQueryRecorder) FindNodesByNamesInRepoLanguages(
	names []string, repoPrefix string, languages []string,
) map[string][]*graph.Node {
	s.asked = append(s.asked, names...)
	return graph.FindNodesByNamesInRepoLanguages(s.Store, names, repoPrefix, languages)
}

// TestZeroKindNameIsNeverAskedOfTheStore pins the other half of the role
// split. Filtering a popular name's candidates AFTER fetching them still pays
// the query and still parks the whole group in the pass-wide hot cache
// (putNames stores the RAW store result), so on the Odoo shape `create` and
// `write` would stay resident for the entire pass even though nothing can ever
// select them. A name whose roles admit no node kind must not reach the store
// at all, and must stay unloaded so a later role can still top it up.
func TestZeroKindNameIsNeverAskedOfTheStore(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{
			ID: "repo/use.py::caller", Kind: graph.KindFunction, Name: "caller",
			FilePath: "repo/use.py", Language: "python", RepoPrefix: "repo",
		},
		{
			ID: "repo/models.py::Worker", Kind: graph.KindType, Name: "Worker",
			FilePath: "repo/models.py", Language: "python", RepoPrefix: "repo",
		},
		{
			ID: "repo/models.py::Worker.create", Kind: graph.KindMethod, Name: "create",
			FilePath: "repo/models.py", Language: "python", RepoPrefix: "repo",
		},
	}, []*graph.Edge{{
		From: "repo/models.py::Worker.create", To: "repo/models.py::Worker",
		Kind: graph.EdgeMemberOf,
	}})

	store := &nameQueryRecorder{Store: g}
	ap := newApplier(store, PythonSpec(), "test-types")
	ap.preload([]*fileFacts{{
		file: "repo/use.py", repoPrefix: "repo",
		calls: []callFact{{line: 2, method: "create", recvType: "Worker"}},
	}})

	for _, name := range ap.asked() {
		if name == "create" {
			t.Fatalf("the store was asked for %q, a name only a call's method "+
				"position produced; asked=%v", name, ap.asked())
		}
	}
	if len(ap.asked()) == 0 {
		t.Fatal("no name reached the store at all; the receiver type must still be fetched")
	}
	// Left unloaded, not marked loaded: namedNodes must still be able to top
	// the group up if a later role ever needs it.
	key := typeCandidateKey{repoPrefix: "repo", name: "create"}
	if got := ap.nameLoaded[key]; got != 0 {
		t.Fatalf("zero-kind name marked loaded as %d; it must stay unloaded", got)
	}
	if got := ap.namedNodes("repo", "create", roleCallee); len(got) != 1 {
		t.Fatalf("namedNodes top-up for a skipped name returned %d nodes, want 1", len(got))
	}
}

func (a *applier) asked() []string {
	if rec, ok := a.g.(*nameQueryRecorder); ok {
		return rec.asked
	}
	return nil
}

// TestFrontierWalkReachesFullInheritanceDepth guards the seeding change's
// blast radius. The old preloadBounded re-seeded every round from everything
// in a.nodesByID, so nodes the frontier walk itself discovered were re-walked
// next round and the EFFECTIVE reach was rounds x extendsWalkDepth. The new
// seeding walks only page-file nodes plus type-kind candidates, so the reach
// is exactly extendsWalkDepth from a seed — which must still cover the whole
// chain methodOn itself is willing to climb (it gives up past
// extendsWalkDepth). A -> B -> C -> D is that exact bound, and D is the only
// declarer of the called member, in a file the page does not contain.
func TestFrontierWalkReachesFullInheritanceDepth(t *testing.T) {
	g := graph.New()
	nodes := []*graph.Node{{
		ID: "repo/use.ts::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "repo/use.ts", Language: "typescript", RepoPrefix: "repo",
	}}
	var edges []*graph.Edge
	chain := []string{"A", "B", "C", "D"}
	for i, name := range chain {
		file := "repo/" + strings.ToLower(name) + ".ts"
		nodes = append(nodes, &graph.Node{
			ID: file + "::" + name, Kind: graph.KindType, Name: name,
			FilePath: file, Language: "typescript", RepoPrefix: "repo",
		})
		if i > 0 {
			parent := "repo/" + strings.ToLower(chain[i-1]) + ".ts::" + chain[i-1]
			edges = append(edges, &graph.Edge{
				From: parent, To: file + "::" + name, Kind: graph.EdgeExtends,
			})
		}
	}
	// The member exists ONLY on the deepest ancestor, three extends hops from
	// the receiver the page's facts name.
	deep := &graph.Node{
		ID: "repo/d.ts::D.deep", Kind: graph.KindMethod, Name: "deep",
		FilePath: "repo/d.ts", Language: "typescript", RepoPrefix: "repo",
	}
	nodes = append(nodes, deep)
	edges = append(edges, &graph.Edge{
		From: deep.ID, To: "repo/d.ts::D", Kind: graph.EdgeMemberOf,
	})
	g.AddBatch(nodes, edges)

	ap := newApplier(g, TypeScriptSpec(), "test-types")
	ap.preload([]*fileFacts{{
		file: "repo/use.ts", repoPrefix: "repo",
		calls: []callFact{{line: 2, method: "deep", recvType: "A"}},
	}})

	receiver := ap.node("repo/a.ts::A")
	if receiver == nil {
		t.Fatal("the receiver type named by the page's facts was not hydrated")
	}
	if got := ap.methodOn(receiver, "deep", -1, 0); got == nil || got.ID != deep.ID {
		t.Fatalf("methodOn(A, deep) = %#v, want %s: the frontier walk must reach "+
			"every ancestor methodOn is willing to climb", got, deep.ID)
	}
	// The reach is adjacency, not just nodes: without D's in-edges preloaded
	// methodOn would have read an empty member set and returned nil quietly.
	if !ap.inLoaded["repo/d.ts::D"] {
		t.Fatal("the deepest ancestor's member adjacency was never preloaded")
	}
}
