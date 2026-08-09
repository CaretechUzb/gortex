package mcp

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// exploreFencedMiningMaxRunes bounds how much code-block content one task
	// contributes to mining. A pasted log dump is unbounded in principle; the
	// evidence that names an implementation is at its head.
	exploreFencedMiningMaxRunes = 2000
	exploreFencedIndentSpaces   = 4
	// A mined literal has to be long enough to be discriminating in source
	// content and short enough to still be one source string.
	exploreFencedLiteralMinRunes = 12
	exploreFencedLiteralMaxRunes = 128
	// Stack frames and code lines carry punctuation a message does not. Beyond
	// this many structural symbols the line is a frame, not an error message.
	exploreFencedLiteralMaxSymbols = 2
	// Distinct literal candidates read per task before the bounded term list
	// decides which of them survive.
	exploreFencedLiteralMaxCandidates = 16
)

// exploreFencedCodeLines returns the trimmed lines of fenced and indented code
// blocks, bounded to exploreFencedMiningMaxRunes of content per task. Query
// shaping deletes these blocks before retrieval because their commands and log
// noise out-weigh the defect sentence; the anchor and literal lanes mine them
// first, so a stack frame or snippet pasted into a report still names the
// implementation it came from.
func exploreFencedCodeLines(task string) []string {
	if !strings.Contains(task, "```") && !strings.Contains(task, "~~~") &&
		!strings.Contains(task, "\n    ") && !strings.Contains(task, "\n\t") {
		return nil
	}
	out := make([]string, 0, 16)
	budget := exploreFencedMiningMaxRunes
	fenced := false
	for _, raw := range strings.Split(task, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
			continue
		}
		if line == "" || (!fenced && !exploreIndentedCodeLine(raw)) {
			continue
		}
		if runes := []rune(line); len(runes) > budget {
			line = string(runes[:budget])
		}
		budget -= utf8.RuneCountInString(line)
		out = append(out, line)
		if budget <= 0 {
			break
		}
	}
	return out
}

// exploreIndentedCodeLine recognises the indented-block spelling of a code
// block. Markdown list items and quoted lines are indented too, so their
// markers exclude the line: they are prose the ordinary lanes already read.
func exploreIndentedCodeLine(raw string) bool {
	if !strings.HasPrefix(raw, strings.Repeat(" ", exploreFencedIndentSpaces)) &&
		!strings.HasPrefix(raw, "\t") {
		return false
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	switch trimmed[0] {
	case '-', '*', '+', '>', '#':
		return false
	}
	return true
}

// exploreFencedCodeTokens returns the mined tokens in document order using the
// prose lane's tokenization, so both anchor lanes apply their existing filters
// to mined and prose tokens alike.
func exploreFencedCodeTokens(task string) []string {
	lines := exploreFencedCodeLines(task)
	if len(lines) == 0 {
		return nil
	}
	return exploreUnquotedCodeTokens(strings.Join(lines, "\n"))
}

// exploreFencedRecallLiterals returns the distinctive literals of a task's code
// blocks: quoted strings first, then message-shaped lines. Both are values a
// symbol corpus cannot carry — they exist only inside an implementation — which
// is exactly what the bounded content lane searches for.
func exploreFencedRecallLiterals(task string) []string {
	lines := exploreFencedCodeLines(task)
	if len(lines) == 0 {
		return nil
	}
	quoted := make([]string, 0, exploreFencedLiteralMaxCandidates)
	messages := make([]string, 0, exploreFencedLiteralMaxCandidates)
	for _, line := range lines {
		if len(quoted)+len(messages) >= exploreFencedLiteralMaxCandidates {
			break
		}
		for _, match := range reInlineQuoted.FindAllString(line, -1) {
			if inner := strings.TrimSpace(match[1 : len(match)-1]); inner != "" {
				quoted = append(quoted, inner)
			}
		}
		if message, ok := exploreFencedMessageLine(line); ok {
			messages = append(messages, message)
		}
	}
	return append(quoted, messages...)
}

// exploreFencedMessageLine extracts the message a code-block line reports. The
// universal shape is "<origin>: <message>" — a level, an exception class, or a
// file coordinate followed by the sentence a source literal produced — so the
// tail after the last such separator is preferred over the whole line, which
// carries the origin the source never spells.
func exploreFencedMessageLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if separator := strings.LastIndex(line, ": "); separator >= 0 {
		if tail := exploreFencedLiteralShape(line[separator+2:]); tail != "" {
			return tail, true
		}
	}
	if whole := exploreFencedLiteralShape(line); whole != "" {
		return whole, true
	}
	return "", false
}

// exploreFencedLiteralShape accepts message-shaped text and rejects code:
// several words, mostly letters, and almost no structural punctuation.
func exploreFencedLiteralShape(text string) string {
	text = strings.Trim(strings.TrimSpace(text), "\"'`")
	runes := utf8.RuneCountInString(text)
	if runes < exploreFencedLiteralMinRunes || runes > exploreFencedLiteralMaxRunes ||
		len(strings.Fields(text)) < 2 {
		return ""
	}
	letters, symbols := 0, 0
	for _, r := range text {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r) || unicode.IsSpace(r):
		case r == '.' || r == ',' || r == '\'' || r == '-' || r == '_':
		default:
			symbols++
		}
	}
	if symbols > exploreFencedLiteralMaxSymbols || letters*10 < runes*6 {
		return ""
	}
	return text
}
