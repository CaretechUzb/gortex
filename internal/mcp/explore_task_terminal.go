package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	exploreTaskOutlineBudgetShare = 4
	exploreTaskOutlineHeading     = "## File outlines"
)

// exploreTaskPageOutline returns the same bounded declaration indexes used by
// localize pages. Task-mode completion is visible guidance only: building this
// page never arms the session-local localization gate.
func (s *Server) exploreTaskPageOutline(task string, targets []exploreTarget) *localizationPageOutline {
	if s == nil || s.engine == nil {
		return nil
	}
	declarations := newLocalizationFileDeclarationCache(s.engine.Reader())
	provider := localizationPageOutlineProvider(
		nil, targets, exploreTerminalTerms(task), declarations.definitions,
	)
	if provider == nil {
		return nil
	}
	return provider()
}

// renderExploreTask appends task-mode terminal guidance and a bounded file
// index without changing renderExplore's established output contract. The
// caller remains free to diagnose or edit from the evidence: unlike localize,
// ordinary task mode deliberately retains no authorization state.
func (s *Server) renderExploreTask(task string, targets []exploreTarget, budget int, outline *localizationPageOutline) string {
	base := s.renderExplore(task, targets, budget)
	completion := renderExploreTaskCompletion()
	outlineBudget := max(budget/exploreTaskOutlineBudgetShare, exploreMinBudgetTokens/exploreTaskOutlineBudgetShare)
	outlines := renderExploreTaskOutlines(outline, outlineBudget)

	var b strings.Builder
	b.Grow(len(base) + len(outlines) + len(completion) + 2)
	b.WriteString(base)
	if !strings.HasSuffix(base, "\n") {
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
	completion := newLocalizationCompletion(true, "")
	completion.FinalResponse = "Answer from the ranked evidence and file outlines above."
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
		fmt.Fprintf(&b, "\n### %s — %d declaration(s)", outline.File, outline.Declared)
		if outline.Elided > 0 {
			fmt.Fprintf(&b, ", %d elided", outline.Elided)
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
