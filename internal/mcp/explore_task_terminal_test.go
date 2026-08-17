package mcp

import (
	"context"
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
		`"state": "localized"`,
		`"scope": "task"`,
		`"required_action": "continue_task"`,
		`"allowed_tool_calls": 0`,
		`"terminal": false`,
		"Editing, navigation, building, and testing remain available.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("task completion missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, localizationAnswerReadyInstruction) || strings.Contains(got, `"state": "answer_ready"`) {
		t.Fatalf("ordinary task mode incorrectly terminalized navigation:\n%s", got)
	}
	if used := estimateTokens(got); used > 1600 {
		t.Fatalf("task response used %d tokens, budget 1600", used)
	}
}

func TestRenderExploreTaskCompletionLeavesLocalizeTerminalContractUnchanged(t *testing.T) {
	contract := localizationContractFor(newLocalizationCompletion(true, ""))
	if !contract.Terminal || contract.Completion.State != localizationStateAnswerReady ||
		contract.Completion.RequiredAction != "respond" || contract.Completion.Instruction != localizationAnswerReadyInstruction {
		t.Fatalf("localize terminal contract changed: %#v", contract)
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
	provider := newExploreTaskPageOutlineProvider(
		context.Background(), selected, "retry policy",
		graph.LocalizationNodeScope{RepoAllow: map[string]bool{"repo": true}},
	)
	if provider == nil || provider(targets) == nil {
		t.Fatal("selected reader did not produce an outline")
	}
	if selected.fileCalls["retry.go"] != 1 {
		t.Fatalf("selected reader called %d times, want 1", selected.fileCalls["retry.go"])
	}
	if len(selected.calls) != 1 || selected.calls[0].limit != localizationFileNodeLimit ||
		!selected.calls[0].scope.RepoAllow["repo"] || !selected.calls[0].scope.ExcludeKinds[graph.KindParam] {
		t.Fatalf("selected reader call = %#v, want 1024-node request scope plus declaration exclusions", selected.calls)
	}
}

func TestExploreTaskOutlineProviderPreservesBoundedDeclarationCounts(t *testing.T) {
	const file = "generated.go"
	for _, test := range []struct {
		name          string
		declared      int
		wantRows      int
		wantDeclared  int
		wantTruncated bool
		wantRendered  string
	}{
		{
			name: "complete retained top file", declared: 140, wantRows: 140,
			wantDeclared: 140, wantRendered: "140 declaration(s)",
		},
		{
			name: "saturated top file lower bound", declared: localizationFileNodeLimit + 1,
			wantRows: localizationFileNodeLimit, wantDeclared: localizationFileNodeLimit + 1,
			wantTruncated: true, wantRendered: "at least 1025 declaration(s), at least 1 elided",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodes := make([]*graph.Node, 0, test.declared)
			for index := 0; index < test.declared; index++ {
				name := fmt.Sprintf("Declaration%04d", index)
				nodes = append(nodes, &graph.Node{
					ID: file + "::" + name, Name: name, Kind: graph.KindFunction,
					FilePath: file, StartLine: index + 1,
				})
			}
			selected := &localizationDeclarationSpyReader{
				files: map[string][]*graph.Node{file: nodes}, fileCalls: make(map[string]int),
			}
			provider := newExploreTaskPageOutlineProvider(
				context.Background(), selected, "generated declarations", graph.LocalizationNodeScope{},
			)
			page := provider([]exploreTarget{{node: nodes[0]}})

			if page == nil || page.Leading == nil {
				t.Fatal("task provider did not produce a leading outline")
			}
			if page.Leading.Declared != test.wantDeclared || page.Leading.Truncated != test.wantTruncated ||
				len(page.Leading.Rows) != test.wantRows {
				t.Fatalf("outline = %#v, want rows=%d declared=%d truncated=%v",
					page.Leading, test.wantRows, test.wantDeclared, test.wantTruncated)
			}
			wantElided := test.wantDeclared - test.wantRows
			if page.Leading.Elided != wantElided {
				t.Fatalf("elided = %d, want %d", page.Leading.Elided, wantElided)
			}
			if selected.fileCalls[file] != 1 || len(selected.calls) != 1 ||
				selected.calls[0].limit != localizationFileNodeLimit {
				t.Fatalf("selected reader calls = %#v, want one 1024-node read", selected.calls)
			}
			rendered := formatExploreTaskOutlines(page)
			if !strings.Contains(rendered, test.wantRendered) {
				t.Fatalf("task outline did not render the truthful count %q:\n%s", test.wantRendered, rendered)
			}
			if !test.wantTruncated && strings.Contains(rendered, "at least") {
				t.Fatalf("complete task outline presented a lower bound:\n%s", rendered)
			}
		})
	}
}

func TestExploreTaskAndStructuredOutlinesShareRankAwareFetchPlan(t *testing.T) {
	files := make(map[string][]*graph.Node, localizationOutlineFileCap)
	targets := make([]exploreTarget, 0, localizationOutlineFileCap)
	for rank := 0; rank < localizationOutlineFileCap; rank++ {
		file := fmt.Sprintf("repo/file-%d.go", rank)
		ranked := &graph.Node{
			ID: file + "::ranked", Name: "ranked", Kind: graph.KindFunction,
			FilePath: file, StartLine: 1,
		}
		files[file] = []*graph.Node{
			ranked,
			{ID: file + "::sibling", Name: "sibling", Kind: graph.KindFunction, FilePath: file, StartLine: 2},
		}
		targets = append(targets, exploreTarget{node: ranked})
	}
	newReader := func() *localizationDeclarationSpyReader {
		return &localizationDeclarationSpyReader{files: files, fileCalls: make(map[string]int)}
	}

	taskReader := newReader()
	taskProvider := newExploreTaskPageOutlineProvider(
		context.Background(), taskReader, "ranked declarations", graph.LocalizationNodeScope{},
	)
	taskPage := taskProvider(targets)

	structuredReader := newReader()
	structuredCache := newLocalizationFileDeclarationCache(
		context.Background(), structuredReader, graph.LocalizationNodeScope{},
	)
	structuredProvider := boundedLocalizationPageOutlineProvider(
		nil, targets, exploreTerminalTerms("ranked declarations"), structuredCache.outlineDefinitions,
	)
	structuredPage := structuredProvider()
	if taskPage == nil || structuredPage == nil {
		t.Fatalf("task/structured pages = %#v / %#v, want both", taskPage, structuredPage)
	}
	if len(taskReader.calls) != localizationOutlineFileCap || len(structuredReader.calls) != localizationOutlineFileCap {
		t.Fatalf("task/structured calls = %d/%d, want %d each",
			len(taskReader.calls), len(structuredReader.calls), localizationOutlineFileCap)
	}
	for rank := 0; rank < localizationOutlineFileCap; rank++ {
		want := localizationOutlineFileFetchLimit(rank)
		taskCall, structuredCall := taskReader.calls[rank], structuredReader.calls[rank]
		if taskCall.file != structuredCall.file || taskCall.limit != want || structuredCall.limit != want {
			t.Fatalf("rank %d task/structured calls = %#v / %#v, want identical limit %d",
				rank, taskCall, structuredCall, want)
		}
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
