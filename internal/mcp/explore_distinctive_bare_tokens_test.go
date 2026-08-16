package mcp

import (
	"reflect"
	"testing"
)

func TestExploreDistinctiveBareTokensMinesIdentifierShapes(t *testing.T) {
	task := "Localize bug: gin-4372 checkout rejects websocket Accept after it calls " +
		"WriteHeaderNow before Hijack. In responseWriter.Hijack, w.Written is true " +
		"when size is 0 and maxRedirects stays unset."
	tokens := exploreDistinctiveBareTokens(task, "gin-4372")
	want := map[string]bool{
		"WriteHeaderNow":       true,
		"responseWriter.Hijack": true,
		"maxRedirects":         true,
	}
	got := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		got[token] = true
	}
	for token := range want {
		if !got[token] {
			t.Fatalf("expected %q among mined tokens: %v", token, tokens)
		}
	}
	if got["gin-4372"] || got["checkout"] || got["websocket"] {
		t.Fatalf("repo name or plain prose leaked into mined tokens: %v", tokens)
	}
	if len(tokens) > exploreDistinctiveBareTokenMaxTerms {
		t.Fatalf("token cap exceeded: %v", tokens)
	}
}

func TestExploreDistinctiveBareTokensEmitsDottedFinalSegment(t *testing.T) {
	tokens := exploreDistinctiveBareTokens(
		"crash inside c.AbortWithStatus during recovery middleware handling", "gin")
	got := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		got[token] = true
	}
	if !got["AbortWithStatus"] {
		t.Fatalf("dotted citation must contribute its final segment: %v", tokens)
	}
}

func TestExploreDistinctiveBareTokensEmptyForPlainProse(t *testing.T) {
	tokens := exploreDistinctiveBareTokens("the report says the build is slow and fails often", "demo")
	if !reflect.DeepEqual(tokens, []string{}) && tokens != nil {
		t.Fatalf("plain prose must mine nothing: %v", tokens)
	}
}
