package mcp

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/forge"
)

// TestListPRsPayload_BaseArgument pins the `base` argument on list_prs.
//
// This flips real behaviour: the previous call was ClassifyStatus(pr, pr.BaseRef)
// — each PR judged against its OWN base — which made BASE_MISMATCH structurally
// impossible from list_prs on any forge, even though the tool advertises the
// state. A regression back to that form would look clean and assert nothing.
func TestListPRsPayload_BaseArgument(t *testing.T) {
	prs := []forge.PR{
		{Number: 1, BaseRef: "16.0", UpdatedAt: time.Now()},
		{Number: 2, BaseRef: "aurora-redesign", UpdatedAt: time.Now()},
		{Number: 3, BaseRef: "some/feature", UpdatedAt: time.Now()},
	}
	stateOf := func(payload map[string]any, num int) string {
		rows, _ := payload["prs"].([]map[string]any)
		for _, r := range rows {
			if n, _ := r["number"].(int); n == num {
				s, _ := r["state"].(string)
				return s
			}
		}
		t.Fatalf("PR %d missing from payload", num)
		return ""
	}

	// No base argument: the check is DISABLED, not inverted — nothing is flagged.
	off := listPRsPayload(prs, "")
	for _, n := range []int{1, 2, 3} {
		if got := stateOf(off, n); got == "BASE_MISMATCH" {
			t.Errorf("PR %d flagged with no base argument (got %q)", n, got)
		}
	}

	// One base: the other live target is flagged.
	single := listPRsPayload(prs, "16.0")
	if got := stateOf(single, 2); got != "BASE_MISMATCH" {
		t.Errorf("PR 2 with base=16.0 = %q, want BASE_MISMATCH", got)
	}

	// Both live targets accepted, stray still flagged.
	multi := listPRsPayload(prs, "16.0,aurora-redesign")
	for _, n := range []int{1, 2} {
		if got := stateOf(multi, n); got == "BASE_MISMATCH" {
			t.Errorf("PR %d on a live target flagged (got %q)", n, got)
		}
	}
	if got := stateOf(multi, 3); got != "BASE_MISMATCH" {
		t.Errorf("stray PR 3 = %q, want BASE_MISMATCH — the check went inert", got)
	}
}
