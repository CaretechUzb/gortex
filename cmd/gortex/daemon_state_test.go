package main

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/config"
)

// TestLSPDisabledSet_ConfigOnly — a `semantic.providers` entry with
// `enabled: false` whose name matches a known LSP spec lands in the
// disabled set. Entries with unknown names are ignored (so an
// `enabled: false` for a custom non-registry daemon doesn't shadow
// a same-named LSP).
func TestLSPDisabledSet_ConfigOnly(t *testing.T) {
	got := lspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
		{Name: "tsserver", Enabled: true}, // explicitly enabled — must NOT land in disabled
		{Name: "not-a-real-lsp", Enabled: false},
	}, "")
	want := map[string]bool{"gopls": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvOnly — comma-separated names land in the
// disabled set. Whitespace is trimmed; empty entries are skipped.
func TestLSPDisabledSet_EnvOnly(t *testing.T) {
	got := lspDisabledSet(nil, "gopls, tsserver,, ,pyright")
	want := map[string]bool{"gopls": true, "tsserver": true, "pyright": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_EnvAllKillSwitch — the literal value "all" or
// "*" sets the special "__all__" key, signalling callers to skip
// auto-registration entirely.
func TestLSPDisabledSet_EnvAllKillSwitch(t *testing.T) {
	for _, env := range []string{"all", "ALL", "*", " all "} {
		got := lspDisabledSet(nil, env)
		if !got["__all__"] {
			t.Fatalf("env=%q: expected __all__ kill switch, got %v", env, got)
		}
	}
}

// TestLSPDisabledSet_ConfigAndEnvMerge — disables from both sources
// merge cleanly into one map.
func TestLSPDisabledSet_ConfigAndEnvMerge(t *testing.T) {
	got := lspDisabledSet([]config.SemanticProviderConfig{
		{Name: "gopls", Enabled: false},
	}, "tsserver,pyright")
	want := map[string]bool{
		"gopls":    true,
		"tsserver": true,
		"pyright":  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLSPDisabledSet_Empty — no providers, empty env yields an empty
// map (not nil — callers index into it).
func TestLSPDisabledSet_Empty(t *testing.T) {
	got := lspDisabledSet(nil, "")
	if got == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

// TestWarmMtimePrefix covers the key the warm-restart mtime lookup hangs on.
// It must be the prefix the indexer actually wrote the rows under, which is
// now always the effective prefix — a lone repo included.
//
// This used to mirror the indexer's single-vs-multi gate and return "" for a
// lone repo. The two decisions had to agree exactly, and when they drifted
// the symptom was not an error but a full cold re-index (with an API
// embedder, a paid re-embed) on every restart.
func TestWarmMtimePrefix(t *testing.T) {
	cases := []struct {
		name       string
		effective  string
		wantPrefix string
		wantOK     bool
	}{
		{"a lone repo is keyed by its prefix like any other", "drools", "drools", true},
		{"a derived worktree prefix is preserved", "drools@ws", "drools@ws", true},
		{"no effective prefix is untrustworthy — cold index instead", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPrefix, gotOK := warmMtimePrefix(tc.effective)
			if gotPrefix != tc.wantPrefix || gotOK != tc.wantOK {
				t.Fatalf("warmMtimePrefix(%q) = (%q, %v), want (%q, %v)",
					tc.effective, gotPrefix, gotOK, tc.wantPrefix, tc.wantOK)
			}
		})
	}
}

func TestCanSkipWarmGlobalResolveRequiresEveryExactSafetySignal(t *testing.T) {
	base := warmGlobalResolveSafety{
		exactDelta:                     true,
		resolveOK:                      true,
		deferredExactCrossRepoComplete: true,
	}
	if !canSkipWarmGlobalResolve(base) {
		t.Fatal("fully exact warm delta should elide the duplicate full cross-repo sweep")
	}

	cases := []struct {
		name   string
		mutate func(*warmGlobalResolveSafety)
	}{
		{"initial frontier not exact", func(s *warmGlobalResolveSafety) { s.exactDelta = false }},
		{"pre-enrichment resolve failed", func(s *warmGlobalResolveSafety) { s.resolveOK = false }},
		{"deferred catch-up fell back", func(s *warmGlobalResolveSafety) { s.deferredExactCrossRepoComplete = false }},
		{"repository scope unknown", func(s *warmGlobalResolveSafety) { s.scopeUnknown = true }},
		{"snapshot partial", func(s *warmGlobalResolveSafety) { s.snapshotPartial = true }},
		{"store needs rebuild", func(s *warmGlobalResolveSafety) { s.needsRebuild = true }},
		{"operator forced full resolve", func(s *warmGlobalResolveSafety) { s.forcedFull = true }},
		{"workspace slug stamped nodes", func(s *warmGlobalResolveSafety) { s.backfilledNodes = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			safety := base
			tc.mutate(&safety)
			if canSkipWarmGlobalResolve(safety) {
				t.Fatal("unsafe warmup shape elided the full cross-repo sweep")
			}
		})
	}
}
