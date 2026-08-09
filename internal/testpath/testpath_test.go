package testpath

import "testing"

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"", false},
		{"internal/indexer/indexer_test.go", true},
		{"cmd/gortex/main.go", false},
		{"src/components/Button.test.tsx", true},
		{"src/components/Button.spec.ts", true},
		{"src/components/Button.tsx", false},
		{"tests/test_user.py", true},
		{"app/test_helpers.py", true},
		{"app/helpers.py", false},
		{"lib/user_test.dart", true},
		{"lib/user.dart", false},
		{"spec/user_spec.rb", true},
		{"app/user.rb", false},
		{"src/UserTest.java", true},
		{"src/User.java", false},
		{"src/UserTests.kt", true},
		{"src/User.kt", false},
		{"X/Y/__tests__/Z.ts", true},
		{"X/Y/__tests__/Z.js", true},
		{"X/test/foo.go", true}, // dir marker overrides extension miss
		{"src/UserTests.cs", true},
		{"src/UserTests.swift", true},
		{"src/UserTest.php", true},

		// A repo-root test directory has no leading separator to anchor on.
		{"__tests__/Z.ts", true},
		{"tests/helpers.rb", true},
		{"test/main.c", true},
		{"spec/support/factory.rb", true},

		// Windows-shaped paths classify like their POSIX spelling. Graph
		// nodes on Windows carry backslash-separated paths; a separator-blind
		// substring check used to miss the directory markers entirely.
		{`src\components\Button.test.ts`, true},
		{`src\__tests__\Z.ts`, true},
		{`src\components\Button.ts`, false},

		// A "test" substring is not a test file. The old analysis-side copy
		// matched "test_" anywhere in the path, so these read as covering
		// tests and inflated coverage.
		{"src/latest_release.go", false},
		{"pkg/contest/entry.py", false},
		{"internal/attestation.ts", false},

		// Directory markers must be whole path segments.
		{"src/testing/helper.go", false},
		{"src/spectrum/color.ts", false},

		// Directory markers match case-insensitively. .NET solutions
		// capitalize test directories (Test/, Tests/), and the files
		// beneath them frequently don't carry a Test/Tests filename
		// suffix — the marker is the only signal.
		{"App/Test/Unit/CalculatorProbe.cs", true},
		{`App\Tests\Support\Builder.cs`, true},
		{"Test/main.cs", true},

		// The whole-segment rule holds case-insensitively too.
		{"src/Testing/helper.go", false},
		{"src/Spectrum/color.ts", false},
	}
	for _, c := range cases {
		if got := IsTestFile(c.path); got != c.want {
			t.Errorf("IsTestFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
