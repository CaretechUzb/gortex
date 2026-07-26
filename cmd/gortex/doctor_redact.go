package main

import (
	"path/filepath"
	"strings"
)

// doctor_redact.go makes --redact mean something. The report is built to be
// pasted into an issue, and almost everything identifying in it is a path:
// the repo name in every planned-file line, the account name in every home
// path. Hashing the two roots keeps the structure legible — `<repo>/.mcp.json`
// still reads as a config path — while removing the parts a user would
// otherwise blur out by hand before sharing.

// doctorPath rewrites one path for display. Replaced for the duration of a
// redacted run; the identity default keeps the normal report verbatim.
var doctorPath = func(p string) string { return p }

// newDoctorPathRedactor rewrites paths under the repo root to <repo>/… and
// paths under the home directory to ~/…. Repo first: the repo usually lives
// under home, and its name is the more sensitive of the two.
func newDoctorPathRedactor(home, root string) func(string) string {
	return func(p string) string {
		if strings.TrimSpace(p) == "" {
			return p
		}
		clean := filepath.Clean(p)
		if rel, ok := relativeTo(root, clean); ok {
			return filepath.Join("<repo>", rel)
		}
		if rel, ok := relativeTo(home, clean); ok {
			return filepath.Join("~", rel)
		}
		// Outside both roots: no name of ours to leak, so leave it readable.
		return clean
	}
}

// relativeTo reports base-relative form when path is inside base. "." maps to
// the empty string so the caller's Join yields the bare placeholder.
func relativeTo(base, path string) (string, bool) {
	if strings.TrimSpace(base) == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(base), path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}
