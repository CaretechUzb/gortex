package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/testenv"
)

// TestResolveLaunchCWDFallsBackFromHome exercises the gortexhq/gortex#19
// path: Cursor launches the user-level MCP entry with cwd=$HOME. The
// resolver should prefer the editor-provided CURSOR_WORKSPACE env var
// over the ambiguous home cwd.
func TestResolveLaunchCWDFallsBackFromHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no resolvable home directory in test env")
	}

	// Make a real directory we can chdir into and treat as $HOME for
	// the duration of the test, so isAmbiguousLaunchCWD sees the same
	// path os.Getwd() reports.
	fakeHome := t.TempDir()
	project := filepath.Join(t.TempDir(), "myproject")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	testenv.SetHome(t, fakeHome)
	t.Setenv("PWD", "") // clear so we exercise the editor-env path
	t.Setenv("CURSOR_WORKSPACE", project)
	t.Setenv("CLAUDE_CODE_WORKSPACE", "")
	t.Setenv("WINDSURF_WORKSPACE", "")
	t.Setenv("KIRO_WORKSPACE", "")
	t.Setenv("CODEX_WORKSPACE", "")
	t.Setenv("ANTIGRAVITY_WORKSPACE", "")
	t.Setenv("VSCODE_WORKSPACE", "")

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })
	if err := os.Chdir(fakeHome); err != nil {
		t.Fatal(err)
	}

	got, err := resolveLaunchCWD()
	if err != nil {
		t.Fatalf("resolveLaunchCWD: %v", err)
	}
	if got != project {
		t.Errorf("resolveLaunchCWD = %q, want %q (the CURSOR_WORKSPACE env)", got, project)
	}
}

// TestResolveLaunchCWDPrefersGetwdWhenSafe confirms the resolver
// doesn't second-guess a normal cwd. When os.Getwd() returns a
// project directory (not `/`, not $HOME), the env-var fallbacks must
// not override it — otherwise a user with a stale CURSOR_WORKSPACE
// would be pinned to the wrong project.
func TestResolveLaunchCWDPrefersGetwdWhenSafe(t *testing.T) {
	project := t.TempDir()
	stale := t.TempDir()
	t.Setenv("CURSOR_WORKSPACE", stale)

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}

	got, err := resolveLaunchCWD()
	if err != nil {
		t.Fatalf("resolveLaunchCWD: %v", err)
	}
	// On macOS t.TempDir() lives under /private/var/... but Getwd may
	// return /var/... after resolving the /var → /private/var symlink
	// (or vice versa). Compare by resolved path.
	wantResolved, _ := filepath.EvalSymlinks(project)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("resolveLaunchCWD = %q, want %q (real cwd, not stale CURSOR_WORKSPACE)", got, project)
	}
}

// TestResolveLaunchCWDFallsBackToPWDFromRoot covers the Antigravity
// `cwd=/` case the previous code special-cased: when Getwd() is `/`
// and PWD points at a real directory, use PWD.
func TestResolveLaunchCWDFallsBackToPWDFromRoot(t *testing.T) {
	project := t.TempDir()
	t.Setenv("PWD", project)
	t.Setenv("CURSOR_WORKSPACE", "")
	t.Setenv("CLAUDE_CODE_WORKSPACE", "")
	t.Setenv("WINDSURF_WORKSPACE", "")
	t.Setenv("KIRO_WORKSPACE", "")
	t.Setenv("CODEX_WORKSPACE", "")
	t.Setenv("ANTIGRAVITY_WORKSPACE", "")
	t.Setenv("VSCODE_WORKSPACE", "")

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })
	if err := os.Chdir("/"); err != nil {
		t.Fatal(err)
	}

	got, err := resolveLaunchCWD()
	if err != nil {
		t.Fatalf("resolveLaunchCWD: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(project)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("resolveLaunchCWD from / = %q, want %q (the PWD fallback)", got, project)
	}
}

// TestOrphanWatch_FiresOnReparent confirms the watchdog fires once when the
// parent PID changes (the parent process died and we were reparented).
func TestOrphanWatch_FiresOnReparent(t *testing.T) {
	var calls atomic.Int32
	getppid := func() int {
		if calls.Add(1) == 1 {
			return 4242 // original parent observed at arm time
		}
		return 1 // reparented to init after the parent exited
	}
	fired := make(chan struct{}, 1)
	go orphanWatch(context.Background(), time.Millisecond, getppid, func() { fired <- struct{}{} })

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("orphanWatch did not fire after the parent PID changed")
	}
}

// TestOrphanWatch_FiresOnSubreaperReparent confirms the change-detection
// catches reparenting to a subreaper (PID != 1) — the case a bare `== 1`
// check would miss in containers / systemd user sessions.
func TestOrphanWatch_FiresOnSubreaperReparent(t *testing.T) {
	var calls atomic.Int32
	getppid := func() int {
		if calls.Add(1) == 1 {
			return 4242
		}
		return 99 // reparented to a subreaper, not init
	}
	fired := make(chan struct{}, 1)
	go orphanWatch(context.Background(), time.Millisecond, getppid, func() { fired <- struct{}{} })

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("orphanWatch must fire on subreaper reparenting, not just PID 1")
	}
}

// TestOrphanWatch_StableParentNeverFires confirms a live, unchanging parent
// never trips the watchdog.
func TestOrphanWatch_StableParentNeverFires(t *testing.T) {
	getppid := func() int { return 4242 }
	fired := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go orphanWatch(ctx, time.Millisecond, getppid, func() { fired <- struct{}{} })

	time.Sleep(50 * time.Millisecond) // ~50 stable polls
	cancel()
	select {
	case <-fired:
		t.Fatal("orphanWatch fired despite a stable parent PID")
	default:
	}
}

// TestOrphanWatch_Disarms confirms the watchdog never arms when there is no
// meaningful parent to watch (already init / no parent) or the interval is
// non-positive — it returns immediately and never fires.
func TestOrphanWatch_Disarms(t *testing.T) {
	cases := map[string]struct {
		ppid     int
		interval time.Duration
	}{
		"already init":     {1, time.Millisecond},
		"no parent":        {0, time.Millisecond},
		"non-pos interval": {4242, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fired := make(chan struct{}, 1)
			done := make(chan struct{})
			go func() {
				orphanWatch(context.Background(), tc.interval,
					func() int { return tc.ppid },
					func() { fired <- struct{}{} })
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("orphanWatch should return immediately when disarmed")
			}
			select {
			case <-fired:
				t.Fatal("orphanWatch must not fire when disarmed")
			default:
			}
		})
	}
}

// TestColdStartHandshakeStaticTools proves the proxy can answer the handshake
// frames locally before the daemon connects: initialize and tools/list get a
// synthesized response (id echoed), while tools/call is left for the daemon.
func TestColdStartHandshakeStaticTools(t *testing.T) {
	// initialize → a static result carrying serverInfo + instructions.
	reply, ok := answerColdStart([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`), nil)
	if !ok {
		t.Fatal("initialize must be answerable at cold start")
	}
	var initResp struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			ServerInfo   struct{ Name string `json:"name"` } `json:"serverInfo"`
			Instructions string                              `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reply, &initResp); err != nil {
		t.Fatalf("initialize reply not JSON: %v", err)
	}
	if initResp.Result.ServerInfo.Name != "gortex" {
		t.Errorf("serverInfo.name = %q, want gortex", initResp.Result.ServerInfo.Name)
	}
	if string(initResp.ID) != "1" {
		t.Errorf("request id not echoed: %s", initResp.ID)
	}
	if initResp.Result.Instructions == "" {
		t.Error("cold-start initialize must carry instructions")
	}

	// tools/list → the static cold-start core set.
	reply, ok = answerColdStart([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), nil)
	if !ok {
		t.Fatal("tools/list must be answerable at cold start")
	}
	var listResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reply, &listResp); err != nil {
		t.Fatalf("tools/list reply not JSON: %v", err)
	}
	if len(listResp.Result.Tools) != len(coldStartTools) {
		t.Errorf("cold-start tools = %d, want %d", len(listResp.Result.Tools), len(coldStartTools))
	}
	names := map[string]bool{}
	for _, tl := range listResp.Result.Tools {
		names[tl.Name] = true
	}
	if !names["smart_context"] || !names["search_symbols"] {
		t.Errorf("cold-start list must include the hot tools; got %v", names)
	}

	// tools/call must NOT be answered locally — it needs the daemon (or the
	// embedded fallback).
	if _, ok := answerColdStart([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x"}}`), nil); ok {
		t.Error("tools/call must not be answered at cold start")
	}
}
