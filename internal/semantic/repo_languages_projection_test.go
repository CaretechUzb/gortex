package semantic

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type managerProjectionStore struct {
	graph.Store
	calls    int
	prefixes []string
	rows     []graph.RepoLanguageFileCount
}

func (s *managerProjectionStore) RepoLanguageFileCounts(prefixes []string) []graph.RepoLanguageFileCount {
	s.calls++
	s.prefixes = append([]string(nil), prefixes...)
	return append([]graph.RepoLanguageFileCount(nil), s.rows...)
}

func (s *managerProjectionStore) RepoStats() map[string]graph.GraphStats {
	panic("repoLanguages must use the compact projection, not RepoStats")
}

func TestManagerRepoLanguagesUsesOneCompactProjection(t *testing.T) {
	store := &managerProjectionStore{
		Store: graph.New(),
		rows: []graph.RepoLanguageFileCount{
			{RepoPrefix: "a", FilePath: "src/main.go", Language: "go", Count: 3},
			{RepoPrefix: "a", FilePath: "generated/parser.c", Language: "c", Count: 10},
			{RepoPrefix: "b", FilePath: "src/main.py", Language: "python", Count: 2},
			{RepoPrefix: "other", FilePath: "src/lib.rs", Language: "rust", Count: 7},
		},
	}
	manager := &Manager{}
	present, repoCounts, languageCounts, repoLangs := manager.repoLanguages(store, map[string]string{
		"b": "/repos/b",
		"a": "/repos/a",
	})

	// Applicability is per-repo: the workspace contains Python, but repo "a"
	// does not, and declaring python-types applicable to "a" would peg a pure-Go
	// checkout below ready forever.
	wantRepoLangs := map[string]map[string]bool{"a": {"go": true}, "b": {"python": true}}
	if !reflect.DeepEqual(repoLangs, wantRepoLangs) {
		t.Fatalf("repoLangs = %#v, want %#v", repoLangs, wantRepoLangs)
	}

	if store.calls != 1 {
		t.Fatalf("projection calls = %d, want 1", store.calls)
	}
	if got, want := store.prefixes, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projection prefixes = %#v, want %#v", got, want)
	}
	if got, want := present, map[string]bool{"go": true, "python": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("present = %#v, want %#v", got, want)
	}
	if got, want := repoCounts, map[string]int{"a": 3, "b": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("repo counts = %#v, want %#v", got, want)
	}
	if got, want := languageCounts, map[string]int{"go": 3, "python": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("language counts = %#v, want %#v", got, want)
	}
}
