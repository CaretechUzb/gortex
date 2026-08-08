package gitcmd

import (
	"fmt"
	"strings"
)

// ValidateRef rejects a caller-supplied git revision that git would read as
// an OPTION rather than as a ref.
//
// Every git subcommand this codebase runs takes the revision as a positional
// argv element — `git log <rev>`, `git blame -p <rev>`, `git diff
// <base>...HEAD`. argv carries no quoting, so a value beginning with "-" is
// not a ref at all: git parses it as a flag. The diff family — diff, log,
// blame, show — accepts `--output=<path>`, which redirects the command's
// output into a file of the caller's choosing. Refs reach these call sites
// from MCP tool arguments (`base`, `base_ref`, `branch`, `rev`), so they are
// attacker-controlled whenever the agent driving the session is.
//
// Measured reach, so this comment does not overstate it. The diff path builds
// `<ref>...HEAD` as ONE argv token, so the file git writes carries a literal
// "...HEAD" suffix: an existing file is created beside, not truncated. That
// is arbitrary file CREATION, in a directory that already exists, carrying
// the real diff output — not arbitrary overwrite. The bare-token sites
// (`git log <ref>`, `git blame -p <ref>`) have no such suffix and do
// truncate. Git-level options like --git-dir and --work-tree are NOT
// reachable from here: git accepts those only before the subcommand, and
// every call site places the caller's token after it.
//
// The rule is a denylist, not an allowlist, on purpose: legitimate revision
// expressions are far broader than ref names — `HEAD~3`, `origin/main`,
// `a...b`, `v1.2^{commit}`, `HEAD:path/to/file` are all valid — and an
// allowlist tight enough to be safe would reject real input. What actually
// carries the vulnerability is the leading "-", so that is what is refused.
// Whitespace and control characters are refused alongside it: no legal
// revision contains them, and they corrupt the error strings and logs the
// argv is echoed into.
//
// An empty ref is valid. Every call site treats "" as "omit this argument"
// and never places it in argv; rejecting it here would turn the documented
// default (HEAD, or the configured default branch) into an error.
//
// Deliberately NOT solved with `--end-of-options`: it is git >= 2.24 only,
// its accepted position differs per subcommand (git refuses it after a
// positional argument), and it would leave the same value flowing into the
// forge and blame paths that build argv differently. Rejecting the input is
// one rule that holds everywhere.
func ValidateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid git revision %q: must not begin with '-' (git would read it as an option, not a ref)", ref)
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\t' {
			return fmt.Errorf("invalid git revision %q: contains whitespace or a control character", ref)
		}
	}
	return nil
}

// SafeRef returns ref when ValidateRef accepts it and "" otherwise. For the
// call sites that cannot surface an error and already treat "" as "use the
// default" — dropping a hostile ref there degrades to the documented
// default instead of running it.
func SafeRef(ref string) string {
	if ValidateRef(ref) != nil {
		return ""
	}
	return ref
}
