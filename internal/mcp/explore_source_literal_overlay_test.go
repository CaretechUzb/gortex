package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/search/trigram"
)

func TestScanExploreSourceLiteralOverlaysDeterministicScopeAndBoundaries(t *testing.T) {
	files := []exploreSourceLiteralOverlayFile{
		{path: `repo\\z.go`, content: "package z\nvar _ = \"café\"\n", eligible: true},
		{path: "repo/a.go", content: "var _ = \"caféteria\"\nvar x = `café`\n", eligible: true},
		{path: "repo/case.go", content: "var _ = `CAFÉ`\n", eligible: true},
		{path: "repo/deleted.go", content: "café", deleted: true, eligible: true},
		{path: "outside/no.go", content: "café", eligible: false},
	}

	got, err := scanExploreSourceLiteralOverlays(context.Background(), "café", files, 24)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.incomplete {
		t.Fatal("bounded scan unexpectedly incomplete")
	}
	if len(got.matches) != 2 {
		t.Fatalf("matches = %#v, want 2", got.matches)
	}
	if got.matches[0].Path != "repo/a.go" || got.matches[0].Line != 2 {
		t.Fatalf("first match = %#v", got.matches[0])
	}
	if got.matches[1].Path != "repo/z.go" || got.matches[1].Line != 2 {
		t.Fatalf("second match = %#v", got.matches[1])
	}
	for _, covered := range []string{"repo/a.go", "repo/case.go", "repo/z.go", "repo/deleted.go", "outside/no.go"} {
		if _, ok := got.covered[covered]; !ok {
			t.Errorf("covered path %q missing: %#v", covered, got.covered)
		}
	}

	for _, text := range []string{"cafe\u0301", "cafe‿value", "decafeteria"} {
		if exploreOverlayLiteralHasBoundary(text, "cafe") {
			t.Errorf("%q accepted as an identifier-bounded cafe", text)
		}
	}
	if !exploreOverlayLiteralHasBoundary("call(cafe)", "cafe") {
		t.Fatal("punctuation-delimited literal not accepted")
	}
}

func TestScanExploreSourceLiteralOverlaysHitSentinel(t *testing.T) {
	makeFiles := func(count int) []exploreSourceLiteralOverlayFile {
		files := make([]exploreSourceLiteralOverlayFile, 0, count)
		for index := 0; index < count; index++ {
			files = append(files, exploreSourceLiteralOverlayFile{
				path:     strings.Repeat("a", index+1) + ".go",
				content:  "needle\n",
				eligible: true,
			})
		}
		return files
	}

	exact, err := scanExploreSourceLiteralOverlays(context.Background(), "needle", makeFiles(24), 24)
	if err != nil {
		t.Fatalf("exact scan: %v", err)
	}
	if len(exact.matches) != 24 || exact.incomplete {
		t.Fatalf("exact cap: matches=%d incomplete=%v, want 24/false", len(exact.matches), exact.incomplete)
	}

	over, err := scanExploreSourceLiteralOverlays(context.Background(), "needle", makeFiles(25), 24)
	if err != nil {
		t.Fatalf("sentinel scan: %v", err)
	}
	if len(over.matches) != 24 || !over.incomplete {
		t.Fatalf("sentinel: matches=%d incomplete=%v, want 24/true", len(over.matches), over.incomplete)
	}
}

func TestScanExploreSourceLiteralOverlaysCapsAndCancellation(t *testing.T) {
	tooLong := strings.Repeat("x", exploreSourceLiteralOverlayMaxLineBytes+1) + " needle"
	got, err := scanExploreSourceLiteralOverlays(context.Background(), "needle", []exploreSourceLiteralOverlayFile{
		{path: "repo/long.go", content: tooLong, eligible: true},
	}, 24)
	if err != nil {
		t.Fatalf("long-line scan: %v", err)
	}
	if !got.incomplete || len(got.matches) != 0 {
		t.Fatalf("long line: matches=%#v incomplete=%v", got.matches, got.incomplete)
	}

	got, err = scanExploreSourceLiteralOverlays(context.Background(), "needle", []exploreSourceLiteralOverlayFile{
		{path: "repo/huge.go", content: strings.Repeat("x", exploreSourceLiteralOverlayMaxBytes+1), eligible: true},
	}, 24)
	if err != nil {
		t.Fatalf("byte-cap scan: %v", err)
	}
	if !got.incomplete || len(got.matches) != 0 {
		t.Fatalf("byte cap: matches=%#v incomplete=%v", got.matches, got.incomplete)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = scanExploreSourceLiteralOverlays(ctx, "needle", []exploreSourceLiteralOverlayFile{
		{path: "repo/a.go", content: "needle", eligible: true},
	}, 24)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
}

func TestScanExploreSourceLiteralOverlaysChargesOnlyInspectedBytes(t *testing.T) {
	largeTail := strings.Repeat("x\n", exploreSourceLiteralOverlayMaxBytes/2+1)
	early, err := scanExploreSourceLiteralOverlays(context.Background(), "needle", []exploreSourceLiteralOverlayFile{
		{path: "repo/early.go", content: "needle\n" + largeTail, eligible: true},
	}, 24)
	if err != nil {
		t.Fatalf("early scan: %v", err)
	}
	if early.incomplete || len(early.matches) != 1 {
		t.Fatalf("early hit: matches=%#v incomplete=%v", early.matches, early.incomplete)
	}

	line := strings.Repeat("x", exploreSourceLiteralOverlayMaxLineBytes-1) + "\n"
	lateContent := strings.Repeat(line, exploreSourceLiteralOverlayMaxBytes/(exploreSourceLiteralOverlayMaxLineBytes-1)+1) + "needle\n"
	late, err := scanExploreSourceLiteralOverlays(context.Background(), "needle", []exploreSourceLiteralOverlayFile{
		{path: "repo/late.go", content: lateContent, eligible: true},
	}, 24)
	if err != nil {
		t.Fatalf("late scan: %v", err)
	}
	if !late.incomplete || len(late.matches) != 0 {
		t.Fatalf("late hit: matches=%#v incomplete=%v", late.matches, late.incomplete)
	}
}

func TestScanExploreSourceLiteralOverlaysDeadline(t *testing.T) {
	started := time.Unix(1, 0)
	calls := 0
	now := func() time.Time {
		calls++
		if calls == 1 {
			return started
		}
		return started.Add(exploreSourceLiteralOverlayBudget)
	}
	got, err := scanExploreSourceLiteralOverlaysWithClock(
		context.Background(), "needle",
		[]exploreSourceLiteralOverlayFile{{path: "repo/a.go", content: "needle", eligible: true}},
		24, now,
	)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !got.incomplete || len(got.matches) != 0 {
		t.Fatalf("deadline: matches=%#v incomplete=%v", got.matches, got.incomplete)
	}
}

func TestMergeExploreSourceLiteralMatchesOverlayWinsAndSortsBeforeLimit(t *testing.T) {
	overlay := []trigram.Match{
		{Path: `repo\\same.go`, Line: 9, Text: "overlay replacement"},
		{Path: "repo/z.go", Line: 1, Text: "overlay z"},
	}
	durable := []trigram.Match{
		{Path: "repo/same.go", Line: 9, Text: "stale same-line text"},
		{Path: "repo/same.go", Line: 1, Text: "stale covered path"},
		{Path: "repo/a.go", Line: 2, Text: "durable a"},
		{Path: "repo/a.go", Line: 2, Text: "durable duplicate"},
	}
	matches, incomplete := mergeExploreSourceLiteralMatches(overlay, durable, map[string]struct{}{
		"repo/same.go": {},
	}, 24)
	if incomplete || len(matches) != 3 {
		t.Fatalf("full merge = %#v incomplete=%v", matches, incomplete)
	}
	if matches[0].Path != "repo/a.go" || matches[1].Text != "overlay replacement" || matches[2].Path != "repo/z.go" {
		t.Fatalf("merge order/precedence = %#v", matches)
	}

	capped, incomplete := mergeExploreSourceLiteralMatches(overlay, durable, map[string]struct{}{
		"repo/same.go": {},
	}, 2)
	if !incomplete || len(capped) != 2 || capped[1].Text != "overlay replacement" {
		t.Fatalf("capped merge = %#v incomplete=%v", capped, incomplete)
	}
}
