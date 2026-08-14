package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
