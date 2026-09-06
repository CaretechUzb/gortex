package mcp

// PURPOSE — pure unit tests for untrackResultPayload's pending/transition_id
// fields: a caller (CLI or agent) uses these to tell an admitted-but-still-
// building demotion (StartApplyUntrack) apart from a finished one, without
// needing a live daemon or checkout fixture.
// KEYWORDS — untrack, demote, pending, transition, payload

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/indexer"
)

func TestUntrackResultPayloadReportsAPendingDemotion(t *testing.T) {
	payload := untrackResultPayload("demoting", indexer.UntrackResult{
		Prefix:       "repo@wt",
		CheckoutID:   "co-123",
		Plan:         indexer.UntrackPlanDemote,
		Pending:      true,
		TransitionID: "tr-123",
	})
	assert.Equal(t, "demoting", payload["status"])
	// A poller (untrackViaDaemon --wait) matches this against list_checkouts
	// rows by checkout_id; regressing this silently breaks --wait (it never
	// finds a match and either refuses or, worse, treats "not found" as
	// "settled" — see waitForDemotionSettled).
	assert.Equal(t, "co-123", payload["checkout_id"])
	assert.Equal(t, true, payload["pending"])
	assert.Equal(t, "tr-123", payload["transition_id"])
	assert.NotContains(t, payload, "demoted", "not demoted yet")
	// And it has to say what "not yet" means. The effective mode flips first
	// and the teardown runs behind it, so a caller that stops at automatic
	// declares success while the corpus, the rows and the config entry are
	// still going — the end state is automatic AND an empty transition slot.
	detail, ok := payload["detail"].(string)
	assert.True(t, ok, "a pending demotion has to name what it is still doing")
	assert.Contains(t, detail, "no transition in flight")
}

func TestUntrackResultPayloadOmitsCheckoutIDWhenUnset(t *testing.T) {
	payload := untrackResultPayload("untracked", indexer.UntrackResult{
		Prefix: "repo",
		Plan:   indexer.UntrackPlanEvict,
	})
	assert.NotContains(t, payload, "checkout_id")
}

func TestUntrackResultPayloadOmitsPendingWhenSettled(t *testing.T) {
	payload := untrackResultPayload("demoted", indexer.UntrackResult{
		Prefix:  "repo@wt",
		Plan:    indexer.UntrackPlanDemote,
		Demoted: true,
	})
	assert.NotContains(t, payload, "pending")
	assert.NotContains(t, payload, "transition_id")
	assert.Equal(t, true, payload["demoted"])
}
