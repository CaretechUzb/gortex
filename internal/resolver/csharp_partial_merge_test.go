package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func hasOutEdge(edges []*graph.Edge, kind graph.EdgeKind, to string) bool {
	for _, e := range edges {
		if e.Kind == kind && e.To == to {
			return true
		}
	}
	return false
}

func csharpType(id, name, ns, file string) *graph.Node {
	return &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: file, Language: "csharp",
		Meta: map[string]any{"scope_ns": ns},
	}
}

// TestMergeCSharpPartialTypes_CollapsesFragments is the core case: a
// `partial class Foo` split across A.cs and B.cs (same namespace) must
// fold onto one canonical node, with members, defines, and the base
// list (declared on the non-canonical fragment) rebound onto it.
func TestMergeCSharpPartialTypes_CollapsesFragments(t *testing.T) {
	g := graph.New()
	const canonical = "A.cs::Foo" // lowest ID wins
	const fragment = "B.cs::Foo"

	g.AddNode(&graph.Node{ID: "A.cs", Kind: graph.KindFile, FilePath: "A.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "B.cs", Kind: graph.KindFile, FilePath: "B.cs", Language: "csharp"})
	g.AddNode(csharpType(canonical, "Foo", "App", "A.cs"))
	g.AddNode(csharpType(fragment, "Foo", "App", "B.cs"))
	g.AddNode(&graph.Node{ID: "A.cs::Foo.MethodA", Kind: graph.KindMethod, Name: "MethodA", FilePath: "A.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "B.cs::Foo.MethodB", Kind: graph.KindMethod, Name: "MethodB", FilePath: "B.cs", Language: "csharp"})

	// defines: each file defines its own fragment.
	g.AddEdge(&graph.Edge{From: "A.cs", To: canonical, Kind: graph.EdgeDefines, FilePath: "A.cs", Line: 1})
	definesB := &graph.Edge{From: "B.cs", To: fragment, Kind: graph.EdgeDefines, FilePath: "B.cs", Line: 1}
	g.AddEdge(definesB)

	// member_of: MethodA on canonical (same-file control), MethodB on fragment.
	g.AddEdge(&graph.Edge{From: "A.cs::Foo.MethodA", To: canonical, Kind: graph.EdgeMemberOf, FilePath: "A.cs", Line: 2})
	memberB := &graph.Edge{From: "B.cs::Foo.MethodB", To: fragment, Kind: graph.EdgeMemberOf, FilePath: "B.cs", Line: 2}
	g.AddEdge(memberB)

	// implements: declared on the NON-canonical fragment (From: fragment).
	g.AddEdge(&graph.Edge{From: fragment, To: "unresolved::IBar", Kind: graph.EdgeImplements, FilePath: "B.cs", Line: 1})

	r := New(g)
	r.mergeCSharpPartialTypes()

	// To-rewrites: member + defines from the fragment land on canonical.
	assert.Equal(t, canonical, memberB.To, "fragment member_of must retarget to canonical")
	assert.Equal(t, canonical, definesB.To, "fragment defines must retarget to canonical")

	// From-rewrite: implements moves off the fragment onto canonical.
	assert.True(t, hasOutEdge(g.GetOutEdges(canonical), graph.EdgeImplements, "unresolved::IBar"),
		"implements declared on a fragment must be rebound onto canonical")
	assert.False(t, hasOutEdge(g.GetOutEdges(fragment), graph.EdgeImplements, "unresolved::IBar"),
		"implements must no longer originate from the merged-away fragment")

	// Markers (chosen design: leave the orphan node, stamp it).
	frag := g.GetNode(fragment)
	require.NotNil(t, frag)
	assert.Equal(t, canonical, frag.Meta["partial_merged_into"], "fragment must record where it merged")
	assert.Equal(t, true, g.GetNode(canonical).Meta["partial"], "canonical must be flagged partial")

	// Idempotent: a second run (incremental path re-runs the pass) is a no-op.
	r.mergeCSharpPartialTypes()
	assert.Equal(t, canonical, memberB.To, "second run must not corrupt the already-merged edge")
	assert.True(t, hasOutEdge(g.GetOutEdges(canonical), graph.EdgeImplements, "unresolved::IBar"),
		"second run must not drop or duplicate the rebound implements")
	require.Len(t, g.GetOutEdges(canonical), 1, "no duplicate implements after a second run; member_of/defines are in-edges, only implements is an out-edge")
}

// TestMergeCSharpPartialTypes_ThreeFragmentsAndIncomingRef exercises N>2
// fragments (canonical = lowest ID, not first-added) and confirms the
// documented "incoming references" case: an EdgeReferences pointing at a
// fragment is retargeted onto canonical, not just member_of / defines.
func TestMergeCSharpPartialTypes_ThreeFragmentsAndIncomingRef(t *testing.T) {
	g := graph.New()
	const canonical = "A.cs::Foo" // lowest of the three
	g.AddNode(csharpType("A.cs::Foo", "Foo", "App", "A.cs"))
	g.AddNode(csharpType("B.cs::Foo", "Foo", "App", "B.cs"))
	g.AddNode(csharpType("C.cs::Foo", "Foo", "App", "C.cs"))
	g.AddNode(&graph.Node{ID: "B.cs::Foo.MB", Kind: graph.KindMethod, Name: "MB", FilePath: "B.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "C.cs::Foo.MC", Kind: graph.KindMethod, Name: "MC", FilePath: "C.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "Other.cs::User.Use", Kind: graph.KindMethod, Name: "Use", FilePath: "Other.cs", Language: "csharp"})
	mB := &graph.Edge{From: "B.cs::Foo.MB", To: "B.cs::Foo", Kind: graph.EdgeMemberOf, FilePath: "B.cs", Line: 1}
	mC := &graph.Edge{From: "C.cs::Foo.MC", To: "C.cs::Foo", Kind: graph.EdgeMemberOf, FilePath: "C.cs", Line: 1}
	ref := &graph.Edge{From: "Other.cs::User.Use", To: "C.cs::Foo", Kind: graph.EdgeReferences, FilePath: "Other.cs", Line: 9}
	g.AddEdge(mB)
	g.AddEdge(mC)
	g.AddEdge(ref)

	New(g).mergeCSharpPartialTypes()

	assert.Equal(t, canonical, mB.To, "fragment B member retargets to canonical")
	assert.Equal(t, canonical, mC.To, "fragment C member retargets to canonical")
	assert.Equal(t, canonical, ref.To, "incoming reference to a fragment retargets to canonical")
}

// TestMergeCSharpPartialTypes_DifferentNamespacesNotMerged is the
// reason the key is (namespace, name) and not bare name: two same-named
// types in different namespaces are distinct and must NOT be fused.
func TestMergeCSharpPartialTypes_DifferentNamespacesNotMerged(t *testing.T) {
	g := graph.New()
	g.AddNode(csharpType("A.cs::Foo", "Foo", "App.Web", "A.cs"))
	g.AddNode(csharpType("B.cs::Foo", "Foo", "App.Data", "B.cs"))
	g.AddNode(&graph.Node{ID: "A.cs::Foo.MethodA", Kind: graph.KindMethod, Name: "MethodA", FilePath: "A.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "B.cs::Foo.MethodB", Kind: graph.KindMethod, Name: "MethodB", FilePath: "B.cs", Language: "csharp"})
	memberA := &graph.Edge{From: "A.cs::Foo.MethodA", To: "A.cs::Foo", Kind: graph.EdgeMemberOf, FilePath: "A.cs", Line: 2}
	memberB := &graph.Edge{From: "B.cs::Foo.MethodB", To: "B.cs::Foo", Kind: graph.EdgeMemberOf, FilePath: "B.cs", Line: 2}
	g.AddEdge(memberA)
	g.AddEdge(memberB)

	New(g).mergeCSharpPartialTypes()

	assert.Equal(t, "A.cs::Foo", memberA.To, "different-namespace type must be left untouched")
	assert.Equal(t, "B.cs::Foo", memberB.To, "different-namespace type must be left untouched")
	assert.Nil(t, g.GetNode("A.cs::Foo").Meta["partial"], "a non-collided type must not be flagged partial")
}

// TestMergeCSharpPartialTypes_InterfacesNotMerged: the v1 merge is scoped
// to class/struct/record. Partial INTERFACES are out of scope — InferImplements
// reads interface method sets from Meta["methods"] (not member edges), which the
// merge does not union, so merging interface fragments would corrupt interface
// satisfaction. Fragments must be left untouched.
func TestMergeCSharpPartialTypes_InterfacesNotMerged(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "A.cs::IFoo", Kind: graph.KindInterface, Name: "IFoo", FilePath: "A.cs", Language: "csharp", Meta: map[string]any{"scope_ns": "App"}})
	g.AddNode(&graph.Node{ID: "B.cs::IFoo", Kind: graph.KindInterface, Name: "IFoo", FilePath: "B.cs", Language: "csharp", Meta: map[string]any{"scope_ns": "App"}})
	g.AddNode(&graph.Node{ID: "B.cs::IFoo.M", Kind: graph.KindMethod, Name: "M", FilePath: "B.cs", Language: "csharp"})
	memberB := &graph.Edge{From: "B.cs::IFoo.M", To: "B.cs::IFoo", Kind: graph.EdgeMemberOf, FilePath: "B.cs", Line: 1}
	g.AddEdge(memberB)

	New(g).mergeCSharpPartialTypes()

	assert.Equal(t, "B.cs::IFoo", memberB.To, "partial interface fragments must NOT be merged (out of scope)")
	assert.Nil(t, g.GetNode("A.cs::IFoo").Meta["partial"])
}

// TestMergeCSharpPartialTypes_LanguageGated guards the csharp gate: a
// same-(namespace,name) collision in another language must be ignored.
func TestMergeCSharpPartialTypes_LanguageGated(t *testing.T) {
	g := graph.New()
	tsA := &graph.Node{ID: "a.ts::Foo", Kind: graph.KindType, Name: "Foo", FilePath: "a.ts", Language: "typescript", Meta: map[string]any{"scope_ns": "App"}}
	tsB := &graph.Node{ID: "b.ts::Foo", Kind: graph.KindType, Name: "Foo", FilePath: "b.ts", Language: "typescript", Meta: map[string]any{"scope_ns": "App"}}
	g.AddNode(tsA)
	g.AddNode(tsB)
	g.AddNode(&graph.Node{ID: "b.ts::Foo.M", Kind: graph.KindMethod, Name: "M", FilePath: "b.ts", Language: "typescript"})
	memberB := &graph.Edge{From: "b.ts::Foo.M", To: "b.ts::Foo", Kind: graph.EdgeMemberOf, FilePath: "b.ts", Line: 1}
	g.AddEdge(memberB)

	New(g).mergeCSharpPartialTypes()

	assert.Equal(t, "b.ts::Foo", memberB.To, "non-csharp type collision must NOT be merged by the csharp pass")
	assert.Nil(t, g.GetNode("b.ts::Foo").Meta["partial_merged_into"])
}

// TestMergeCSharpPartialTypes_DifferentReposNotMerged: in a multi-repo
// workspace the same (namespace, name) can exist in two repositories —
// the repo prefix is part of the merge key, so they must not fuse.
func TestMergeCSharpPartialTypes_DifferentReposNotMerged(t *testing.T) {
	g := graph.New()
	a := csharpType("repoA/A.cs::Foo", "Foo", "App", "repoA/A.cs")
	a.RepoPrefix = "repoA"
	b := csharpType("repoB/B.cs::Foo", "Foo", "App", "repoB/B.cs")
	b.RepoPrefix = "repoB"
	g.AddNode(a)
	g.AddNode(b)
	g.AddNode(&graph.Node{ID: "repoB/B.cs::Foo.M", Kind: graph.KindMethod, Name: "M", FilePath: "repoB/B.cs", Language: "csharp"})
	memberB := &graph.Edge{From: "repoB/B.cs::Foo.M", To: "repoB/B.cs::Foo", Kind: graph.EdgeMemberOf, FilePath: "repoB/B.cs", Line: 1}
	g.AddEdge(memberB)

	New(g).mergeCSharpPartialTypes()

	assert.Equal(t, "repoB/B.cs::Foo", memberB.To, "same-namespace types in different repos must NOT be merged")
	assert.Nil(t, g.GetNode("repoA/A.cs::Foo").Meta["partial"])
}

// TestMergeCSharpPartialTypesForFile_ScopesToFileKeys: the per-file
// variant merges only groups keyed by a type the edited file defines,
// and leaves unrelated partial groups for the whole-graph sweep.
func TestMergeCSharpPartialTypesForFile_ScopesToFileKeys(t *testing.T) {
	g := graph.New()
	g.AddNode(csharpType("A.cs::Foo", "Foo", "App", "A.cs"))
	g.AddNode(csharpType("B.cs::Foo", "Foo", "App", "B.cs"))
	g.AddNode(csharpType("C.cs::Bar", "Bar", "App", "C.cs"))
	g.AddNode(csharpType("D.cs::Bar", "Bar", "App", "D.cs"))
	g.AddNode(&graph.Node{ID: "B.cs::Foo.M", Kind: graph.KindMethod, Name: "M", FilePath: "B.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "D.cs::Bar.M", Kind: graph.KindMethod, Name: "M", FilePath: "D.cs", Language: "csharp"})
	fooMember := &graph.Edge{From: "B.cs::Foo.M", To: "B.cs::Foo", Kind: graph.EdgeMemberOf, FilePath: "B.cs", Line: 1}
	barMember := &graph.Edge{From: "D.cs::Bar.M", To: "D.cs::Bar", Kind: graph.EdgeMemberOf, FilePath: "D.cs", Line: 1}
	g.AddEdge(fooMember)
	g.AddEdge(barMember)

	r := New(g)
	r.mergeCSharpPartialTypesForFile("B.cs")

	assert.Equal(t, "A.cs::Foo", fooMember.To, "the edited file's partial group must merge")
	assert.Equal(t, "D.cs::Bar", barMember.To, "an unrelated partial group must be left for the whole-graph sweep")

	// A file with no C# types is a cheap no-op.
	r.mergeCSharpPartialTypesForFile("unrelated.go")
	assert.Equal(t, "D.cs::Bar", barMember.To)
}
