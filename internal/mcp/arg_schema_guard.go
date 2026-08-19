package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// #597: request decoding reads the keys it knows and unknown keys simply
// vanished — read_file(line_range: …) returned the full file, at maximum
// token cost, for a call that asked for a 40-line window, and the caller
// got no signal to self-correct. The guard makes dispatch honor what the
// published schema says: on a closed schema an unknown key is an immediate
// tool error naming the key and the valid options, BEFORE the handler runs
// — the expensive wrong answer is never produced.

// toolArgGuardEnv dials enforcement for one run: unset (or anything
// unrecognised) rejects — the schema's own contract; "warn" runs the
// handler and appends an _ignored_options rider; "off"/"0"/"false"/"none"
// restores the pre-guard behavior.
const toolArgGuardEnv = "GORTEX_TOOL_ARG_GUARD"

// toolArgGuardKeys extracts a tool's declared top-level option names and
// whether its schema closes itself. Only an explicit
// additionalProperties:false closes a schema — one left open, explicitly
// or by JSON-Schema's permissive default, is honored in that direction
// too: no enforcement.
func toolArgGuardKeys(tool mcp.Tool) (map[string]struct{}, bool) {
	if tool.RawInputSchema != nil {
		var s struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties json.RawMessage            `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.RawInputSchema, &s); err != nil {
			return nil, false
		}
		if strings.TrimSpace(string(s.AdditionalProperties)) != "false" {
			return nil, false
		}
		keys := make(map[string]struct{}, len(s.Properties))
		for k := range s.Properties {
			keys[k] = struct{}{}
		}
		return keys, true
	}
	allowExtra, ok := tool.InputSchema.AdditionalProperties.(bool)
	if !ok || allowExtra {
		return nil, false
	}
	keys := make(map[string]struct{}, len(tool.InputSchema.Properties))
	for k := range tool.InputSchema.Properties {
		keys[k] = struct{}{}
	}
	return keys, true
}

// wrapToolArgGuard enforces a closed schema's key set at dispatch. Open
// schemas pass through untouched.
func wrapToolArgGuard(tool mcp.Tool, handler server.ToolHandlerFunc) server.ToolHandlerFunc {
	allowed, closed := toolArgGuardKeys(tool)
	if !closed {
		return handler
	}
	valid := make([]string, 0, len(allowed))
	for k := range allowed {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	validGloss := "valid options: " + strings.Join(valid, ", ")
	if len(valid) == 0 {
		validGloss = "this tool takes no options"
	}
	name := tool.Name
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mode := strings.ToLower(strings.TrimSpace(os.Getenv(toolArgGuardEnv)))
		switch mode {
		case "off", "0", "false", "none":
			return handler(ctx, req)
		}
		var unknown []string
		for k := range req.GetArguments() {
			if _, ok := allowed[k]; !ok {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) == 0 {
			return handler(ctx, req)
		}
		sort.Strings(unknown)
		if mode == "warn" {
			res, err := handler(ctx, req)
			if err == nil && res != nil {
				res.Content = append(res.Content, mcp.NewTextContent(fmt.Sprintf(
					"_ignored_options: %s — not options of %s; %s",
					strings.Join(unknown, ", "), name, validGloss)))
			}
			return res, err
		}
		return mcp.NewToolResultError(fmt.Sprintf(
			"%s does not accept option(s): %s; %s. The call was not executed — resend it with declared options only.",
			name, strings.Join(unknown, ", "), validGloss)), nil
	}
}
