package analyzer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/resolver"
)

func TestVerifyReportToMap(t *testing.T) {
	rep := resolver.TemporalVerifyReport{
		Checked: 3, Confirmed: 1, Rejected: 1, Uncertain: 1, Errors: 0,
		Details: []resolver.TemporalVerifyDetail{
			{From: "wf::A", To: "act::B", Name: "B", Kind: "activity", Source: "env_default:heuristic", Verdict: resolver.TemporalVerdictConfirmed, Reason: "ok"},
		},
	}
	m := VerifyReportToMap(rep)
	assert.Equal(t, 3, m["checked"])
	assert.Equal(t, 1, m["confirmed"])
	totals, _ := m["totals"].(map[string]int)
	assert.Equal(t, 1, totals["rejected"])
	details, _ := m["details"].([]map[string]any)
	require.Len(t, details, 1)
	assert.Equal(t, "confirmed", details[0]["verdict"])
	assert.Equal(t, "env_default:heuristic", details[0]["source"])
}

func TestParseTemporalVerdict(t *testing.T) {
	cases := []struct {
		raw  string
		want resolver.TemporalVerdict
	}{
		{`{"verdict":"confirmed","reason":"matches"}`, resolver.TemporalVerdictConfirmed},
		{`{"verdict":"rejected","reason":"wrong target"}`, resolver.TemporalVerdictRejected},
		{`{"verdict":"uncertain"}`, resolver.TemporalVerdictUncertain},
		{`CONFIRMED is my answer: {"verdict":"CONFIRMED"}`, resolver.TemporalVerdictConfirmed}, // prose-wrapped + case
		{"```json\n{\"verdict\":\"rejected\"}\n```", resolver.TemporalVerdictRejected},         // fenced
		{`not json at all`, resolver.TemporalVerdictUncertain},                                 // unparseable → uncertain
		{`{"verdict":"banana"}`, resolver.TemporalVerdictUncertain},                            // unknown → uncertain
	}
	for _, c := range cases {
		got := parseTemporalVerdict(c.raw)
		assert.Equal(t, c.want, got.Verdict, "raw=%q", c.raw)
	}
}

type countingVerifier struct {
	calls int
	res   resolver.TemporalVerifyResult
}

func (c *countingVerifier) Verify(context.Context, resolver.TemporalVerifyRequest) (resolver.TemporalVerifyResult, error) {
	c.calls++
	return c.res, nil
}

func TestCachingVerifier_HitsCacheAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gortex", "temporal-verify-cache.json")
	inner := &countingVerifier{res: resolver.TemporalVerifyResult{Verdict: resolver.TemporalVerdictConfirmed, Reason: "ok"}}
	req := resolver.TemporalVerifyRequest{Kind: "activity", DispatchName: "ChargeActivity", CallerSource: "wf", TargetSource: "act"}

	c := NewCachingVerifier(inner, "model-x", path)
	r1, _ := c.Verify(context.Background(), req)
	r2, _ := c.Verify(context.Background(), req)
	assert.Equal(t, resolver.TemporalVerdictConfirmed, r1.Verdict)
	assert.Equal(t, resolver.TemporalVerdictConfirmed, r2.Verdict)
	assert.Equal(t, 1, inner.calls, "second identical request must hit the cache")

	// A different model is a different key → delegates again.
	c.model = "model-y"
	_, _ = c.Verify(context.Background(), req)
	assert.Equal(t, 2, inner.calls, "different model must miss the cache")
	c.model = "model-x"

	require.NoError(t, c.Flush())

	// A fresh caching verifier loads the persisted verdict — no delegate call.
	inner2 := &countingVerifier{res: resolver.TemporalVerifyResult{Verdict: resolver.TemporalVerdictRejected}}
	c2 := NewCachingVerifier(inner2, "model-x", path)
	r3, _ := c2.Verify(context.Background(), req)
	assert.Equal(t, resolver.TemporalVerdictConfirmed, r3.Verdict, "loaded from disk, not the new delegate")
	assert.Equal(t, 0, inner2.calls)
}
