package mcp

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/elide"
)

// fidelityGlobsParamDescription documents the fidelity_globs param for
// read_file / get_editing_context. Kept as a constant so both tool
// registrations share one source of truth.
const fidelityGlobsParamDescription = "Per-glob fidelity tiers, applied when compress_bodies is set: a comma-separated, ordered list of `glob:fidelity` rules where fidelity is one of full | compress | omit (e.g. \"internal/**:full,*_test.go:omit,vendor/**:compress\"). The first rule whose glob matches the file's repo-relative path wins; a file matching no rule falls back to the compress_bodies boolean (compress). Glob semantics: `*` matches within a single path segment (never across `/`), basenames are matched too (so `*_test.go` works without a `**/` prefix), a trailing `/**` matches the directory and everything beneath it, a leading `**/` matches any directory depth, and a bare directory prefix (`internal`) matches everything under it, as does a trailing `/*` (`internal/*` is a prefix, not a segment glob). The per-symbol `keep` predicate still composes: a kept symbol stays full even when its file's rule says compress or omit."

// fidelityRule is one parsed `glob:fidelity` clause. Rules are matched
// in declaration order; the first matching glob wins.
type fidelityRule struct {
	glob     string
	fidelity elide.Fidelity
}

// parseFidelityGlobs parses the fidelity_globs param value into an
// ordered rule list. Unrecognised or malformed clauses are skipped
// (fail-soft — a typo never aborts the read). Returns nil when the
// value yields no usable rule, so the caller falls back to the plain
// compress_bodies boolean.
func parseFidelityGlobs(spec string) []fidelityRule {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	var rules []fidelityRule
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		// Split on the LAST colon so a Windows-style or
		// colon-bearing glob keeps its colons; the fidelity token is
		// always the trailing field.
		idx := strings.LastIndex(clause, ":")
		if idx <= 0 || idx == len(clause)-1 {
			continue
		}
		glob := strings.TrimSpace(clause[:idx])
		fid, ok := parseFidelity(clause[idx+1:])
		if glob == "" || !ok {
			continue
		}
		rules = append(rules, fidelityRule{glob: glob, fidelity: fid})
	}
	return rules
}

// parseFidelity maps a fidelity token to the elide enum.
func parseFidelity(tok string) (elide.Fidelity, bool) {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case "full":
		return elide.FidelityFull, true
	case "compress", "compressed":
		return elide.FidelityCompress, true
	case "omit", "omitted":
		return elide.FidelityOmit, true
	default:
		return elide.FidelityCompress, false
	}
}

// fidelityDecideForPath builds an elide.Decide that applies the first
// matching rule's fidelity to every declaration in the file at relPath.
// Returns nil when no rule matches (the caller then falls back to plain
// compress). The verdict is per-file, not per-declaration: all decls in
// one file share the file's tier. The per-symbol keep predicate is
// layered on separately by elide.Options.verdict.
func fidelityDecideForPath(rules []fidelityRule, relPath string) func(elide.Decl) elide.Fidelity {
	if len(rules) == 0 {
		return nil
	}
	rel := filepath.ToSlash(relPath)
	for _, r := range rules {
		if matchFidelityGlob(r.glob, rel) {
			fid := r.fidelity
			return func(elide.Decl) elide.Fidelity { return fid }
		}
	}
	return nil
}

// matchFidelityGlob matches a glob against a forward-slash relative
// path. It extends matchPathPattern's basename/prefix semantics with
// explicit `**` support so the documented `internal/**` / `**/*.go`
// forms work as written. Glob matching is segment-bounded — a single `*`
// never crosses `/`, which is why matchSegmentGlob needs path.Match and
// not filepath.Match — but matchSegmentGlob also carries a separate
// directory-prefix rule that is not glob matching at all, and a trailing
// `/*` goes through it. See matchSegmentGlob.
func matchFidelityGlob(pattern, rel string) bool {
	pattern = filepath.ToSlash(pattern)
	rel = filepath.ToSlash(rel)

	// `**` in any position, matching zero or more whole segments. The
	// branches below only ever recognised it bare, leading or trailing, so
	// a middle `**` fell through to path.Match and came back
	// segment-bounded — `internal/**/*_test.go`, the example in the
	// find_files schema, missed every file more than one directory down.
	//
	// This runs first and only ever adds a match: every branch below is
	// left exactly as it was, so the directory-prefix rules and the
	// basename fallback keep deciding everything they decided before.
	if strings.Contains(pattern, "**") &&
		matchGlobstarSegments(strings.Split(pattern, "/"), strings.Split(rel, "/")) {
		return true
	}

	// Trailing `/**` (or bare `**`): match the directory and the whole
	// subtree beneath it.
	if pattern == "**" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return rel == prefix || strings.HasPrefix(rel, prefix+"/")
	}

	// Leading `**/`: match the suffix glob at any directory depth.
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		if matchSegmentGlob(suffix, rel) {
			return true
		}
		// Try the suffix against every trailing path component so
		// `**/foo/*.go` matches `a/b/foo/x.go`.
		segs := strings.Split(rel, "/")
		for i := range segs {
			if matchSegmentGlob(suffix, strings.Join(segs[i:], "/")) {
				return true
			}
		}
		return false
	}

	return matchSegmentGlob(pattern, rel)
}

// matchGlobstarSegments matches a segment-split pattern against a
// segment-split path, giving `**` its usual meaning: zero or more whole
// segments, wherever it appears. Every other segment is matched with
// path.Match, so a single `*` stays inside its own segment.
//
// Zero segments is deliberate — `internal/**/*_test.go` has to match
// `internal/foo_test.go` as well as `internal/a/b/c_test.go`, or the
// pattern means something different at each depth.
func matchGlobstarSegments(pattern, rel []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(rel); i++ {
				if matchGlobstarSegments(pattern[1:], rel[i:]) {
					return true
				}
			}
			return false
		}
		if len(rel) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], rel[0]); !ok {
			return false
		}
		pattern, rel = pattern[1:], rel[1:]
	}
	return len(rel) == 0
}

// matchSegmentGlob applies the single-segment glob semantics shared
// with matchPathPattern: a glob match against the full path and the
// basename, plus a bare directory-prefix shortcut.
//
// path.Match, not filepath.Match. Both callers hand this function a
// forward-slash path, and filepath.Match's separator is the platform's:
// on Windows '/' is an ordinary character there, so `*` crosses it and
// `internal/*.go` matches `internal/sub/x.go`. path.Match's separator is
// always '/', which is the semantics this file's `**` handling — and its
// own doc-comment — already assume.
//
// The two prefix shortcuts below are deliberately not glob matching, and
// they are what makes `internal/*` recursive: `dir/*` and a bare `dir`
// both match the directory and everything beneath it, the same reach as
// `dir/**`. That predates the path.Match change and callers depend on
// it, so it stays — but it means "a single `*` never crosses `/`"
// describes the glob rule, not this function's whole verdict.
// TestMatchFidelityGlob_DirStarStaysRecursive pins it.
func matchSegmentGlob(pattern, rel string) bool {
	if ok, _ := path.Match(pattern, rel); ok {
		return true
	}
	if ok, _ := path.Match(pattern, path.Base(rel)); ok {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	if strings.HasPrefix(rel, pattern+"/") {
		return true
	}
	return false
}
