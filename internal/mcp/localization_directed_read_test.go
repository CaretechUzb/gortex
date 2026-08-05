package mcp

import (
	"strings"
	"testing"
)

func directedFileRead(path string) map[string]any {
	return map[string]any{"target": map[string]any{"file": path}}
}

func armedRefinement(t *testing.T, task string) *localizationTerminalState {
	t.Helper()
	state := newLocalizationTerminalState()
	candidate := "repo/tools/unrelated.go::Script"
	state.armRefinementRoutesForTask(
		task,
		candidate,
		[]string{candidate},
		map[string]localizationRefinementRoute{candidate: {}},
		nil,
	)
	if state.state != localizationStateNeedsRefinement {
		t.Fatalf("arm produced state %q, want needs_refinement", state.state)
	}
	return state
}

// A ranked page whose candidates are all wrong used to gate every read-only
// navigation call while leaving edit and refactor untouched. Naming a file is
// the caller declaring it no longer needs this localization.
func TestDirectedReadReleasesRefinementContract(t *testing.T) {
	state := armedRefinement(t, "locate the resolver behaviour")

	blocked, token := state.authorizeWithToken("read", "file", directedFileRead("internal/other/other.go"))
	if blocked != nil || token != 0 {
		t.Fatalf("directed read was not admitted: blocked=%#v token=%d", blocked, token)
	}
	if state.state != localizationStateInactive {
		t.Fatalf("directed read left state %q, want inactive", state.state)
	}

	// The whole point of releasing: the rest of the navigation surface works.
	if blocked, token := state.authorizeWithToken("search", "text", map[string]any{"query": "anything at all"}); blocked != nil || token != 0 {
		t.Fatalf("search still gated after release: blocked=%#v token=%d", blocked, token)
	}
}

func TestDirectedReadReleasesEveryNonTerminalContract(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *localizationTerminalState
	}{
		{"needs_refinement", armedRefinement(t, "locate the resolver behaviour")},
		{"needs_exact_read", func() *localizationTerminalState {
			s := newLocalizationTerminalState()
			s.armForTask(newLocalizationCompletion(false, "repo/pkg/file.go::Run"), "locate run")
			return s
		}()},
		{"needs_recovery", func() *localizationTerminalState {
			s := newLocalizationTerminalState()
			s.armForTask(newLocalizationRecoveryCompletion(), "locate run")
			return s
		}()},
		{"planned_recovery", func() *localizationTerminalState {
			s := newLocalizationTerminalState()
			s.armForTask(newLocalizationPlannedRecoveryCompletion("search.symbols", "Resolver"), "locate run")
			return s
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// interceptAnswerReady runs first in dispatch; it must not refuse the
			// call, and authorizeWithToken is where the release lands (intercept
			// only owns answer_ready and needs_recovery).
			if blocked, _ := tc.state.interceptAnswerReady("read", "file", directedFileRead("internal/other/other.go")); blocked != nil {
				t.Fatalf("intercept refused the directed read: %#v", blocked)
			}
			if blocked, token := tc.state.authorizeWithToken("read", "file", directedFileRead("internal/other/other.go")); blocked != nil || token != 0 {
				t.Fatalf("authorize refused the directed read: blocked=%#v token=%d", blocked, token)
			}
			if tc.state.state != localizationStateInactive {
				t.Fatalf("dispatch left state %q, want inactive", tc.state.state)
			}
			if blocked, token := tc.state.authorizeWithToken("search", "text", map[string]any{"query": "anything at all"}); blocked != nil || token != 0 {
				t.Fatalf("navigation still gated after release: blocked=%#v token=%d", blocked, token)
			}
		})
	}
}

// The measured terminality contract is not a deadlock: after answer_ready a
// directed read still replays the retained evidence.
func TestDirectedReadDoesNotReleaseAnswerReady(t *testing.T) {
	state := newLocalizationTerminalState()
	state.arm(newLocalizationCompletion(true, ""))

	blocked, token := state.authorizeWithToken("read", "file", directedFileRead("internal/other/other.go"))
	if blocked == nil || token != 0 {
		t.Fatalf("directed read escaped answer_ready: blocked=%#v token=%d", blocked, token)
	}
	if state.state != localizationStateAnswerReady {
		t.Fatalf("answer_ready was released to %q", state.state)
	}
	if intercepted, _ := state.interceptAnswerReady("read", "file", directedFileRead("internal/other/other.go")); intercepted == nil {
		t.Fatal("intercept admitted a directed read after answer_ready")
	}
}

// An in-flight state means another permitted call is mid-handler. Releasing
// there would race its finalizer, and the condition resolves on its own.
func TestDirectedReadDoesNotReleaseInFlightContract(t *testing.T) {
	for _, inFlight := range []string{
		localizationStateRefineInFlight,
		localizationStateExactReadInFlight,
		localizationStateRecoveryInFlight,
	} {
		t.Run(inFlight, func(t *testing.T) {
			state := &localizationTerminalState{state: inFlight, generation: 3}
			blocked, token := state.authorizeWithToken("read", "file", directedFileRead("internal/other/other.go"))
			if blocked == nil || token != 0 {
				t.Fatalf("directed read admitted mid-flight: blocked=%#v token=%d", blocked, token)
			}
			if state.state != inFlight {
				t.Fatalf("in-flight state became %q", state.state)
			}
		})
	}
}

// Releasing on read.source would discard the refinement steering instead of
// following it, so the prescribed call keeps its existing meaning.
func TestReadSourceKeepsRefinementSteering(t *testing.T) {
	state := armedRefinement(t, "locate the resolver behaviour")

	wrong := map[string]any{"target": map[string]any{"symbol": "repo/pkg/wrong.go::Wrong"}}
	if blocked, token := state.authorizeWithToken("read", "source", wrong); blocked == nil || token != 0 {
		t.Fatalf("unauthorized read.source was admitted: blocked=%#v token=%d", blocked, token)
	}
	if state.state != localizationStateNeedsRefinement {
		t.Fatalf("unauthorized read.source released the contract to %q", state.state)
	}

	allowed := map[string]any{"target": map[string]any{"symbol": "repo/tools/unrelated.go::Script"}}
	if blocked, token := state.authorizeWithToken("read", "source", allowed); blocked != nil || token == 0 {
		t.Fatalf("prescribed read.source was not reserved: blocked=%#v token=%d", blocked, token)
	}
}

func TestDirectedReadRequiresAnExplicitTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation string
		arguments map[string]any
		want      bool
	}{
		{"file target", "file", directedFileRead("a/b.go"), true},
		{"summary target", "summary", directedFileRead("a/b.go"), true},
		{"editing_context target", "editing_context", directedFileRead("a/b.go"), true},
		{"symbol target", "history", map[string]any{"target": map[string]any{"symbol": "a/b.go::C"}}, true},
		{"symbols target", "symbols", map[string]any{"target": map[string]any{"symbols": []any{"a/b.go::C"}}}, true},
		{"source is prescribed, not directed", "source", directedFileRead("a/b.go"), false},
		{"no target", "file", map[string]any{"path": "a/b.go"}, false},
		{"empty target", "file", map[string]any{"target": map[string]any{"file": "  "}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := localizationDirectedRead("read", tc.operation, tc.arguments); got != tc.want {
				t.Fatalf("localizationDirectedRead = %v, want %v", got, tc.want)
			}
		})
	}
	if localizationDirectedRead("search", "text", directedFileRead("a/b.go")) {
		t.Fatal("a non-read facade was treated as a directed read")
	}
}

// The reporter's session: a literal lifted verbatim out of a task that wrapped
// it across lines could not match the whitespace-collapsed fingerprint.
func TestRecoveryAcceptsMultiLineVerbatimTaskLiteral(t *testing.T) {
	task := localizationTaskFingerprint("`gortex review` reports nothing on a dirty worktree. It prints:\n\n    No changes\n    in the current branch\n\nFind the emit site.")
	query := "No changes\n    in the current branch"

	if !localizationRecoveryQueryAligned(task, query) {
		t.Fatal("a literal copied verbatim from the task was rejected as unspecific")
	}
	if localizationRecoveryQueryAligned(task, "No changes in some other project entirely") {
		t.Fatal("a literal absent from the task was accepted")
	}
}

// A rejected recovery query is no longer a dead end: the release is reachable
// from needs_recovery whether or not the next anchor is ever accepted.
func TestRejectedRecoveryQueryStillLeavesAReachableMove(t *testing.T) {
	state := newLocalizationTerminalState()
	state.armForTask(newLocalizationRecoveryCompletion(), "locate the emit site behind the staleness gate")

	blocked, token := state.authorizeWithToken("search", "text", map[string]any{"query": "neighbor"})
	if blocked == nil || token != 0 {
		t.Fatalf("unanchored search.text was admitted: blocked=%#v token=%d", blocked, token)
	}
	if state.state != localizationStateNeedsRecovery {
		t.Fatalf("a refused anchor consumed the contract: state=%q", state.state)
	}
	text := toolResultText(blocked)
	if !strings.Contains(text, "releases this localization") {
		t.Fatalf("recovery refusal names no reachable move: %s", text)
	}

	if blocked, token := state.authorizeWithToken("read", "file", directedFileRead("internal/other/other.go")); blocked != nil || token != 0 {
		t.Fatalf("release refused after a rejected anchor: blocked=%#v token=%d", blocked, token)
	}
}

// Every refusal that leaves the caller with no reachable move must name one.
func TestBlockedNavigationNamesTheRelease(t *testing.T) {
	state := armedRefinement(t, "locate the resolver behaviour")
	blocked, _ := state.authorizeWithToken("relations", "usages", map[string]any{"target": map[string]any{"symbol": "a/b.go::C"}})
	if blocked == nil {
		t.Fatal("relations was admitted under a refinement contract")
	}
	// The payload is JSON, so the inner quotes arrive escaped.
	text := toolResultText(blocked)
	for _, required := range []string{`read(operation:`, `file`, `releases this localization`} {
		if !strings.Contains(text, required) {
			t.Fatalf("refusal is missing %q, so it names no reachable move: %s", required, text)
		}
	}
}
