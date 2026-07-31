package resolver

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRustTypeParamTraitNamesStructuredLegacyView(t *testing.T) {
	tests := []struct {
		name  string
		value any
		param string
		want  []string
	}{
		{
			name: "qualified associated constraint and lifetime",
			value: []map[string]string{{
				"name":  "M",
				"bound": "crate::matcher::Matcher<Item = Vec<u8>> + Send + 'static",
			}},
			param: "M",
			want:  []string{"crate::matcher::Matcher", "Send"},
		},
		{
			name: "simple higher ranked trait bound",
			value: []any{map[string]any{
				"name":  "F",
				"bound": "for <'a> Fn(&'a [u8]) -> bool + Sync",
			}},
			param: "F",
			want:  []string{"Fn", "Sync"},
		},
		{
			name:  "map compatibility shape",
			value: map[string]string{"T": "?Sized + r#AsyncRead"},
			param: "T",
			want:  []string{"AsyncRead"},
		},
		{
			name: "duplicate lookup keys remain stable",
			value: []map[string]string{
				{"name": "T", "bound": "pkg::Read + Read"},
				{"name": "T", "bound": "Read + Send"},
			},
			param: "T",
			want:  []string{"pkg::Read", "Read", "Send"},
		},
		{
			name:  "unicode and raw identifiers normalize",
			value: []map[string]string{{"name": "T", "bound": "crate::r#match::Διαβάζει"}},
			param: "T",
			want:  []string{"crate::match::Διαβάζει"},
		},
		{
			name:  "malformed generic is conservative",
			value: []map[string]string{{"name": "T", "bound": "Matcher<Item = Vec<u8>"}},
			param: "T",
			want:  nil,
		},
		{
			name:  "unmatched close is conservative",
			value: []map[string]string{{"name": "T", "bound": "Matcher> + Send"}},
			param: "T",
			want:  nil,
		},
		{
			name:  "missing parameter",
			value: []map[string]string{{"name": "T", "bound": "Read"}},
			param: "U",
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rustTypeParamTraitNames(tt.value, tt.param)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("rustTypeParamTraitNames() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestUniqueGenericBoundTraitMethodStructuredBounds(t *testing.T) {
	matcherMethod := &graph.Node{
		ID:         "repo/matcher.rs::Matcher.find",
		Kind:       graph.KindMethod,
		Name:       "find",
		Language:   "rust",
		RepoPrefix: "repo",
		Meta: map[string]any{
			"receiver":   "Matcher",
			"trait_decl": "true",
		},
	}
	idx := &rustScopeIndex{methodsByOwner: map[rustOwnerKey][]*graph.Node{
		{repo: "repo", owner: "crate::matcher::Matcher"}: {matcherMethod},
		{repo: "repo", owner: "Matcher"}:                 {matcherMethod},
	}}
	caller := &graph.Node{Meta: map[string]any{
		"type_params": []map[string]string{{
			"name":  "M",
			"bound": "crate::matcher::Matcher<Item = Vec<u8>> + Send",
		}},
	}}
	if got := idx.uniqueGenericBoundTraitMethod("repo", caller, "M", "find"); got != matcherMethod.ID {
		t.Fatalf("uniqueGenericBoundTraitMethod() = %q, want %q", got, matcherMethod.ID)
	}

	other := &graph.Node{
		ID:         "repo/other.rs::Send.find",
		Kind:       graph.KindMethod,
		Name:       "find",
		Language:   "rust",
		RepoPrefix: "repo",
		Meta:       map[string]any{"receiver": "Send", "trait_decl": "true"},
	}
	idx.methodsByOwner[rustOwnerKey{repo: "repo", owner: "Send"}] = []*graph.Node{other}
	if got := idx.uniqueGenericBoundTraitMethod("repo", caller, "M", "find"); got != "" {
		t.Fatalf("ambiguous trait method resolved to %q", got)
	}

	caller.Meta["type_params"] = []map[string]string{{"name": "M", "bound": "Matcher<Item = Vec<u8>"}}
	if got := idx.uniqueGenericBoundTraitMethod("repo", caller, "M", "find"); got != "" {
		t.Fatalf("malformed bound resolved to %q", got)
	}

	caller.Meta["type_params"] = []map[string]string{{"name": "M", "bound": "external::Matcher"}}
	if got := idx.uniqueGenericBoundTraitMethod("repo", caller, "M", "find"); got != "" {
		t.Fatalf("external qualified bound degraded to local Matcher: %q", got)
	}
}

func TestRustTraitTargetIndexConservativeResolution(t *testing.T) {
	const (
		repo = "repo"
		root = "crates/app"
	)
	child := &graph.Node{
		ID: "crates/app/src/nested/child.rs::Child", Name: "Child",
		FilePath: "crates/app/src/nested/child.rs", RepoPrefix: repo, Language: "rust",
	}
	idx := &rustTraitTargetIndex{
		exact:    make(map[rustTraitTargetKey]rustTraitTargetEntry),
		basename: make(map[rustTraitTargetKey]rustTraitTargetEntry),
		nodes: map[string]graph.QualifiedNodeIdentity{child.ID: {
			ID: child.ID, Name: child.Name, Language: child.Language,
			FilePath: child.FilePath, RepoPrefix: child.RepoPrefix,
		}},
	}
	addRustTraitTarget(idx.exact, rustTraitTargetKey{repo: repo, crateRoot: root, path: "crate::nested::child::Local"}, "local-id")
	addRustTraitTarget(idx.exact, rustTraitTargetKey{repo: repo, crateRoot: root, path: "crate::nested::Parent"}, "parent-id")
	addRustTraitTarget(idx.exact, rustTraitTargetKey{repo: repo, crateRoot: root, path: "crate::Root"}, "root-id")
	addRustTraitTarget(idx.basename, rustTraitTargetKey{repo: repo, crateRoot: root, path: "Unique"}, "unique-id")
	addRustTraitTarget(idx.basename, rustTraitTargetKey{repo: repo, crateRoot: root, path: "Ambiguous"}, "a-id")
	addRustTraitTarget(idx.basename, rustTraitTargetKey{repo: repo, crateRoot: root, path: "Ambiguous"}, "b-id")

	tests := []struct {
		raw  string
		want string
	}{
		{raw: "self::Local", want: "local-id"},
		{raw: "super::Parent", want: "parent-id"},
		{raw: "crate::Root", want: "root-id"},
		{raw: "nested::Parent", want: "parent-id"},
		{raw: "Unique", want: "unique-id"},
		{raw: "Ambiguous", want: ""},
		{raw: "serde::Unique", want: ""},
		{raw: "?Unique", want: ""},
	}
	for _, tt := range tests {
		if got := idx.resolve(child, tt.raw); got != tt.want {
			t.Errorf("resolve(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestRustInheritedMethodsKeepQualifiedOwnerAliases(t *testing.T) {
	const repo = "repo"
	trait := func(id, name, file string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindInterface, Name: name, FilePath: file, Language: "rust", RepoPrefix: repo}
	}
	method := func(id, file string) *graph.Node {
		return &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: "p", FilePath: file, Language: "rust", RepoPrefix: repo,
			Meta: map[string]any{"receiver": "Parent", "trait_decl": "true"},
		}
	}
	aParent := trait("src/a.rs::Parent", "Parent", "src/a.rs")
	aChild := trait("src/a.rs::Child", "Child", "src/a.rs")
	bParent := trait("src/b.rs::Parent", "Parent", "src/b.rs")
	bChild := trait("src/b.rs::Child", "Child", "src/b.rs")
	aMethod := method("src/a.rs::Parent.p", "src/a.rs")
	bMethod := method("src/b.rs::Parent.p", "src/b.rs")

	g := graph.New()
	g.AddBatch(
		[]*graph.Node{aParent, aChild, bParent, bChild, aMethod, bMethod},
		[]*graph.Edge{
			{From: aMethod.ID, To: aParent.ID, Kind: graph.EdgeMemberOf},
			{From: bMethod.ID, To: bParent.ID, Kind: graph.EdgeMemberOf},
			{From: aChild.ID, To: aParent.ID, Kind: graph.EdgeExtends},
			{From: bChild.ID, To: bParent.ID, Kind: graph.EdgeExtends},
		},
	)
	idx, changed := buildRustScopeIndex(g, nil)
	if changed != 0 || idx == nil {
		t.Fatalf("buildRustScopeIndex() = (%v, %d), want non-nil, 0", idx, changed)
	}
	if got := idx.methodsByOwner[rustOwnerKey{repo: repo, owner: "Child"}]; len(got) != 0 {
		t.Fatalf("ambiguous basename Child was indexed: %#v", got)
	}
	assertMethodIDs := func(owner string, want *graph.Node) {
		t.Helper()
		got := idx.methodsByOwner[rustOwnerKey{repo: repo, owner: owner}]
		if len(got) != 1 || got[0].ID != want.ID {
			t.Fatalf("methodsByOwner[%q] = %#v, want only %s", owner, got, want.ID)
		}
	}
	assertMethodIDs("a::Child", aMethod)
	assertMethodIDs("b::Child", bMethod)
}

func TestRustInheritedTraitOwnersCycleAndDepthCap(t *testing.T) {
	key := func(name string) rustOwnerKey { return rustOwnerKey{repo: "repo", owner: name} }
	a, b, c := key("A"), key("B"), key("C")
	got := rustInheritedTraitOwners(a, map[rustOwnerKey][]rustOwnerKey{
		a: {b},
		b: {c},
		c: {a},
	})
	want := []rustOwnerKey{b, c}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cycle closure = %#v, want %#v", got, want)
	}

	supers := make(map[rustOwnerKey][]rustOwnerKey)
	for i := 0; i < rustTraitSuperDepthLimit+4; i++ {
		supers[key(fmt.Sprintf("T%02d", i))] = []rustOwnerKey{key(fmt.Sprintf("T%02d", i+1))}
	}
	got = rustInheritedTraitOwners(key("T00"), supers)
	if len(got) != rustTraitSuperDepthLimit {
		t.Fatalf("depth-capped closure has %d owners, want %d", len(got), rustTraitSuperDepthLimit)
	}
}
