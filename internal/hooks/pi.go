package hooks

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// pi.go carries the Gortex-owned *bridge* protocol: a neutral event envelope
// on stdin and a decision on stdout, for a host that has no native hook
// system of its own. Pi (earendil-works/pi) was the first such host and gave
// the protocol its name; OpenCode (opencode.go) is the second and shares
// every line of the core below.
//
// The shape exists because those hosts extend through a JS/TS plugin, not a
// hook config. The plugin is a thin shim that, on each relevant lifecycle
// event, shells `gortex hook --agent=<host>`, writes an envelope to stdin,
// and applies the decision it reads back. Keeping the decision logic here
// rather than re-implementing it in TypeScript is what keeps the deny /
// enrich / consult-unlock / nudge postures and the indexed-source
// classification identical across hosts.
//
// The wire contract is owned end-to-end by Gortex (this file plus the
// embedded extensions), so it is deliberately minimal and already normalized
// into the canonical Claude-Code tool vocabulary: the extension maps its
// host's tool names and input keys onto Claude-Code's tool names and the
// `file_path` / `pattern` / `command` shape that `enrich` switches on, before
// sending, so `enrich` can be reused unchanged.

// BridgeEvent is the normalized envelope a host extension writes to stdin.
type BridgeEvent struct {
	// Event is the lifecycle phase. Each host extension spells its phases
	// its own way; bridgeEventSlot folds every accepted spelling onto the
	// three slots the core answers (tool call, orientation, prompt).
	Event string `json:"event"`

	// ToolName / ToolInput carry a tool_call, already mapped into the
	// canonical Claude-Code vocabulary by the extension.
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`

	// CWD is the host's working directory — drives cwd-coverage messaging
	// and the file-indexed lookup inside enrich.
	CWD string `json:"cwd,omitempty"`

	// SessionID keys the per-session state store (consult-unlock marker,
	// nudge streak). Best-effort: empty is tolerated (those postures then
	// degrade to a fresh zero-value state).
	SessionID string `json:"session_id,omitempty"`

	// Prompt carries the user's turn text on a prompt event, so a bridged
	// host gets the same per-turn "relevant indexed symbols" injection
	// Claude and Codex get on UserPromptSubmit.
	Prompt string `json:"prompt,omitempty"`

	// IsGortexTool is set by the extension when the call targets one of
	// the Gortex graph tools it registered. A bridged host gives those
	// plain names (e.g. "search_symbols"), not the `mcp__gortex__` prefix
	// Claude uses, so the extension flags them explicitly. Lets the
	// consult-unlock handshake and the nudge streak-reset recognise a
	// graph query the same way the in-process Claude hook does.
	IsGortexTool bool `json:"is_gortex_tool,omitempty"`
}

// BridgeDecision is what a bridge run writes to stdout. The extension applies
// it: Block→deny the tool call with Reason; AdditionalContext→inject a soft
// tip; Orientation→inject as a tail user message before the next LLM call.
// All fields are optional; an empty decision means "do nothing" (fail-open).
type BridgeDecision struct {
	Block             bool   `json:"block,omitempty"`
	Reason            string `json:"reason,omitempty"`
	AdditionalContext string `json:"additional_context,omitempty"`
	Orientation       string `json:"orientation,omitempty"`
}

// PiEvent and PiDecision are the names the protocol shipped under before a
// second host adopted it. Kept as aliases so the Pi extension's documented
// wire contract — and every caller written against it — stays valid.
type (
	PiEvent    = BridgeEvent
	PiDecision = BridgeDecision
)

// bridgeSlot is the semantic phase an envelope resolves to. Hosts differ in
// how many lifecycle events they expose and what they call them; the core
// answers exactly three.
type bridgeSlot int

const (
	bridgeSlotNone bridgeSlot = iota
	bridgeSlotToolCall
	bridgeSlotOrientation
	bridgeSlotPrompt
)

// RunPi reads a single Pi event envelope from stdin, dispatches on the event
// phase, and writes a decision to stdout. Any read/parse error is a silent
// no-op (emits an empty decision) — a hook must never break the host agent's
// flow.
func RunPi(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		emitBridgeDecision(BridgeDecision{})
		return
	}
	emitBridgeDecision(handlePi(data, port, mode))
}

// handlePi is the Pi entry point into the shared bridge core. Pi's extension
// sends the envelope verbatim — its tool names are already normalized — so
// there is nothing host-specific left to do here.
func handlePi(data []byte, port int, mode Mode) BridgeDecision {
	return handleBridge(data, port, mode)
}

// handleBridge parses an envelope and routes it. Kept separate from the Run*
// entry points so tests can drive it with bytes and assert on the returned
// struct without touching stdio.
func handleBridge(data []byte, port int, mode Mode) BridgeDecision {
	var ev BridgeEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return BridgeDecision{}
	}
	return handleBridgeEvent(ev, port, mode)
}

// handleBridgeEvent routes an already-decoded envelope, so a host adapter can
// normalize fields (OpenCode's tool names, for one) before the core runs.
func handleBridgeEvent(ev BridgeEvent, port int, mode Mode) BridgeDecision {
	switch bridgeEventSlot(ev.Event) {
	case bridgeSlotToolCall:
		return bridgeToolCall(ev, port, mode)
	case bridgeSlotOrientation:
		return bridgeOrientation(ev)
	case bridgeSlotPrompt:
		return bridgePrompt(ev)
	default:
		return BridgeDecision{}
	}
}

// bridgeEventSlot folds every spelling the bridged hosts send onto one of the
// three slots. Pi names its phases session_start / before_agent_start /
// before_compact / tool_call; OpenCode's plugin hooks are tool.execute.before,
// permission.ask and chat.message, and its bus event carries a session open.
// All three orientation moments answer the same way — inject the briefing as
// a tail user message — so they share a slot.
func bridgeEventSlot(event string) bridgeSlot {
	switch normalizeBridgeEvent(event) {
	case "tool_call", "tool_execute_before", "permission_ask":
		return bridgeSlotToolCall
	case "session", "session_start", "session_open", "before_agent_start", "before_compact":
		return bridgeSlotOrientation
	case "prompt", "user_prompt", "chat_message":
		return bridgeSlotPrompt
	default:
		return bridgeSlotNone
	}
}

// normalizeBridgeEvent lower-cases the phase and folds the separators a
// plugin author is likely to forward verbatim (OpenCode's own hook names are
// dotted), so `tool.execute.before` and `tool_call` do not need two tables.
func normalizeBridgeEvent(event string) string {
	return strings.NewReplacer(".", "_", "-", "_", " ", "_").
		Replace(strings.ToLower(strings.TrimSpace(event)))
}

// piToolCall is the Pi-named entry into the shared tool-call handler. The
// name is pinned by the daemon-outage contract tests, which drive the Pi
// posture through it directly.
func piToolCall(ev BridgeEvent, port int, mode Mode) BridgeDecision {
	return bridgeToolCall(ev, port, mode)
}

// bridgeToolCall maps a normalized tool_call onto the shared enrich +
// applyMode pipeline and lowers the resulting enrichResult into a decision.
func bridgeToolCall(ev BridgeEvent, port int, mode Mode) (decision BridgeDecision) {
	started := time.Now()
	daemonUp := daemonReachableFn()

	input := HookInput{
		HookEventName: "PreToolUse",
		ToolName:      ev.ToolName,
		ToolInput:     ev.ToolInput,
		CWD:           ev.CWD,
		SessionID:     ev.SessionID,
	}

	// Every other adapter logs one effectiveness record per invocation; the
	// bridge used to log none at all, which left `gortex doctor` with no
	// denominator and no way to tell a bridge that never fires from one that
	// fires and stays quiet. Recording it here covers both bridged hosts.
	defer func() {
		logHookEffectiveness("PreToolUse", decision != (BridgeDecision{}), daemonUp,
			hookAlternationSegmentCount(input), time.Since(started))
	}()

	// A Gortex graph tool call is the "symbolic" call the postures key off.
	// Under consult-unlock it flips the per-session marker that unlocks
	// fallback reads; it is always allowed through, so there is nothing else
	// to decide.
	if ev.IsGortexTool {
		if mode == ModeConsultUnlock {
			markGraphConsulted(ev.SessionID)
		}
		// Still let adaptive-nudge reset its streak on a symbolic call —
		// but only while the daemon is up. During an outage every adapter
		// freezes the streak entirely (Claude's and Hermes' gates sit above
		// their streak bookkeeping), so post-outage nudge behavior resumes
		// exactly where it left off; resetting here would make the bridge
		// the one adapter whose streak drifts mid-outage (#486).
		if mode == ModeAdaptiveNudge && daemonUp {
			_ = applyMode(input, true, mode, enrichResult{})
		}
		return BridgeDecision{}
	}

	// Daemon outage: stand down (#486) — a block or advisory here would
	// point at graph tools the daemon cannot serve. The consult-unlock
	// marker above is local state and deliberately stays live.
	if !daemonUp {
		return BridgeDecision{}
	}

	result := applyMode(input, false, mode, enrich(input, port))
	if result.deny {
		return BridgeDecision{Block: true, Reason: result.reason}
	}
	if result.context != "" {
		return BridgeDecision{AdditionalContext: result.context}
	}
	return BridgeDecision{}
}

// bridgeOrientation answers a session/orientation moment with the session
// briefing. It runs during an outage too: buildSessionStartBriefing answers
// an unreachable daemon with the degraded "rule-only enforcement" block,
// which is exactly what a session opening into an outage needs to be told.
func bridgeOrientation(ev BridgeEvent) BridgeDecision {
	started := time.Now()
	ctx := buildSessionStartBriefing(ev.CWD)
	logHookEffectiveness("SessionStart", ctx != "", daemonReachableFn(), 0, time.Since(started))
	if ctx == "" {
		return BridgeDecision{}
	}
	return BridgeDecision{Orientation: ctx}
}

// bridgePrompt answers a per-turn prompt with the graph symbols relevant to
// it, as soft context the extension injects ahead of the model call.
func bridgePrompt(ev BridgeEvent) BridgeDecision {
	started := time.Now()
	daemonUp := daemonReachableFn()
	ctx := ""
	if daemonUp {
		ctx = buildUserPromptSubmitContext("UserPromptSubmit", ev.Prompt)
	}
	logHookEffectiveness("UserPromptSubmit", ctx != "", daemonUp, 0, time.Since(started))
	if ctx == "" {
		return BridgeDecision{}
	}
	return BridgeDecision{AdditionalContext: ctx}
}

// emitBridgeDecision marshals a decision to stdout. A marshal failure is
// swallowed — the extension treats empty/garbled output as a no-op.
func emitBridgeDecision(d BridgeDecision) {
	out, err := json.Marshal(d)
	if err != nil {
		return
	}
	_, _ = os.Stdout.Write(out)
}
