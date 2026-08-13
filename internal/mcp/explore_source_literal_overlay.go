package mcp

import (
	"bufio"
	"context"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zzet/gortex/internal/search/trigram"
)

const (
	exploreSourceLiteralOverlayMaxHits      = 24
	exploreSourceLiteralOverlayMaxBytes     = 4 << 20
	exploreSourceLiteralOverlayMaxLineBytes = 64 << 10
	exploreSourceLiteralOverlayBudget       = 75 * time.Millisecond
)

// exploreSourceLiteralOverlayFile is the request-local form of an editor
// overlay. Path is already in canonical graph-path form and eligible records
// whether the request scope admits the file. Every path, including tombstones
// and out-of-scope replacements, still shadows its durable counterpart.
type exploreSourceLiteralOverlayFile struct {
	path     string
	content  string
	deleted  bool
	eligible bool
}

type exploreSourceLiteralOverlayScan struct {
	matches    []trigram.Match
	covered    map[string]struct{}
	incomplete bool
}

// scanExploreSourceLiteralOverlays searches editor buffers without building an
// overlay graph. It keeps one representative line per file, uses a hard
// request-local byte/time envelope, and reads one sentinel hit beyond the
// caller's limit so truncation is explicit.
func scanExploreSourceLiteralOverlays(
	ctx context.Context,
	term string,
	files []exploreSourceLiteralOverlayFile,
	maxHits int,
) (exploreSourceLiteralOverlayScan, error) {
	return scanExploreSourceLiteralOverlaysWithClock(ctx, term, files, maxHits, time.Now)
}

func scanExploreSourceLiteralOverlaysWithClock(
	ctx context.Context,
	term string,
	files []exploreSourceLiteralOverlayFile,
	maxHits int,
	now func() time.Time,
) (exploreSourceLiteralOverlayScan, error) {
	result := exploreSourceLiteralOverlayScan{
		covered: make(map[string]struct{}, len(files)),
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	term = strings.TrimSpace(term)
	if term == "" || len(files) == 0 {
		return result, nil
	}

	hitLimit := exploreSourceLiteralOverlayMaxHits
	if maxHits > 0 && maxHits < hitLimit {
		hitLimit = maxHits
	}
	ordered := make([]exploreSourceLiteralOverlayFile, 0, len(files))
	for _, file := range files {
		file.path = canonicalExploreSourceLiteralPath(file.path)
		if file.path == "" {
			continue
		}
		result.covered[file.path] = struct{}{}
		if file.eligible && !file.deleted {
			ordered = append(ordered, file)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].path < ordered[j].path
	})

	if now == nil {
		now = time.Now
	}
	deadline := now().Add(exploreSourceLiteralOverlayBudget)
	inputBytes := 0
	for _, file := range ordered {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !now().Before(deadline) {
			result.incomplete = true
			break
		}
		if len(file.content) > exploreSourceLiteralOverlayMaxBytes-inputBytes {
			result.incomplete = true
			break
		}
		inputBytes += len(file.content)

		scanner := bufio.NewScanner(strings.NewReader(file.content))
		scanner.Buffer(make([]byte, 4096), exploreSourceLiteralOverlayMaxLineBytes)
		line := 0
		for scanner.Scan() {
			line++
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if !now().Before(deadline) {
				result.incomplete = true
				break
			}
			text := scanner.Text()
			if !exploreOverlayLiteralHasBoundary(text, term) {
				continue
			}
			result.matches = append(result.matches, trigram.Match{
				Path: file.path,
				Line: line,
				Text: strings.Clone(text),
			})
			break
		}
		if scanner.Err() != nil {
			result.incomplete = true
		}
		if result.incomplete && !now().Before(deadline) {
			break
		}
		if len(result.matches) > hitLimit {
			result.incomplete = true
			result.matches = result.matches[:hitLimit]
			break
		}
	}
	return result, nil
}

// mergeExploreSourceLiteralMatches masks durable rows for every covered
// overlay path, gives overlay rows precedence, then deduplicates and sorts the
// combined cohort before applying the result cap.
func mergeExploreSourceLiteralMatches(
	overlay []trigram.Match,
	durable []trigram.Match,
	covered map[string]struct{},
	maxHits int,
) (matches []trigram.Match, incomplete bool) {
	limit := exploreSourceLiteralOverlayMaxHits
	if maxHits > 0 && maxHits < limit {
		limit = maxHits
	}
	matches = make([]trigram.Match, 0, len(overlay)+len(durable))
	seen := make(map[string]struct{}, len(overlay)+len(durable))
	appendUnique := func(match trigram.Match) {
		match.Path = canonicalExploreSourceLiteralPath(match.Path)
		if match.Path == "" {
			return
		}
		key := match.Path + "\x00" + strconv.Itoa(match.Line)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		match.Text = strings.Clone(match.Text)
		matches = append(matches, match)
	}
	for _, match := range overlay {
		appendUnique(match)
	}
	for _, match := range durable {
		canonical := canonicalExploreSourceLiteralPath(match.Path)
		if _, shadowed := covered[canonical]; shadowed {
			continue
		}
		appendUnique(match)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Path != matches[j].Path {
			return matches[i].Path < matches[j].Path
		}
		if matches[i].Line != matches[j].Line {
			return matches[i].Line < matches[j].Line
		}
		return matches[i].Text < matches[j].Text
	})
	if len(matches) > limit {
		matches = matches[:limit]
		incomplete = true
	}
	return matches, incomplete
}

func canonicalExploreSourceLiteralPath(candidate string) string {
	candidate = strings.TrimSpace(strings.ReplaceAll(candidate, "\\", "/"))
	candidate = strings.TrimPrefix(candidate, "./")
	if candidate == "" || candidate == "." {
		return ""
	}
	return path.Clean(candidate)
}

func exploreOverlayLiteralHasBoundary(text, term string) bool {
	textRunes := []rune(text)
	termRunes := []rune(strings.TrimSpace(term))
	if len(termRunes) == 0 || len(textRunes) < len(termRunes) {
		return false
	}
	for start := 0; start+len(termRunes) <= len(textRunes); start++ {
		matched := true
		for offset := range termRunes {
			if textRunes[start+offset] != termRunes[offset] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		end := start + len(termRunes)
		leftOK := !exploreOverlayIdentifierRune(termRunes[0]) || start == 0 ||
			!exploreOverlayIdentifierRune(textRunes[start-1])
		rightOK := !exploreOverlayIdentifierRune(termRunes[len(termRunes)-1]) || end == len(textRunes) ||
			!exploreOverlayIdentifierRune(textRunes[end])
		if leftOK && rightOK {
			return true
		}
	}
	return false
}

func exploreOverlayIdentifierRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) ||
		unicode.IsMark(r) || unicode.Is(unicode.Pc, r)
}
