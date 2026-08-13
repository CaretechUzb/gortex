package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	exploreTaskOutlineBudgetShare        = 4
	exploreTaskOutlineHeading            = "## File outlines"
	exploreTaskMinimumOutlineTokens      = 64
	exploreTaskDeclarationRetentionLimit = 128
	exploreTaskSectionSeparatorTokens    = 2
)

type exploreTaskOutlineProvider func([]exploreTarget) *localizationPageOutline

// newExploreTaskPageOutlineProvider returns the same bounded declaration index
// used by localize pages. It captures the reader selected for this request, so
// session overlays cannot fall back to the server's base engine. Enumeration is
// deferred until the renderer proves a useful outline can fit.
func newExploreTaskPageOutlineProvider(reader graph.Reader, task string) exploreTaskOutlineProvider {
	if reader == nil {
		return nil
	}
	terms := exploreTerminalTerms(task)
	return func(targets []exploreTarget) *localizationPageOutline {
		declarations := newBoundedLocalizationFileDeclarationCache(reader, exploreTaskDeclarationRetentionLimit)
		provider := localizationPageOutlineProvider(nil, targets, terms, declarations.definitions)
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
	outlineReserve := 0
	if outlineProvider != nil && budget > completionTokens+exploreTaskMinimumOutlineTokens {
		outlineReserve = min(
			budget/exploreTaskOutlineBudgetShare,
			budget-completionTokens-exploreTaskSectionSeparatorTokens,
		)
	}
	baseBudget := max(budget-completionTokens-outlineReserve-exploreTaskSectionSeparatorTokens, 0)
	renderedTargets := targets
	base := s.renderExplore(task, renderedTargets, baseBudget)
	for len(renderedTargets) > 1 && estimateTokens(base) > baseBudget {
		renderedTargets = renderedTargets[:len(renderedTargets)-1]
		base = s.renderExplore(task, renderedTargets, baseBudget)
	}
	if estimateTokens(joinExploreTaskSections(base, "", completion)) > budget {
		base = renderExploreTaskMinimalBase(task, renderedTargets)
	}

	withoutOutline := joinExploreTaskSections(base, "", completion)
	remaining := budget - estimateTokens(withoutOutline)
	if outlineProvider == nil || remaining < exploreTaskMinimumOutlineTokens || outlineReserve < exploreTaskMinimumOutlineTokens {
		return withoutOutline
	}
	outlineBudget := min(remaining, outlineReserve)
	outlines := renderExploreTaskOutlines(outlineProvider(renderedTargets), outlineBudget)
	withOutline := joinExploreTaskSections(base, outlines, completion)
	if outlines == "" || estimateTokens(withOutline) > budget {
		return withoutOutline
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

// renderExploreTaskMinimalBase is the final budget relief after every trailing
// candidate has been shed. API requests are clamped to exploreMinBudgetTokens,
// so these bounded fields leave ample room for the mandatory completion JSON.
func renderExploreTaskMinimalBase(task string, targets []exploreTarget) string {
	var b strings.Builder
	fmt.Fprintf(&b, "EXPLORE — %s\n\n## Likely target\n", truncateOneLine(task, 120))
	if len(targets) == 0 || targets[0].node == nil {
		b.WriteString("No ranked target was available.\n")
	} else {
		node := targets[0].node
		fmt.Fprintf(
			&b,
			"- FILE: %s · SYMBOL: %s · ID: %s\n",
			compactLocalizationField(nodeDisplayPath(node), 180),
			compactLocalizationField(exploreDraftSymbol(node), 120),
			compactLocalizationField(node.ID, 180),
		)
	}
	b.WriteString("Task evidence was reduced to preserve the requested response budget.\n")
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
