package main

import (
	"testing"

	semver "github.com/zzet/gortex/internal/version"
)

func TestDaemonSkewWarning(t *testing.T) {
	cases := []struct{ name, daemonV, localV, want string }{
		{"equal versions", "v0.63.4+5f5fce2", "v0.63.4+5f5fce2", ""},
		{"daemon older", "v0.63.4+5f9ce2a", "v0.63.5+abcdef1",
			"warning: daemon v0.63.4+5f9ce2a != binary v0.63.5+abcdef1 — run 'gortex daemon restart' to upgrade the daemon"},
		{"daemon newer", "v0.63.5+abcdef1", "v0.63.4+5f9ce2a",
			"warning: daemon v0.63.5+abcdef1 != binary v0.63.4+5f9ce2a — this binary is older than the running daemon — run 'gortex upgrade'"},
		{"same version different build", "v0.63.4+aaaaaaa", "v0.63.4+bbbbbbb",
			"warning: daemon v0.63.4+aaaaaaa != binary v0.63.4+bbbbbbb — run 'gortex daemon restart' or 'gortex upgrade'"},
		{"unparseable daemon version", "strawberry", "v0.63.5+abcdef1",
			"warning: daemon strawberry != binary v0.63.5+abcdef1 — run 'gortex daemon restart' or 'gortex upgrade'"},
		{"daemon version empty", "", "v0.63.5+abcdef1", ""},
		{"local dev build", "v0.63.4+5f9ce2a", "v0.0.0-dev", ""},
		{"daemon dev build", "v0.0.0-dev", "v0.63.5+abcdef1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonSkewWarning(tc.daemonV, tc.localV); got != tc.want {
				t.Fatalf("daemonSkewWarning(%q, %q) = %q, want %q", tc.daemonV, tc.localV, got, tc.want)
			}
		})
	}
}

// TestCompareSemverPrecedence pins the SemVer 2.0.0 §11 precedence
// chain on compareSemver, which the release-only table above never
// exercised: the canonical ascending pre-release ladder (each adjacent
// pair in spec order), build metadata being ignored for precedence
// (§10), and numeric identifiers ranking below alphanumeric ones (§11).
// Each row asserts the comparator's exact sign (-1 / 0 / +1) in the
// argument order given.
func TestCompareSemverPrecedence(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		// §11's ascending example chain, adjacent pair by adjacent pair.
		{"alpha lt alpha.1 (larger field set ranks higher)", "v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"alpha.1 lt alpha.beta (numeric lt alphanumeric)", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", -1},
		{"alpha.beta lt beta (ASCII sort)", "v1.0.0-alpha.beta", "v1.0.0-beta", -1},
		{"beta lt beta.2 (larger field set ranks higher)", "v1.0.0-beta", "v1.0.0-beta.2", -1},
		{"beta.2 lt beta.11 (numeric compare, not ASCII)", "v1.0.0-beta.2", "v1.0.0-beta.11", -1},
		{"beta.11 lt rc.1 (ASCII sort)", "v1.0.0-beta.11", "v1.0.0-rc.1", -1},
		{"rc.1 lt release (pre-release ranks below release)", "v1.0.0-rc.1", "v1.0.0", -1},
		{"release gt rc.1 (same pair, reversed)", "v1.0.0", "v1.0.0-rc.1", 1},
		// §10: build metadata MUST be ignored when determining precedence.
		{"build metadata ignored", "v1.0.0+a", "v1.0.0+b", 0},
		// §11: numeric identifiers always rank below alphanumeric ones.
		{"bare numeric ident lt alphanumeric ident", "v1.0.0-1", "v1.0.0-alpha", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := semver.MustParse(tc.a)
			b := semver.MustParse(tc.b)
			if got := compareSemver(a, b); got != tc.want {
				t.Fatalf("compareSemver(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
