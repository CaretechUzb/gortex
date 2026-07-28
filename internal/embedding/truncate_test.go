package embedding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gomlx/go-huggingface/tokenizers/api"
)

// tinyWordPieceTokenizer is a minimal handcrafted BGE-style WordPiece tokenizer.json
// (lowercasing normalizer, whitespace pre-tokenizer, [CLS]/[SEP] post-processing,
// and a small vocab). It covers one-token words plus ASCII and multibyte subwords,
// making truncation cut points deterministic without a model download.
const tinyWordPieceTokenizer = `{
  "version": "1.0",
  "truncation": null,
  "padding": null,
  "added_tokens": [
    {"id": 0, "content": "[PAD]", "single_word": false, "lstrip": false, "rstrip": false, "normalized": false, "special": true},
    {"id": 100, "content": "[UNK]", "single_word": false, "lstrip": false, "rstrip": false, "normalized": false, "special": true},
    {"id": 101, "content": "[CLS]", "single_word": false, "lstrip": false, "rstrip": false, "normalized": false, "special": true},
    {"id": 102, "content": "[SEP]", "single_word": false, "lstrip": false, "rstrip": false, "normalized": false, "special": true}
  ],
  "normalizer": {"type": "BertNormalizer", "lowercase": true},
  "pre_tokenizer": {"type": "BertPreTokenizer"},
  "post_processor": {
    "type": "TemplateProcessing",
    "single": [
      {"SpecialToken": {"id": "[CLS]", "type_id": 0}},
      {"Sequence": {"id": "A", "type_id": 0}},
      {"SpecialToken": {"id": "[SEP]", "type_id": 0}}
    ],
    "pair": [
      {"SpecialToken": {"id": "[CLS]", "type_id": 0}},
      {"Sequence": {"id": "A", "type_id": 0}},
      {"SpecialToken": {"id": "[SEP]", "type_id": 0}},
      {"Sequence": {"id": "B", "type_id": 1}},
      {"SpecialToken": {"id": "[SEP]", "type_id": 1}}
    ],
    "special_tokens": {
      "[CLS]": {"id": "[CLS]", "ids": [101], "tokens": ["[CLS]"]},
      "[SEP]": {"id": "[SEP]", "ids": [102], "tokens": ["[SEP]"]}
    }
  },
  "decoder": {"type": "WordPiece", "prefix": "##"},
  "model": {
    "type": "WordPiece",
    "unk_token": "[UNK]",
    "continuing_subword_prefix": "##",
    "max_input_chars_per_word": 100,
    "vocab": {
      "[PAD]": 0,
      "hello": 1,
      "world": 2,
      "test": 3,
      "[UNK]": 100,
      "[CLS]": 101,
      "[SEP]": 102,
      "the": 104,
      "a": 105,
      "is": 106,
      "this": 107,
      "##hello": 108,
      "привет": 109,
      "##мир": 110
    }
  }
}`

// writeModelDir creates a temp model directory containing tokenizer.json and,
// optionally, a config.json declaring max_position_embeddings.
func writeModelDir(t *testing.T, maxPos int) string {
	return writeModelDirWithTokenizer(t, tinyWordPieceTokenizer, maxPos)
}

func writeModelDirWithTokenizer(t *testing.T, tokenizer string, maxPos int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte(tokenizer), 0o644); err != nil {
		t.Fatal(err)
	}
	if maxPos > 0 {
		cfg := fmt.Sprintf(`{"max_position_embeddings": %d}`, maxPos)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNewTokenTruncator_BudgetFromConfig(t *testing.T) {
	// max_position_embeddings is the total sequence budget, including specials.
	dir := writeModelDir(t, 7)
	tr, err := newTokenTruncator(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.tk == nil {
		t.Fatal("expected a real tokenizer to load")
	}
	if tr.budget != 7 {
		t.Fatalf("budget = %d, want 7", tr.budget)
	}
	if tr.asciiFastBudget != 5 {
		t.Fatalf("ASCII fast budget = %d, want 5", tr.asciiFastBudget)
	}
}

func TestNewTokenTruncator_DisablesFastPathForDuplicatedSequenceTemplate(t *testing.T) {
	const sequenceItem = `{"Sequence": {"id": "A", "type_id": 0}}`
	duplicated := strings.Replace(
		tinyWordPieceTokenizer,
		sequenceItem,
		sequenceItem+", "+sequenceItem,
		1,
	)
	if duplicated == tinyWordPieceTokenizer {
		t.Fatal("test precondition: sequence template was not duplicated")
	}
	dir := writeModelDirWithTokenizer(t, duplicated, 7)
	tr, err := newTokenTruncator(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tr.asciiFastBudget != -1 {
		t.Fatalf("ASCII fast budget = %d, want disabled (-1)", tr.asciiFastBudget)
	}

	const input = "a a a" // duplicated A: six content tokens plus [CLS]/[SEP]
	if got := len(tr.tk.EncodeWithAnnotations(input).IDs); got != 8 {
		t.Fatalf("test precondition: duplicated template encodes to %d tokens, want 8", got)
	}
	if got := len(tr.tk.EncodeWithAnnotations(tr.Truncate(input)).IDs); got > tr.budget {
		t.Fatalf("truncated duplicated-template input encodes to %d tokens, over budget %d", got, tr.budget)
	}
}

func TestNewTokenTruncator_MissingConfigFallback(t *testing.T) {
	dir := writeModelDir(t, 0) // no config.json
	tr, err := newTokenTruncator(dir)
	if err == nil {
		t.Fatal("expected an informational error about the fallback budget")
	}
	if tr.tk == nil {
		t.Fatal("tokenizer must still load even when config.json is missing")
	}
	if tr.budget != fallbackTokenBudget {
		t.Fatalf("budget = %d, want fallback %d", tr.budget, fallbackTokenBudget)
	}
}

func TestTokenTruncator_ShortTextUntouched(t *testing.T) {
	dir := writeModelDir(t, 7) // budget 5
	tr, err := newTokenTruncator(dir)
	if err != nil {
		t.Fatal(err)
	}
	// One content token plus [CLS]/[SEP] fits the total budget.
	if got := tr.Truncate("hello"); got != "hello" {
		t.Fatalf("Truncate(hello) = %q, want unchanged", got)
	}
	// Two content tokens plus [CLS]/[SEP] also fit.
	if got := tr.Truncate("hello world"); got != "hello world" {
		t.Fatalf("Truncate(hello world) = %q, want unchanged", got)
	}
}

func TestTokenTruncator_OverBudgetCutAtSpanBoundary(t *testing.T) {
	dir := writeModelDir(t, 7) // five content tokens plus [CLS]/[SEP]
	tr, err := newTokenTruncator(dir)
	if err != nil {
		t.Fatal(err)
	}
	const input = "hello world test the a is this" // 7 in-vocab words → 7 tokens
	if n := len(tr.tk.EncodeWithAnnotations(input).IDs); n <= tr.budget {
		t.Fatalf("test precondition: input encodes to %d tokens, need > budget %d", n, tr.budget)
	}
	got := tr.Truncate(input)
	if got != "hello world test the a" {
		t.Fatalf("Truncate cut = %q, want %q", got, "hello world test the a")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated text is not valid UTF-8: %q", got)
	}
	if n := len(tr.tk.EncodeWithAnnotations(got).IDs); n > tr.budget {
		t.Fatalf("truncated text still encodes to %d tokens, over budget %d", n, tr.budget)
	}
}

func TestTokenTruncator_MultibyteSubwordBoundary(t *testing.T) {
	dir := writeModelDir(t, 7) // five content tokens plus [CLS]/[SEP]
	tr, err := newTokenTruncator(dir)
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Repeat("приветмир ", 3) // six content tokens plus specials
	if n := len(tr.tk.EncodeWithAnnotations(input).IDs); n <= tr.budget {
		t.Fatalf("test precondition: input encodes to %d tokens, need > budget %d", n, tr.budget)
	}
	got := tr.Truncate(input)
	if got == input {
		t.Fatal("over-budget multibyte input was not truncated")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated text is not valid UTF-8: %q", got)
	}
	if n := len(tr.tk.EncodeWithAnnotations(got).IDs); n > tr.budget {
		t.Fatalf("truncated text still encodes to %d tokens, over budget %d", n, tr.budget)
	}
}

// TestTokenTruncator_TruncateAll covers the batch API actually wired into every
// provider's EmbedBatch: over-budget entries are shortened, in-budget entries are
// returned untouched, and an all-fits batch returns the input slice unmodified.
func TestTokenTruncator_TruncateAll(t *testing.T) {
	dir := writeModelDir(t, 7) // five content tokens plus [CLS]/[SEP]
	tr, err := newTokenTruncator(dir)
	if err != nil {
		t.Fatal(err)
	}
	const over = "hello world test the a is this" // 7 tokens > budget 5

	out := tr.TruncateAll([]string{over, "hello", "hello world"})
	if len(out) != 3 {
		t.Fatalf("got %d results, want 3", len(out))
	}
	if len(tr.tk.EncodeWithAnnotations(out[0]).IDs) > tr.budget {
		t.Fatalf("over-budget entry not truncated: %q", out[0])
	}
	if out[1] != "hello" || out[2] != "hello world" {
		t.Fatalf("in-budget entries changed: %q, %q", out[1], out[2])
	}

	// Nothing over budget -> the exact same slice is returned (no allocation).
	fits := []string{"hello", "world"}
	if got := tr.TruncateAll(fits); &got[0] != &fits[0] {
		t.Fatal("TruncateAll should return the input slice when nothing needs truncating")
	}
}

func TestTokenTruncator_BGEStyleBatchBoundary(t *testing.T) {
	dir := writeModelDir(t, 512)
	tr, err := newTokenTruncator(dir)
	if err != nil {
		t.Fatal(err)
	}

	// This mirrors #255 exactly: 512 content tokens become a 514-wide sequence
	// after BGE adds [CLS]/[SEP].
	offender := strings.Repeat("a ", 512)
	if got := len(tr.tk.EncodeWithAnnotations(offender).IDs); got != 514 {
		t.Fatalf("test precondition: got %d pipeline tokens, want 514", got)
	}

	texts := make([]string, 500)
	for i := range texts {
		texts[i] = "short input"
	}
	texts[len(texts)-1] = offender

	out := tr.TruncateAll(texts)
	if len(out) != len(texts) {
		t.Fatalf("got %d texts, want %d", len(out), len(texts))
	}
	for i, text := range out {
		if got := len(tr.tk.EncodeWithAnnotations(text).IDs); got > tr.budget {
			t.Fatalf("text %d still encodes to %d tokens, over budget %d", i, got, tr.budget)
		}
	}
	if got := len(tr.tk.EncodeWithAnnotations(out[len(out)-1]).IDs); got > 512 {
		t.Fatalf("offender encodes to %d tokens after truncation, want at most 512", got)
	}
	if out[0] != "short input" {
		t.Fatalf("in-budget batch member changed: %q", out[0])
	}
}

// driftingTokenizer models the offset behavior that exposed #255 in the
// reporter's repository: the first 510-token cut retains a complete subword group,
// so its prefix re-tokenizes to the same 514-wide sequence. A second exact check is
// required before inference.
type driftingTokenizer struct{}

func (driftingTokenizer) EncodeWithAnnotations(text string) api.AnnotatedEncoding {
	contentTokens := len(text)
	if len(text) == 513 {
		// Model a trailing byte that emits no token: 512 content tokens plus two
		// specials is the exact [514] sequence from the issue.
		contentTokens = 512
	}
	n := contentTokens + 2
	enc := api.AnnotatedEncoding{
		IDs:               make([]int, n),
		Spans:             make([]api.TokenSpan, n),
		SpecialTokensMask: make([]int, n),
	}
	enc.SpecialTokensMask[0] = 1
	enc.SpecialTokensMask[n-1] = 1
	for i := 0; i < contentTokens; i++ {
		enc.Spans[i+1] = api.TokenSpan{Start: i, End: i + 1}
	}
	if len(text) == 513 {
		// The budget-th content token shares the end of the complete subword
		// group, so a one-pass span cut keeps all 512 content tokens.
		for i := 510; i <= 512; i++ {
			enc.Spans[i].End = 512
		}
	}
	return enc
}

func TestTokenTruncator_RetokenizesDriftingBatchBoundary(t *testing.T) {
	tr := &tokenTruncator{
		tk:              driftingTokenizer{},
		budget:          512,
		asciiFastBudget: -1,
	}
	offender := strings.Repeat("x", 513)
	encoded := tr.tk.EncodeWithAnnotations(offender)
	if got := len(encoded.IDs); got != 514 {
		t.Fatalf("test precondition: got %d pipeline tokens, want 514", got)
	}
	onePassCut := tokenPrefixCut(
		encoded.Spans,
		encoded.SpecialTokensMask,
		tr.budget,
		len(offender),
	)
	if got := len(tr.tk.EncodeWithAnnotations(offender[:onePassCut]).IDs); got != 514 {
		t.Fatalf("test precondition: one-pass cut encodes to %d tokens, want reported shape 514", got)
	}

	texts := make([]string, 500)
	for i := range texts {
		texts[i] = "short"
	}
	texts[len(texts)-1] = offender
	out := tr.TruncateAll(texts)
	for i, text := range out {
		if got := len(tr.tk.EncodeWithAnnotations(text).IDs); got > tr.budget {
			t.Fatalf("text %d still encodes to %d tokens, over budget %d", i, got, tr.budget)
		}
	}
}

func TestTokenPrefixCut_MalformedAnnotationsUseBoundedFallback(t *testing.T) {
	spans := []api.TokenSpan{{Start: 0, End: 1}, {Start: 1, End: 3}}
	if got := tokenPrefixCut(spans, []int{0}, 1, 3); got != 0 {
		t.Fatalf("mismatched annotations returned cut %d, want fallback signal 0", got)
	}

	const input = "aébc"
	cut := geometricPrefixCut(input)
	if cut <= 0 || cut >= len(input) {
		t.Fatalf("geometric cut = %d, want a strict prefix of %d bytes", cut, len(input))
	}
	if !utf8.ValidString(input[:cut]) {
		t.Fatalf("geometric cut split UTF-8: %q", input[:cut])
	}
	if cut > len(input)/2 {
		t.Fatalf("geometric cut = %d, want at most half of %d bytes", cut, len(input))
	}
}

func TestReadTokenBudget(t *testing.T) {
	cases := []struct {
		name       string
		configBody string // "" means no config.json
		want       int
		wantErr    bool
	}{
		{"bert-512", `{"max_position_embeddings": 512}`, 512, false},
		{"jina-8192", `{"max_position_embeddings": 8192}`, 8192, false},
		{"missing", "", fallbackTokenBudget, true},
		{"unparseable", `{not json`, fallbackTokenBudget, true},
		{"absent-field", `{"hidden_size": 384}`, fallbackTokenBudget, true},
		{"implausible", `{"max_position_embeddings": -1}`, fallbackTokenBudget, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.configBody != "" {
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.configBody), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readTokenBudget(dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("budget = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestHugotProvider_LiveTruncatesOverBudgetInput is the end-to-end regression
// for the shape-mismatch crash: an input well past the 512-token window used
// to abort both MiniLM and BGE pipelines. Gated on real cached/downloadable
// models — set GORTEX_TEST_LIVE_MODELS=1.
func TestHugotProvider_LiveTruncatesOverBudgetInput(t *testing.T) {
	if os.Getenv("GORTEX_TEST_LIVE_MODELS") != "1" {
		t.Skip("set GORTEX_TEST_LIVE_MODELS=1 to run against real embedding models")
	}
	for _, variant := range []string{DefaultHugotVariant, "bge_small"} {
		t.Run(variant, func(t *testing.T) {
			prov, err := NewHugotProviderWithVariant(variant)
			if err != nil {
				t.Skipf("%s model unavailable: %v", variant, err)
			}
			defer prov.Close()

			// ~120 repetitions of a 9-token sentence is comfortably past the
			// 512-token window that used to crash the pure-Go tokenizer path.
			over := strings.Repeat("the quick brown fox jumps over the lazy dog ", 120)

			vec, err := prov.Embed(context.Background(), over)
			if err != nil {
				t.Fatalf("embedding an over-budget input failed: %v", err)
			}
			if len(vec) != prov.Dimensions() {
				t.Fatalf("got a %d-dim vector, want %d", len(vec), prov.Dimensions())
			}
			nonZero := false
			for _, v := range vec {
				if v != 0 {
					nonZero = true
					break
				}
			}
			if !nonZero {
				t.Fatal("embedding vector is all zeros")
			}

			// A batch mixing an over-budget input with a short one must also succeed.
			vecs, err := prov.EmbedBatch(context.Background(), []string{over, "short input"})
			if err != nil {
				t.Fatalf("mixed batch failed: %v", err)
			}
			if len(vecs) != 2 || len(vecs[0]) != prov.Dimensions() || len(vecs[1]) != prov.Dimensions() {
				t.Fatalf("mixed batch produced wrong shapes: %d vectors", len(vecs))
			}
		})
	}
}

func TestNewTokenTruncator_RejectsCorruptTokenizer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte("{ not a tokenizer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"max_position_embeddings": 12}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := newTokenTruncator(dir)
	if err == nil {
		t.Fatal("expected an error when tokenizer.json is corrupt")
	}
	if tr != nil {
		t.Fatalf("truncator = %#v, want nil when the exact tokenizer is unavailable", tr)
	}
}
