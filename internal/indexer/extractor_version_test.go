package indexer

import (
	"reflect"
	"testing"
)

// TestStaleLangsDetection proves the per-language extractor-staleness signal:
// only languages whose stored version is behind the current one are flagged
// (so the advisory names the exact languages to reindex — a scoped reindex
// rather than a full cold rebuild), a language the snapshot never recorded is
// never spuriously flagged, and an empty baseline reports nothing.
func TestStaleLangsDetection(t *testing.T) {
	t.Run("only_behind_langs", func(t *testing.T) {
		stored := map[string]int{"go": 1, "python": 2, "ruby": 1}
		current := map[string]int{"go": 2, "python": 2, "ruby": 1, "rust": 3}
		got := staleLangsBetween(stored, current)
		// go is behind (1<2); python/ruby are current; rust is absent from
		// stored (no baseline) so it is NOT flagged.
		if want := []string{"go"}; !reflect.DeepEqual(got, want) {
			t.Errorf("staleLangsBetween = %v, want %v", got, want)
		}
	})

	t.Run("sorted_multiple", func(t *testing.T) {
		stored := map[string]int{"typescript": 1, "go": 1, "python": 1}
		current := map[string]int{"typescript": 2, "go": 2, "python": 1}
		got := staleLangsBetween(stored, current)
		if want := []string{"go", "typescript"}; !reflect.DeepEqual(got, want) {
			t.Errorf("staleLangsBetween = %v, want %v (sorted)", got, want)
		}
	})

	t.Run("json_and_empty", func(t *testing.T) {
		// An empty / unparseable baseline reports nothing.
		if got := ExtractorVersionStaleLangs(""); got != nil {
			t.Errorf("empty baseline = %v, want nil", got)
		}
		if got := ExtractorVersionStaleLangs("not json"); got != nil {
			t.Errorf("bad json = %v, want nil", got)
		}
		// Against the live extractor versions, an unchanged baseline language
		// is not stale.
		if got := ExtractorVersionStaleLangs(`{"go":1}`); len(got) != 0 {
			t.Errorf("stored at current = %v, want empty", got)
		}
		if got := ExtractorVersionStaleLangs(`{"go":1,"php":1}`); !reflect.DeepEqual(got, []string{"php"}) {
			t.Errorf("stored PHP structural-edge version = %v, want [php]", got)
		}
		// A store extracted before the C# usings-scope/builtin-receiver
		// stamps must re-extract its .cs files on upgrade — the stamps
		// only exist post-extraction, so without the bump the extension
		// visibility features stay dormant on existing stores.
		if got := ExtractorVersionStaleLangs(`{"csharp":2}`); !reflect.DeepEqual(got, []string{"csharp"}) {
			t.Errorf("stored C# usings-stamp version = %v, want [csharp]", got)
		}
		if got := merkleSaltFor("src/Handler.php"); got != "php@2" {
			t.Errorf("PHP extractor salt = %q, want php@2", got)
		}
	})

	t.Run("lang_for_file", func(t *testing.T) {
		if got := ExtractorLangForFile("internal/auth/token.go"); got != "go" {
			t.Errorf("ExtractorLangForFile(.go) = %q, want go", got)
		}
		if got := ExtractorLangForFile("README.zzz"); got != "" {
			t.Errorf("ExtractorLangForFile(unknown) = %q, want \"\"", got)
		}
	})
}
