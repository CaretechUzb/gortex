package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/pathkey"
)

// daemonRoutingProbeTimeout bounds the "does the daemon own this repo?" probe
// that precedes every CLI graph query. Short on purpose: the answer only picks
// an execution backend, and waiting on a busy daemon to learn it is strictly
// worse than getting on with the query.
const daemonRoutingProbeTimeout = 3 * time.Second

// daemonProbeIndeterminate reports whether a control probe failed in a way
// that means "the daemon is busy", as opposed to giving a real answer. Covers
// both ends: the client's own deadline, and the daemon answering that it could
// not finish in time.
func daemonProbeIndeterminate(err error, resp daemon.ControlResponse) bool {
	if errors.Is(err, daemon.ErrDaemonUnresponsive) {
		return true
	}
	return err == nil && !resp.OK && resp.ErrorCode == daemon.ErrTimeout
}

// ErrNoExecutor signals that no warm daemon owns the repo and no explicit
// daemonless path (--oneshot) was selected — the caller decides whether to
// fall back (Stage 1) or refuse (Stage 3).
var ErrNoExecutor = errors.New("no warm daemon and --oneshot not set")

// ErrRepoNotTracked is the typed form of the daemon's repo_not_tracked
// refusal, distinguished so a CLI command can fall back rather than treat
// it as a hard error.
var ErrRepoNotTracked = errors.New("repository not tracked by the daemon")

const (
	cliLegacyToolSurface = "core"
	cliLegacyToolMode    = "defer"
)

// cliExecutor runs a registered MCP tool by name and returns its raw
// result JSON (the same payload the MCP server returns).
type cliExecutor interface {
	CallTool(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error)
	Close() error
}

// daemonExecutor relays a one-shot tools/call over the daemon's AF_UNIX
// ModeMCP channel — the same warm graph the editor proxies hit, no cold
// index. It pins the JSON wire format so per-tool decoding is stable.
type daemonExecutor struct {
	client         *daemon.Client
	nextID         int
	pinJSONDefault bool
}

func (d *daemonExecutor) CallTool(_ context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	d.nextID++
	frame, err := buildToolCallFrameWithDefault(d.nextID, tool, args, d.pinJSONDefault)
	if err != nil {
		return nil, err
	}
	if err := d.client.WriteMCPFrame(frame); err != nil {
		return nil, err
	}
	resp, err := d.client.ReadMCPFrame()
	if err != nil {
		return nil, err
	}
	return extractToolResult(resp)
}

// buildToolCallFrame constructs the JSON-RPC tools/call frame, pinning the
// JSON wire format so the daemon's per-client GCX/TOON auto-selection does
// not defeat the per-tool decode.
func buildToolCallFrame(id int, tool string, args map[string]any) ([]byte, error) {
	return buildToolCallFrameWithDefault(id, tool, args, true)
}

func buildToolCallFrameWithDefault(id int, tool string, args map[string]any, pinJSON bool) ([]byte, error) {
	if args == nil {
		args = map[string]any{}
	}
	// Default to JSON, but honour a caller-provided format (e.g.
	// mermaid / dot for diagram output) so the CLI can request the
	// daemon's other renderers.
	if _, ok := args["format"]; pinJSON && !ok {
		args["format"] = "json"
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
}

func (d *daemonExecutor) Close() error { return d.client.Close() }

// extractToolResult unwraps a JSON-RPC tools/call response: a
// repo_not_tracked error maps to the typed sentinel, any other error is
// surfaced verbatim, and a success returns the tool's JSON payload (the
// text of the first content block).
func extractToolResult(resp []byte) (json.RawMessage, error) {
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				ErrorCode string `json:"error_code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	if rpc.Error != nil {
		if rpc.Error.Data.ErrorCode == "repo_not_tracked" {
			return nil, ErrRepoNotTracked
		}
		return nil, errors.New(rpc.Error.Message)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rpc.Result, &res); err != nil || len(res.Content) == 0 {
		return rpc.Result, nil
	}
	payload := json.RawMessage(res.Content[0].Text)
	if res.IsError {
		return nil, errors.New(res.Content[0].Text)
	}
	return payload, nil
}

// resolveExecutor decides where a CLI graph query runs. This Stage-1 slice
// covers the daemon-first case (a warm daemon that owns the repo) and the
// no-executor case; --oneshot and autostart land with the shared
// constructor and the autostart primitive.
func resolveExecutor(repoPath string) (cliExecutor, error) {
	return resolveExecutorWithToolSurface(repoPath, cliLegacyToolSurface, cliLegacyToolMode)
}

// resolveExecutorWithToolSurface is the daemon-first executor with an
// optional per-connection MCP surface. Ordinary CLI verbs explicitly request
// core/defer so legacy tool names keep their historical semantics regardless
// of daemon/client defaults; compact calls request facade-v1/hide. Neither path
// changes the shared daemon or any other session.
func resolveExecutorWithToolSurface(repoPath, tools, toolsMode string) (cliExecutor, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	if !daemon.IsRunning() {
		return nil, ErrNoExecutor
	}
	if !daemonOwnsRepo(abs) {
		return nil, ErrNoExecutor
	}
	c, err := daemon.Dial(daemon.Handshake{
		Mode:       daemon.ModeMCP,
		ClientName: "cli",
		CWD:        abs,
		Tools:      tools,
		ToolsMode:  toolsMode,
	})
	if err != nil {
		return nil, ErrNoExecutor
	}
	return &daemonExecutor{
		client:         c,
		pinJSONDefault: tools != gortexmcp.FacadeSurfaceVersion,
	}, nil
}

// daemonOwnsRepo reports whether the running daemon tracks a repo that
// covers abs (so a daemon query won't answer empty for an untracked path).
//
// This runs ahead of every CLI graph query, which made it the single point
// where a busy daemon stalled the whole CLI: Status serialises behind the
// controller mutex that track / reload / enrichment hold for minutes, and the
// call had no bound on either end.
//
// It gets a short budget, and when that budget expires it FAILS OPEN — the
// answer is unknown, not "no". Treating an indeterminate probe as "not ours"
// would be a lie with an actively harmful remedy: the caller would be told
// "the daemon does not track <path> — add it with `gortex track <path>`",
// when the daemon does track it and `track` is itself the unbounded operation
// holding the lock. Proceeding to the MCP path costs nothing to be wrong
// about: that path does not need the controller mutex, and a genuinely
// untracked repo comes back as repo_not_tracked, which surfaces the same
// message this probe would have produced.
func daemonOwnsRepo(abs string) bool {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return false
	}
	defer c.Close()
	resp, err := c.ControlWithTimeout(daemon.ControlStatus, nil, daemonRoutingProbeTimeout)
	if daemonProbeIndeterminate(err, resp) {
		fmt.Fprintf(os.Stderr,
			"[gortex] daemon did not answer within %s (a track / reload / enrichment may be holding it) — asking it anyway\n",
			daemonRoutingProbeTimeout)
		return true
	}
	if err != nil || !resp.OK {
		return false
	}
	var st daemon.StatusResponse
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return false
	}
	for _, repo := range st.TrackedRepos {
		if repo.Path == "" {
			continue
		}
		// Forward containment: abs is inside a tracked repo.
		if pathkey.HasPathPrefix(abs, repo.Path) {
			return true
		}
		// Reverse containment: abs is a root ABOVE tracked repos. The
		// MCP dispatcher serves this shape (cmd/gortex/daemon_mcp.go
		// cwdReachable), and the two gates answering differently for
		// the same directory is a difference the user experiences as
		// "the agent can query this folder but the CLI cannot".
		if pathkey.HasPathPrefix(repo.Path, abs) {
			return true
		}
	}
	return false
}
