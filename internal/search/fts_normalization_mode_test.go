package search

import "testing"

func TestFTSNormalizationModeTracksConfiguredCorpusMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		want string
	}{
		{name: "disabled", on: false, want: ""},
		{name: "enabled", on: true, want: "porter-v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStemming(t, tc.on)
			if got := FTSNormalizationMode(); got != tc.want {
				t.Fatalf("FTSNormalizationMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
