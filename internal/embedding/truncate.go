package embedding

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/gomlx/go-huggingface/tokenizers/api"
	"github.com/gomlx/go-huggingface/tokenizers/hftokenizer"
)

const (
	// fallbackTokenBudget is the total sequence budget used when a model directory
	// has no parseable config.json. It matches the classic BERT/MiniLM context
	// window, including tokenizer-added special tokens.
	fallbackTokenBudget = 512
)

// tokenTruncator caps input texts at a model's positional budget before they reach
// the inference pipeline.
//
// The pure-Go Hugot tokenizer path does not honour max_position_embeddings, so a
// text longer than the model's context window reaches inference at full length and
// produces a tensor shape mismatch that aborts the entire vector index. Truncating
// client-side keeps that failure from ever occurring; because a transformer cannot
// attend past its positional budget, cutting the tail is lossless by construction.
//
// tokenTruncator uses the same post-processing semantics as the inference
// pipeline. budget is the model's total sequence width, including special tokens.
type annotatedTokenizer interface {
	EncodeWithAnnotations(string) api.AnnotatedEncoding
}

type tokenTruncator struct {
	tk              annotatedTokenizer
	budget          int
	asciiFastBudget int // >= 0 only for the known BERT + WordPiece pipeline
}

// newTokenTruncator builds a truncator for the model cached under modelDir. It reads
// <modelDir>/tokenizer.json (the same file Hugot loads) and derives the total
// sequence budget from <modelDir>/config.json's max_position_embeddings.
//
// A missing or malformed config.json degrades to fallbackTokenBudget and returns an
// informational error alongside a usable truncator. Tokenizer failures are fatal:
// without the exact tokenizer there is no safe way to prove an input fits the model.
func newTokenTruncator(modelDir string) (*tokenTruncator, error) {
	budget, budgetErr := readTokenBudget(modelDir)

	tkBytes, err := os.ReadFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("read tokenizer.json: %w", err)
	}
	tk, err := hftokenizer.NewFromContent(nil, tkBytes)
	if err != nil {
		return nil, fmt.Errorf("parse tokenizer.json: %w", err)
	}
	// Match Hugot's inference tokenizer: post-processing is enabled, so [CLS],
	// [SEP], and any model-specific special tokens count against the same total
	// max_position_embeddings budget as the tensor sent to the model.
	if err := tk.With(api.EncodeOptions{
		AddSpecialTokens:         true,
		IncludeSpans:             true,
		IncludeSpecialTokensMask: true,
	}); err != nil {
		return nil, fmt.Errorf("configure tokenizer: %w", err)
	}
	specials := len(tk.EncodeWithAnnotations("").IDs)
	if specials > budget {
		return nil, fmt.Errorf("tokenizer adds %d special tokens, exceeding model budget %d", specials, budget)
	}
	t := &tokenTruncator{tk: tk, budget: budget, asciiFastBudget: -1}
	var tokenizerConfig struct {
		Model struct {
			Type string `json:"type"`
		} `json:"model"`
		Normalizer struct {
			Type string `json:"type"`
		} `json:"normalizer"`
		PreTokenizer struct {
			Type string `json:"type"`
		} `json:"pre_tokenizer"`
		PostProcessor struct {
			Type   string                       `json:"type"`
			Single []map[string]json.RawMessage `json:"single"`
		} `json:"post_processor"`
	}
	if json.Unmarshal(tkBytes, &tokenizerConfig) == nil &&
		tokenizerConfig.Model.Type == "WordPiece" &&
		tokenizerConfig.Normalizer.Type == "BertNormalizer" &&
		tokenizerConfig.PreTokenizer.Type == "BertPreTokenizer" &&
		tokenizerConfig.PostProcessor.Type == "TemplateProcessing" &&
		hasSingleSequenceTemplate(tokenizerConfig.PostProcessor.Single) {
		// This exact pipeline emits at most one WordPiece token per ASCII byte:
		// BERT normalization cannot expand ASCII, and every subword consumes at
		// least one byte. Non-ASCII and other tokenizer pipelines always take the
		// exact path.
		t.asciiFastBudget = budget - specials
	}

	if budgetErr != nil {
		return t, fmt.Errorf("using fallback token budget %d: %w", budget, budgetErr)
	}
	return t, nil
}

func hasSingleSequenceTemplate(items []map[string]json.RawMessage) bool {
	sequences := 0
	for _, item := range items {
		if len(item) != 1 {
			return false
		}
		if sequence, ok := item["Sequence"]; ok {
			var spec struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(sequence, &spec) != nil || spec.ID != "A" {
				return false
			}
			sequences++
			continue
		}
		if _, ok := item["SpecialToken"]; !ok {
			return false
		}
	}
	return sequences == 1
}

// Truncate returns text unchanged when the inference tokenizer produces a sequence
// within the model budget. Otherwise it repeatedly cuts to the longest token prefix
// that can fit, re-tokenizing each candidate until the total sequence (including
// special tokens) satisfies the hard postcondition.
func (t *tokenTruncator) Truncate(text string) string {
	if t == nil || t.tk == nil || t.budget <= 0 || text == "" {
		return text
	}
	if t.asciiFastBudget >= 0 && len(text) <= t.asciiFastBudget && isASCII(text) {
		return text
	}
	for text != "" {
		enc := t.tk.EncodeWithAnnotations(text)
		if len(enc.IDs) <= t.budget {
			return text
		}

		cut := tokenPrefixCut(enc.Spans, enc.SpecialTokensMask, t.budget, len(text))
		if cut <= 0 {
			cut = geometricPrefixCut(text)
		}
		for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return text
}

func isASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// tokenPrefixCut returns the byte end of the longest non-special token prefix
// that can coexist with the encoded sequence's special tokens inside budget. If
// several subwords share a full-word span, it walks back to a strict byte prefix;
// otherwise a one-pass cut could retain the whole word and remain over budget.
func tokenPrefixCut(spans []api.TokenSpan, specialMask []int, budget, textBytes int) int {
	if budget <= 0 || len(spans) == 0 || len(spans) != len(specialMask) {
		return 0
	}
	specials := 0
	for _, special := range specialMask {
		if special != 0 {
			specials++
		}
	}
	contentBudget := budget - specials
	if contentBudget <= 0 {
		return 0
	}
	contentTokens := 0
	target := -1
	for i := range spans {
		if specialMask[i] != 0 {
			continue
		}
		contentTokens++
		if contentTokens == contentBudget {
			target = i
			break
		}
	}
	for i := target; i >= 0; i-- {
		if specialMask[i] == 0 && spans[i].End > 0 && spans[i].End < textBytes {
			return spans[i].End
		}
	}
	return 0
}

// geometricPrefixCut is the bounded defensive path for malformed annotations.
// Halving ensures strict UTF-8-safe progress and at most logarithmically many
// additional tokenizer passes, rather than removing and re-tokenizing one rune at
// a time.
func geometricPrefixCut(text string) int {
	cut := len(text) / 2
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return cut
}

// TruncateAll applies Truncate to every text. It returns the input slice
// unchanged (no allocation) when nothing needed truncating — the common case on
// this hot path — and otherwise a copy with the over-budget entries shortened.
func (t *tokenTruncator) TruncateAll(texts []string) []string {
	if t == nil || t.budget <= 0 {
		return texts
	}
	var out []string
	for i, s := range texts {
		cut := t.Truncate(s)
		if len(cut) == len(s) { // Truncate only ever returns s or a strict prefix.
			continue
		}
		if out == nil {
			out = make([]string, len(texts))
			copy(out, texts)
		}
		out[i] = cut
	}
	if out == nil {
		return texts
	}
	return out
}

// readTokenBudget derives the total sequence budget from
// <modelDir>/config.json's max_position_embeddings. It returns the fallback budget
// with a non-nil error when the file is missing, unparseable, or implausible.
func readTokenBudget(modelDir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(modelDir, "config.json"))
	if err != nil {
		return fallbackTokenBudget, fmt.Errorf("read config.json: %w", err)
	}
	var cfg struct {
		MaxPositionEmbeddings int `json:"max_position_embeddings"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fallbackTokenBudget, fmt.Errorf("parse config.json: %w", err)
	}
	if cfg.MaxPositionEmbeddings <= 0 {
		return fallbackTokenBudget, fmt.Errorf("config.json max_position_embeddings=%d is implausible", cfg.MaxPositionEmbeddings)
	}
	return cfg.MaxPositionEmbeddings, nil
}
