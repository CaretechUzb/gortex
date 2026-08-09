package search

import (
	"testing"
)

// =============================================================================
// Tokenizer benchmarks — hot path for every search query
// =============================================================================

var tokenizeInputs = []struct {
	name  string
	input string
}{
	{"CamelCase", "getUserByIdFromDatabase"},
	{"SnakeCase", "get_user_by_id_from_database"},
	{"DotPath", "internal/mcp/server.go::NewServer"},
	{"ALLCAPS", "HTMLParserFactory"},
	{"Mixed", "parseJSON_toXML.Convert"},
	{"Long", "internal/parser/languages/typescript.go::TypeScriptExtractor.extractClassDeclaration"},
}

func BenchmarkTokenize(b *testing.B) {
	for _, tc := range tokenizeInputs {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Tokenize(tc.input)
			}
		})
	}
}

func BenchmarkTokenizeQuery(b *testing.B) {
	queries := []string{
		"get user auth",
		"Server NewServer handle request",
		"graph add node edge",
		"resolve import cross repo",
	}
	for _, q := range queries {
		b.Run(q, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				TokenizeQuery(q)
			}
		})
	}
}
