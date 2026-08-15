package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// copilot.go is the GitHub Copilot CLI hook wire protocol (@github/copilot
// v1.0.80, docs.github.com/en/copilot/reference/hooks-reference). Copilot is
// close enough to Claude Code to tempt a copy of the Claude handlers and
// different enough that the copy would be silently wrong in three ways:
//
//   - The stdout response shape is per-event and FLAT. There is no
//     hookSpecificOutput envelope: preToolUse answers with
//     {"permissionDecision","permissionDecisionReason","modifiedArgs"},
//     postToolUse with {"modifiedResult","additionalContext"},
//     userPromptSubmitted with {"modifiedPrompt"} — and nothing else.
//     Emitting Claude's envelope parses as an unknown object, so the hook
//     would look healthy and deliver nothing.
//   - Its built-in tool names are lowercase and its own vocabulary
//     (view / bash / powershell / create / rg …), so the shared enrichment
//     leaves — which switch on Claude's Read / Grep / Bash names — see
//     nothing to classify until the names are mapped (copilotToolNames).
//   - Its 14 lifecycle events are camelCase (PascalCase is also accepted
//     since v1.0.6, so both spellings are accepted on input), which the
//     hook-effectiveness allowlist does not know about
//     (copilotTelemetryEvents).
//
// Exit-code contract: exit 0 means "parse stdout"; exit 2 denies with the
// reason taken from stderr, and on preToolUse exit 2 is fail-closed — it
// overrides an "allow" in stdout. Gortex never takes that door: the
// documented stdout deny carries the same refusal plus the graph-tool
// redirect as structured text the model is shown, whereas a stderr reason
// is unstructured and an exit-2 fail-closed path turns any future bug in
// this handler into a blocked agent. Timeouts fail open, which is the same
// posture every other Gortex adapter takes on a malformed payload.
const (
	copilotEventSessionStart = "sessionStart"
	copilotEventUserPrompt   = "userPromptSubmitted"
	copilotEventPreToolUse   = "preToolUse"
	copilotEventPostToolUse  = "postToolUse"
	copilotEventAgentStop    = "agentStop"
)

// copilotToolNames maps Copilot's built-in tool vocabulary onto the Claude
// names the shared enrichment leaves switch on. Every entry is load-bearing,
// but `powershell` is the one whose absence would be invisible: it is the
// shell tool on Windows, so omitting it leaves the entire Windows shell
// surface — every cat / grep / find / sed the policy exists to redirect —
// unenforced on a machine whose hook log looks perfectly healthy.
//
// Names outside this table (ask_user, web_fetch, web_search, update_todo)
// pass through unchanged and fall out of enrich's switch as a no-op.
var copilotToolNames = map[string]string{
	"view":       "Read",
	"grep":       "Grep",
	"rg":         "Grep",
	"glob":       "Glob",
	"bash":       "Bash",
	"powershell": "Bash",
	"create":     "Write",
	"edit":       "Edit",
	"task":       "Task",
}

// copilotTelemetryEvents maps Copilot's event names onto the Claude
// vocabulary before anything is logged. hookEffectivenessEvents is a
// hardcoded allowlist of Claude's PascalCase names and
// logHookEffectivenessReachability drops everything else, so a camelCase
// "preToolUse" would be written nowhere — and `gortex doctor` would report
// "configured but never ran" forever against a Copilot install that is
// working perfectly. Normalising here (rather than widening the allowlist
// with camelCase duplicates) also keeps one event's records in one bucket,
// so Copilot and Claude sessions aggregate instead of splitting.
var copilotTelemetryEvents = map[string]string{
	copilotEventSessionStart: "SessionStart",
	copilotEventUserPrompt:   "UserPromptSubmit",
	copilotEventPreToolUse:   "PreToolUse",
	copilotEventPostToolUse:  "PostToolUse",
}

// copilotInput decodes the Copilot hook payload. Copilot's documented keys
// are camelCase (sessionId / toolName / toolArgs); the snake_case twins are
// accepted alongside them because a Copilot hook is routinely configured by
// copying a Claude Code hook entry, and a key mismatch would degrade the
// whole integration to a silent no-op rather than to an error anyone sees.
type copilotInput struct {
	HookEventName      string `json:"hookEventName"`
	HookEventNameSnake string `json:"hook_event_name"`
	EventName          string `json:"event"`

	SessionID      string `json:"sessionId"`
	SessionIDSnake string `json:"session_id"`
	CWD            string `json:"cwd"`

	ToolName      string         `json:"toolName"`
	ToolNameSnake string         `json:"tool_name"`
	ToolArgs      map[string]any `json:"toolArgs"`
	ToolInput     map[string]any `json:"tool_input"`

	ToolResult   any `json:"toolResult"`
	ToolResponse any `json:"tool_response"`

	Prompt string `json:"prompt"`
}

func (in copilotInput) eventName() string {
	return firstNonEmpty(in.HookEventName, in.HookEventNameSnake, in.EventName)
}

func (in copilotInput) sessionID() string {
	return firstNonEmpty(in.SessionID, in.SessionIDSnake)
}

func (in copilotInput) toolName() string {
	return firstNonEmpty(in.ToolName, in.ToolNameSnake)
}

func (in copilotInput) toolArgs() map[string]any {
	if len(in.ToolArgs) > 0 {
		return in.ToolArgs
	}
	return in.ToolInput
}

func (in copilotInput) toolResult() any {
	if in.ToolResult != nil {
		return in.ToolResult
	}
	return in.ToolResponse
}

// copilotPreToolUseResponse is Copilot's flat preToolUse answer. modifiedArgs
// is declared for shape completeness; Gortex does not rewrite Copilot tool
// arguments (the Codex rewrite posture is opt-in and Codex-specific).
type copilotPreToolUseResponse struct {
	PermissionDecision       string         `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	ModifiedArgs             map[string]any `json:"modifiedArgs,omitempty"`
}

// copilotPostToolUseResponse is Copilot's flat postToolUse answer. Only
// additionalContext is used — replacing a tool's result would change what the
// agent believes it ran.
type copilotPostToolUseResponse struct {
	ModifiedResult    any    `json:"modifiedResult,omitempty"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// copilotPromptResponse is the whole userPromptSubmitted channel. Copilot has
// no additionalContext on this event, so graph context has to be carried
// inside the prompt itself.
type copilotPromptResponse struct {
	ModifiedPrompt string `json:"modifiedPrompt"`
}

// copilotSessionStartResponse is the sessionStart channel.
type copilotSessionStartResponse struct {
	AdditionalContext string `json:"additionalContext"`
}

// RunCopilot reads one Copilot hook payload from stdin, answers on stdout,
// and exits 0. Any read failure is a silent no-op — a hook must never break
// the host agent's flow.
func RunCopilot(port int, mode Mode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	out, code := handleCopilot(data, port, mode)
	if out != "" {
		fmt.Print(out)
	}
	if code != 0 {
		os.Exit(code)
	}
}

// handleCopilot is the testable core: parse the payload, route the event,
// return the stdout body and the process exit code.
//
// Only the five events Gortex actually produces a payload for are
// implemented — session-open, per-turn prompt, pre-tool decision, post-tool
// enrichment, and turn-end. The other nine (userPromptTransformed,
// postToolUseFailure, preCompact, subagentStart / subagentStop,
// errorOccurred, permissionRequest, notification, sessionEnd) map to nothing
// Gortex emits, so handling them would buy no capability and cost one
// subprocess spawn per event; they fall through to empty stdout at exit 0.
func handleCopilot(data []byte, port int, mode Mode) (string, int) {
	var in copilotInput
	if err := json.Unmarshal(data, &in); err != nil {
		// Fail open: a payload we cannot parse is not a reason to stop a turn.
		return "", 0
	}

	event := normalizeCopilotEvent(in.eventName())
	if event == "" {
		return "", 0
	}

	// Record the workspace for callServerTool's daemon-socket fallback, the
	// same way the Codex and Kimi dispatchers do. Cleared on return so it
	// never leaks across the sequential tests that share one process.
	setHookCWD(in.CWD)
	defer setHookCWD("")

	switch event {
	case copilotEventSessionStart:
		return copilotSessionStart(in)
	case copilotEventUserPrompt:
		return copilotUserPrompt(in)
	case copilotEventPreToolUse:
		return copilotPreToolUse(in, port, mode)
	case copilotEventPostToolUse:
		return copilotPostToolUse(in)
	case copilotEventAgentStop:
		// Turn-end stop line. Copilot's agentStop answers only
		// {"decision","reason"} — there is no context channel on it at all,
		// so the post-task briefing (test targets, guards, dead code,
		// coverage, contracts) has nowhere to land. Building it anyway would
		// spend a fan-out of daemon round-trips per turn and then discard
		// every byte, and "decision" is a stop-blocker, not a delivery
		// vehicle: using it would turn a diagnostics briefing into a refusal
		// to end the turn. So Copilot deliberately has no turn-end briefing.
		return "", 0
	default:
		return "", 0
	}
}

// normalizeCopilotEvent folds a Copilot event name to its canonical camelCase
// spelling, or "" when the event is one of the nine Gortex does not answer.
// The fold is case-insensitive because Copilot has accepted PascalCase event
// names alongside camelCase since v1.0.6, so a real config carries either.
func normalizeCopilotEvent(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sessionstart":
		return copilotEventSessionStart
	case "userpromptsubmitted":
		return copilotEventUserPrompt
	case "pretooluse":
		return copilotEventPreToolUse
	case "posttooluse":
		return copilotEventPostToolUse
	case "agentstop":
		return copilotEventAgentStop
	default:
		return ""
	}
}

// copilotToolName maps one Copilot tool name into the Claude vocabulary,
// leaving anything unrecognised untouched.
func copilotToolName(name string) string {
	if mapped, ok := copilotToolNames[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mapped
	}
	return strings.TrimSpace(name)
}

// copilotSessionStart injects the session orientation. It deliberately runs
// even while the daemon is unreachable: buildSessionStartBriefing answers an
// outage with the degraded "transport unreachable, rule-only enforcement"
// block, which is the one message a session actually needs at that moment —
// the same posture Claude's runSessionStart and the Pi bridge take.
func copilotSessionStart(in copilotInput) (string, int) {
	started := time.Now()
	ctx := buildSessionStartBriefing(in.CWD)
	logHookEffectiveness(copilotTelemetryEvents[copilotEventSessionStart],
		ctx != "", daemonReachableFn(), 0, time.Since(started))
	if ctx == "" {
		return "", 0
	}
	return marshalCopilot(copilotSessionStartResponse{AdditionalContext: ctx}), 0
}

// copilotUserPrompt lowers the per-turn graph context into modifiedPrompt.
//
// This is the single most bug-prone lowering in the file: Copilot's
// userPromptSubmitted answer has NO additionalContext field, so reusing
// Claude's lowering compiles, emits well-formed JSON, passes a naive shape
// assertion — and discards every byte of prompt-time context. The only
// channel is the prompt itself, so the injected block is appended to the
// user's own text and the whole thing is handed back.
func copilotUserPrompt(in copilotInput) (string, int) {
	started := time.Now()
	daemonUp := daemonReachableFn()
	out := ""
	if daemonUp {
		if ctx := buildUserPromptSubmitContext("UserPromptSubmit", in.Prompt); ctx != "" {
			out = marshalCopilot(copilotPromptResponse{ModifiedPrompt: in.Prompt + "\n\n" + ctx})
		}
	}
	logHookEffectiveness(copilotTelemetryEvents[copilotEventUserPrompt],
		out != "", daemonUp, 0, time.Since(started))
	return out, 0
}

// copilotPreToolUse runs the shared enrich + applyMode pipeline and lowers a
// deny into Copilot's flat permissionDecision shape.
//
// Soft guidance has no channel on this event: preToolUse answers only with a
// permission decision, its reason, and modifiedArgs. Riding advisory text on
// permissionDecision "allow" would auto-approve a call the user's own
// permission settings had not, and "ask" would put a confirmation prompt in
// front of every advisory — so an advisory result is dropped here and the
// equivalent guidance lands on postToolUse instead, where Copilot does have
// an additionalContext channel.
func copilotPreToolUse(in copilotInput, port int, mode Mode) (string, int) {
	started := time.Now()
	daemonUp := daemonReachableFn()

	rawTool := in.toolName()
	mappedTool := copilotToolName(rawTool)
	input := HookInput{
		HookEventName: "PreToolUse",
		ToolName:      mappedTool,
		ToolInput:     copilotToolInput(mappedTool, in.toolArgs()),
		CWD:           in.CWD,
		SessionID:     in.sessionID(),
	}

	out := ""
	isGortexTool := isGortexMCPToolName(rawTool)
	if isGortexTool && mode == ModeConsultUnlock {
		// The consult-unlock marker is local state, so it is recorded even
		// during an outage — exactly as the Pi bridge does.
		markGraphConsulted(input.SessionID)
	}
	// Daemon outage: stand down (#486). Every deny this pipeline can produce
	// mandates Gortex MCP tools; while the daemon cannot serve them, blocking
	// a call and pointing at them deadlocks the agent.
	if daemonUp {
		if result := applyMode(input, isGortexTool, mode, enrich(input, port)); result.deny {
			out = marshalCopilot(copilotPreToolUseResponse{
				PermissionDecision:       "deny",
				PermissionDecisionReason: result.reason,
			})
		}
	}

	logHookEffectiveness(copilotTelemetryEvents[copilotEventPreToolUse],
		out != "", daemonUp, hookAlternationSegmentCount(input), time.Since(started))
	return out, 0
}

// copilotPostToolUse appends graph context to a completed search / read, on
// the one event where Copilot does carry an additionalContext channel.
func copilotPostToolUse(in copilotInput) (string, int) {
	started := time.Now()
	daemonUp := daemonReachableFn()

	out := ""
	// Same stand-down as runPostToolUse's own gate (#486): the follow-ups
	// need daemon answers and instruct "do not re-Read / re-Grep", which must
	// not land while the daemon cannot serve the tools they point at.
	if daemonUp {
		mappedTool := copilotToolName(in.toolName())
		ctx := postToolContext(postHookInput{
			HookEventName: "PostToolUse",
			ToolName:      mappedTool,
			ToolInput:     copilotToolInput(mappedTool, in.toolArgs()),
			ToolResponse:  in.toolResult(),
			CWD:           in.CWD,
			SessionID:     in.sessionID(),
		})
		if ctx != "" {
			out = marshalCopilot(copilotPostToolUseResponse{AdditionalContext: ctx})
		}
	}

	logHookEffectiveness(copilotTelemetryEvents[copilotEventPostToolUse],
		out != "", daemonUp, 0, time.Since(started))
	return out, 0
}

// copilotToolInput fills in the argument keys the shared enrichment leaves
// switch on (file_path / pattern / command) when Copilot's own tool spelled
// them differently. It is additive alias tolerance, not a rewrite: an
// existing canonical key always wins, nothing is removed, and an argument
// shape none of the aliases match simply yields no enrichment (fail open).
func copilotToolInput(mappedTool string, args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	out := cloneStringAnyMap(args)
	switch mappedTool {
	case "Read", "Write", "Edit":
		copilotAliasKey(out, "file_path", "path", "filePath", "file")
	case "Grep":
		copilotAliasKey(out, "pattern", "query", "regex", "search")
	case "Glob":
		copilotAliasKey(out, "pattern", "glob", "query")
	case "Bash":
		copilotAliasKey(out, "command", "cmd", "script")
	}
	return out
}

func copilotAliasKey(args map[string]any, canonical string, aliases ...string) {
	if value, ok := args[canonical].(string); ok && strings.TrimSpace(value) != "" {
		return
	}
	for _, alias := range aliases {
		if value, ok := args[alias].(string); ok && strings.TrimSpace(value) != "" {
			args[canonical] = value
			return
		}
	}
}

// marshalCopilot renders a per-event response. A marshal failure degrades to
// empty stdout, which Copilot reads as "the hook had nothing to say".
func marshalCopilot(response any) string {
	out, err := json.Marshal(response)
	if err != nil {
		return ""
	}
	return string(out)
}
