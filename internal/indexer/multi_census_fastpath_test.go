package indexer

import "testing"

func TestFullCoverageGlobalPass(t *testing.T) {
	t.Parallel()

	partial := map[string]struct{}{"repo-a": {}}
	tests := []struct {
		name           string
		scope          map[string]struct{}
		censusEligible bool
		want           bool
	}{
		{name: "nil scope is whole graph", scope: nil, want: true},
		{name: "partial scope remains scoped", scope: partial, want: false},
		{name: "empty explicit scope remains scoped", scope: map[string]struct{}{}, want: false},
		{name: "census attests full explicit scope", scope: partial, censusEligible: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := fullCoverageGlobalPass(tt.scope, tt.censusEligible); got != tt.want {
				t.Fatalf("fullCoverageGlobalPass() = %v, want %v", got, tt.want)
			}
		})
	}
}
