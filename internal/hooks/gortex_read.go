package hooks

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/toolref"
)

// The Gortex MCP read tools that can return whole-file source. The compact
// public read dispatcher is the normal path; the two legacy names remain for
// explicitly selected compatibility surfaces.
const (
	gortexReadFileTool       = gortexMCPToolPrefix + "read_file"
	gortexEditingContextTool = gortexMCPToolPrefix + "get_editing_context"
	gortexCompactReadTool    = gortexMCPToolPrefix + "read"
)

// gortexForceCompressEnvVar upgrades the compress-bodies advisory from a
// soft nudge to a hard deny. It mirrors GORTEX_HOOK_BLOCK_EDIT: the
// default posture is a non-blocking reminder — a full-body read is
// sometimes genuinely needed — and a team can flip this on to enforce
// the rule once they trust it.
const gortexForceCompressEnvVar = "GORTEX_HOOK_FORCE_COMPRESS"

// enrichGortexRead nudges (or, when GORTEX_HOOK_FORCE_COMPRESS is set,
// denies) a whole-file read that omits compression on a source file. It
// understands both compact and explicitly selected legacy request shapes.
func enrichGortexRead(toolName string, toolInput map[string]any) enrichResult {
	msg := gortexReadNudge(toolName, toolInput)
	if msg == "" {
		return enrichResult{}
	}
	if gortexForceCompressEnabled() {
		return enrichResult{deny: true, reason: msg}
	}
	return enrichResult{context: msg}
}

// gortexReadNudge returns the advisory message for a Gortex read-tool
// call that should be nudged, or "" when the call needs no nudge. It is
// pure (no env gate, no daemon round-trip — the decision is made
// entirely from the tool input, so the hook stays sub-millisecond) so
// callers can surface the message either as soft context or as a deny.
func gortexReadNudge(toolName string, toolInput map[string]any) string {
	path, options, output, wholeFile := normalizeGortexReadInput(toolName, toolInput)
	if !wholeFile {
		return ""
	}
	// Already compressing — nothing to suggest.
	if asBool(toolInput["compress_bodies"]) || asBool(options["compress_bodies"]) {
		return ""
	}
	if path == "" {
		return ""
	}
	// compress_bodies only elides code bodies; on prose / config it is a
	// no-op, so don't nag on non-source reads.
	if !looksLikeSourceFile(path) {
		return ""
	}
	// The agent already bounded the read (a slice or a token / byte cap)
	// — it knows what it wants; don't second-guess a constrained call.
	if hasReadSizeCap(toolInput) || hasReadSizeCap(options) || hasReadSizeCap(output) {
		return ""
	}
	return gortexReadAdvisory(toolName, path)
}

// normalizeGortexReadInput returns the file and nested controls for a read
// operation that can pull whole-file bodies. Compact source/symbol/summary
// operations are already narrow and therefore intentionally return false.
func normalizeGortexReadInput(toolName string, input map[string]any) (path string, options, output map[string]any, wholeFile bool) {
	options = map[string]any{}
	output = map[string]any{}
	switch shortGortexToolName(strings.TrimSpace(toolName)) {
	case "read_file", "get_editing_context":
		path, _ = input["path"].(string)
		return path, input, output, true
	case "read":
		operation, _ := input["operation"].(string)
		operation = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(operation)), "-", "_")
		target, _ := input["target"].(map[string]any)
		if operation == "" {
			if file, _ := target["file"].(string); strings.TrimSpace(file) != "" {
				operation = "file"
			}
		}
		switch operation {
		case "file", "editing_context":
		default:
			return "", options, output, false
		}
		path, _ = target["file"].(string)
		options = mergeReadControls(input["context"], input["options"])
		if shaped, ok := input["output"].(map[string]any); ok {
			output = shaped
		}
		return path, options, output, true
	default:
		return "", options, output, false
	}
}

func mergeReadControls(values ...any) map[string]any {
	out := make(map[string]any)
	for _, value := range values {
		if fields, ok := value.(map[string]any); ok {
			for key, field := range fields {
				out[key] = field
			}
		}
	}
	return out
}

// gortexReadAdvisory builds the reminder shown when a Gortex read tool is
// about to pull full bodies. It names only compact public operations and gives
// the mandatory native-MCP transport rule for this configured profile.
func gortexReadAdvisory(toolName, path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] %s on %s without compress_bodies — a full-body read can dominate context.\n",
		shortGortexToolName(toolName), path)
	b.WriteString("  - Locate sites with `search(operation:\"text\", query:\"<literal>\")`; it reads no bodies.\n")
	b.WriteString("  - Read with `read(target:{file:\"<path>\"}, options:{compress_bodies:true})` when full bodies are unnecessary.\n")
	b.WriteString("  - Add `options:{keep:\"Name1,Name2\"}` when selected bodies must stay complete.\n")
	b.WriteString(toolref.MCPRequiredLine())
	return b.String()
}

// shortGortexToolName strips either Claude Code namespace: direct MCP config
// uses mcp__gortex__, while marketplace plugins use
// mcp__plugin_gortex_gortex__. The advisory should display only the operation.
func shortGortexToolName(toolName string) string {
	toolName = strings.TrimPrefix(toolName, gortexMCPToolPrefix)
	toolName = strings.TrimPrefix(toolName, gortexPluginMCPToolPrefix)
	return strings.TrimPrefix(toolName, gortexCodexMCPToolPrefix)
}

// hasReadSizeCap reports whether the read already bounds its output via a
// line / byte / token cap. read_file uses max_lines / max_bytes;
// get_editing_context uses max_bytes / max_tokens. A zero or non-numeric
// value is treated as "no cap" so an explicit `max_tokens: 0` opt-out
// still draws the nudge.
func hasReadSizeCap(toolInput map[string]any) bool {
	for _, k := range []string{"max_lines", "max_bytes", "max_tokens", "limit"} {
		if n, ok := toFloat64(toolInput[k]); ok && n > 0 {
			return true
		}
	}
	return false
}

// asBool coerces a JSON-decoded tool-input value to bool. Claude Code
// sends booleans as JSON true/false (decoded to Go bool); the string
// fallback covers hosts that stringify tool inputs.
func asBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes", "on":
			return true
		}
	}
	return false
}

// gortexForceCompressEnabled reports whether the hard-deny gate is on.
// Same truthiness rules as editBlockingEnabled (see envGateEnabled).
func gortexForceCompressEnabled() bool {
	return envGateEnabled(gortexForceCompressEnvVar)
}
