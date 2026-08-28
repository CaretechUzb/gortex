package indexer

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestStaleLangsDetection proves both the precise per-language extractor signal
// and the global post-extraction policy epoch used for admission-rule upgrades.
func TestStaleLangsDetection(t *testing.T) {
	t.Run("only_behind_langs", func(t *testing.T) {
		stored := map[string]int{"go": 1, "python": 2, "ruby": 1}
		current := map[string]int{"go": 2, "python": 2, "ruby": 1, "rust": 3}
		got := staleLangsBetween(stored, current)
		// Synthetic maps without the reserved policy key retain the legacy
		// per-language behavior: absent rust has no baseline and is not stale.
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

	t.Run("post_extraction_policy_epoch", func(t *testing.T) {
		current := map[string]int{
			postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
			"go":                            3,
			"java":                          1,
			"ruby":                          1,
		}
		cases := []struct {
			name   string
			stored map[string]int
			want   []string
		}{
			{
				name:   "legacy_snapshot_missing_epoch",
				stored: map[string]int{"go": 3, "java": 1, "ruby": 1},
				want:   []string{"go", "java", "ruby"},
			},
			{
				name: "epoch_behind",
				stored: map[string]int{
					postExtractionPolicySnapshotKey: postExtractionPolicyVersion - 1,
					"go":                            3,
					"java":                          1,
					"ruby":                          1,
				},
				want: []string{"go", "java", "ruby"},
			},
			{
				name: "epoch_current_keeps_per_language_precision",
				stored: map[string]int{
					postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
					"go":                            2,
					"java":                          1,
					"ruby":                          1,
				},
				want: []string{"go"},
			},
			{
				name: "all_current",
				stored: map[string]int{
					postExtractionPolicySnapshotKey: postExtractionPolicyVersion,
					"go":                            3,
					"java":                          1,
					"ruby":                          1,
				},
				want: []string{},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := staleLangsBetween(tc.stored, current)
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("staleLangsBetween = %v, want %v", got, tc.want)
				}
				for _, lang := range got {
					if lang == postExtractionPolicySnapshotKey {
						t.Fatalf("reserved policy key leaked as a language: %v", got)
					}
				}
			})
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
		atCurrentPolicy := func(versions map[string]int) string {
			snapshot := make(map[string]int, len(versions)+1)
			for lang, version := range versions {
				snapshot[lang] = version
			}
			snapshot[postExtractionPolicySnapshotKey] = postExtractionPolicyVersion
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatalf("marshal extractor versions: %v", err)
			}
			return string(encoded)
		}
		// Against the live extractor versions, an unchanged baseline language
		// is not stale. Java is the exemplar because its extractor has never
		// been bumped — a language whose version this suite also asserts
		// would make the check tautological.
		if got := ExtractorVersionStaleLangs(atCurrentPolicy(map[string]int{"java": 1})); len(got) != 0 {
			t.Errorf("stored at current = %v, want empty", got)
		}
		if got := ExtractorVersionStaleLangs(atCurrentPolicy(map[string]int{"java": 1, "php": 1})); !reflect.DeepEqual(got, []string{"php"}) {
			t.Errorf("stored PHP structural-edge version = %v, want [php]", got)
		}
		// A store extracted before the generic-call fix must re-extract:
		// until then, every call spelling explicit type arguments is missing
		// from its graph entirely, and no content change will trigger it.
		if got := ExtractorVersionStaleLangs(atCurrentPolicy(map[string]int{"go": 1, "scala": 1, "cpp": 1, "swift": 1})); !reflect.DeepEqual(
			got, []string{"cpp", "go", "scala", "swift"}) {
			t.Errorf("stored pre-generic-call version = %v, want all four", got)
		}
		// A store extracted before the C# params-shape fix must re-extract
		// unchanged .cs, .razor, and .cshtml files. Without the bump, their
		// persisted graph keeps the old arity and parameter evidence.
		if got := ExtractorVersionStaleLangs(atCurrentPolicy(map[string]int{"csharp": 10})); !reflect.DeepEqual(got, []string{"csharp"}) {
			t.Errorf("stored pre-params C# version = %v, want [csharp]", got)
		}
		if got := extractorVersionsSnapshot()[postExtractionPolicySnapshotKey]; got != postExtractionPolicyVersion {
			t.Errorf("persisted policy epoch = %d, want %d", got, postExtractionPolicyVersion)
		}

		policySalt := "_post_extraction_policy@2"
		for _, path := range []string{"model.py", "Model.java", "model.rb", "model.ts", "schema.ex", "model.js"} {
			if got := merkleSaltFor(path); got != policySalt {
				t.Errorf("global policy salt for %s = %q, want %q", path, got, policySalt)
			}
		}
		if got, want := merkleSaltFor("model.go"), policySalt+"|go@3"; got != want {
			t.Errorf("Go extractor salt = %q, want %q", got, want)
		}
		for _, path := range []string{"src/Handler.cs", "Views/Page.razor", "Views/Page.cshtml"} {
			want := policySalt + "|csharp@13"
			if got := merkleSaltFor(path); got != want {
				t.Errorf("C# extractor salt for %s = %q, want %q", path, got, want)
			}
		}
		if got, want := merkleSaltFor("src/Handler.php"), policySalt+"|php@2"; got != want {
			t.Errorf("PHP extractor salt = %q, want %q", got, want)
		}
		if got, want := merkleSaltFor("include/widget.hxx"), policySalt+"|cpp@2"; got != want {
			t.Errorf("C++ extractor salt for .hxx = %q, want %q", got, want)
		}
		if got := merkleSaltFor("README.zzz"); got != "" {
			t.Errorf("unmapped extension salt = %q, want empty", got)
		}
	})

	t.Run("lang_for_file", func(t *testing.T) {
		if got := ExtractorLangForFile("internal/auth/token.go"); got != "go" {
			t.Errorf("ExtractorLangForFile(.go) = %q, want go", got)
		}
		if got := ExtractorLangForFile("include/widget.hxx"); got != "cpp" {
			t.Errorf("ExtractorLangForFile(.hxx) = %q, want cpp", got)
		}
		if got := ExtractorLangForFile("README.zzz"); got != "" {
			t.Errorf("ExtractorLangForFile(unknown) = %q, want \"\"", got)
		}
	})
}
