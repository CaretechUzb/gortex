//go:build !windows && static_link

package languages

import (
	"errors"
	"unsafe"
)

// errNoDlopen explains why a statically linked build cannot load a custom
// grammar, and what to do about it. RegisterCustomGrammars logs this as a
// warning and skips the grammar, so an unusable `grammars:` entry degrades to
// "that one language isn't indexed" rather than taking the daemon down.
var errNoDlopen = errors.New(
	"custom tree-sitter grammars need dlopen, which the statically linked release binary does not have; " +
		"build gortex from source (go build ./cmd/gortex/) to use them, or use one of the built-in grammars")

// loadGrammarLanguage refuses to load a custom grammar in a static build.
//
// The linux release links -static so it runs on any distro (see
// .goreleaser.yml). glibc's dlopen is the one thing that does not survive that:
// it needs the shared libc that the binary was linked against to be present at
// runtime, and when the host's glibc differs it does not fail cleanly — the
// process SEGVs inside the loader. That would turn an opt-in config entry into
// a daemon crash, so the static build declines up front instead. The dynamic
// implementation is in grammar_load_unix.go and is what every source build
// gets; windows is unaffected because it loads grammars through LoadDLL, an OS
// API rather than a libc one.
func loadGrammarLanguage(_, _ string) (unsafe.Pointer, error) {
	return nil, errNoDlopen
}
