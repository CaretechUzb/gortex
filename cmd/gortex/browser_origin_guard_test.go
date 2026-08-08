package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBrowserOriginGuard covers the DNS-rebinding / cross-origin path into
// the daemon's Streamable HTTP surface.
//
// A loopback bind is not private: any page the user opens can reach
// 127.0.0.1, and this surface defaults to no auth token on a loopback bind.
// The transport's own AllowedOrigins mechanism existed but was never wired,
// so every origin was admitted.
func TestBrowserOriginGuard(t *testing.T) {
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	call := func(guard http.Handler, origin string) int {
		reached = false
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Run("no Origin header is allowed", func(t *testing.T) {
		// Ordinary MCP clients are not browsers and never send Origin.
		// Refusing here would break every legitimate caller.
		code := call(browserOriginGuard(inner, nil), "")
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, reached)
	})

	t.Run("a browser origin is refused when none is allowed", func(t *testing.T) {
		code := call(browserOriginGuard(inner, nil), "https://evil.example")
		assert.Equal(t, http.StatusForbidden, code)
		assert.False(t, reached, "the request must not reach the tool surface")
	})

	t.Run("an explicitly allowed origin passes", func(t *testing.T) {
		guard := browserOriginGuard(inner, []string{"https://ui.example"})
		assert.Equal(t, http.StatusOK, call(guard, "https://ui.example"))
		assert.True(t, reached)
	})

	t.Run("the allowlist is exact, and case-insensitive on the origin", func(t *testing.T) {
		guard := browserOriginGuard(inner, []string{"https://ui.example"})
		assert.Equal(t, http.StatusOK, call(guard, "HTTPS://UI.EXAMPLE"))
		assert.Equal(t, http.StatusForbidden, call(guard, "https://ui.example.evil.com"),
			"a suffix match must not be treated as the allowed origin")
		assert.Equal(t, http.StatusForbidden, call(guard, "http://ui.example"),
			"scheme is part of the origin")
	})

	t.Run("blank entries in the allowlist do not admit a missing origin check", func(t *testing.T) {
		guard := browserOriginGuard(inner, []string{"", "   "})
		assert.Equal(t, http.StatusForbidden, call(guard, "https://evil.example"))
	})
}

// TestIsLocalhostBind_RejectsWildcards pins the predicate the eval-server and
// pprof binds now rely on: a wildcard bind is externally reachable and must
// not be treated as loopback.
func TestIsLocalhostBind_RejectsWildcards(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:4747", "localhost:4747", "[::1]:4747", "127.0.0.1"} {
		assert.True(t, isLocalhostBind(addr), "%q is loopback", addr)
	}
	for _, addr := range []string{"0.0.0.0:4747", ":4747", "[::]:4747", "192.168.1.5:4747", "example.com:4747"} {
		assert.False(t, isLocalhostBind(addr), "%q is externally reachable", addr)
	}
}
