package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PostTaskInput is the JSON structure Claude Code sends to Stop hooks.
// stop_hook_active is true when the hook is already being rerun (another
// Stop hook asked the agent to continue) — we must skip in that case to
// avoid recursion.
type PostTaskInput struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	PromptID       string `json:"prompt_id"`
	AgentID        string `json:"agent_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// runPostTask handles a Stop hook invocation with the raw stdin bytes.
// It runs diagnostics on any changed symbols (dead-code, test targets,
// guards, contract violations) and injects the findings as
// additionalContext so the agent self-corrects before handing off.
//
// Degrades silently when: the bridge is unreachable, there are no
// changes, or stop_hook_active is true.
func runPostTask(data []byte, port int) {
	started := time.Now()
	var input PostTaskInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "Stop" {
		return
	}
	_ = clearLocalizationProblemStatementForTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	emitted := false
	defer func() {
		logHookEffectiveness("Stop", emitted, daemonReachableFn(), 0, time.Since(started))
	}()
	// Prevent recursion — if we're already rerunning a Stop hook, don't fire again.
	if input.StopHookActive {
		return
	}

	briefing := buildPostTaskBriefing(port)
	if briefing == "" {
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "Stop",
			AdditionalContext: briefing,
		},
	}
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	emitted = true
	fmt.Print(string(out))
}

// buildPostTaskBriefing runs diagnostics on the current working tree and
// returns a compact markdown summary. Returns empty string when there's
// nothing to report or the bridge is unreachable.
func buildPostTaskBriefing(port int) string {
	raw := callServerTool(port, "detect_changes", map[string]any{
		"scope": "unstaged",
	})
	if raw == "" {
		return ""
	}

	var changes struct {
		ChangedFiles   []string `json:"changed_files"`
		ChangedSymbols []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"changed_symbols"`
		Risk    string `json:"risk"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &changes); err != nil {
		return ""
	}

	if len(changes.ChangedSymbols) == 0 {
		// No indexed symbols touched — skip silently. The agent doesn't need
		// a post-task briefing when nothing meaningful changed in the graph.
		return ""
	}

	ids := make([]string, len(changes.ChangedSymbols))
	for i, cs := range changes.ChangedSymbols {
		ids[i] = cs.ID
	}
	idsCSV := strings.Join(ids, ",")

	var sb strings.Builder
	sb.WriteString("## Gortex Post-Task Diagnostics\n\n")
	fmt.Fprintf(&sb, "**Changed:** %d symbols across %d files — risk `%s`.\n\n",
		len(changes.ChangedSymbols), len(changes.ChangedFiles), changes.Risk)

	// Test targets — what to run.
	if tests := renderTestTargets(port, idsCSV); tests != "" {
		sb.WriteString("### Tests to Run\n\n")
		sb.WriteString(tests)
		sb.WriteString("\n")
	}

	// Guard rule violations.
	if guards := renderGuardViolations(port, idsCSV); guards != "" {
		sb.WriteString("### Guard Violations\n\n")
		sb.WriteString(guards)
		sb.WriteString("\n")
	}

	// Dead code — specifically whether any of the changed symbols are now orphaned.
	if dead := renderDeadCodeHits(port, ids); dead != "" {
		sb.WriteString("### Potential Dead Code (among changed symbols)\n\n")
		sb.WriteString(dead)
		sb.WriteString("\n")
	}

	// Coverage gaps among changed symbols. Silent when the graph carries
	// no coverage_pct meta (the analyzer returns "no coverage gaps
	// matched" or just an empty body) — we don't want to nag when the
	// repo simply doesn't run `gortex enrich coverage`.
	if gaps := renderCoverageGapsHits(port, ids); gaps != "" {
		sb.WriteString("### Coverage Gaps (among changed symbols)\n\n")
		sb.WriteString(gaps)
		sb.WriteString("\n")
	}

	// Stale-flag regression check. Silent unless one of the changed
	// symbols is itself a flag site or toggles one — pointing the agent
	// at flag rollouts they may have just touched.
	if flags := renderStaleFlagHits(port, ids); flags != "" {
		sb.WriteString("### Stale Flag Sites Touched\n\n")
		sb.WriteString(flags)
		sb.WriteString("\n")
	}

	// Contract mismatches.
	if contracts := renderContractMismatches(port); contracts != "" {
		sb.WriteString("### API Contract Issues\n\n")
		sb.WriteString(contracts)
		sb.WriteString("\n")
	}

	sb.WriteString("_Run the tests above and review any flagged items before handoff._\n")
	return sb.String()
}

// buildMutationBriefing is the focused PostToolUse pipeline for mutations that
// happen outside Gortex, notably Codex apply_patch. It detects affected graph
// symbols first, then asks for tests, guards, and contracts using that observed
// change set. The hook is advisory: an unavailable daemon or an edit touching
// no indexed symbol yields no output and never changes the completed mutation.
func buildMutationBriefing(port int) string {
	raw := callServerTool(port, "detect_changes", map[string]any{"scope": "unstaged"})
	if raw == "" {
		return ""
	}
	var changes struct {
		ChangedFiles   []string `json:"changed_files"`
		ChangedSymbols []struct {
			ID string `json:"id"`
		} `json:"changed_symbols"`
		Risk string `json:"risk"`
	}
	if json.Unmarshal([]byte(raw), &changes) != nil || len(changes.ChangedSymbols) == 0 {
		return ""
	}
	ids := make([]string, 0, len(changes.ChangedSymbols))
	for _, symbol := range changes.ChangedSymbols {
		if strings.TrimSpace(symbol.ID) != "" {
			ids = append(ids, symbol.ID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	idsCSV := strings.Join(ids, ",")

	var out strings.Builder
	out.WriteString("## Gortex mutation follow-up\n\n")
	fmt.Fprintf(&out, "Detected %d affected symbol(s) across %d file(s); risk `%s`.\n\n", len(ids), len(changes.ChangedFiles), changes.Risk)
	out.WriteString("Affected symbols:\n")
	for _, id := range ids {
		fmt.Fprintf(&out, "- `%s`\n", id)
	}
	out.WriteString("\n")
	if tests := renderTestTargets(port, idsCSV); tests != "" {
		out.WriteString("### Tests\n\n")
		out.WriteString(tests)
		out.WriteString("\n")
	}
	if guards := renderGuardViolations(port, idsCSV); guards != "" {
		out.WriteString("### Guards\n\n")
		out.WriteString(guards)
		out.WriteString("\n")
	}
	if contracts := renderContractMismatches(port); contracts != "" {
		out.WriteString("### Contracts\n\n")
		out.WriteString(contracts)
		out.WriteString("\n")
	}
	out.WriteString("Run the selected tests and resolve any guard or contract findings before handoff.\n")
	return out.String()
}

// renderTestTargets asks the bridge for test files that exercise the changed symbols.
func renderTestTargets(port int, idsCSV string) string {
	raw := callServerTool(port, "get_test_targets", map[string]any{
		"ids":     idsCSV,
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "no test targets found" {
		return ""
	}
	return cappedLines(raw, 15)
}

// renderGuardViolations asks the bridge for .gortex.yaml guard rule violations.
func renderGuardViolations(port int, idsCSV string) string {
	raw := callServerTool(port, "check_guards", map[string]any{
		"ids":     idsCSV,
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "no guard rule violations") {
		return ""
	}
	return cappedLines(raw, 10)
}

// renderDeadCodeHits filters analyze:dead_code results to the intersection
// with the currently-changed symbols (i.e. "did this task leave anything
// orphaned"). Emits nothing when the intersection is empty.
func renderDeadCodeHits(port int, ids []string) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "dead_code",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	var hits []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		// Each compact line starts with the symbol descriptor including the
		// file path — we substring-match any of the changed IDs.
		for id := range idSet {
			if strings.Contains(line, id) {
				hits = append(hits, line)
				break
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	return strings.Join(hits, "\n") + "\n"
}

// renderCoverageGapsHits filters analyze:coverage_gaps results to
// the intersection with currently-changed symbols. Silent when no
// changed symbol carries coverage_pct (i.e. coverage hasn't been
// hydrated for this graph) or none of the changed symbols are
// undertested. The agent gets a nudge precisely when a coverage
// gap was just touched — not a noisy package-wide rollup.
func renderCoverageGapsHits(port int, ids []string) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "coverage_gaps",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "no coverage gaps") {
		return ""
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	var hits []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		for id := range idSet {
			if strings.Contains(line, id) {
				hits = append(hits, line)
				break
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	return strings.Join(hits, "\n") + "\n"
}

// renderStaleFlagHits filters analyze:stale_flags results to the
// intersection with currently-changed symbols. Silent unless one
// of the changed symbols is a flag site (its id appears in the
// stale_flags output). Useful for catching a refactor that
// inadvertently touched a flag rollout that's been quiet for ages.
func renderStaleFlagHits(port int, ids []string) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "stale_flags",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "no stale flags") {
		return ""
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	var hits []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		for id := range idSet {
			if strings.Contains(line, id) {
				hits = append(hits, line)
				break
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	return strings.Join(hits, "\n") + "\n"
}

// renderContractMismatches runs the contracts check and returns a short list
// of orphan providers/consumers. Empty when all contracts are matched.
func renderContractMismatches(port int) string {
	raw := callServerTool(port, "contracts", map[string]any{
		"action":  "check",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// The compact summary prefixes "no contract issues" when clean.
	if strings.HasPrefix(strings.ToLower(raw), "no contract") {
		return ""
	}
	return cappedLines(raw, 10)
}
