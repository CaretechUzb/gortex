package config

import (
	"errors"
	"io/fs"
	"os"
)

// RepoPathMissing reports whether a tracked repo's configured path no
// longer names a directory on disk.
//
// A repo entry outlives the directory it points at: nothing removes the
// entry when the user deletes, renames, or unmounts the checkout, so the
// daemon keeps a registration for a repo that can never be indexed
// again. Every inventory view (`gortex repos`, `gortex daemon status`,
// `gortex status`, `index_health`) calls this so they agree on which
// entries are still real instead of each inferring liveness from a
// different downstream symptom — an empty git HEAD in one view, a
// dropped row in another.
//
// Only a definitive "not there" counts as missing:
//
//   - the path resolves to something that is not a directory (a file
//     replaced the checkout) — missing;
//   - stat reports fs.ErrNotExist — missing;
//   - any other stat error (a permission denial, an unresponsive network
//     mount) — NOT missing. We could not determine the answer, and
//     telling a user their repo was deleted when the truth is "we cannot
//     look right now" is worse than staying quiet.
//
// An empty path counts as missing: an entry with nothing to stat can
// never be indexed either.
func RepoPathMissing(path string) bool {
	if path == "" {
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return !fi.IsDir()
}

// MissingRepoPaths returns the paths of every entry whose directory is
// gone, preserving the input order. Convenience wrapper over
// RepoPathMissing for callers holding a whole registry.
func MissingRepoPaths(entries []RepoEntry) []string {
	var missing []string
	for _, e := range entries {
		if RepoPathMissing(e.Path) {
			missing = append(missing, e.Path)
		}
	}
	return missing
}
