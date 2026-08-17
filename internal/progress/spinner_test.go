package progress

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSpinnerDisabledModePrintsPlainText(t *testing.T) {
	var buf bytes.Buffer
	sp := NewSpinner(&buf)
	sp.Disable()

	sp.Start("Indexing repository")
	sp.Report("walking files", 0, 0)
	sp.Report("walking files", 50, 100) // same stage, must not re-emit
	sp.Report("parsing", 0, 0)          // new stage: finishes the previous, starts its own
	sp.Done()

	out := buf.String()
	wants := []string{
		"Indexing repository",
		"walking files ...",     // stage start
		"✓ walking files · 50",  // stage finish with final counter
		"parsing ...",           // next stage start
		"✓ parsing",             // finished by Done
		"✓ Indexing repository", // overall summary
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, out)
		}
	}
	// Each stage prints exactly twice — one start line, one finish line —
	// regardless of how many progress ticks arrive in between.
	if got := strings.Count(out, "walking files"); got != 2 {
		t.Errorf("expected stage 'walking files' to print exactly twice (start+finish), got %d\n--- got ---\n%s", got, out)
	}
	if idx := strings.Index(out, "parsing ..."); idx < strings.Index(out, "✓ walking files") {
		t.Errorf("stage finish line must precede the next stage start\n--- got ---\n%s", out)
	}
}

func TestSpinnerDisabledFailPrintsErrorLine(t *testing.T) {
	var buf bytes.Buffer
	sp := NewSpinner(&buf)
	sp.Disable()

	sp.Start("Stamping blame")
	sp.Fail(errors.New("boom"))

	out := buf.String()
	if !strings.Contains(out, "✗ Stamping blame") {
		t.Errorf("expected ✗ summary, got: %q", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("expected error message in output, got: %q", out)
	}
}

func TestSpinnerDoneIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	sp := NewSpinner(&buf)
	sp.Disable()

	sp.Start("Indexing")
	sp.Done()
	sp.Done()                   // must not double-print or panic
	sp.Fail(errors.New("late")) // must be a no-op after Done

	if got := strings.Count(buf.String(), "✓"); got != 1 {
		t.Errorf("expected exactly one ✓, got %d in:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "✗") {
		t.Errorf("expected no ✗ after successful Done, got: %s", buf.String())
	}
}

// TestASCIIGlyphFallbackOnOEMCodepage proves the F8 contract: a terminal that
// cannot render UTF-8 (a legacy OEM codepage, a linux virtual console, or an
// explicit GORTEX_ASCII) gets an ASCII glyph set for the spinner finish
// markers AND the box-drawing border — not just the check/cross glyphs.
func TestASCIIGlyphFallbackOnOEMCodepage(t *testing.T) {
	t.Run("ascii_override_selects_ascii_set", func(t *testing.T) {
		t.Setenv("GORTEX_UNICODE", "")
		t.Setenv("GORTEX_ASCII", "1")
		if supportsUnicode() {
			t.Fatal("GORTEX_ASCII=1 must disable unicode")
		}
		g := activeGlyphs()
		if g.OK != "+" || g.Fail != "x" {
			t.Errorf("ascii markers = %q/%q, want +/x", g.OK, g.Fail)
		}
		// The border charset is ASCII too — the beat-codegraph axis.
		if g.Border.TopLeft != "+" {
			t.Errorf("ascii border TopLeft = %q, want +", g.Border.TopLeft)
		}
	})

	t.Run("unicode_override_wins_over_term_linux", func(t *testing.T) {
		t.Setenv("GORTEX_ASCII", "")
		t.Setenv("TERM", "linux")
		t.Setenv("GORTEX_UNICODE", "1")
		if !supportsUnicode() {
			t.Fatal("GORTEX_UNICODE=1 must enable unicode even on TERM=linux")
		}
		if activeGlyphs().OK != "✓" {
			t.Error("unicode set must use ✓")
		}
	})

	t.Run("term_linux_falls_back_to_ascii", func(t *testing.T) {
		t.Setenv("GORTEX_ASCII", "")
		t.Setenv("GORTEX_UNICODE", "")
		t.Setenv("TERM", "linux")
		if supportsUnicode() {
			t.Fatal("TERM=linux must fall back to ASCII")
		}
	})

	t.Run("spinner_done_uses_ascii_marker", func(t *testing.T) {
		t.Setenv("GORTEX_UNICODE", "")
		t.Setenv("GORTEX_ASCII", "1")
		var buf bytes.Buffer
		sp := NewSpinner(&buf) // not a TTY → disabled → direct Fprintf path
		sp.Start("Indexing")
		sp.Done()
		out := buf.String()
		if !strings.Contains(out, "+ Indexing") {
			t.Errorf("done line should use the ASCII marker; got %q", out)
		}
		if strings.Contains(out, "✓") {
			t.Errorf("done line must not contain the unicode check; got %q", out)
		}
	})

	t.Run("spinner_fail_uses_ascii_cross", func(t *testing.T) {
		t.Setenv("GORTEX_UNICODE", "")
		t.Setenv("GORTEX_ASCII", "1")
		var buf bytes.Buffer
		sp := NewSpinner(&buf)
		sp.Start("Indexing")
		sp.Fail(errors.New("boom"))
		out := buf.String()
		if !strings.Contains(out, "x Indexing") {
			t.Errorf("fail line should use the ASCII cross; got %q", out)
		}
		if strings.Contains(out, "✗") {
			t.Errorf("fail line must not contain the unicode cross; got %q", out)
		}
	})

	t.Run("card_border_is_ascii", func(t *testing.T) {
		t.Setenv("GORTEX_UNICODE", "")
		t.Setenv("GORTEX_ASCII", "1")
		card := Card("", "hello")
		if !strings.Contains(card, "+") {
			t.Errorf("ascii card must use + corners; got %q", card)
		}
		if strings.ContainsAny(card, "╭╮╰╯─│") {
			t.Errorf("ascii card must not use rounded box-drawing; got %q", card)
		}
	})
}
