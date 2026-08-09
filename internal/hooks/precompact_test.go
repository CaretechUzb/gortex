package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestRunPreCompact_RejectsWrongEvent(t *testing.T) {
	// Should be a no-op for PreToolUse payload.
	data := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Read"}`)
	out := captureStdout(t, func() { runPreCompact(data, 0) })
	if out != "" {
		t.Errorf("expected silent no-op, got: %q", out)
	}
}

func TestRunPreCompact_NoBridge(t *testing.T) {
	// Port 1 is guaranteed to be closed. Hook must fail silently.
	data := []byte(`{"hook_event_name":"PreCompact","session_id":"s","trigger":"auto"}`)
	out := captureStdout(t, func() { runPreCompact(data, 1) })
	if out != "" {
		t.Errorf("expected no output when bridge unreachable, got: %q", out)
	}
}

// TestRunPreCompact_EmitsNothingEvenWithBridge is a contract guard, not a
// behavioural test. PreCompact has no context-injection contract in Claude Code:
// additionalContext is honoured only for SessionStart / Setup / SubagentStart /
// the tool events / Stop / SubagentStop, and PreCompact's only documented output
// (`decision: "block"`, exit 2) blocks compaction. Anything this hook prints is
// therefore either discarded or actively harmful, so it must print nothing —
// even when the bridge is up and a briefing could be built.
func TestRunPreCompact_EmitsNothingEvenWithBridge(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats": `{"total_nodes":4500,"total_edges":47000,"by_language":{"go":3000}}`,
		"analyze":     "method Graph.AddNode internal/graph/graph.go fan_in=42 fan_out=3",
	})
	defer srv.Close()

	port := portFromURL(t, srv.URL)
	data := []byte(`{"hook_event_name":"PreCompact","session_id":"s","trigger":"auto"}`)
	out := captureStdout(t, func() { runPreCompact(data, port) })

	if out != "" {
		t.Errorf("PreCompact must emit nothing — it has no additionalContext contract; got:\n%s", out)
	}
}

func TestDispatch_RoutesPreCompactSilently(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats": `{"total_nodes":1,"total_edges":0,"by_language":{"go":1}}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"PreCompact","session_id":"s","trigger":"auto"}`)
	var out string
	withStdin(t, data, func() {
		out = captureStdout(t, func() { Run(port, ModeDeny) })
	})
	if out != "" {
		t.Errorf("PreCompact routing must stay silent, got:\n%s", out)
	}
}

// TestBuildCompactionBriefing_CarriesAdvisoryAndSnapshot covers the block that
// SessionStart(source="compact") injects — the content that used to be emitted,
// uselessly, from PreCompact.
func TestBuildCompactionBriefing_CarriesAdvisoryAndSnapshot(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"graph_stats": `{"total_nodes":4500,"total_edges":47000,"by_language":{"go":3000,"typescript":400,"markdown":500}}`,
		"get_symbol_history": "method Server.handleBatchEdit internal/mcp/tools_enhancements.go:1200 (edits=3, CHURNING)\n" +
			"function renderContextMarkdown internal/mcp/tools_enhancements.go:1790 (edits=1)",
		"analyze": "method Graph.AddNode internal/graph/graph.go fan_in=42 fan_out=3\n" +
			"function New internal/indexer/indexer.go fan_in=31 fan_out=5",
		"feedback": "pkg/server.go::HandleRequest useful=12 not_needed=1",
	})
	defer srv.Close()

	got := buildCompactionBriefing(portFromURL(t, srv.URL))

	for _, want := range []string{
		"Gortex Post-Compaction Snapshot",
		"re-injected as `system-reminder` blocks",
		"Do not re-read indexed files.",
		"4500 nodes, 47000 edges",
		"handleBatchEdit",
		"Graph.AddNode",
		"HandleRequest",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("post-compaction briefing missing %q:\n%s", want, got)
		}
	}
}

// TestBuildCompactionBriefing_AdvisorySurvivesDeadBridge pins the degrade path.
// The re-injection advisory needs no daemon, and it is the half that actually
// stops the agent paying twice for content the harness already pushed in — so a
// dead bridge must cost the graph sections, not the whole block.
func TestBuildCompactionBriefing_AdvisorySurvivesDeadBridge(t *testing.T) {
	got := buildCompactionBriefing(1) // port 1 is guaranteed closed

	if !strings.Contains(got, "Do not re-read indexed files.") {
		t.Errorf("advisory must survive an unreachable bridge, got:\n%s", got)
	}
	if strings.Contains(got, "**Index:**") {
		t.Errorf("graph sections must be absent without a bridge, got:\n%s", got)
	}
}

func TestDispatch_UnknownEventSilent(t *testing.T) {
	data := []byte(`{"hook_event_name":"UserPromptSubmit"}`)
	old := os.Stdin
	defer func() { os.Stdin = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write(data)
		_ = w.Close()
	}()
	os.Stdin = r

	out := captureStdout(t, func() { Run(1, ModeDeny) })
	if out != "" {
		t.Errorf("expected silent no-op for unknown event, got: %q", out)
	}
}

// ---- helpers ----

// newFakeServer returns a test HTTP server that mimics /v1/tools/{name}
// responses. `toolResponses` maps tool name to the raw `text` field of the
// first content block.
func newFakeServer(toolResponses map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/tools/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
		text, ok := toolResponses[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		}
		body, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

// recordingFakeServer is newFakeServer plus a record of the `arguments`
// object each tool call carried, so a test can assert what the hook
// *asked for* and not only what it rendered. Separate from newFakeServer
// because that helper has ~10 call sites across four test files.
type recordingFakeServer struct {
	*httptest.Server
	mu   sync.Mutex
	args map[string][]map[string]any
}

func newRecordingFakeServer(toolResponses map[string]string) *recordingFakeServer {
	rec := &recordingFakeServer{args: make(map[string][]map[string]any)}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/tools/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/tools/")

		var req struct {
			Arguments map[string]any `json:"arguments"`
		}
		if raw, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(raw, &req)
		}
		rec.mu.Lock()
		rec.args[name] = append(rec.args[name], req.Arguments)
		rec.mu.Unlock()

		text, ok := toolResponses[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	return rec
}

// argsFor returns the recorded `arguments` objects for one tool, in call order.
func (r *recordingFakeServer) argsFor(tool string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.args[tool]
}

// callCount reports how many times a tool was invoked.
func (r *recordingFakeServer) callCount(tool string) int {
	return len(r.argsFor(tool))
}

func portFromURL(t *testing.T, u string) int {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse url %q: %v", u, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", u, err)
	}
	return port
}

// captureStdout runs fn with os.Stdout redirected to a pipe, returning what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()
	_ = w.Close()
	os.Stdout = old
	<-done
	return buf.String()
}

// withStdin runs fn with os.Stdin swapped to a pipe fed with data.
func withStdin(t *testing.T, data []byte, fn func()) {
	t.Helper()
	old := os.Stdin
	defer func() { os.Stdin = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write(data)
		_ = w.Close()
	}()
	os.Stdin = r
	fn()
}
