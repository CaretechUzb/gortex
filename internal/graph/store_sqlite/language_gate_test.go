package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestSQLiteHasLanguage: the probe must see a language behind any repo
// prefix — including the empty solo-repo prefix, which RepoPrefixes()
// deliberately excludes — and answer false for absent languages.
func TestSQLiteHasLanguage(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.AddBatch([]*graph.Node{
		{ID: "app/main.go::main", Kind: graph.KindFunction, Name: "main", RepoPrefix: "", Language: "go", FilePath: "app/main.go"},
		{ID: "web::src/a.ts::render", Kind: graph.KindFunction, Name: "render", RepoPrefix: "web", Language: "typescript", FilePath: "src/a.ts"},
		{ID: "api::Svc.cs::Svc", Kind: graph.KindType, Name: "Svc", RepoPrefix: "api", Language: "csharp", FilePath: "Svc.cs"},
	}, nil)

	for lang, want := range map[string]bool{
		"go":         true, // lives behind the solo '' prefix
		"typescript": true,
		"csharp":     true,
		"python":     false,
		"":           false,
	} {
		if got := store.HasLanguage(lang); got != want {
			t.Errorf("HasLanguage(%q) = %v, want %v", lang, got, want)
		}
	}
}

// TestSQLiteHasLanguageEmptyStore: a store with no nodes at all must
// report false for every language, not error.
func TestSQLiteHasLanguageEmptyStore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if store.HasLanguage("go") {
		t.Error("HasLanguage on an empty store must be false")
	}
}

// TestSQLiteHasLanguageNarrowsToNamedNodes pins the documented boundary:
// the `name <> ''` predicate is what makes the partial index eligible, so
// the probe answers "are there NAMED nodes in this language". A language
// present only as unnamed nodes reads as absent. Every caller gates a
// pass over symbols, so that is the intended question — but it is a real
// narrowing, and dropping the predicate to widen it would silently cost
// the index scan the probe exists to avoid.
func TestSQLiteHasLanguageNarrowsToNamedNodes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.AddBatch([]*graph.Node{
		{ID: "a.rb::Widget", Kind: graph.KindType, Name: "Widget", Language: "ruby", FilePath: "a.rb"},
		{ID: "b.zig", Kind: graph.KindFile, Name: "", Language: "zig", FilePath: "b.zig"},
	}, nil)

	if !store.HasLanguage("ruby") {
		t.Error("a language with a named node must be present")
	}
	if store.HasLanguage("zig") {
		t.Error("a language with only unnamed nodes reads as absent — see HasLanguage's doc comment")
	}
}

// TestSQLiteNodesByKindLang: the pushed-down filter must yield exactly the
// kind∩language intersection and honour early stop.
func TestSQLiteNodesByKindLang(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.AddBatch([]*graph.Node{
		{ID: "A.cs::A", Kind: graph.KindType, Name: "A", Language: "csharp", FilePath: "A.cs"},
		{ID: "B.cs::B", Kind: graph.KindType, Name: "B", Language: "csharp", FilePath: "B.cs"},
		{ID: "C.go::C", Kind: graph.KindType, Name: "C", Language: "go", FilePath: "C.go"},
		{ID: "A.cs::A.Run", Kind: graph.KindMethod, Name: "Run", Language: "csharp", FilePath: "A.cs"},
	}, nil)

	got := map[string]bool{}
	for n := range store.NodesByKindLang(graph.KindType, "csharp") {
		got[n.ID] = true
	}
	if len(got) != 2 || !got["A.cs::A"] || !got["B.cs::B"] {
		t.Errorf("NodesByKindLang(type, csharp) = %v, want exactly {A.cs::A, B.cs::B}", got)
	}

	seen := 0
	for range store.NodesByKindLang(graph.KindType, "csharp") {
		seen++
		break // early stop must not panic or leak
	}
	if seen != 1 {
		t.Errorf("early-stop iteration saw %d nodes, want 1", seen)
	}

	for n := range store.NodesByKindLang(graph.KindType, "rust") {
		t.Errorf("NodesByKindLang(type, rust) yielded %s, want nothing", n.ID)
	}
}
