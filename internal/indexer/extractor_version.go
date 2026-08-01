package indexer

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// extractorVersions records the logic version of each language's
// extractor. Bump a language's entry when its extraction logic changes
// in a way that should re-extract already-indexed files whose content
// did not change (a new edge kind, a corrected node shape, a fixed
// parser bug). The version is mixed into the Merkle leaf salt (see
// merkleSaltFor), so a bump re-flags exactly that language's files as
// stale on the next reconcile — without re-reading unchanged content
// and without disturbing other languages.
//
// A language absent here, or pinned at 1, carries no salt and therefore
// behaves exactly as before: the registry is dormant until a version is
// deliberately raised. This is the surgical alternative to the
// binary-wide snapshot invalidation (which restages the whole repo on
// any binary change): a Go-extractor fix re-extracts only `.go` files.
var extractorVersions = map[string]int{
	// Languages default to version 1 (no salt). Raise an entry here in
	// the same change that alters a language's extraction logic, e.g.
	//   "go": 2,
	"c":      generatedParserProjectionPolicyVersion, // generated parser projection now performs fail-closed whole-file accounting
	"php":    2,                                      // class/interface inheritance now emits typed structural edges
	"csharp": 2,                                      // null-conditional call edges; reference forms for caseless type names
}

// extractorSaltExtLang maps a lower-case file extension to the language
// key used in extractorVersions. It need not be exhaustive: an unmapped
// extension simply carries no extractor-version salt (content-only
// staleness, the pre-existing behaviour). Extensions are grouped to the
// extractor that owns them.
var extractorSaltExtLang = map[string]string{
	".go":    "go",
	".py":    "python",
	".pyi":   "python",
	".js":    "javascript",
	".jsx":   "javascript",
	".mjs":   "javascript",
	".cjs":   "javascript",
	".ts":    "typescript",
	".tsx":   "typescript",
	".mts":   "typescript",
	".cts":   "typescript",
	".java":  "java",
	".rb":    "ruby",
	".rs":    "rust",
	".c":     "c",
	".h":     "c",
	".cc":    "cpp",
	".cpp":   "cpp",
	".cxx":   "cpp",
	".hpp":   "cpp",
	".hh":    "cpp",
	".cs":    "csharp",
	".php":   "php",
	".swift": "swift",
	".kt":    "kotlin",
	".kts":   "kotlin",
	".scala": "scala",
	".m":     "objc",
	".mm":    "objcpp",
	".lua":   "lua",
	".dart":  "dart",
	".ex":    "elixir",
	".exs":   "elixir",
	".sh":    "bash",
	".bash":  "bash",
}

// ExtractorLangForFile returns the extractor-staleness language key for a
// repo-relative path (by file extension), or "" when the extension carries no
// extractor-version tracking. Used to tell whether a touched file belongs to a
// language whose extractor is stale.
func ExtractorLangForFile(rel string) string {
	return extractorSaltExtLang[strings.ToLower(filepath.Ext(rel))]
}

// extractorVersionForLang returns the registered extractor version for a
// language, defaulting to 1.
func extractorVersionForLang(lang string) int {
	if v, ok := extractorVersions[lang]; ok && v > 0 {
		return v
	}
	return 1
}

// merkleSaltFor returns the Merkle leaf salt for a repo-relative path:
// "" when the file's language extractor is at the baseline version 1
// (so the leaf equals the content hash and nothing changes), or
// "lang@N" once a language's extractor version is bumped, so its files
// re-extract on the next reconcile even when their content is unchanged.
func merkleSaltFor(rel string) string {
	lang := extractorSaltExtLang[strings.ToLower(filepath.Ext(rel))]
	if lang == "" {
		return ""
	}
	v := extractorVersionForLang(lang)
	if v <= 1 {
		return ""
	}
	return lang + "@" + strconv.Itoa(v)
}

// ExtractorVersionStaleLangs reports which languages' extractors have been
// bumped SINCE the graph was last indexed — comparing the per-language
// versions persisted on RepoIndexState (a JSON object lang->version) against
// the running binary's current versions. A language is stale when its stored
// version is behind the current one: its already-indexed files would
// re-extract on the next reconcile. Returns the stale languages, sorted.
//
// This is the per-LANGUAGE precision that turns "your index is from an older
// binary" into "reindex only Go + Python" — a scoped reindex instead of a full
// cold rebuild. An empty/absent stored map (no baseline) reports nothing.
func ExtractorVersionStaleLangs(storedJSON string) []string {
	storedJSON = strings.TrimSpace(storedJSON)
	if storedJSON == "" {
		return nil
	}
	var stored map[string]int
	if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil || len(stored) == 0 {
		return nil
	}
	return staleLangsBetween(stored, extractorVersionsSnapshot())
}

// staleLangsBetween returns the languages whose stored version is behind the
// current version — only languages present in BOTH maps are compared, so a
// language the stored snapshot never recorded is not spuriously flagged.
func staleLangsBetween(stored, current map[string]int) []string {
	var stale []string
	for lang, storedV := range stored {
		if cur, ok := current[lang]; ok && storedV < cur {
			stale = append(stale, lang)
		}
	}
	sort.Strings(stale)
	return stale
}

// extractorVersionsSnapshot returns a copy of the current per-language
// extractor versions for persistence in repo_index_state, so a future
// reconcile can tell which extractor produced the stored graph.
func extractorVersionsSnapshot() map[string]int {
	out := make(map[string]int, len(extractorSaltExtLang))
	seen := map[string]bool{}
	for _, lang := range extractorSaltExtLang {
		if seen[lang] {
			continue
		}
		seen[lang] = true
		out[lang] = extractorVersionForLang(lang)
	}
	return out
}
