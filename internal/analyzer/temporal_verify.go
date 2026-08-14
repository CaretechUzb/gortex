package analyzer

// LLM-backed adapter for the Temporal dispatch verification pass.
//
// PURPOSE — adapt the deterministic verification core in
// internal/resolver/temporal_verify.go to a real LLM provider, a reproducibility
// cache, and the canonical map[string]any output shape. Source grounding remains
// an injected resolver.TemporalSourceProvider supplied by the host that owns
// repository path resolution.
// RATIONALE — the cache keeps re-runs cheap and deterministic (same code + model
// → cached verdict) without coupling the resolver core to LLM or filesystem I/O.
// KEYWORDS — temporal, verify, llm, source, cache, adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/resolver"
)

// VerifyReportToMap converts a TemporalVerifyReport to the canonical
// map[string]any shape the CLI / MCP surfaces marshal. Field names are locked:
// checked, confirmed, rejected, uncertain, errors, details, totals.
func VerifyReportToMap(rep resolver.TemporalVerifyReport) map[string]any {
	details := make([]map[string]any, 0, len(rep.Details))
	for _, d := range rep.Details {
		details = append(details, map[string]any{
			"from":    d.From,
			"to":      d.To,
			"name":    d.Name,
			"kind":    d.Kind,
			"source":  d.Source,
			"verdict": string(d.Verdict),
			"reason":  d.Reason,
		})
	}
	return map[string]any{
		"checked":   rep.Checked,
		"confirmed": rep.Confirmed,
		"rejected":  rep.Rejected,
		"uncertain": rep.Uncertain,
		"errors":    rep.Errors,
		"details":   details,
		"totals": map[string]int{
			"checked":   rep.Checked,
			"confirmed": rep.Confirmed,
			"rejected":  rep.Rejected,
			"uncertain": rep.Uncertain,
			"errors":    rep.Errors,
		},
	}
}

// --- LLM verifier ----------------------------------------------------------

const temporalVerifySystemPrompt = `You verify guesses made by a static analyzer for the Temporal workflow engine.
You are given a workflow function that dispatches an activity or child-workflow by a NAME the analyzer GUESSED (often from an env-var default, a naming convention, or a fuzzy match), plus the candidate target function the analyzer resolved that name to.
Decide whether the workflow really dispatches that exact candidate.
Reply with STRICT JSON ONLY, no prose: {"verdict":"confirmed|rejected|uncertain","reason":"<short>"}.
- "confirmed": the workflow does dispatch this exact activity/workflow.
- "rejected": it clearly does NOT — wrong/unrelated target, the name is overridden at runtime, or the candidate is a test stub.
- "uncertain": not enough evidence.
Be conservative: prefer "uncertain" over a wrong "confirmed".`

type llmTemporalVerifier struct {
	p llm.Provider
}

// NewLLMTemporalVerifierFromProvider wraps an already-constructed provider.
//
// PURPOSE — let a host that already owns a live llm.Provider (e.g. the MCP
// server's shared LLM service) reuse it for temporal verification instead of
// spinning up a second provider from raw config.
// RATIONALE — the verifier is a thin Verify(req)→verdict adapter over
// Provider.Complete; binding it to an existing provider avoids duplicate model
// loads / API clients and respects the caller's provider lifecycle (no Close
// returned — the caller still owns p).
// KEYWORDS — temporal, verify, llm, provider, reuse
func NewLLMTemporalVerifierFromProvider(p llm.Provider) resolver.TemporalVerifier {
	return &llmTemporalVerifier{p: p}
}

func (v *llmTemporalVerifier) Verify(ctx context.Context, req resolver.TemporalVerifyRequest) (resolver.TemporalVerifyResult, error) {
	resp, err := v.p.Complete(ctx, llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: temporalVerifySystemPrompt},
			{Role: llm.RoleUser, Content: buildTemporalVerifyPrompt(req)},
		},
		MaxTokens: 300,
		Shape:     llm.ShapeFreeform,
	})
	if err != nil {
		return resolver.TemporalVerifyResult{}, err
	}
	return parseTemporalVerdict(resp.Text), nil
}

func buildTemporalVerifyPrompt(req resolver.TemporalVerifyRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dispatch name: %q (kind=%s, recognised via=%s)\n\n", req.DispatchName, req.Kind, req.Source)
	fmt.Fprintf(&b, "Calling workflow %q:\n```go\n%s\n```\n\n", req.CallerName, req.CallerSource)
	fmt.Fprintf(&b, "Candidate target %q:\n```go\n%s\n```\n\n", req.TargetName, req.TargetSource)
	b.WriteString(`Does the workflow dispatch this candidate? Reply with strict JSON {"verdict":...,"reason":...}.`)
	return b.String()
}

// parseTemporalVerdict tolerantly extracts a verdict from a possibly-wrapped
// LLM reply; an unparseable / unknown verdict degrades to "uncertain" so a
// flaky model never silently promotes or suppresses an edge.
func parseTemporalVerdict(raw string) resolver.TemporalVerifyResult {
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &v); err != nil {
		return resolver.TemporalVerifyResult{Verdict: resolver.TemporalVerdictUncertain, Reason: "unparseable verdict"}
	}
	switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
	case "confirmed":
		return resolver.TemporalVerifyResult{Verdict: resolver.TemporalVerdictConfirmed, Reason: v.Reason}
	case "rejected":
		return resolver.TemporalVerifyResult{Verdict: resolver.TemporalVerdictRejected, Reason: v.Reason}
	default:
		return resolver.TemporalVerifyResult{Verdict: resolver.TemporalVerdictUncertain, Reason: v.Reason}
	}
}

// extractJSONObject returns the first {...} span of s (models often wrap JSON in
// prose or code fences), or s unchanged when no braces are present.
func extractJSONObject(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// --- Reproducibility cache -------------------------------------------------

// CachingVerifier wraps a TemporalVerifier with a disk-persisted verdict cache
// keyed by a hash of (model, kind, dispatch name, caller source, target
// source). Re-runs over unchanged code + model hit the cache, so the pass is
// reproducible (important for CI) and cheap. Errors are never cached.
type CachingVerifier struct {
	inner resolver.TemporalVerifier
	model string
	path  string
	cache map[string]cachedVerdict
	dirty bool
}

type cachedVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// NewCachingVerifier wraps inner, loading any existing cache at path (a missing
// or malformed file is ignored). A "" path disables persistence (in-memory only).
func NewCachingVerifier(inner resolver.TemporalVerifier, model, path string) *CachingVerifier {
	c := &CachingVerifier{inner: inner, model: model, path: path, cache: map[string]cachedVerdict{}}
	c.load()
	return c
}

func (c *CachingVerifier) key(req resolver.TemporalVerifyRequest) string {
	h := sha256.New()
	h.Write([]byte(c.model + "\x00" + req.Kind + "\x00" + req.DispatchName + "\x00" + req.CallerSource + "\x00" + req.TargetSource))
	return hex.EncodeToString(h.Sum(nil))
}

// Verify returns a cached verdict when present, else delegates and caches the
// result. A delegate error is propagated and NOT cached.
func (c *CachingVerifier) Verify(ctx context.Context, req resolver.TemporalVerifyRequest) (resolver.TemporalVerifyResult, error) {
	k := c.key(req)
	if cv, ok := c.cache[k]; ok {
		return resolver.TemporalVerifyResult{Verdict: resolver.TemporalVerdict(cv.Verdict), Reason: cv.Reason}, nil
	}
	res, err := c.inner.Verify(ctx, req)
	if err != nil {
		return res, err
	}
	c.cache[k] = cachedVerdict{Verdict: string(res.Verdict), Reason: res.Reason}
	c.dirty = true
	return res, nil
}

func (c *CachingVerifier) load() {
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var m map[string]cachedVerdict
	if json.Unmarshal(raw, &m) == nil && m != nil {
		c.cache = m
	}
}

// Flush persists the cache to disk when dirty and a path is set. Safe to call
// once after the verification run.
func (c *CachingVerifier) Flush() error {
	if !c.dirty || c.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c.cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, raw, 0o644)
}
