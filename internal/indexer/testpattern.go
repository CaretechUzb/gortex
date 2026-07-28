package indexer

import (
	"path/filepath"
	"strings"
)

// IsTestFile returns true when the file's name or directory matches a
// recognised test convention from the table below. False positives
// here are downgraded downstream by the symbol-name filter
// (IsTestSymbol).
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
//	  test/, spec/                     (any language using these dirs)
func IsTestFile(path string) bool {
	if path == "" {
		return false
	}
	// Directory-based hints first — covers projects that don't follow
	// the per-file naming convention.
	dir := filepath.ToSlash(path)
	for _, marker := range []string{"/__tests__/", "/tests/", "/test/", "/spec/"} {
		if strings.Contains(dir, marker) {
			return true
		}
	}
	if strings.HasPrefix(dir, "tests/") || strings.HasPrefix(dir, "test/") || strings.HasPrefix(dir, "spec/") {
		return true
	}

	base := filepath.Base(path)
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

// TestRole classifies a function/method name by its language's test
// convention and returns the specific role — "test", "benchmark",
// "fuzz", or "example" — or "" when the name matches no convention.
// For languages where test runners pick up by annotation (Java @Test,
// Rust #[test]) or by file membership alone (TS/JS), the name carries
// no role signal; callers fall back to IsTestFile and treat such
// symbols as a plain "test".
func TestRole(name, language string) string {
	if name == "" {
		return ""
	}
	switch language {
	case "go":
		switch {
		case hasTestPrefix(name, "Benchmark"):
			return "benchmark"
		case hasTestPrefix(name, "Fuzz"):
			return "fuzz"
		case hasTestPrefix(name, "Example"):
			return "example"
		case hasTestPrefix(name, "Test"):
			return "test"
		}
	case "python":
		if strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "Test") {
			return "test"
		}
	case "ruby":
		if strings.HasPrefix(name, "test_") {
			return "test"
		}
	case "php":
		// PHPUnit discovers `public function testFoo()`. The prefix must be
		// followed by an uppercase letter or nothing, so `testing()` and
		// `testable()` — ordinary methods — are not swept in.
		if hasTestPrefix(name, "test") {
			return "test"
		}
	}
	return ""
}

// IsTestSymbol returns true when a function/method name looks like a
// test entry point per its language's convention. It is a back-compat
// wrapper over TestRole — callers that need the specific role should
// use TestRole directly.
func IsTestSymbol(name, language string) bool {
	return TestRole(name, language) != ""
}

// AnnotationTestRole maps a (language, annotation-name) pair to a test
// role for the languages whose runners discover tests by attribute
// rather than by file location — Rust's #[test] / #[bench] family and
// the JVM JUnit / TestNG @Test family. Returns "" when the annotation
// does not denote a test.
//
// The name is the bare attribute path as captured by the extractor (no
// leading `@` / `#[`). Rust scoped attributes arrive as "tokio::test" /
// "async_std::test"; JVM annotations may be written fully qualified
// ("org.junit.jupiter.api.Test"), so the JVM branch matches on the
// last path segment. This is the signal that lets an inline #[test] fn
// in a production-path src/foo.rs — or a @Test method in a class whose
// file name carries no test suffix — classify as a test even though
// IsTestFile is false for its file.
func AnnotationTestRole(language, name string) string {
	if name == "" {
		return ""
	}
	switch language {
	case "rust":
		switch {
		case name == "bench":
			return "benchmark"
		case name == "test" || strings.HasSuffix(name, "::test"):
			// #[test], #[tokio::test], #[async_std::test], #[actix_rt::test], …
			return "test"
		case name == "rstest" || name == "test_case" || name == "googletest":
			return "test"
		}
	case "php":
		// PHPUnit 10+ marks tests with attributes under
		// PHPUnit\Framework\Attributes, written either bare (`#[Test]`) or
		// fully qualified; the docblock form (`@test`) reaches here through
		// the same path. `#[DataProvider]` and `#[TestDox]` describe a test
		// rather than declaring one, so they carry no role.
		short := name
		if i := strings.LastIndex(short, `\`); i >= 0 {
			short = short[i+1:]
		}
		switch short {
		case "Test", "test", "TestWith", "TestWithJson":
			return "test"
		}
	case "java", "kotlin":
		short := name
		if i := strings.LastIndexByte(short, '.'); i >= 0 {
			short = short[i+1:]
		}
		switch short {
		case "Test", "ParameterizedTest", "RepeatedTest", "TestFactory", "TestTemplate":
			return "test"
		}
	}
	return ""
}

// AnnotationTestRunner names the test runner for an annotation-discovered
// test that lives in a production-path file, where the file-name and
// import heuristics in detectTestRunnerForFile do not apply. Returns ""
// for languages without an attribute-driven runner.
func AnnotationTestRunner(language string) string {
	switch language {
	case "rust":
		return "cargo-test"
	case "java", "kotlin":
		return "junit"
	case "php":
		return "phpunit"
	}
	return ""
}

func hasTestPrefix(name string, prefixes ...string) bool {
	for _, p := range prefixes {
		if !strings.HasPrefix(name, p) {
			continue
		}
		// Must be followed by an uppercase letter or end of name —
		// "Testing" is not a Go test fn but "TestFoo" is. "Test" alone
		// is not picked up by `go test` either; require a suffix.
		if len(name) == len(p) {
			return false
		}
		c := name[len(p)]
		if c >= 'A' && c <= 'Z' {
			return true
		}
	}
	return false
}
