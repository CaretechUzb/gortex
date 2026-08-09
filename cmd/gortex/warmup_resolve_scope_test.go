package main

import (
	"testing"
)

// TestWarmupResolveScope covers the whole-graph-fallback matrix for the
// warm-restart resolve scope. The happy path returns the changed set; every
// safety-precondition failure returns nil (whole-graph).
func TestWarmupResolveScope(t *testing.T) {
	changed := map[string]struct{}{"repoa": {}}
	const totalRepos = 3 // more repos than changed, so scoping is beneficial.

	t.Run("scoped happy path", func(t *testing.T) {
		got := warmupResolveScope(changed, totalRepos, true, false, false)
		if got == nil {
			t.Fatal("expected the changed set, got nil")
		}
		if _, ok := got["repoa"]; !ok || len(got) != 1 {
			t.Fatalf("expected scope {repoa}, got %v", got)
		}
	})

	t.Run("nothing changed → nil", func(t *testing.T) {
		if got := warmupResolveScope(changed, totalRepos, false, false, false); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("scopeUnknown → nil", func(t *testing.T) {
		if got := warmupResolveScope(changed, totalRepos, true, true, false); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("storeNeedsRebuild → nil", func(t *testing.T) {
		if got := warmupResolveScope(changed, totalRepos, true, false, true); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("empty changed set → nil", func(t *testing.T) {
		if got := warmupResolveScope(map[string]struct{}{}, totalRepos, true, false, false); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("all repos changed (cold) → nil", func(t *testing.T) {
		all := map[string]struct{}{"repoa": {}, "repob": {}, "repoc": {}}
		if got := warmupResolveScope(all, totalRepos, true, false, false); got != nil {
			t.Fatalf("expected nil for all-repos-changed, got %v", got)
		}
	})

	t.Run("GORTEX_WARMUP_FULL_RESOLVE forces nil", func(t *testing.T) {
		t.Setenv("GORTEX_WARMUP_FULL_RESOLVE", "1")
		if got := warmupResolveScope(changed, totalRepos, true, false, false); got != nil {
			t.Fatalf("expected nil under GORTEX_WARMUP_FULL_RESOLVE=1, got %v", got)
		}
	})
}

// TestWarmupFullResolveForced checks the env override parsing.
func TestWarmupFullResolveForced(t *testing.T) {
	cases := map[string]bool{
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"True":  true,
		"0":     false,
		"":      false,
		"nope":  false,
		"false": false,
	}
	for v, want := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("GORTEX_WARMUP_FULL_RESOLVE", v)
			if got := warmupFullResolveForced(); got != want {
				t.Fatalf("warmupFullResolveForced(%q) = %v, want %v", v, got, want)
			}
		})
	}
}
