package main

import "testing"

func TestDaemonSkewWarning(t *testing.T) {
	cases := []struct{ name, daemonV, localV, want string }{
		{"equal versions", "v0.63.4+5f5fce2", "v0.63.4+5f5fce2", ""},
		{"daemon older", "v0.63.4+5f9ce2a", "v0.63.5+abcdef1",
			"warning: daemon v0.63.4+5f9ce2a != binary v0.63.5+abcdef1 — run 'gortex daemon restart'"},
		{"daemon version empty", "", "v0.63.5+abcdef1", ""},
		{"local dev build", "v0.63.4+5f9ce2a", "v0.0.0-dev", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonSkewWarning(tc.daemonV, tc.localV); got != tc.want {
				t.Fatalf("daemonSkewWarning(%q, %q) = %q, want %q", tc.daemonV, tc.localV, got, tc.want)
			}
		})
	}
}
