package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/toolref"
)

// RunKimi handles the Kimi Code CLI hook wire shape across its lifecycle
// events. Kimi appends a hook's plain stdout to the model context on exit 0, so
// soft guidance (UserPromptSubmit context, PreToolUse read nudges, the Stop
// diagnostics briefing, and subagent briefings) is emitted as plain text rather
// than Claude-style additionalContext JSON. A hard block uses the one JSON
// shape Kimi documents — hookSpecificOutput.permissionDecision = "deny" — so
// the PreToolUse redirect of indexed whole-file reads actually stops the call.
//
// Every path degrades to a silent no-op when the payload is malformed, the cwd
// is not a Gortex-enabled project, or the daemon is unreachable, so normal Kimi
// flow is never blocked by the integration.
func RunKimi(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runKimi(data, port, mode)
}

func runKimi(data []byte, port int, mode Mode) {
	var peek struct {
		HookEventName string `json:"hook_event_name"`
		CWD           string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}
	switch peek.HookEventName {
	case "UserPromptSubmit", "PreToolUse", "Stop", "SubagentStart":
	default:
		return
	}
	// Only enrich turns taken inside a Gortex-enabled project. The user-level
	// Kimi hook fires machine-wide, so this front gate keeps the graph tools
	// out of unrelated repos before any daemon round-trip.
	if !kimiGortexEnabledProject(peek.CWD) {
		return
	}

	// Record the workspace for callServerTool's daemon-socket fallback (the
	// HTTP surface the hook port names is not served unless the daemon runs
	// with --http-addr). Cleared on return so it never leaks across the
	// sequential tests that share one process.
	setHookCWD(peek.CWD)
	defer setHookCWD("")

	switch peek.HookEventName {
	case "UserPromptSubmit":
		runKimiUserPromptSubmit(data)
	case "PreToolUse":
		runKimiPreToolUseEvent(data, port, mode)
	case "Stop":
		runKimiStop(data, port)
	case "SubagentStart":
		runKimiSubagentStart(data, port)
	}
}

// runKimiUserPromptSubmit injects graph symbols relevant to the user's prompt
// before the model runs, as plain stdout.
func runKimiUserPromptSubmit(data []byte) {
	var input struct {
		HookEventName string `json:"hook_event_name"`
		Prompt        any    `json:"prompt"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	ctx := buildUserPromptSubmitContext(input.HookEventName, kimiPromptText(input.Prompt))
	if ctx == "" {
		return
	}
	fmt.Print(ctx)
}

// runKimiPreToolUseEvent parses the PreToolUse envelope and routes it through
// the shared enrichment machinery, emitting results in Kimi's wire shape.
func runKimiPreToolUseEvent(data []byte, port int, mode Mode) {
	var input struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
		SessionID string         `json:"session_id"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	runKimiPreToolUse(input.ToolName, input.ToolInput, input.SessionID, port, mode)
}

// runKimiPreToolUse applies the graph-aware PreToolUse posture to one tool call.
//
//   - Gortex's compact read tool (plus legacy compatibility names) keeps the
//     historical plain-stdout compress-bodies nudge, never a hard deny.
//   - Native tools (Read / Grep / Glob / Bash) and Task subagent spawns run
//     through the shared enrich + applyMode path: a deny of an indexed
//     whole-file read becomes a Kimi permission-decision block; soft guidance
//     is appended as plain-stdout context so the call still proceeds.
func runKimiPreToolUse(toolName string, toolInput map[string]any, sessionID string, port int, mode Mode) {
	if kimiGortexReadPreToolUseTool(toolName) {
		if ctx := gortexReadNudge(toolName, toolInput); ctx != "" {
			fmt.Print(ctx)
		}
		return
	}

	input := HookInput{
		HookEventName: "PreToolUse",
		ToolName:      toolName,
		ToolInput:     toolInput,
		CWD:           loadHookCWD(),
		SessionID:     sessionID,
	}
	result := applyMode(input, false, mode, enrich(input, port))
	if result.deny {
		emitKimiDeny(result.reason)
		return
	}
	if result.context != "" {
		fmt.Print(result.context)
	}
}

// runKimiStop runs the post-turn diagnostics on the changed working tree and
// appends the findings as plain-stdout context so the agent can self-correct
// before handoff. Reuses the shared briefing builder, so Kimi gets identical
// diagnostics (test targets, guards, dead code, coverage, contracts) to Claude.
// Skips when a Stop hook is already rerunning to avoid recursion.
func runKimiStop(data []byte, port int) {
	var input PostTaskInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.StopHookActive {
		return
	}
	briefing := buildPostTaskBriefing(port, sessionScope{
		SessionID: input.SessionID,
		CWD:       firstNonEmpty(input.CWD, loadHookCWD()),
	})
	if briefing == "" {
		return
	}
	fmt.Print(briefing)
}

// runKimiSubagentStart gives a spawned subagent a task-scoped briefing (relevant
// symbols + the tool-swap table) as plain stdout, so it reaches for the graph
// tools instead of defaulting to raw Read/Grep on indexed source.
func runKimiSubagentStart(data []byte, port int) {
	briefing := buildKimiSubagentBriefing(port, kimiSubagentTask(data))
	if briefing == "" {
		return
	}
	fmt.Print(briefing)
}

// buildKimiSubagentBriefing prefers the full enrichTask briefing (graph stats +
// smart_context over the subagent task + recent churn) when a task can be
// derived and the daemon answers; otherwise it falls back to the static
// tool-swap table, which needs no daemon round-trip, so a subagent is never
// left without the redirect rules.
func buildKimiSubagentBriefing(port int, task string) string {
	if task != "" {
		if r := enrichTask(map[string]any{"description": task}, port); r.context != "" {
			return r.context
		}
	}
	return kimiSubagentFallbackBriefing()
}

// kimiSubagentFallbackBriefing restates the Gortex tool-swap rules inline for a
// subagent when no task text is available (or the bridge is down). Kept short —
// it is pure guidance text, injected on every spawn.
func kimiSubagentFallbackBriefing() string {
	var sb strings.Builder
	sb.WriteString("[Gortex] Subagent briefing — this repo has a Gortex MCP server.\n")
	sb.WriteString("Subagents don't inherit project instructions, so the rules below are restated inline:\n\n")
	sb.WriteString(gortexToolGuidance)
	sb.WriteString(toolref.MCPRequiredLine())
	sb.WriteString("\n_First call: `explore` with the task. Before mutation, call `change(operation:\"impact\")`; inspect with `read`; mutate only with `edit` or `refactor`. After mutation, call `change(operation:\"detect\")`, then use its symbol IDs with `change` operations `tests`, `guards`, and `contract`. Never Read/Grep indexed source._\n")
	return sb.String()
}

// kimiSubagentTask extracts the subagent's task description from a SubagentStart
// payload. Kimi's documented base payload is thin (hook_event_name / session_id
// / cwd) and the subagent-specific fields aren't pinned down, so several likely
// shapes are probed; a miss yields "" and the fallback briefing.
func kimiSubagentTask(data []byte) string {
	var in struct {
		Prompt      any    `json:"prompt"`
		Description string `json:"description"`
		Task        string `json:"task"`
		ToolInput   struct {
			Prompt      any    `json:"prompt"`
			Description string `json:"description"`
		} `json:"tool_input"`
		Subagent struct {
			Prompt      any    `json:"prompt"`
			Description string `json:"description"`
			Task        string `json:"task"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return ""
	}
	for _, s := range []string{
		kimiPromptText(in.Prompt), in.Description, in.Task,
		kimiPromptText(in.ToolInput.Prompt), in.ToolInput.Description,
		kimiPromptText(in.Subagent.Prompt), in.Subagent.Description, in.Subagent.Task,
	} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// emitKimiDeny writes the one JSON response shape Kimi documents for blocking a
// tool call: hookSpecificOutput.permissionDecision = "deny" with a reason Kimi
// feeds back into context so the model can pick the graph-aware alternative.
func emitKimiDeny(reason string) {
	out, err := json.Marshal(HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: reason,
		},
	})
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

func kimiGortexReadPreToolUseTool(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case gortexCompactReadTool, gortexReadFileTool, gortexEditingContextTool, "read", "read_file", "get_editing_context":
		return true
	default:
		return false
	}
}

func kimiPromptText(v any) string {
	switch p := v.(type) {
	case string:
		return p
	case []any:
		var parts []string
		for _, item := range p {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ != "" && typ != "text" {
				continue
			}
			text, _ := m["text"].(string)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func kimiGortexEnabledProject(cwd string) bool {
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return false
	}
	for {
		if kimiProjectMCPHasGortex(abs) || gortexProjectMarker(abs) {
			return true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return false
		}
		abs = parent
	}
}

func kimiProjectMCPHasGortex(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".kimi-code", "mcp.json"))
	if err != nil {
		return false
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = servers["gortex"].(map[string]any)
	return ok
}

func gortexProjectMarker(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".gortex.yaml")); err == nil {
		return true
	}
	info, err := os.Stat(filepath.Join(dir, ".gortex"))
	if err != nil || !info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, ".gortex", ".gitignore"))
	if err != nil {
		return false
	}
	normalized := strings.TrimSpace(string(data))
	return strings.Contains(normalized, "Gortex-managed") || normalized == "*"
}
