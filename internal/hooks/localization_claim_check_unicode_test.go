package hooks

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundedLocalizationClaimCheckPreservesUTF8AndByteLimit(t *testing.T) {
	// Place a multibyte rune across the nominal byte cut. The hook reason must
	// remain valid UTF-8 because encoding/json otherwise replaces the partial
	// rune and can expand the supposedly bounded response.
	prompt := strings.Repeat("a", localizationClaimCheckMaxChars-35) + "界" + strings.Repeat("b", 100)
	got := boundedLocalizationClaimCheck(prompt)
	if !utf8.ValidString(got) {
		t.Fatalf("bounded claim check is invalid UTF-8: %q", got)
	}
	if len(got) > localizationClaimCheckMaxChars {
		t.Fatalf("bounded claim check bytes=%d want <=%d", len(got), localizationClaimCheckMaxChars)
	}
	if !strings.HasSuffix(got, "Do not retrieve more evidence.") {
		t.Fatalf("bounded claim check lost terminal instruction: %q", got)
	}
}
