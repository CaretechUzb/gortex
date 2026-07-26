package savings

import (
	"os"
	"testing"
)

func TestCostAvoided_Known(t *testing.T) {
	// Claude Opus 4.8: $5 / 1M input → 1M tokens saved = $5.
	got := CostAvoided(1_000_000, "claude-opus-4-8")
	if got < 4.99 || got > 5.01 {
		t.Errorf("CostAvoided(1M, claude-opus-4-8) = %.4f, want ≈5.00", got)
	}
	// GPT-4o-mini: $0.15 / 1M → 100k tokens saved = $0.015.
	got = CostAvoided(100_000, "gpt-4o-mini")
	if got < 0.0149 || got > 0.0151 {
		t.Errorf("CostAvoided(100k, gpt-4o-mini) = %.4f, want ≈0.015", got)
	}
	// DeepSeek chat: $0.27 / 1M → 1M tokens saved = $0.27 (cross-provider).
	got = CostAvoided(1_000_000, "deepseek-chat")
	if got < 0.2699 || got > 0.2701 {
		t.Errorf("CostAvoided(1M, deepseek-chat) = %.4f, want ≈0.27", got)
	}
}

func TestCostAvoided_FuzzyMatch(t *testing.T) {
	// Substring matches so "opus" resolves to a claude-opus-4-x row.
	if CostAvoided(1_000_000, "opus") == 0 {
		t.Error("CostAvoided with fuzzy model name 'opus' should resolve to a claude-opus row")
	}
	// Unrelated name → 0.
	if got := CostAvoided(1_000_000, "nonexistent-model"); got != 0 {
		t.Errorf("CostAvoided with unknown model = %.4f, want 0", got)
	}
}

func TestCostAvoided_ZeroOrNegative(t *testing.T) {
	if got := CostAvoided(0, "claude-opus-4"); got != 0 {
		t.Errorf("CostAvoided(0, _) = %.4f, want 0", got)
	}
	if got := CostAvoided(-100, "claude-opus-4"); got != 0 {
		t.Errorf("CostAvoided(-100, _) = %.4f, want 0", got)
	}
}

func TestCostAvoidedAll_IncludesDefaults(t *testing.T) {
	all := CostAvoidedAll(1_000_000)
	wantModels := []string{
		"claude-mythos-preview", "claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5",
		"gpt-5.5", "gpt-5.4", "gpt-4.1", "gpt-4o", "o4-mini",
		"gemini-3.1-pro", "gemini-3.5-flash", "gemini-2.5-pro",
		"deepseek-chat", "deepseek-reasoner",
	}
	for _, m := range wantModels {
		if _, ok := all[m]; !ok {
			t.Errorf("CostAvoidedAll missing model %q", m)
		}
	}
}

func TestPricing_EnvOverride(t *testing.T) {
	t.Setenv("GORTEX_MODEL_PRICING_JSON", `[{"model":"testmodel","usd_per_m_input":100}]`)
	prices := Pricing()
	if len(prices) != 1 || prices[0].Model != "testmodel" || prices[0].USDPerMInput != 100 {
		t.Errorf("env override ignored: %+v", prices)
	}

	// Malformed override falls back to defaults.
	t.Setenv("GORTEX_MODEL_PRICING_JSON", "not json")
	if got := Pricing(); len(got) != len(defaultPricing) {
		t.Errorf("malformed override should fall back to defaults, got %d entries", len(got))
	}
}

func TestPricing_EnvUnsetUsesDefaults(t *testing.T) {
	_ = os.Unsetenv("GORTEX_MODEL_PRICING_JSON")
	if got := Pricing(); len(got) != len(defaultPricing) {
		t.Errorf("unset env should use defaults, got %d", len(got))
	}
}

// TestDefaultPricingCoversCurrentGeneration pins list prices for the model
// generation gortex's own clients actually run on. A missing row here is not
// a cosmetic gap: findPrice returns nil for an unknown id and CostAvoided
// then reports $0.00 next to millions of tokens saved, which reads as "this
// model avoided nothing" rather than "we have no rate for it".
func TestDefaultPricingCoversCurrentGeneration(t *testing.T) {
	// USD per 1M input tokens.
	want := map[string]float64{
		"claude-fable-5":    10.00,
		"claude-mythos-5":   10.00,
		"claude-opus-5":     5.00,
		"claude-opus-4-8":   5.00,
		"claude-sonnet-5":   3.00,
		"claude-sonnet-4-6": 3.00,
		"claude-haiku-4-5":  1.00,
		// OpenAI slugs the codex CLI reports into savings attribution.
		"gpt-5.6-sol":   5.00,
		"gpt-5.6-terra": 2.50,
		"gpt-5.6-luna":  1.00,
	}
	for model, rate := range want {
		got := ModelRate(model)
		if got == 0 {
			t.Errorf("ModelRate(%q) = 0 — model missing from the pricing table, so its "+
				"cost avoided renders as $0.00", model)
			continue
		}
		if got != rate {
			t.Errorf("ModelRate(%q) = %.2f, want %.2f", model, got, rate)
		}
	}
}

// TestPricingExactMatchBeatsSubstring guards the resolution order that keeps
// same-family ids from stealing each other's rate — claude-opus-5 must not
// resolve against claude-opus-4-8, and claude-sonnet-5 must not resolve
// against claude-sonnet-4-6.
func TestPricingExactMatchBeatsSubstring(t *testing.T) {
	if got := CostAvoided(1_000_000, "claude-opus-5"); got != 5.00 {
		t.Errorf("CostAvoided(1M, claude-opus-5) = %.4f, want 5.00", got)
	}
	if got := CostAvoided(1_000_000, "claude-sonnet-5"); got != 3.00 {
		t.Errorf("CostAvoided(1M, claude-sonnet-5) = %.4f, want 3.00", got)
	}
	if got := CostAvoided(1_000_000, "claude-fable-5"); got != 10.00 {
		t.Errorf("CostAvoided(1M, claude-fable-5) = %.4f, want 10.00", got)
	}
}

// TestModelRateReportsUnknownAsZero is the contract formatModelCost relies on
// to tell "unpriced" apart from "avoided nothing".
func TestModelRateReportsUnknownAsZero(t *testing.T) {
	if got := ModelRate("totally-unknown-model-xyz"); got != 0 {
		t.Errorf("ModelRate(unknown) = %.4f, want 0", got)
	}
	if got := ModelRate(""); got != 0 {
		t.Errorf("ModelRate(\"\") = %.4f, want 0", got)
	}
}

// TestPricingGPT56TierDoesNotCollideWithGPT55 guards the substring resolver:
// the gpt-5.6 tiers and gpt-5.5 are distinct rows, and neither may resolve
// against the other. gpt-5.6-sol and gpt-5.5 happen to share a rate, so
// assert on the resolved row rather than only on the dollar figure.
func TestPricingGPT56TierDoesNotCollideWithGPT55(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"gpt-5.6-sol", 5.00},
		{"gpt-5.6-terra", 2.50},
		{"gpt-5.6-luna", 1.00},
		{"gpt-5.5", 5.00},
		{"gpt-5.4", 2.50},
	} {
		if got := ModelRate(tc.model); got != tc.want {
			t.Errorf("ModelRate(%q) = %.2f, want %.2f", tc.model, got, tc.want)
		}
	}
	// The tiers must not be interchangeable — terra and luna are cheaper
	// than sol, so a collision would silently misprice a third of a bill.
	if ModelRate("gpt-5.6-terra") == ModelRate("gpt-5.6-sol") {
		t.Error("gpt-5.6-terra resolved to the same rate as gpt-5.6-sol — substring collision")
	}
}
