package hooks

import (
	"encoding/json"
	"io"
	"os"
)

// pi.go is the Go side of the Pi (earendil-works/pi) integration. Pi has
// no MCP support — the Gortex Pi extension (shipped by the `pi` agent
// adapter) is a thin TypeScript bridge that, on each relevant lifecycle
// event, shells `gortex hook --agent=pi`, writes a normalized event
// envelope to stdin, and applies the decision it reads back from stdout.
//
// Keeping the decision logic here (rather than re-implementing it in
// TypeScript) means the deny / enrich / consult-unlock / nudge postures
// and the indexed-source classification stay in one place.
//
// The wire contract is owned end-to-end by Gortex (this file plus the
// embedded extension/index.ts), so it is deliberately minimal and already
// normalized into the canonical Claude-Code tool vocabulary: the
// extension maps Pi's own tool names and input keys onto Claude-Code's
// tool names and the `file_path` / `pattern` / `command` shape that
// `enrich` switches on, before sending so `enrich` can be reused unchanged.

// PiEvent is the normalized envelope the Pi extension writes to stdin.
type PiEvent struct {
	// Event is the lifecycle phase: "tool_call", "session_start",
	// "before_agent_start", "tool_result", or "before_compact".
	Event string `json:"event"`

	// ToolName / ToolInput carry a tool_call, already mapped into the
	// canonical Claude-Code vocabulary by the extension.
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`

	// CWD is Pi's working directory — drives cwd-coverage messaging and
	// the file-indexed lookup inside enrich.
	CWD string `json:"cwd,omitempty"`

	// SessionID keys the per-session state store (consult-unlock marker,
	// nudge streak). Best-effort: empty is tolerated (those postures then
	// degrade to a fresh zero-value state).
	SessionID string `json:"session_id,omitempty"`

	// IsGortexTool is set by the extension when the call targets one of
	// the Gortex graph tools it registered. Pi gives those plain names
	// (e.g. "search_symbols"), not the `mcp__gortex__` prefix Claude
	// uses, so the extension flags them explicitly. Lets the
	// consult-unlock handshake and the nudge streak-reset recognise a
	// graph query the same way the in-process Claude hook does.
	IsGortexTool bool `json:"is_gortex_tool,omitempty"`
}

// PiDecision is what RunPi writes to stdout. The extension applies it:
// Block→deny the tool call with Reason; AdditionalContext→inject a soft
// tip; Orientation→inject as a tail user message before the next LLM call.
// All fields are optional; an empty decision means "do nothing" (fail-open).
type PiDecision struct {
	Block             bool   `json:"block,omitempty"`
	Reason            string `json:"reason,omitempty"`
	AdditionalContext string `json:"additional_context,omitempty"`
	Orientation       string `json:"orientation,omitempty"`
}

// RunPi reads a single Pi event envelope from stdin, dispatches on the
// event phase, and writes a PiDecision to stdout. Any read/parse error is
// a silent no-op (emits an empty decision) — a hook must never break the
// host agent's flow.
func RunPi(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		emitPiDecision(PiDecision{})
		return
	}
	emitPiDecision(handlePi(data, port, mode))
}

// handlePi is the testable core: parse the envelope, route by phase,
// return the decision. Kept separate from RunPi so tests can drive it
// with bytes and assert on the returned struct without touching stdio.
func handlePi(data []byte, port int, mode Mode) PiDecision {
	var ev PiEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return PiDecision{}
	}

	switch ev.Event {
	case "tool_call":
		return piToolCall(ev, port, mode)
	case "session_start", "before_agent_start", "before_compact":
		// All three are "inject the orientation" moments; the extension
		// injects the briefing as a tail user message.
		ctx := buildSessionStartBriefing(ev.CWD)
		if ctx == "" {
			return PiDecision{}
		}
		return PiDecision{Orientation: ctx}
	default:
		return PiDecision{}
	}
}

// piToolCall maps a normalized Pi tool_call onto the shared enrich +
// applyMode pipeline and lowers the resulting enrichResult into a
// PiDecision.
func piToolCall(ev PiEvent, port int, mode Mode) PiDecision {
	input := HookInput{
		HookEventName: "PreToolUse",
		ToolName:      ev.ToolName,
		ToolInput:     ev.ToolInput,
		CWD:           ev.CWD,
		SessionID:     ev.SessionID,
	}

	// A Gortex graph tool call is the "symbolic" call the postures key
	// off. Under consult-unlock it flips the per-session marker that
	// unlocks fallback reads; it is always allowed through, so there is
	// nothing else to decide.
	isGortexTool := ev.IsGortexTool
	if isGortexTool {
		if mode == ModeConsultUnlock {
			markGraphConsulted(ev.SessionID)
		}
		// Still let adaptive-nudge reset its streak on a symbolic call —
		// but only while the daemon is up. During an outage every adapter
		// freezes the streak entirely (Claude's and Hermes' gates sit above
		// their streak bookkeeping), so post-outage nudge behavior resumes
		// exactly where it left off; resetting here would make Pi the one
		// adapter whose streak drifts mid-outage (#486).
		if mode == ModeAdaptiveNudge && daemonReachableFn() {
			_ = applyMode(input, true, mode, enrichResult{})
		}
		return PiDecision{}
	}

	// Daemon outage: stand down (#486) — a block or advisory here would
	// point at graph tools the daemon cannot serve. The consult-unlock
	// marker above is local state and deliberately stays live.
	if !daemonReachableFn() {
		return PiDecision{}
	}

	result := applyMode(input, isGortexTool, mode, enrich(input, port))
	if result.deny {
		return PiDecision{Block: true, Reason: result.reason}
	}
	if result.context != "" {
		return PiDecision{AdditionalContext: result.context}
	}
	return PiDecision{}
}

// emitPiDecision marshals a PiDecision to stdout. A marshal failure is
// swallowed — the extension treats empty/garbled output as a no-op.
func emitPiDecision(d PiDecision) {
	out, err := json.Marshal(d)
	if err != nil {
		return
	}
	_, _ = os.Stdout.Write(out)
}
