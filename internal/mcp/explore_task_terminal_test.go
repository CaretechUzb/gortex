package mcp

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRenderExploreTaskAddsCompletionWithoutChangingBaseRenderer(t *testing.T) {
	targets := exploreTestTargets()
	server := &Server{}
	base := server.renderExplore("retry backoff", targets, 1600)
	got := server.renderExploreTask("retry backoff", targets, 1600, nil)

	if !strings.HasPrefix(got, base) {
		t.Fatalf("task wrapper changed established explore output:\n%s", got)
	}
	for _, want := range []string{
		"## Completion",
		`"state": "answer_ready"`,
		`"required_action": "respond"`,
		`"allowed_tool_calls": 0`,
		"Answer from the ranked evidence and file outlines above.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("task completion missing %q:\n%s", want, got)
		}
	}
}

func TestRenderExploreTaskAddsBoundedTopFileOutline(t *testing.T) {
	targets := exploreTestTargets()
	nodes := []*graph.Node{
		targets[0].node,
		targets[1].node,
		{ID: "retry.go::RetryPolicy", Name: "RetryPolicy", QualName: "RetryPolicy", Kind: graph.KindType, FilePath: "retry.go", StartLine: 2},
	}
	reads := 0
	provider := localizationPageOutlineProvider(nil, targets, exploreTerminalTerms("retry policy"), func(file string) []*graph.Node {
		reads++
		if file != "retry.go" {
			t.Fatalf("enumerated unexpected file %q", file)
		}
		return nodes
	})
	outline := provider()
	got := (&Server{}).renderExploreTask("retry policy", targets, 1600, outline)

	if reads != 1 {
		t.Fatalf("file declarations read %d times, want 1", reads)
	}
	for _, want := range []string{
		exploreTaskOutlineHeading,
		"### retry.go — 3 declaration(s)",
		"- 2: RetryPolicy [t]",
		"- 6: Backoff [f]",
		"- 11: DoWithRetry [f]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("task outline missing %q:\n%s", want, got)
		}
	}
	if estimateTokens(formatExploreTaskOutlines(outline)) > 1600/exploreTaskOutlineBudgetShare {
		t.Fatal("fixture unexpectedly exceeds task outline allowance")
	}
}

func TestRenderExploreTaskOutlineShrinksCloneOnly(t *testing.T) {
	rows := make([]localizationOutlineRow, 0, localizationOutlineRowCap)
	for index := 0; index < localizationOutlineRowCap; index++ {
		rows = append(rows, localizationOutlineRow{Name: strings.Repeat("LongDeclaration", 8), Line: index + 1, Kind: "f"})
	}
	outline := &localizationFileOutline{
		File: "wide.go", Declared: len(rows), Rows: rows, all: rows,
	}
	page := &localizationPageOutline{Leading: outline}
	originalRows := len(page.Leading.Rows)
	got := renderExploreTaskOutlines(page, 64)

	if got != "" && estimateTokens(got) > 64 {
		t.Fatalf("outline exceeded allowance: %d tokens", estimateTokens(got))
	}
	if len(page.Leading.Rows) != originalRows {
		t.Fatalf("cached outline mutated: got %d rows want %d", len(page.Leading.Rows), originalRows)
	}
}
