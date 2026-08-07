// Package testpath carries the canonical "is this a test source file?"
// predicate.
//
// The predicate is consumed by packages on both sides of every import
// boundary that matters — the indexer stamps test symbols with it, the
// resolver excludes test files from dispatch inference, and the impact
// analyzer uses it to attribute covering tests. Each of those grew its own
// copy, and the copies drifted: the analysis copy recognised seven filename
// fragments where the indexer recognised eleven language conventions plus
// four directory markers, so a Ruby `_spec.rb` or a JUnit `FooTest.java`
// counted as a test when the indexer emitted its `tests` edges but not when
// the impact analyzer looked for covering tests. A change could then read as
// untested in one tool and covered in another, from the same graph.
//
// This package is a stdlib-only leaf so every one of those callers can
// depend on it without an import cycle. It is the single definition.
package testpath

import (
	"path/filepath"
	"strings"
)

// dirMarkers are the directory names that mark everything beneath them as
// test code, regardless of the per-file naming convention.
var dirMarkers = []string{"__tests__", "tests", "test", "spec"}

// IsTestFile reports whether the file's name or directory matches a
// recognised test convention. False positives are downgraded downstream by
// the symbol-name filter (indexer.IsTestSymbol).
//
// Backslashes are folded to slashes on every platform, not just via
// filepath.ToSlash on Windows. Graph node paths carry the indexing host's
// separator while git emits forward slashes, so both spellings reach this
// predicate in the same process; classifying them differently is what let a
// directory marker slip past. The cost is that a POSIX file whose *name*
// literally contains a backslash may read as a directory marker — a trade
// worth making against a cross-platform misclassification.
//
// Recognised conventions:
//
//	*_test.go                          (Go)
//	*.test.{ts,tsx,js,jsx,mts,cts}     (TS/JS via Jest/Vitest convention)
//	*.spec.{ts,tsx,js,jsx,mts,cts}     (TS/JS spec convention)
//	test_*.py / *_test.py              (Python)
//	*_test.dart                        (Dart)
//	*_spec.rb / *_test.rb              (Ruby)
//	*Test.java / *Tests.java           (JUnit / Spring)
//	*Test.kt  / *Tests.kt              (Kotlin)
//	*Tests.cs                          (C# xUnit/NUnit)
//	*Tests.swift                       (Swift)
//	*Test.php / *test.php              (PHPUnit / Pest)
//	files under __tests__/, tests/,
//	  test/, spec/                     (any language using these dirs;
//	                                    matched case-insensitively)
func IsTestFile(path string) bool {
	if path == "" {
		return false
	}
	// Directory-based hints first — covers projects that don't follow
	// the per-file naming convention. Markers match case-insensitively:
	// .NET solutions capitalize their test directories (Test/, Tests/)
	// and often name the files beneath them without a Test suffix, so a
	// case-sensitive scan left every symbol in them stamped as
	// production code.
	slashed := strings.ReplaceAll(path, `\`, "/")
	folded := strings.ToLower(slashed)
	for _, marker := range dirMarkers {
		if strings.Contains(folded, "/"+marker+"/") || strings.HasPrefix(folded, marker+"/") {
			return true
		}
	}

	base := slashed
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.TrimSuffix(base, ext)

	switch ext {
	case ".go":
		return strings.HasSuffix(stem, "_test")
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
	case ".py":
		return strings.HasPrefix(stem, "test_") || strings.HasSuffix(stem, "_test")
	case ".dart":
		return strings.HasSuffix(stem, "_test")
	case ".rb":
		return strings.HasSuffix(stem, "_spec") || strings.HasSuffix(stem, "_test")
	case ".java", ".kt":
		return strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests")
	case ".cs":
		return strings.HasSuffix(stem, "Tests") || strings.HasSuffix(stem, "Test")
	case ".swift":
		return strings.HasSuffix(stem, "Tests")
	case ".php":
		return strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "test")
	}
	return false
}
