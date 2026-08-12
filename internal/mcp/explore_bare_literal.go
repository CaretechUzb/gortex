package mcp

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	exploreBareLiteralMaxTaskBytes = 16 << 10
	exploreBareLiteralMaxTerms     = 5
	exploreBareLiteralMinRunes     = 4
	exploreBareLiteralMaxRunes     = 128
	exploreBareDiagnosticMinRunes  = 12
	exploreBareDiagnosticMinWords  = 2
	exploreBareDiagnosticMaxWords  = 6
)

var exploreBareLiteralWordRE = regexp.MustCompile(`[\pL\pN_][\pL\pN_'-]*`)

// exploreBareLiteralRecallTerms mines only high-signal source values the
// requester wrote without quotes. It is intentionally separate from quoted
// recall: these inferred terms aid retrieval but never become authored claims
// that can hold localization open.
func exploreBareLiteralRecallTerms(task string) []string {
	task = exploreBareLiteralBoundedTask(task)
	if strings.TrimSpace(task) == "" {
		return nil
	}

	lines := exploreBareLiteralProseLines(task)
	anchors := exploreSyntacticAnchors(task)
	diagnostics := make([]string, 0, exploreBareLiteralMaxTerms)
	structured := make([]string, 0, exploreBareLiteralMaxTerms)
	plain := make([]string, 0, exploreBareLiteralMaxTerms)
	for _, line := range lines {
		if phrase := exploreBareDiagnosticPhrase(line); phrase != "" {
			diagnostics = append(diagnostics, phrase)
		}
		for _, raw := range exploreUnquotedCodeTokens(line) {
			term := strings.Trim(raw, "-_.:()[]{}<>,;'\"")
			if !exploreBareAllCapsTerm(term) || exploreBareLiteralRepresentedByAnchor(term, anchors) {
				continue
			}
			if strings.Contains(term, "-") {
				if !exploreBareStructuredCue(line) {
					continue
				}
				structured = append(structured, term)
				continue
			}
			if strings.Contains(term, "_") {
				structured = append(structured, term)
				continue
			}
			plain = append(plain, term)
		}
	}

	seen := make(map[string]struct{}, exploreBareLiteralMaxTerms)
	out := make([]string, 0, exploreBareLiteralMaxTerms)
	for _, lane := range [3][]string{diagnostics, structured, plain} {
		for _, term := range lane {
			term = strings.TrimSpace(term)
			key := strings.ToLower(term)
			if term == "" {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, term)
			if len(out) == exploreBareLiteralMaxTerms {
				return out
			}
		}
	}
	return out
}

func exploreBareLiteralBoundedTask(task string) string {
	if len(task) <= exploreBareLiteralMaxTaskBytes {
		return task
	}
	task = task[:exploreBareLiteralMaxTaskBytes]
	for task != "" && !utf8.ValidString(task) {
		task = task[:len(task)-1]
	}
	return task
}

// exploreBareLiteralProseLines drops code blocks and masks explicit literals.
// Those inputs already have their own bounded lanes and must not consume this
// lane a second time.
func exploreBareLiteralProseLines(task string) []string {
	lines := make([]string, 0, strings.Count(task, "\n")+1)
	fenced := false
	for _, raw := range strings.Split(task, "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || exploreIndentedCodeLine(raw) {
			continue
		}
		if line := strings.TrimSpace(exploreBareMaskQuotedSpans(raw)); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func exploreBareMaskQuotedSpans(text string) string {
	var masked strings.Builder
	masked.Grow(len(text))
	var quote rune
	escaped := false
	for _, r := range text {
		if quote != 0 {
			if escaped {
				escaped = false
				masked.WriteRune(' ')
				continue
			}
			if r == '\\' && quote == '"' {
				escaped = true
				masked.WriteRune(' ')
				continue
			}
			if r == quote {
				quote = 0
			}
			masked.WriteRune(' ')
			continue
		}
		if r == '"' || r == '`' {
			quote = r
			masked.WriteRune(' ')
			continue
		}
		masked.WriteRune(r)
	}
	return masked.String()
}

func exploreBareDiagnosticPhrase(line string) string {
	lower := strings.ToLower(line)
	start := -1
	for _, cue := range []string{
		"fails with", "error:", "message:", "reports", "returns", "raises", "throws", "emits", "prints", "says", "logs",
	} {
		if at := exploreBareCueIndex(lower, cue); at >= 0 {
			candidateStart := at + len(cue)
			if start < 0 || candidateStart < start {
				start = candidateStart
			}
		}
	}
	if start < 0 || start >= len(line) {
		return ""
	}
	tail := strings.TrimLeft(line[start:], " \t:-—")
	if tail == "" || strings.Contains(strings.ToLower(tail), "://") || strings.Contains(strings.ToLower(tail), "www.") {
		return ""
	}
	if cut := strings.IndexAny(tail, ".;!?"); cut >= 0 {
		tail = tail[:cut]
	}
	wordIndexes := exploreBareLiteralWordRE.FindAllStringIndex(tail, exploreBareDiagnosticMaxWords+1)
	if len(wordIndexes) < exploreBareDiagnosticMinWords {
		return ""
	}
	endWord := len(wordIndexes)
	if endWord > exploreBareDiagnosticMaxWords {
		endWord = exploreBareDiagnosticMaxWords
	}
	for index := 2; index < endWord; index++ {
		word := strings.ToLower(tail[wordIndexes[index][0]:wordIndexes[index][1]])
		switch word {
		case "after", "before", "because", "when", "while":
			endWord = index
		}
	}
	if endWord < exploreBareDiagnosticMinWords {
		return ""
	}
	phrase := strings.TrimSpace(tail[:wordIndexes[endWord-1][1]])
	runes := utf8.RuneCountInString(phrase)
	if runes < exploreBareDiagnosticMinRunes || runes > exploreBareLiteralMaxRunes {
		return ""
	}
	meaningful := 0
	for _, index := range wordIndexes[:endWord] {
		word := strings.ToLower(tail[index[0]:index[1]])
		if _, stop := assistStopWords[word]; stop {
			continue
		}
		if _, generic := exploreTerminalGenericTerms[exploreTerminalTermRoot(word)]; generic {
			continue
		}
		meaningful++
	}
	if meaningful < 2 {
		return ""
	}
	return phrase
}

func exploreBareCueIndex(lower, cue string) int {
	for offset := 0; offset < len(lower); {
		at := strings.Index(lower[offset:], cue)
		if at < 0 {
			return -1
		}
		at += offset
		beforeOK := at == 0 || !exploreBareWordByte(lower[at-1])
		after := at + len(cue)
		afterOK := strings.HasSuffix(cue, ":") || after == len(lower) || !exploreBareWordByte(lower[after])
		if beforeOK && afterOK {
			return at
		}
		offset = at + 1
	}
	return -1
}

func exploreBareWordByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func exploreBareAllCapsTerm(term string) bool {
	runes := utf8.RuneCountInString(term)
	if runes < exploreBareLiteralMinRunes || runes > exploreBareLiteralMaxRunes ||
		strings.Contains(term, "://") || strings.HasPrefix(strings.ToLower(term), "www.") {
		return false
	}
	letters := 0
	hex := true
	for _, r := range term {
		switch {
		case unicode.IsLetter(r):
			letters++
			if unicode.IsLower(r) {
				return false
			}
			if !strings.ContainsRune("ABCDEF", r) {
				hex = false
			}
		case unicode.IsDigit(r):
		case r == '_' || r == '-':
			hex = false
		default:
			return false
		}
	}
	if letters < 2 || hex && runes >= 7 {
		return false
	}
	compact := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(term))
	if exploreSyntacticAnchorNoise(compact) {
		return false
	}
	lower := strings.ToLower(term)
	if _, stop := assistStopWords[lower]; stop {
		return false
	}
	if _, generic := exploreTerminalGenericTerms[exploreTerminalTermRoot(lower)]; generic {
		return false
	}
	return true
}

func exploreBareStructuredCue(line string) bool {
	lower := strings.ToLower(line)
	for _, cue := range []string{"config", "configuration", "flag", "header", "key", "option", "property", "value"} {
		if exploreBareCueIndex(lower, cue) >= 0 {
			return true
		}
	}
	return false
}

func exploreBareLiteralRepresentedByAnchor(term string, anchors []exploreSyntacticAnchor) bool {
	candidate, ok := newExploreSyntacticAnchor(term)
	if !ok {
		return false
	}
	for _, anchor := range anchors {
		if exploreSyntacticAnchorEquivalent(anchor, candidate) {
			return true
		}
	}
	return false
}
