package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// PreCompactInput is the JSON structure Claude Code sends to PreCompact hooks.
// Fields that aren't used here are still included so future logic can branch
// on the trigger ("auto" vs "manual") or respect custom_instructions.
type PreCompactInput struct {
	HookEventName      string `json:"hook_event_name"`
	SessionID          string `json:"session_id"`
	TranscriptPath     string `json:"transcript_path"`
	CWD                string `json:"cwd"`
	Trigger            string `json:"trigger"`
	CustomInstructions string `json:"custom_instructions"`
}

// runPreCompact handles a PreCompact hook invocation given the raw stdin bytes
// already read by the dispatcher. It queries the Gortex bridge at the given
// port for an orientation snapshot and emits additionalContext so the agent
// survives compaction without having to re-explore.
//
// Graceful degradation: if the bridge is unreachable (port wrong, server down),
// the hook returns silently with no output. It never blocks compaction.
func runPreCompact(data []byte, port int) {
	started := time.Now()
	var input PreCompactInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "PreCompact" {
		return
	}
	emitted := false
	defer func() {
		logHookEffectiveness("PreCompact", emitted, daemonReachableFn(), 0, time.Since(started))
	}()

	briefing := buildPreCompactBriefing(port)
	if briefing == "" {
		return
	}

	output := HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "PreCompact",
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

// buildPreCompactBriefing queries the bridge and renders a markdown briefing.
// Returns empty string when the bridge is unreachable or returns no data.
func buildPreCompactBriefing(port int) string {
	stats := callServerTool(port, "graph_stats", nil)
	if stats == "" {
		// No bridge => nothing useful to say; let compaction proceed silently.
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Gortex PreCompact Snapshot\n\n")
	sb.WriteString("Continue with Gortex after compaction: call `explore` for the task, then inspect with `search`, `read`, `relations`, or `trace`; do not re-read indexed files.\n\n")

	if summary := renderStatsSummary(stats); summary != "" {
		sb.WriteString("**Index:** ")
		sb.WriteString(summary)
		sb.WriteString("\n\n")
	}

	if churn := renderSymbolHistory(port); churn != "" {
		// Not session-scoped: get_symbol_history reads the process-global
		// Server.symHistory, which every client of a shared daemon writes to.
		sb.WriteString("### Recently Modified Symbols (Gortex-tracked edits on this daemon, all clients)\n\n")
		sb.WriteString(churn)
		sb.WriteString("\n")
	}

	if hot := renderHotspots(port); hot != "" {
		sb.WriteString("### Top Hotspots (highest fan-in/out — likely load-bearing)\n\n")
		sb.WriteString(hot)
		sb.WriteString("\n")
	}

	if fb := renderFeedback(port); fb != "" {
		sb.WriteString("### Feedback-Ranked Symbols (most useful in past tasks)\n\n")
		sb.WriteString(fb)
		sb.WriteString("\n")
	}

	sb.WriteString("_End of Gortex snapshot._\n")
	return sb.String()
}

// renderStatsSummary produces one-line "nodes=X edges=Y languages=a,b,c".
func renderStatsSummary(raw string) string {
	var stats struct {
		TotalNodes int            `json:"total_nodes"`
		TotalEdges int            `json:"total_edges"`
		ByLanguage map[string]int `json:"by_language"`
	}
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		return ""
	}

	// Pick the 3 most-represented languages.
	type langCount struct {
		name string
		n    int
	}
	var langs []langCount
	for k, v := range stats.ByLanguage {
		langs = append(langs, langCount{k, v})
	}
	sort.Slice(langs, func(i, j int) bool { return langs[i].n > langs[j].n })
	if len(langs) > 3 {
		langs = langs[:3]
	}
	var names []string
	for _, l := range langs {
		names = append(names, l.name)
	}

	return fmt.Sprintf("%d nodes, %d edges, languages: %s",
		stats.TotalNodes, stats.TotalEdges, strings.Join(names, ", "))
}

// renderSymbolHistory lists the top modified symbols (compact text).
func renderSymbolHistory(port int) string {
	raw := callServerTool(port, "get_symbol_history", map[string]any{"compact": true})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 10)
}

// renderHotspots returns the top hotspots from the analyze tool.
func renderHotspots(port int) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "hotspots",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 8)
}

// renderFeedback returns the most-useful symbols from past sessions.
func renderFeedback(port int) string {
	raw := callServerTool(port, "feedback", map[string]any{
		"action":  "query",
		"top_n":   5,
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 8)
}

// cappedLines returns the first `max` lines of s, joined with newlines.
func cappedLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > max {
		lines = lines[:max]
	}
	return strings.Join(lines, "\n") + "\n"
}

// callServerTool resolves a Gortex tool call for a hook handler. Live hook
// invocations carry a CWD and use the daemon's default AF_UNIX socket first;
// the optional HTTP REST surface is a compatibility fallback for older
// integrations and focused tests. Returns "" on any error so callers degrade
// silently.
//
// The socket fallback is gated on a set hookCWD (see setHookCWD): the pure-HTTP
// unit tests never set it, so they keep their existing "no bridge" semantics,
// while a live hook process — which sets hookCWD from the payload — reaches a
// normally-running daemon that only listens on the socket.
func callServerTool(port int, name string, args map[string]any) string {
	cwd := loadHookCWD()
	if cwd != "" {
		if raw := callServerToolDaemonFn(cwd, name, args); raw != "" {
			return raw
		}
	}
	return callServerToolHTTP(port, name, args)
}

// callServerToolHTTP issues a POST /v1/tools/{name} against the server and
// returns the text content from the first response block. Returns empty string
// on any error (server unreachable, non-200, missing content).
func callServerToolHTTP(port int, name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"arguments": args})
	if err != nil {
		return ""
	}

	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://localhost:%d/v1/tools/%s", port, name)
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ""
	}
	if parsed.IsError || len(parsed.Content) == 0 {
		return ""
	}
	return parsed.Content[0].Text
}
