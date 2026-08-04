package main

import "testing"

func TestEnrichmentOverlapRequiresExplicitOptIn(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset"},
		{name: "disabled", value: "0"},
		{name: "word true is not implicit opt in", value: "true"},
		{name: "enabled", value: "1", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GORTEX_ENRICH_OVERLAP", tt.value)
			if got := enrichmentOverlapEnabled(); got != tt.want {
				t.Fatalf("enrichmentOverlapEnabled() = %v, want %v for %q", got, tt.want, tt.value)
			}
		})
	}
}
