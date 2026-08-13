package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRenderExploreTaskAddsCompletionWithinBudget(t *testing.T) {
	targets := exploreTestTargets()
	got := (&Server{}).renderExploreTask("retry backoff", targets, 1600, nil)

	for _, want := range []string{
		"EXPLORE — retry backoff",
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
	if used := estimateTokens(got); used > 1600 {
		t.Fatalf("task response used %d tokens, budget 1600", used)
	}
}

func TestRenderExploreTaskNilOutlineDoesNotDisplaceRankedTargets(t *testing.T) {
	targets := make([]exploreTarget, 0, exploreDefaultMaxSymbols)
	for index := 0; index < exploreDefaultMaxSymbols; index++ {
		name := fmt.Sprintf("Candidate%02d", index)
		targets = append(targets, exploreTarget{node: &graph.Node{
			ID:        "candidate.go::" + name,
			Name:      name,
			Kind:      graph.KindFunction,
			FilePath:  "candidate.go",
			StartLine: index + 1,
		}})
	}
	withoutProvider := (&Server{}).renderExploreTask("retry policy", targets, exploreDefaultBudgetTokens, nil)
	calls := 0
	withNilOutline := (&Server{}).renderExploreTask("retry policy", targets, exploreDefaultBudgetTokens, func(actual []exploreTarget) *localizationPageOutline {
		calls++
		if len(actual) != len(targets) {
			t.Fatalf("outline provider received %d targets, want %d", len(actual), len(targets))
		}
		return nil
	})

	if calls != 1 {
		t.Fatalf("outline provider called %d times, want 1", calls)
	}
	if withNilOutline != withoutProvider {
		t.Fatalf("nil optional outline changed ranked task response:\n--- without provider ---\n%s\n--- with provider ---\n%s", withoutProvider, withNilOutline)
	}
	for _, target := range targets {
		if !strings.Contains(withNilOutline, target.node.ID) {
			t.Fatalf("ranked target %q was displaced:\n%s", target.node.ID, withNilOutline)
		}
	}
}

func TestRenderExploreTaskLazilyAddsBoundedTopFileOutline(t *testing.T) {
	targets := exploreTestTargets()
	nodes := []*graph.Node{
		targets[0].node,
		targets[1].node,
		{ID: "retry.go::RetryPolicy", Name: "RetryPolicy", QualName: "RetryPolicy", Kind: graph.KindType, FilePath: "retry.go", StartLine: 2},
	}
	reads := 0
	provider := exploreTaskOutlineProvider(func(actualTargets []exploreTarget) *localizationPageOutline {
		outline := localizationPageOutlineProvider(nil, actualTargets, exploreTerminalTerms("retry policy"), func(file string) []*graph.Node {
			reads++
			if file != "retry.go" {
				t.Fatalf("enumerated unexpected file %q", file)
			}
			return nodes
		})
		return outline()
	})
	got := (&Server{}).renderExploreTask("retry policy", targets, exploreMaxBudgetTokens, provider)

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
	if used := estimateTokens(got); used > exploreMaxBudgetTokens {
		t.Fatalf("task response used %d tokens, budget %d", used, exploreMaxBudgetTokens)
	}
}

func TestRenderExploreTaskDoesNotLoadOutlineWithoutUsefulResidual(t *testing.T) {
	calls := 0
	provider := exploreTaskOutlineProvider(func([]exploreTarget) *localizationPageOutline {
		calls++
		return nil
	})
	budget := estimateTokens(renderExploreTaskCompletion()) + exploreTaskMinimumOutlineTokens - 1
	_ = (&Server{}).renderExploreTask("retry policy", exploreTestTargets(), budget, provider)
	if calls != 0 {
		t.Fatalf("outline provider called %d times without useful residual", calls)
	}
}

func TestRenderExploreTaskHonorsClampedBudgets(t *testing.T) {
	for _, budget := range []int{exploreMinBudgetTokens, exploreDefaultBudgetTokens, exploreMaxBudgetTokens} {
		t.Run(strings.Repeat("b", budget/1000), func(t *testing.T) {
			got := (&Server{}).renderExploreTask("retry policy", exploreTestTargets(), budget, nil)
			if used := estimateTokens(got); used > budget {
				t.Fatalf("task response used %d tokens, budget %d", used, budget)
			}
		})
	}
}

func TestExploreTaskOutlineProviderUsesSelectedReader(t *testing.T) {
	targets := exploreTestTargets()
	selected := &localizationDeclarationSpyReader{
		files:     map[string][]*graph.Node{"retry.go": {targets[0].node}},
		fileCalls: make(map[string]int),
	}
	provider := newExploreTaskPageOutlineProvider(selected, "retry policy")
	if provider == nil || provider(targets) == nil {
		t.Fatal("selected reader did not produce an outline")
	}
	if selected.fileCalls["retry.go"] != 1 {
		t.Fatalf("selected reader called %d times, want 1", selected.fileCalls["retry.go"])
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
