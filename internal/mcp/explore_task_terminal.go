package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	exploreTaskOutlineBudgetShare     = 4
	exploreTaskOutlineHeading         = "## File outlines"
	exploreTaskMinimumOutlineTokens   = 64
	exploreTaskSectionSeparatorTokens = 2
)

type exploreTaskOutlineProvider func([]exploreTarget) *localizationPageOutline

// newExploreTaskPageOutlineProvider returns the same bounded declaration index
// used by localize pages. It captures the reader selected for this request, so
// session overlays cannot fall back to the server's base engine. Enumeration is
// deferred until the renderer proves a useful outline can fit.
func newExploreTaskPageOutlineProvider(
	ctx context.Context,
	reader graph.Reader,
	task string,
	scope graph.LocalizationNodeScope,
) exploreTaskOutlineProvider {
	if reader == nil {
		return nil
	}
	terms := exploreTerminalTerms(task)
	return func(targets []exploreTarget) *localizationPageOutline {
		declarations := newLocalizationFileDeclarationCache(ctx, reader, scope)
		provider := localizationPageOutlineProvider(nil, targets, terms, declarations.outlineDefinitions)
		if provider == nil {
			return nil
		}
		return provider()
	}
}

// renderExploreTask appends task-mode terminal guidance and, when the remaining
// budget can carry a useful index, lazily loads bounded file outlines. Ordinary
// task mode deliberately retains no authorization state.
func (s *Server) renderExploreTask(
	task string,
	targets []exploreTarget,
	budget int,
	outlineProvider exploreTaskOutlineProvider,
) string {
	completion := renderExploreTaskCompletion()
	completionTokens := estimateTokens(completion)
	fullBaseBudget := max(budget-completionTokens-exploreTaskSectionSeparatorTokens, 0)
	renderWithoutOutline := func() string {
		base := s.renderExplore(task, targets, fullBaseBudget)
		return joinExploreTaskSections(base, "", completion)
	}
	if outlineProvider == nil || budget <= completionTokens+exploreTaskMinimumOutlineTokens {
		return renderWithoutOutline()
	}

	// Reserve source-packing space for an outline, never candidate space.
	// renderExplore always keeps every ranked location and signature even when
	// those mandatory rows exceed its approximate source budget.
	outlineReserve := min(
		budget/exploreTaskOutlineBudgetShare,
		budget-completionTokens-exploreTaskSectionSeparatorTokens,
	)
	if outlineReserve < exploreTaskMinimumOutlineTokens {
		return renderWithoutOutline()
	}
	baseBudget := max(fullBaseBudget-outlineReserve, 0)
	base := s.renderExplore(task, targets, baseBudget)
	withoutOutline := joinExploreTaskSections(base, "", completion)
	remaining := budget - estimateTokens(withoutOutline)
	if remaining < exploreTaskMinimumOutlineTokens {
		return renderWithoutOutline()
	}

	outlineBudget := min(remaining, outlineReserve)
	outlines := renderExploreTaskOutlines(outlineProvider(targets), outlineBudget)
	withOutline := joinExploreTaskSections(base, outlines, completion)
	if outlines == "" || estimateTokens(withOutline) > budget {
		// The optional index did not use its allowance. Re-render with the full
		// source budget so a nil or over-budget outline cannot make task mode
		// poorer than the same request with outlines disabled.
		return renderWithoutOutline()
	}
	return withOutline
}

func joinExploreTaskSections(base, outlines, completion string) string {
	var b strings.Builder
	b.Grow(len(base) + len(outlines) + len(completion) + 3)
	b.WriteString(base)
	if base != "" && !strings.HasSuffix(base, "\n") {
		b.WriteByte('\n')
	}
	if outlines != "" {
		b.WriteByte('\n')
		b.WriteString(outlines)
	}
	if completion != "" {
		b.WriteByte('\n')
		b.WriteString(completion)
	}
	return b.String()
}

func renderExploreTaskCompletion() string {
	completion := localizationCompletion{
		State:            localizationStateLocalized,
		Scope:            "task",
		RequiredAction:   "continue_task",
		Instruction:      "Continue the requested diagnosis or implementation from the ranked evidence and file outlines above. Editing, navigation, building, and testing remain available.",
		FinalResponse:    "Use the ranked evidence and file outlines above to continue the requested task.",
		AllowedToolCalls: 0,
		ContractVersion:  localizationTerminalContractV2,
	}
	contract := localizationContractFor(completion)
	encoded, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return ""
	}
	return "## Completion\n```json\n" + string(encoded) + "\n```\n"
}

// renderExploreTaskOutlines applies the shared rank-aware relief policy until
// the declaration index fits its fixed allowance. Ranked rows and source bodies
// are never displaced, and the original cached outline is never mutated.
func renderExploreTaskOutlines(page *localizationPageOutline, tokenBudget int) string {
	if page == nil || page.empty() || tokenBudget <= 0 {
		return ""
	}
	page = page.clone()
	for !page.empty() {
		text := formatExploreTaskOutlines(page)
		if estimateTokens(text) <= tokenBudget {
			return text
		}
		before := exploreTaskOutlineWeight(page)
		page.relieve()
		if exploreTaskOutlineWeight(page) >= before {
			return ""
		}
	}
	return ""
}

func formatExploreTaskOutlines(page *localizationPageOutline) string {
	if page == nil || page.empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString(exploreTaskOutlineHeading + "\n")
	appendOutline := func(outline *localizationFileOutline) {
		if outline == nil {
			return
		}
		if outline.Truncated {
			fmt.Fprintf(&b, "\n### %s — at least %d declaration(s)", outline.File, outline.Declared)
		} else {
			fmt.Fprintf(&b, "\n### %s — %d declaration(s)", outline.File, outline.Declared)
		}
		if outline.Elided > 0 {
			if outline.Truncated {
				fmt.Fprintf(&b, ", at least %d elided", outline.Elided)
			} else {
				fmt.Fprintf(&b, ", %d elided", outline.Elided)
			}
		}
		b.WriteByte('\n')
		for _, row := range outline.Rows {
			name := truncateOneLine(row.Name, localizationMaxNameRunes)
			if row.Kind != "" {
				fmt.Fprintf(&b, "- %d: %s [%s]\n", row.Line, name, row.Kind)
			} else {
				fmt.Fprintf(&b, "- %d: %s\n", row.Line, name)
			}
		}
	}
	appendOutline(page.Leading)
	for _, outline := range page.Others {
		appendOutline(outline)
	}
	return b.String()
}

func exploreTaskOutlineWeight(page *localizationPageOutline) int {
	if page == nil {
		return 0
	}
	weight := 0
	if page.Leading != nil {
		weight += 1 + len(page.Leading.Rows)
	}
	for _, outline := range page.Others {
		if outline != nil {
			weight += 1 + len(outline.Rows)
		}
	}
	return weight
}
