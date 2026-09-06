package main

// PURPOSE — unit tests for `gortex untrack`'s demotion path. The daemon
// starts a demotion without waiting for it to finish (see StartApplyUntrack
// in internal/indexer): that build routinely runs past the MCP tool's ~59s
// deadline (mcp/tool_deadline.go) for a checkout that diverges from the
// family primary by any meaningful number of files. --wait polls the
// read-only list_checkouts view for the checkout's effective mode AND its
// in-flight transition's own state instead of re-invoking untrack_repository,
// which would re-derive its plan from catalog rows the running worker is
// still moving. Exercised without a running daemon by stubbing the
// untrackDaemonTool / listCheckoutsFn seams and shrinking the poll interval.
// KEYWORDS — untrack, demote, wait, daemon, poll, tool deadline

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

// demotingToolPayload models what untrack_repository actually answers for a
// demotion still building: it must carry checkout_id (untrackResultPayload
// emits it from indexer.UntrackResult.CheckoutID) or --wait has nothing to
// poll for.
func demotingToolPayload(checkoutID string) json.RawMessage {
	body := `{"status":"demoting","plan":"demote","prefix":"repo@wt",` +
		`"pending":true,"transition_id":"tr-1"`
	if checkoutID != "" {
		body += `,"checkout_id":"` + checkoutID + `"`
	}
	return json.RawMessage(body + "}")
}

// listCheckoutsBody models one list_checkouts answer. transition is the
// in-flight mode change's own state ("pending"/"running"/"failed"), empty
// when none is in flight — see checkoutPayload.Transition.
func listCheckoutsBody(checkoutID, mode, transition string) json.RawMessage {
	return json.RawMessage(`{"families":[{"family_id":"fam-1","checkouts":[` +
		`{"checkout_id":"` + checkoutID + `","effective_mode":"` + mode + `",` +
		`"transition":"` + transition + `"}]}]}`)
}

func emptyCheckoutsBody() json.RawMessage {
	return json.RawMessage(`{"families":[{"family_id":"fam-1","checkouts":[]}]}`)
}

func withUntrackSeams(t *testing.T) {
	t.Helper()
	origTool, origList, origInterval := untrackDaemonTool, listCheckoutsFn, untrackPollInterval
	origWindow := untrackPollAnomalyWindow
	t.Cleanup(func() {
		untrackDaemonTool, listCheckoutsFn, untrackPollInterval = origTool, origList, origInterval
		untrackPollAnomalyWindow = origWindow
	})
	t.Cleanup(func() {
		untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", false, 0
	})
	untrackPollInterval = time.Millisecond
	// Scaled to the poll interval the same way the production pair is: a
	// window worth a few dozen polls, so a test that means "this run of
	// failures is fatal" still proves the poller RETRIED before giving up.
	untrackPollAnomalyWindow = 25 * time.Millisecond
}

// unboundViewErr is the pre-flight a lookup routed at a checkout with no view
// of its own is refused with. Built by the real constructor, so the test is
// bound to the identity the poller actually classifies on and not to prose:
// a demotion's middle IS that state — route pending, no generation, first
// build deferred until a read — so the poller meets this error for the whole
// teardown while observing the very transition that causes it.
var unboundViewErr = worktreeCWDErr("/repo/wt", worktreeFamily{mainRepo: "/repo"}, "/repo")

// TestUntrackWithoutWaitReportsPendingAndDoesNotPoll proves the default (no
// --wait) call returns as soon as the daemon admits the demotion — it must
// not block on, or poll for, the automatic-lane build settling.
func TestUntrackWithoutWaitReportsPendingAndDoesNotPoll(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait = false, "text", false
	toolCalls := 0
	untrackDaemonTool = func(_ string, tool string, _ map[string]any) (json.RawMessage, error) {
		toolCalls++
		require.Equal(t, "untrack_repository", tool)
		return demotingToolPayload("co-1"), nil
	}
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		t.Fatal("without --wait, untrack must not poll list_checkouts")
		return nil, nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo/wt"))
	require.Equal(t, 1, toolCalls, "no polling: untrack_repository is called exactly once")
	require.Contains(t, buf.String(), "demoting /repo/wt")
	require.Contains(t, buf.String(), "--wait")
}

// TestUntrackWaitPollsListCheckoutsUntilDemoted proves --wait blocks until
// the checkout's effective mode flips to automatic, polling the read-only
// list_checkouts view rather than re-invoking untrack_repository.
func TestUntrackWaitPollsListCheckoutsUntilDemoted(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait = false, "text", true
	toolCalls := 0
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		toolCalls++
		return demotingToolPayload("co-1"), nil
	}
	type step struct{ mode, transition string }
	seq := []step{{"dedicated", "running"}, {"dedicated", "running"}, {"automatic", ""}}
	i := 0
	listCalls := 0
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		listCalls++
		s := seq[i]
		if i < len(seq)-1 {
			i++
		}
		return listCheckoutsBody("co-1", s.mode, s.transition), nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo/wt"))
	require.Equal(t, 1, toolCalls, "--wait never re-invokes untrack_repository")
	require.GreaterOrEqual(t, listCalls, 2, "polled list_checkouts more than once")
	require.Contains(t, buf.String(),
		"[gortex] demoted /repo/wt to its family's automatic lane (via daemon)")
}

// TestUntrackWaitDoesNotLandWhileTheTransitionRuns is the regression that
// made --wait worth having.
//
// automatic is the FIRST durable write of a demotion, not the last:
// CommitAuthorizedDemotion publishes the mode flip seconds in, and the worker
// then retires the dedicated corpus, evicts every node/edge/file row of the
// repository, drops its entry from the global config and completes the
// transition. Settling on the mode alone printed "demotion landed (3.6s)"
// while all of that was still ahead — and `gortex repos`, the config file and
// the catalog then disagreed with the CLI for as long as the teardown took.
// The end state is automatic AND an empty transition slot.
func TestUntrackWaitDoesNotLandWhileTheTransitionRuns(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait = false, "text", true
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	// The mode is already automatic on the very first poll. Only the standing
	// transition says the teardown behind it is still running.
	type step struct{ mode, transition string }
	seq := []step{
		{"automatic", "running"},
		{"automatic", "running"},
		{"automatic", "pending"},
		{"automatic", ""},
	}
	i := 0
	listCalls := 0
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		listCalls++
		s := seq[i]
		if i < len(seq)-1 {
			i++
		}
		return listCheckoutsBody("co-1", s.mode, s.transition), nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo/wt"))
	require.Equal(t, len(seq), listCalls,
		"--wait must keep polling while the transition stands, not settle on the mode flip")
	require.Contains(t, buf.String(),
		"[gortex] demoted /repo/wt to its family's automatic lane (via daemon)")
}

// TestUntrackWaitFailsOnAFailedTransitionEvenOnceAutomatic proves the failure
// check is asked BEFORE the settle check. A demotion whose worker gave up
// after publishing the mode flip is automatic and failed at the same time;
// reading the mode first would have called that a landing.
func TestUntrackWaitFailsOnAFailedTransitionEvenOnceAutomatic(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, time.Hour
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		return listCheckoutsBody("co-1", "automatic", "failed"), nil
	}

	start := time.Now()
	cmd, buf := newCheckoutsTestCmd(t)
	err := untrackViaDaemon(cmd, buf, ".", "/repo/wt")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)
	require.Contains(t, err.Error(), "failed")
	require.NotContains(t, buf.String(), "demotion landed")
}

// TestUntrackWaitTimesOut proves a demotion that never settles (transition
// stays "running") fails with a timeout rather than blocking forever, and
// says the background work may still be running.
func TestUntrackWaitTimesOut(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, 20*time.Millisecond
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		return listCheckoutsBody("co-1", "dedicated", "running"), nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	err := untrackViaDaemon(cmd, buf, ".", "/repo/wt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
	require.Contains(t, err.Error(), "background")

	// beforeTrackDeadline can return on its timer while its probe goroutine
	// is still in flight (it "winds down" in the background by design — see
	// its doc comment); that goroutine reads listCheckoutsFn one more time.
	// Give it room to finish before t.Cleanup swaps the seam back, or the
	// race detector sees a real read/write race on the package var.
	time.Sleep(50 * time.Millisecond)
}

// TestUntrackWaitFailsFastOnFailedTransition proves a demotion whose
// transition state reports "failed" (a state the catalog retains
// deliberately rather than clearing — see checkoutEffectiveMode) is reported
// as a failure immediately, not burned through as a timeout even though the
// effective mode never moves off "dedicated".
func TestUntrackWaitFailsFastOnFailedTransition(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, time.Hour
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		return listCheckoutsBody("co-1", "dedicated", "failed"), nil
	}

	start := time.Now()
	cmd, buf := newCheckoutsTestCmd(t)
	err := untrackViaDaemon(cmd, buf, ".", "/repo/wt")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "must not burn the --wait-timeout on a known failure")
	require.Contains(t, err.Error(), "failed")
	require.NotContains(t, err.Error(), "timed out")
}

// TestUntrackWaitToleratesTheUnboundViewErrorWhileDemoting is the regression
// for a --wait that died four seconds into a demotion it had correctly
// started.
//
// A demoted checkout has no view of its own until something reads it, so a
// lookup routed at its path is refused by the coverage pre-flight for as long
// as that lasts — deterministically, for the whole teardown. First a
// three-poll budget called that a daemon failure and gave up after 4s; then a
// one-minute window called it a daemon failure and gave up after 1m. No budget
// is the answer, because the refusal is not an anomaly: it is what the state
// being waited on looks like from outside.
func TestUntrackWaitToleratesTheUnboundViewErrorWhileDemoting(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, time.Hour
	// Hostile on purpose: any anomaly at all is instantly fatal under this
	// budget, so what survives the run below survives because the refusal is
	// CLASSIFIED as the demotion's own state, not because the window is
	// generous.
	untrackPollAnomalyWindow = time.Nanosecond
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}

	// Six consecutive refusals — twice the old budget — then the catalog
	// answers again and the transition clears.
	const refusals = 6
	polls := 0
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		polls++
		switch {
		case polls <= refusals:
			return nil, unboundViewErr
		case polls == refusals+1:
			return listCheckoutsBody("co-1", "automatic", "running"), nil
		default:
			return listCheckoutsBody("co-1", "automatic", ""), nil
		}
	}

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo/wt"),
		"a refusal that names the demotion's own state is not a poll failure")
	require.Greater(t, polls, refusals+1, "the wait must have outlived the refusals")
	require.Contains(t, buf.String(),
		"[gortex] demoted /repo/wt to its family's automatic lane (via daemon)")
}

// TestUntrackWaitPollsAnUnboundWorktreeThroughItsFamily is the other half:
// the poller must not route its own lookup at a path it knows has no view.
//
// list_checkouts answers about the whole catalog, not about the connection's
// view, so relaying it through the family's tracked working copy — what every
// other checkout verb does, and what checkoutsRelayPath exists for — changes
// nothing about the answer and everything about whether there is one.
func TestUntrackWaitPollsAnUnboundWorktreeThroughItsFamily(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)
	stub := startStubDaemon(t, []string{mainRepo})
	stub.mcpResult = listCheckoutsBody("co-1", "automatic", "")

	// The wiring this replaced: routed at the worktree's own path, every poll
	// is refused with the pre-flight that names exactly this state.
	_, err := requireDaemonTool(worktree, "list_checkouts", map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "has not bound the worktree",
		"the fixture must actually reproduce the unbound-view refusal")

	// The production listCheckoutsFn, unstubbed.
	mode, transition, found, err := checkoutEffectiveMode(worktree, "co-1")
	require.NoError(t, err, "the poll must not be refused for the state it is polling for")
	require.True(t, found)
	require.Equal(t, "automatic", mode)
	require.Empty(t, transition)
	require.Equal(t, mainRepo, stub.seenMCPHandshake().CWD,
		"the poll must relay through the family's tracked working copy")
	if tool, _ := stub.seenTool(); tool != "list_checkouts" {
		t.Fatalf("relayed the wrong tool: %q", tool)
	}
}

// TestUntrackWaitPollsThroughATrackedRootWhenTheRelayCannotHelp reproduces the
// live shape that survived the first two attempts at this.
//
// The subject is a linked worktree the daemon does not bind (a demotion in
// flight). The checkout-verb relay is present but declines to move the path —
// which is exactly what checkoutsRelayPath does when its control probe fails
// open on a busy daemon, and the daemon is busiest while retiring a corpus.
// The poller must still get an answer, because list_checkouts reads the
// catalog and ANY tracked repository root is a connection the routing
// pre-flight accepts.
func TestUntrackWaitPollsThroughATrackedRootWhenTheRelayCannotHelp(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)
	stub := startStubDaemon(t, []string{mainRepo})
	stub.mcpResult = listCheckoutsBody("co-1", "automatic", "")

	// The relay that cannot help. A checkoutsRelayPath whose control probe
	// failed open returns the path unmoved, which is behaviourally identical
	// to not relaying at all — so both the poll-path resolver's view of the
	// relay and the relay the poll itself goes through are pinned to that.
	origRelay, origTool := checkoutsRelayFn, checkoutsDaemonTool
	t.Cleanup(func() { checkoutsRelayFn, checkoutsDaemonTool = origRelay, origTool })
	checkoutsRelayFn = func(p string) string { return p }
	checkoutsDaemonTool = requireDaemonTool

	// Both the worktree AND the relay's answer are the same refused path.
	_, err := requireDaemonTool(worktree, "list_checkouts", map[string]any{})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnboundWorktreeView,
		"the fixture must reproduce the unbound-view refusal")

	pollPath, inFamily := demotionPollPath(worktree)
	require.Equal(t, mainRepo, pollPath,
		"with no relay to lean on the poller must fall back to a tracked root")
	require.True(t, inFamily,
		"a tracked root the checkout lives under is the family's own repository")

	mode, transition, found, err := checkoutEffectiveMode(pollPath, "co-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "automatic", mode)
	require.Empty(t, transition)
	require.Equal(t, mainRepo, stub.seenMCPHandshake().CWD)

	// End to end through the wait, with the production listCheckoutsFn. The
	// timeout is short on purpose: a poller that keeps aiming at the worktree
	// never reads the catalog and can only ever burn it.
	origUntrack, origInterval := untrackDaemonTool, untrackPollInterval
	t.Cleanup(func() { untrackDaemonTool, untrackPollInterval = origUntrack, origInterval })
	t.Cleanup(func() {
		untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", false, 0
	})
	untrackPollInterval = time.Millisecond
	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, 2*time.Second
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, worktree, worktree),
		"the wait must read the catalog through a path the pre-flight accepts")
	require.Contains(t, buf.String(), "to its family's automatic lane (via daemon)")
}

// TestDemotionPollPathPrefersTheFamilyThenAnyTrackedRoot pins the order the
// poll connection is chosen in, including the arms the stub daemon cannot
// stage: a relay that DOES name the family wins, a tracked root containing
// the checkout is preferred over an unrelated one, and a status the daemon
// will not answer leaves the path alone rather than inventing one.
func TestDemotionPollPathPrefersTheFamilyThenAnyTrackedRoot(t *testing.T) {
	origRelay, origStatus := checkoutsRelayFn, trackStatusFn
	t.Cleanup(func() { checkoutsRelayFn, trackStatusFn = origRelay, origStatus })

	status := func(paths ...string) func() (daemon.StatusResponse, error) {
		return func() (daemon.StatusResponse, error) {
			st := daemon.StatusResponse{}
			for _, p := range paths {
				st.TrackedRepos = append(st.TrackedRepos, daemon.TrackedRepoStatus{Path: p})
			}
			return st, nil
		}
	}

	// inFamily is asserted alongside the path on every arm: list_checkouts
	// clamps its answer to the calling session's workspace
	// (checkoutOverviewInScope), so "which path" and "can that path see the
	// subject's family" are two different answers and the poller acts on both.
	t.Run("the relay names the family", func(t *testing.T) {
		checkoutsRelayFn = func(string) string { return "/fam/main" }
		trackStatusFn = status("/other")
		path, inFamily := demotionPollPath("/fam/wt")
		require.Equal(t, "/fam/main", path)
		require.True(t, inFamily, "the relay's answer is the family's own working copy")
	})

	t.Run("a containing tracked root beats an unrelated one", func(t *testing.T) {
		checkoutsRelayFn = func(p string) string { return p }
		trackStatusFn = status("/other", filepath.Join(t.TempDir(), "nope"), "/fam")
		path, inFamily := demotionPollPath("/fam/wt")
		require.Equal(t, "/fam", path)
		require.True(t, inFamily, "a tracked root the checkout lives under is its family's repository")
	})

	t.Run("any tracked root when none contains the checkout", func(t *testing.T) {
		checkoutsRelayFn = func(p string) string { return p }
		trackStatusFn = status("/elsewhere")
		path, inFamily := demotionPollPath("/fam/wt")
		require.Equal(t, "/elsewhere", path)
		require.False(t, inFamily,
			"an unrelated tracked root may be scoped away from the subject's family")
	})

	t.Run("no status, no invention", func(t *testing.T) {
		checkoutsRelayFn = func(p string) string { return p }
		trackStatusFn = func() (daemon.StatusResponse, error) {
			return daemon.StatusResponse{}, errUntrackWaitTestPoll
		}
		path, inFamily := demotionPollPath("/fam/wt")
		require.Equal(t, "/fam/wt", path)
		require.False(t, inFamily, "the unmoved subject path is not a family connection")
	})
}

// TestUntrackWaitGivesUpAfterConsecutivePollErrors proves a poller that can't
// reach the daemon does not silently retry forever nor discard the error: it
// retries for untrackPollAnomalyWindow and then gives up reporting the last
// failure.
func TestUntrackWaitGivesUpAfterConsecutivePollErrors(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, time.Hour
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	pollCalls := 0
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		pollCalls++
		return nil, errUntrackWaitTestPoll
	}

	start := time.Now()
	cmd, buf := newCheckoutsTestCmd(t)
	err := untrackViaDaemon(cmd, buf, ".", "/repo/wt")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "must not burn the --wait-timeout on a broken poller")
	require.ErrorIs(t, err, errUntrackWaitTestPoll)
	require.Contains(t, err.Error(), "of consecutive poll failures",
		"the budget is a window, so the message states a duration")
	require.Greater(t, pollCalls, 1, "a window must retry, not give up on the first failure")
}

// TestUntrackWaitGivesUpWhenCheckoutGoesMissing proves "the checkout is no
// longer listed" is its own outcome — retried for the anomaly window, then
// an error — not
// silently treated as "the demotion landed" (a restarted daemon that has not
// resumed the transition yet looks the same as a checkout list_checkouts
// simply doesn't know about).
func TestUntrackWaitGivesUpWhenCheckoutGoesMissing(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, time.Hour
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	pollCalls := 0
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		pollCalls++
		return emptyCheckoutsBody(), nil
	}

	start := time.Now()
	cmd, buf := newCheckoutsTestCmd(t)
	err := untrackViaDaemon(cmd, buf, ".", "/repo/wt")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "must not burn the --wait-timeout on a missing checkout")
	require.Contains(t, err.Error(), "no longer listed")
	require.Greater(t, pollCalls, 1, "a window must retry, not give up on the first miss")
}

// TestUntrackWaitRefusesWhenCheckoutIDMissing proves untrackViaDaemon refuses
// to enter the poll loop at all when the daemon's answer carries no
// checkout_id: polling an empty ID would match nothing, and treating that as
// "settled" (P0 regression) reported success while the build was still
// running.
func TestUntrackWaitRefusesWhenCheckoutIDMissing(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait = false, "text", true
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload(""), nil
	}
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		t.Fatal("must not poll when checkout_id is empty")
		return nil, nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	err := untrackViaDaemon(cmd, buf, ".", "/repo/wt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "checkout_id")
}

// TestUntrackJSONWaitPreviewReturnsJSONNotSummary proves `--format json
// --wait` against a destructive plan awaiting --confirm prints JSON, not the
// human preview card: nothing has run yet (there is nothing to wait for
// either way), and the preview branch used to return before the format check
// ran.
func TestUntrackJSONWaitPreviewReturnsJSONNotSummary(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait = false, "json", true
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"status":"preview","action":"untrack","plan":"primary_closure",` +
			`"prefix":"repo","confirm_required":true,"detail":"nothing was written"}`), nil
	}
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		t.Fatal("a preview has nothing running yet; must not poll")
		return nil, nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo"))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded), "output must be JSON: %s", buf.String())
	require.Equal(t, "preview", decoded["status"])
	require.NotContains(t, buf.String(), "would run the")
}

// TestUntrackJSONWaitPreservesUnmodeledFields proves the settled --format
// json response is patched onto the daemon's raw answer rather than
// round-tripped through the checkoutOutcome struct, which does not model
// every field (e.g. dependents) and would silently drop them.
func TestUntrackJSONWaitPreservesUnmodeledFields(t *testing.T) {
	withUntrackSeams(t)
	untrackConfirm, untrackFormat, untrackWait = false, "json", true
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"status":"demoting","plan":"demote","prefix":"repo@wt",` +
			`"checkout_id":"co-1","pending":true,"transition_id":"tr-1",` +
			`"dependents":["view v1 is rooted in this graph"]}`), nil
	}
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		return listCheckoutsBody("co-1", "automatic", ""), nil
	}

	// Distinct stdout/stderr, unlike newCheckoutsTestCmd's single shared
	// buffer: waitForDemotionSettled's progress tracker writes to stderr
	// (the w parameter, same as everywhere else in this file), and only
	// stdout is asserted as JSON here — a script piping stdout to `jq`
	// would see the same separation.
	cmd := &cobra.Command{Use: "untrack"}
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	require.NoError(t, untrackViaDaemon(cmd, &errBuf, ".", "/repo/wt"))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(outBuf.Bytes(), &decoded), "stdout must be JSON: %s", outBuf.String())
	require.Equal(t, "demoted", decoded["status"])
	require.Equal(t, true, decoded["demoted"])
	require.NotContains(t, decoded, "pending")
	deps, ok := decoded["dependents"].([]any)
	require.True(t, ok, "dependents must survive the --wait round trip: %v", decoded)
	require.Contains(t, deps, "view v1 is rooted in this graph")
}

// TestCheckoutEffectiveModeReportsNotFound proves a checkout list_checkouts
// no longer names (e.g. it was forgotten by another actor mid-poll) is
// reported as not found rather than as an error.
func TestCheckoutEffectiveModeReportsNotFound(t *testing.T) {
	orig := listCheckoutsFn
	t.Cleanup(func() { listCheckoutsFn = orig })
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		return listCheckoutsBody("co-other", "dedicated", "running"), nil
	}
	mode, transition, found, err := checkoutEffectiveMode(".", "co-1")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, mode)
	require.Empty(t, transition)
}

// TestCheckoutEffectiveModeReportsTransitionState proves the transition's
// own state (not just the effective mode) is surfaced.
func TestCheckoutEffectiveModeReportsTransitionState(t *testing.T) {
	orig := listCheckoutsFn
	t.Cleanup(func() { listCheckoutsFn = orig })
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		return listCheckoutsBody("co-1", "dedicated", "failed"), nil
	}
	mode, transition, found, err := checkoutEffectiveMode(".", "co-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "dedicated", mode)
	require.Equal(t, "failed", transition)
}

var errUntrackWaitTestPoll = &untrackWaitTestPollError{}

type untrackWaitTestPollError struct{}

func (*untrackWaitTestPollError) Error() string { return "poll error: connection refused" }

// TestUntrackWaitRecoversFromAScopeBlindPollPath pins the interaction between
// the poller and list_checkouts' workspace clamp.
//
// list_checkouts' answer is filtered to the calling session's workspace
// (checkoutOverviewInScope). The poller's connection is normally the family's
// own working copy, which is inside that workspace — but when the relay fails
// open on a busy daemon the poller falls back to any tracked root, and a root
// in another workspace is handed an answer with the subject's family filtered
// out of it. That reads exactly like "the checkout is gone", which used to end
// the wait with an error on a demotion that was running perfectly.
//
// What is pinned: a not-found answer from such a connection re-asks for a poll
// path instead of counting against the anomaly window, and the recovered relay
// carries the next poll to an answer that can actually see the checkout.
func TestUntrackWaitRecoversFromAScopeBlindPollPath(t *testing.T) {
	withUntrackSeams(t)
	origRelay, origStatus := checkoutsRelayFn, trackStatusFn
	t.Cleanup(func() { checkoutsRelayFn, trackStatusFn = origRelay, origStatus })

	// The tracked root the fail-open fallback lands on. It is in another
	// workspace, so its answers never mention the subject's family.
	const blind, family = "/elsewhere", "/fam/main"
	trackStatusFn = func() (daemon.StatusResponse, error) {
		return daemon.StatusResponse{
			TrackedRepos: []daemon.TrackedRepoStatus{{Path: blind}},
		}, nil
	}
	// The relay fails open once — the daemon was too busy to answer Status —
	// and recovers on the retry the not-found answer triggers.
	relayCalls := 0
	checkoutsRelayFn = func(p string) string {
		relayCalls++
		if relayCalls == 1 {
			return p
		}
		return family
	}

	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, time.Hour
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	var polled []string
	listCheckoutsFn = func(path string) (json.RawMessage, error) {
		polled = append(polled, path)
		if path == family {
			return listCheckoutsBody("co-1", "automatic", ""), nil
		}
		// The clamp: the subject's family is not in this session's workspace,
		// so the answer carries someone else's families and never co-1.
		return emptyCheckoutsBody(), nil
	}

	start := time.Now()
	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, "/fam/wt", "/fam/wt"),
		"a family filtered out of a scope-clamped answer is not a missing checkout")
	require.Less(t, time.Since(start), 5*time.Second,
		"the recovery must not wait out the anomaly window, let alone --wait-timeout")
	require.Equal(t, []string{blind, family}, polled,
		"the poller must move to the family's own working copy rather than keep asking a blind connection")
	require.Contains(t, buf.String(), "to its family's automatic lane (via daemon)")
}

// TestUntrackWaitStillGivesUpWhenTheRelayCannotRecover is the other half: the
// second chance is a second chance, not an exemption. A poll path that stays
// outside the family still ends the wait on the anomaly window, because a
// wait that could only ever end at --wait-timeout would be worse than one that
// says what it could not see.
func TestUntrackWaitStillGivesUpWhenTheRelayCannotRecover(t *testing.T) {
	withUntrackSeams(t)
	origRelay, origStatus := checkoutsRelayFn, trackStatusFn
	t.Cleanup(func() { checkoutsRelayFn, trackStatusFn = origRelay, origStatus })

	trackStatusFn = func() (daemon.StatusResponse, error) {
		return daemon.StatusResponse{
			TrackedRepos: []daemon.TrackedRepoStatus{{Path: "/elsewhere"}},
		}, nil
	}
	checkoutsRelayFn = func(p string) string { return p }

	untrackConfirm, untrackFormat, untrackWait, untrackWaitTimeout = false, "text", true, time.Hour
	untrackDaemonTool = func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return demotingToolPayload("co-1"), nil
	}
	polls := 0
	listCheckoutsFn = func(string) (json.RawMessage, error) {
		polls++
		return emptyCheckoutsBody(), nil
	}

	start := time.Now()
	cmd, buf := newCheckoutsTestCmd(t)
	err := untrackViaDaemon(cmd, buf, "/fam/wt", "/fam/wt")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "must not burn the --wait-timeout")
	require.Contains(t, err.Error(), "no longer listed")
	require.Contains(t, err.Error(), "outside the checkout's workspace",
		"the message must name the connection that could not see the family")
	require.Greater(t, polls, 1, "the window must retry before giving up")
}
