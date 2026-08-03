package config

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// maxGitignoreChainDepth bounds the walk from the tracked root up towards
// the git root. A tracked root nested deeper than this below its repository
// is pathological; rather than keep statting, the walk gives up and falls
// back to the tracked root's own `.gitignore`.
const maxGitignoreChainDepth = 64

// gitignoreLayer is one `.gitignore` file that git would apply to content
// under the tracked root.
//
//   - dir is the directory holding the file.
//   - sub is the tracked root's path relative to dir, slash-separated and
//     empty for the tracked root's own file. Patterns from an ancestor file
//     are re-anchored through sub before they join the merged list.
//   - stat is the staleness key for dir/.gitignore.
type gitignoreLayer struct {
	dir  string
	sub  string
	stat gitignoreStat
}

func (l gitignoreLayer) equal(o gitignoreLayer) bool {
	return l.dir == o.dir && l.sub == o.sub && l.stat.equal(o.stat)
}

// equalGitignoreChains compares two chains element-wise. A layer appearing,
// disappearing, or changing content all invalidate the merged exclude list.
func equalGitignoreChains(a, b []gitignoreLayer) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].equal(b[i]) {
			return false
		}
	}
	return true
}

// gitignoreChainShape returns the directories whose `.gitignore` git would
// apply to paths under repoPath, outermost first — the order the merged
// matcher needs, since the last matching pattern wins and git lets a deeper
// file override a shallower one. Stat keys are left unset; stampGitignore
// Stats fills them.
//
// Git applies ignore files from the repository root downward. Gortex is
// routinely pointed at a subdirectory of a repository (the monorepo case:
// git root at `repo/`, tracked root at `repo/projects/App`), so reading
// only the tracked root's own file silently drops the entire ignore layer
// the user wrote. The walk therefore climbs to the nearest enclosing `.git`
// and includes every directory on the way down.
//
// When repoPath is not inside a git repository at all, only its own
// `.gitignore` applies — ancestor files outside a repository are not
// something git would honour either.
func gitignoreChainShape(repoPath string) []gitignoreLayer {
	if repoPath == "" {
		return nil
	}
	self := gitignoreLayer{dir: repoPath}
	ancestors := gitRootAncestors(repoPath)
	if len(ancestors) == 0 {
		return []gitignoreLayer{self}
	}
	out := make([]gitignoreLayer, 0, len(ancestors)+1)
	for _, dir := range ancestors {
		sub, err := filepath.Rel(dir, repoPath)
		if err != nil {
			continue
		}
		out = append(out, gitignoreLayer{dir: dir, sub: filepath.ToSlash(sub)})
	}
	return append(out, self)
}

// stampGitignoreStats returns a copy of shape with each layer's staleness
// key read fresh from disk. The shape is discovered once per repo path and
// reused; the stats are what every call re-reads, so an edit to any file in
// the chain is picked up immediately.
func stampGitignoreStats(shape []gitignoreLayer) []gitignoreLayer {
	if len(shape) == 0 {
		return nil
	}
	out := make([]gitignoreLayer, len(shape))
	copy(out, shape)
	for i := range out {
		out[i].stat = statGitignore(out[i].dir)
	}
	return out
}

// gitignoreChain discovers the chain and stats it in one step. Callers on
// the hot path go through excludeCache.chain instead, which reuses the
// discovered shape.
func gitignoreChain(repoPath string) []gitignoreLayer {
	return stampGitignoreStats(gitignoreChainShape(repoPath))
}

// gitRootAncestors returns the directories from the git root down to (but
// excluding) repoPath, outermost first. It returns nil when repoPath is
// itself the git root, when no `.git` is found above it, or when the walk
// exceeds maxGitignoreChainDepth.
//
// Only `.git`'s presence is tested, never its type: it is a directory in a
// normal clone and a file in a linked worktree or submodule, and both mark
// a root whose ignore files apply downward.
func gitRootAncestors(repoPath string) []string {
	if isGitRoot(repoPath) {
		return nil
	}
	var up []string
	dir := repoPath
	for depth := 0; depth < maxGitignoreChainDepth; depth++ {
		parent := filepath.Dir(dir)
		if parent == dir || parent == "" {
			return nil // filesystem root: repoPath is not inside a repository
		}
		up = append(up, parent)
		if isGitRoot(parent) {
			for i, j := 0, len(up)-1; i < j; i, j = i+1, j-1 {
				up[i], up[j] = up[j], up[i]
			}
			return up
		}
		dir = parent
	}
	return nil
}

func isGitRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// reanchorGitignore rewrites the patterns of a `.gitignore` that sits above
// the tracked root so they carry the same meaning when matched against
// tracked-root-relative paths. sub is the tracked root's path relative to
// the file's directory; an empty sub (the tracked root's own file) returns
// the patterns untouched.
func reanchorGitignore(patterns []string, sub string) []string {
	subSegs := splitPathSegments(sub)
	if len(subSegs) == 0 {
		return patterns
	}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, reanchorPattern(p, subSegs)...)
	}
	return out
}

// reanchorPattern translates one ancestor pattern into the equivalent
// pattern(s) relative to the tracked root, returning nothing when the
// pattern cannot describe anything inside it.
//
// Two cases, mirroring git's own split:
//
//   - A pattern with no separator (beyond a trailing one) is unanchored:
//     git matches it at every depth below the file's directory, which
//     includes every depth below the tracked root. It carries over verbatim.
//   - A pattern with a separator is anchored to the file's directory, so it
//     only survives if it can consume the whole of sub first; what remains
//     is re-anchored at the tracked root.
//
// A pattern that swallows the tracked root itself (`/projects/app/`, or an
// ancestor of it) is dropped rather than turned into a blanket exclude: the
// user asked Gortex to track that directory explicitly, and honouring the
// ignore would empty the index of the very repo they pointed at.
func reanchorPattern(pattern string, subSegs []string) []string {
	body := strings.TrimSpace(pattern)
	if body == "" || strings.HasPrefix(body, "#") {
		return nil
	}
	negated := strings.HasPrefix(body, "!")
	if negated {
		body = body[1:]
	}
	dirOnly := strings.HasSuffix(body, "/")
	body = strings.TrimSuffix(body, "/")
	if body == "" {
		return nil
	}
	if !strings.Contains(body, "/") {
		return []string{pattern}
	}

	segs := splitPathSegments(body)
	if len(segs) == 0 {
		return nil
	}
	var out []string
	for _, rem := range anchoredRemainders(segs, subSegs) {
		p := strings.Join(rem, "/")
		// A remainder still led by "**" already means "at any depth"; the
		// rest stay anchored at the tracked root with a leading slash.
		if rem[0] != "**" {
			p = "/" + p
		}
		if dirOnly {
			p += "/"
		}
		if negated {
			p = "!" + p
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// anchoredRemainders returns every suffix of pat that is left to match
// beneath the tracked root after pat's leading segments have consumed the
// whole of sub. Multiple suffixes can qualify because "**" matches zero or
// more segments and may keep matching below the tracked root.
//
// reach[i][j] means "pat[:i] can consume sub[:j]", with "**" modelled as a
// self-loop that stays at i while advancing j. Every transition increases
// i+j, so a single ascending sweep settles it — and bounds a pattern packed
// with "**" to O(len(pat)*len(sub)) work instead of exponential branching.
func anchoredRemainders(pat, sub []string) [][]string {
	reach := make([][]bool, len(pat)+1)
	for i := range reach {
		reach[i] = make([]bool, len(sub)+1)
	}
	reach[0][0] = true
	for i := 0; i < len(pat); i++ {
		for j := 0; j <= len(sub); j++ {
			if !reach[i][j] {
				continue
			}
			if pat[i] == "**" {
				reach[i+1][j] = true
				if j < len(sub) {
					reach[i][j+1] = true
				}
				continue
			}
			if j < len(sub) && segmentMatches(pat[i], sub[j]) {
				reach[i+1][j+1] = true
			}
		}
	}
	var out [][]string
	// i == len(pat) is deliberately excluded: an exhausted pattern means it
	// named the tracked root (or an ancestor) rather than something inside it.
	for i := 0; i < len(pat); i++ {
		if !reach[i][len(sub)] {
			continue
		}
		// pat[i:] led by "**" already matches at any depth, so the suffix
		// starting one segment later says nothing new. Emitting both would
		// only pad the merged list.
		if i > 0 && pat[i-1] == "**" && reach[i-1][len(sub)] {
			continue
		}
		out = append(out, pat[i:])
	}
	return out
}

// segmentMatches reports whether one gitignore path segment matches one
// concrete directory name. path.Match carries the glob semantics gitignore
// uses within a segment ('*' and '?' never cross a separator, '[...]'
// classes); a malformed class is treated as matching nothing, the same way
// git's own matcher fails closed.
func segmentMatches(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func splitPathSegments(p string) []string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if p == "" || p == "." {
		return nil
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}
