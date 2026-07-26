// Package modelhint is a best-effort, filesystem-backed bridge that lets
// the host's hook layer tell the (separate-process) gortex daemon which
// LLM model is driving a session.
//
// The MCP protocol never transmits the calling model to a tool server,
// but a Claude Code hook *can* see it (the SessionStart payload carries a
// `model` field; the session transcript records it per assistant turn).
// The hook runs as its own short-lived process, so it can't hand the
// model to the daemon in-memory — it writes a hint here, keyed by the
// session's working directory, and the daemon's savings recorder reads it
// back. Both processes share the filesystem, exactly like the per-session
// state the PreToolUse hook already round-trips through disk.
//
// Everything is best-effort: a missing, stale, or unparseable hint simply
// degrades to "model unknown", and the savings dashboard falls back to its
// provider-neutral estimate. Two coordinates exist:
//
//   - <hash(cwd)>.json — the model active in a given working directory.
//     Precise when concurrent sessions live in different directories.
//   - _last.json — the most recently announced model, the fallback when
//     no per-cwd hint matches. Exact for the common single-session case;
//     ambiguous only when concurrent sessions share one cwd on different
//     models (a rare edge we accept rather than chase).
package modelhint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// dirEnvVar lets tests redirect the hint directory at a t.TempDir(),
// parallel to GORTEX_HOOK_SESSION_DIR for the hook session state.
const dirEnvVar = "GORTEX_MODEL_HINT_DIR"

// ttl bounds how long a hint is trusted. A daemon that outlives the
// agent process shouldn't keep attributing savings to a model from a
// session that ended hours ago, so reads older than ttl are ignored.
const ttl = 12 * time.Hour

// lastFile is the global "most recently announced model" fallback.
const lastFile = "_last.json"

// Hint is one model announcement.
type Hint struct {
	CWD     string `json:"cwd,omitempty"`
	Model   string `json:"model"`
	Client  string `json:"client,omitempty"`
	Updated int64  `json:"updated"` // unix nanoseconds
}

// hintDir returns the directory holding model-hint files, honouring
// GORTEX_MODEL_HINT_DIR. Returns "" when no base dir resolves — every
// caller treats "" as "hints disabled" and degrades gracefully.
func hintDir() string {
	if p := strings.TrimSpace(os.Getenv(dirEnvVar)); p != "" {
		return p
	}
	base := platform.OSCacheDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "modelhints")
}

// cwdFile returns the per-cwd hint file path for an absolute, cleaned
// working directory. The cwd is hashed so an arbitrary path collapses to
// one safe filename segment regardless of separators or length.
func cwdFile(dir, cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")
}

// Write records that `model` (driven by `client`) is active in `cwd`.
// Best-effort and non-blocking: any error is swallowed so a read-only
// cache dir or full disk can never stall the hook that called it. An
// empty model is a no-op — there is nothing to attribute.
func Write(cwd, model, client string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	dir := hintDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	h := Hint{CWD: abs, Model: model, Client: strings.TrimSpace(client), Updated: time.Now().UTC().UnixNano()}
	data, err := json.Marshal(h)
	if err != nil {
		return
	}
	// _last always; the per-cwd file only when we have a cwd to key on.
	writeFileAtomic(filepath.Join(dir, lastFile), data)
	if strings.TrimSpace(cwd) != "" {
		writeFileAtomic(cwdFile(dir, abs), data)
	}
}

// NormalizeClient reduces an agent or MCP client name to a comparable form.
// The two sides of this bridge name the same harness differently: the hook
// layer knows the agent it was invoked for ("codex"), while the MCP session
// knows the client app that connected ("codex-mcp-client"). Comparing them
// raw would reject every legitimate match.
func NormalizeClient(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, suffix := range []string{"-mcp-client", "-mcp", "-client"} {
		n = strings.TrimSuffix(n, suffix)
	}
	return n
}

// SameClient reports whether two agent/client names denote the same harness.
//
// An empty side is "unknown" and cannot contradict the other, so it matches.
// That asymmetry is deliberate: a hint with no recorded client predates the
// field, and rejecting it would silently drop attribution that used to work.
// Two *known* names that disagree, though, are proof the hint belongs to a
// different agent — and adopting it there is how a Codex call ends up priced
// as a Claude model.
func SameClient(a, b string) bool {
	na, nb := NormalizeClient(a), NormalizeClient(b)
	return na == "" || nb == "" || na == nb
}

// Read returns the model hint for `cwd`: the per-cwd announcement when
// one exists and is fresh, otherwise the global most-recent hint. The
// bool is false when no usable hint is found (none written, all stale,
// or hints disabled).
func Read(cwd string) (Hint, bool) {
	dir := hintDir()
	if dir == "" {
		return Hint{}, false
	}
	if strings.TrimSpace(cwd) != "" {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			abs = cwd
		}
		if h, ok := readFresh(cwdFile(dir, abs)); ok {
			return h, true
		}
	}
	return readFresh(filepath.Join(dir, lastFile))
}

// readFresh loads a hint file and returns it only when it parses and is
// within the trust window.
func readFresh(path string) (Hint, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Hint{}, false
	}
	var h Hint
	if err := json.Unmarshal(data, &h); err != nil || h.Model == "" {
		return Hint{}, false
	}
	if h.Updated > 0 && time.Since(time.Unix(0, h.Updated)) > ttl {
		return Hint{}, false
	}
	return h, true
}

// writeFileAtomic writes via a temp file + rename so a concurrent reader
// never observes a torn JSON object. Best-effort: failures are ignored.
func writeFileAtomic(path string, data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hint-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}
