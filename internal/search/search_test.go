package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"getUserById", []string{"get", "user", "by", "id"}},
		{"get_user_by_id", []string{"get", "user", "by", "id"}},
		{"HTMLParser", []string{"html", "parser"}},
		{"internal/auth/token.go", []string{"internal", "auth", "token", "go"}},
		{"UserService.FindUser", []string{"user", "service", "find", "user"}},
		{"validateToken", []string{"validate", "token"}},
		{"A", []string{}}, // too short
		{"", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, Tokenize(tt.input))
		})
	}
}

func TestTokenizeQuery(t *testing.T) {
	tokens := TokenizeQuery("validate token auth")
	assert.Equal(t, []string{"validate", "token", "auth"}, tokens)

	// Keeps short tokens (important for language names like "go")
	tokens = TokenizeQuery("go test")
	assert.Equal(t, []string{"go", "test"}, tokens)
}

// runBackendTests runs the same test suite on any Backend implementation.
func runBackendTests(t *testing.T, name string, backend Backend) {
	// This conformance suite asserts baseline exact-match / no-match
	// semantics shared by every backend. The opt-in sub-word n-gram gate
	// deliberately adds fuzzy recall paths that would turn a "no match"
	// assertion into a match, so pin it off here regardless of any
	// ambient GORTEX_SPARSE_NGRAM — sub-word recall is exercised in the
	// dedicated sparse-ngram tests, not here.
	withSparseNgram(t, false)
	t.Run(name+"/BasicSearch", func(t *testing.T) {
		backend.Add("auth/token.go::validateToken", "validateToken", "auth/token.go")
		backend.Add("auth/token.go::parseJWT", "parseJWT", "auth/token.go")
		backend.Add("api/handler.go::handleRequest", "handleRequest", "api/handler.go")
		backend.Add("db/query.go::buildQuery", "buildQuery", "db/query.go")
		backend.Add("payment/charge.go::chargeCard", "chargeCard", "payment/charge.go")

		results := backend.Search("validate token", 10)
		require.NotEmpty(t, results)
		assert.Equal(t, "auth/token.go::validateToken", results[0].ID)
	})

	t.Run(name+"/CamelCaseSearch", func(t *testing.T) {
		results := backend.Search("handle request", 10)
		require.NotEmpty(t, results)
		assert.Equal(t, "api/handler.go::handleRequest", results[0].ID)
	})

	t.Run(name+"/PathSearch", func(t *testing.T) {
		results := backend.Search("payment", 10)
		require.NotEmpty(t, results)
		assert.Equal(t, "payment/charge.go::chargeCard", results[0].ID)
	})

	t.Run(name+"/Remove", func(t *testing.T) {
		backend.Add("tmp.go::tempFunc", "tempFunc", "tmp.go")
		results := backend.Search("temp func", 10)
		require.NotEmpty(t, results)

		backend.Remove("tmp.go::tempFunc")
		results = backend.Search("temp func", 10)
		// Should not find the removed symbol.
		for _, r := range results {
			assert.NotEqual(t, "tmp.go::tempFunc", r.ID)
		}
	})

	t.Run(name+"/Count", func(t *testing.T) {
		assert.Equal(t, 5, backend.Count())
	})

	t.Run(name+"/EmptyQuery", func(t *testing.T) {
		results := backend.Search("", 10)
		assert.Empty(t, results)
	})

	t.Run(name+"/NoMatch", func(t *testing.T) {
		results := backend.Search("xyznonexistent", 10)
		assert.Empty(t, results)
	})
}

func TestBM25Backend(t *testing.T) {
	backend := NewBM25()
	defer backend.Close()
	runBackendTests(t, "BM25", backend)
}

func TestBM25_RankingQuality(t *testing.T) {
	b := NewBM25()
	defer b.Close()

	// Add symbols with varying relevance to "auth token"
	b.Add("auth/token.go::validateToken", "validateToken", "auth/token.go", "func validateToken(token string) bool")
	b.Add("auth/session.go::createSession", "createSession", "auth/session.go", "func createSession(userID string) Session")
	b.Add("config/config.go::loadConfig", "loadConfig", "config/config.go", "func loadConfig(path string) Config")
	b.Add("api/handler.go::tokenHandler", "tokenHandler", "api/handler.go", "func tokenHandler(w http.ResponseWriter)")

	results := b.Search("auth token", 10)
	// OR ranking: every doc matching any query token is scored.
	// validateToken has both terms so it ranks first; createSession
	// and tokenHandler each match one; loadConfig matches neither
	// and is dropped.
	require.Len(t, results, 3)
	assert.Equal(t, "auth/token.go::validateToken", results[0].ID)
}

func TestBM25_DuplicateTokensCollapse(t *testing.T) {
	b := NewBM25()
	defer b.Close()
	b.Add("a", "token token", "a.go")
	b.Add("b", "token parser", "b.go")

	results := b.Search("token token token", 10)
	require.Len(t, results, 2)
}

func BenchmarkBM25_Search(b *testing.B) {
	backend := NewBM25()
	for i := 0; i < 10000; i++ {
		backend.Add(
			"pkg/file.go::func"+string(rune('A'+i%26))+string(rune('0'+i%10)),
			"getUserById", "internal/auth/service.go", "func getUserById(id string) User",
		)
	}
	b.ResetTimer()
	for b.Loop() {
		backend.Search("get user auth", 20)
	}
}
