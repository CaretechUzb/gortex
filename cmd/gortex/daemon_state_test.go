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

func TestCanSkipWarmGlobalResolveRequiresCompletionAndStableRevision(t *testing.T) {
	base := warmGlobalResolveSafety{
		resolveOK:                     true,
		deferredCrossRepoComplete:     true,
		deferredMutationRevision:      42,
		deferredMutationRevisionKnown: true,
		backfillRevisionBefore:        42,
		backfillRevisionBeforeKnown:   true,
		backfillRevisionAfter:         43,
		backfillRevisionAfterKnown:    true,
		currentMutationRevision:       43,
		currentMutationRevisionKnown:  true,
		backfillResolutionAffected:    0,
	}
	if !canSkipWarmGlobalResolve(base) {
		t.Fatal("physical-only backfill at a bounded revision should elide the duplicate full sweep")
	}

	cases := []struct {
		name   string
		mutate func(*warmGlobalResolveSafety)
	}{
		{"pre-enrichment resolve failed", func(s *warmGlobalResolveSafety) { s.resolveOK = false }},
		{"deferred catch-up failed", func(s *warmGlobalResolveSafety) { s.deferredCrossRepoComplete = false }},
		{"completion revision unsupported", func(s *warmGlobalResolveSafety) { s.deferredMutationRevisionKnown = false }},
		{"pre-backfill revision unsupported", func(s *warmGlobalResolveSafety) { s.backfillRevisionBeforeKnown = false }},
		{"post-backfill revision unsupported", func(s *warmGlobalResolveSafety) { s.backfillRevisionAfterKnown = false }},
		{"current revision unsupported", func(s *warmGlobalResolveSafety) { s.currentMutationRevisionKnown = false }},
		{"writer interleaved before backfill", func(s *warmGlobalResolveSafety) { s.backfillRevisionBefore++ }},
		{"writer interleaved after backfill", func(s *warmGlobalResolveSafety) { s.currentMutationRevision++ }},
		{"workspace eligibility changed", func(s *warmGlobalResolveSafety) { s.backfillResolutionAffected = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			safety := base
			tc.mutate(&safety)
			if canSkipWarmGlobalResolve(safety) {
				t.Fatal("incomplete or stale cross-repo proof elided the full sweep")
			}
		})
	}
}

func TestSelectWarmGlobalResolveAction(t *testing.T) {
	complete := warmGlobalResolveSafety{
		resolveOK:                     true,
		deferredCrossRepoComplete:     true,
		deferredMutationRevision:      17,
		deferredMutationRevisionKnown: true,
		backfillRevisionBefore:        17,
		backfillRevisionBeforeKnown:   true,
		backfillRevisionAfter:         18,
		backfillRevisionAfterKnown:    true,
		currentMutationRevision:       18,
		currentMutationRevisionKnown:  true,
	}
	interleaved := complete
	interleaved.currentMutationRevision++
	eligibilityChanged := complete
	eligibilityChanged.backfillResolutionAffected = 1
	cases := []struct {
		name                string
		anyChanged          bool
		safety              warmGlobalResolveSafety
		backfilledContracts int
		want                warmGlobalResolveAction
	}{
		{"changed completed deferred catch-up across physical stamps", true, complete, 0, warmGlobalResolveContracts},
		{"changed failed initial resolve", true, warmGlobalResolveSafety{}, 0, warmGlobalResolveFull},
		{"changed failed deferred catch-up", true, warmGlobalResolveSafety{resolveOK: true}, 0, warmGlobalResolveFull},
		{"changed interleaving graph writer", true, interleaved, 0, warmGlobalResolveFull},
		{"changed workspace eligibility", true, eligibilityChanged, 0, warmGlobalResolveFull},
		{"unchanged physical-only node stamps", false, warmGlobalResolveSafety{}, 0, warmGlobalResolveNone},
		{"unchanged workspace eligibility", false, warmGlobalResolveSafety{backfillResolutionAffected: 1}, 0, warmGlobalResolveFull},
		{"unchanged legacy contract stamp", false, warmGlobalResolveSafety{}, 1, warmGlobalResolveContracts},
		{"unchanged physical and contract stamps", false, warmGlobalResolveSafety{}, 1, warmGlobalResolveContracts},
		{"unchanged clean snapshot", false, warmGlobalResolveSafety{}, 0, warmGlobalResolveNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectWarmGlobalResolveAction(tc.anyChanged, tc.safety, tc.backfilledContracts); got != tc.want {
				t.Fatalf("action = %v, want %v", got, tc.want)
			}
		})
	}
}
