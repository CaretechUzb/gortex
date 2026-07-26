package hooks

import (
	"regexp"
	"strings"
)

// bashWriteProbeLimit bounds how many candidates one command contributes. A
// command normally writes a single file; the cap keeps the hook on the read
// path's daemon-probe budget even when a compound command redirects into
// several paths. A command that names more than this many source-looking
// targets has the ones past the cap ignored.
const bashWriteProbeLimit = 3

// BashWrite is one file a command was recognised as rewriting.
type BashWrite struct {
	Path string
	// Shape names the construct that writes it (redirect, tee, sed -i,
	// script write) so the message can say how the file was going to change.
	Shape string
}

// inPlaceEditors rewrite the file they are handed instead of printing to
// stdout, but only when invoked with an in-place flag.
var inPlaceEditors = map[string]bool{
	"sed": true, "gsed": true, "perl": true, "ruby": true,
}

// scriptInterpreters gate the write-open patterns below. Requiring one keeps
// `echo "open('x.go','w')"` — which writes nothing — from reading as a write.
var scriptInterpreters = map[string]bool{
	"python": true, "python3": true, "perl": true, "ruby": true,
	"node": true, "deno": true, "bun": true, "php": true,
}

// inPlaceFlagRE matches the in-place rewrite flag in its short forms: `-i`,
// `-i.bak`, and clusters such as `-pi` / `-ni.bak`. The letter class is
// deliberately narrow so a flag that merely contains an `i` in its attached
// argument (`perl -MList::Util`) is not read as in-place editing.
var inPlaceFlagRE = regexp.MustCompile(`^-[aelnps]*i[aelnps]*(?:\.[^\s]*)?$`)

// bashWriteOpenPatterns recognise a path opened for writing inside an inline
// interpreter script — `python3 - <<PY … open(p,"w") … PY`, the shape that
// mutates source without ever naming a shell redirect. Each pattern's first
// capture group is the path. Literal paths only: one held in a variable is not
// recoverable from the command text, and guessing would trade a silent miss
// for a false block.
var bashWriteOpenPatterns = []*regexp.Regexp{
	// Python / Ruby: open("p", "w"|"a"|"x"), File.open("p", "wb")
	regexp.MustCompile(`\bopen\s*\(\s*['"]([^'"]+)['"]\s*,\s*['"][rbt+]*[wax][rbt+]*['"]`),
	// Python pathlib: Path("p").write_text(…) / .write_bytes(…)
	regexp.MustCompile(`\bPath\s*\(\s*['"]([^'"]+)['"]\s*\)\s*\.\s*write_(?:text|bytes)\s*\(`),
	// Ruby: File.write("p", …)
	regexp.MustCompile(`\bFile\.write\s*\(?\s*['"]([^'"]+)['"]`),
	// Node: fs.writeFileSync("p", …) / fs.appendFile("p", …)
	regexp.MustCompile("\\b(?:write|append)File(?:Sync)?\\s*\\(\\s*['\"`]([^'\"`]+)['\"`]"),
	// Perl three-argument open: open($fh, '>', 'p')
	regexp.MustCompile(`\bopen\s*\(?\s*(?:my\s+)?[\$A-Za-z_]\w*\s*,\s*['"]\s*>>?\s*['"]\s*,\s*['"]([^'"]+)['"]`),
	// Perl two-argument open: open(FH, ">p")
	regexp.MustCompile(`\bopen\s*\(?\s*(?:my\s+)?[\$A-Za-z_]\w*\s*,\s*['"]\s*>>?\s*([^'"]+?)\s*['"]`),
}

// classifyBashWriteTargets returns the files a command was recognised as
// rewriting, in the order they appear and capped at bashWriteProbeLimit.
// No recognised write shape returns nothing.
//
// Unlike the read classification this walks *every* segment, including the
// ones a pipe would otherwise drop: in `go run ./gen | tee internal/x.go` the
// mutation lives after the pipe, so treating that segment as a filter on
// upstream output would miss the write entirely.
//
// Recognition stays deliberately literal, for the same reason the read
// classifier does: a path assembled at run time is not knowable from the
// command text, and a guess would cost the caller a real command. Known
// misses, so nobody reads this as full coverage: a redirect operator glued to
// the preceding word (`echo x>f.go`); a path that only exists at run time
// (`sed -i … $FILE`, `… internal/*.go`); a file list arriving on stdin
// (`… | xargs sed -i …`, `find … -exec sed -i … {} +`); a script or wrapper
// whose text is not in the command (`python3 script.py`, `bash -c "…"`,
// `eval`); `patch` / `git apply` / `git checkout --`; `cp` and `mv` over an
// existing path; and formatters invoked with `-w`, which are deliberately
// allowed because an agent runs them right after a correct edit.
func classifyBashWriteTargets(cmd string) []BashWrite {
	if cmd == "" {
		return nil
	}
	var writes []BashWrite
	add := func(shape, path string) {
		// A path with a glob or a variable in it names no single file, so
		// there is nothing the daemon could confirm.
		if len(writes) >= bashWriteProbeLimit || !looksLikeSourceFile(path) || strings.ContainsAny(path, "*?$") {
			return
		}
		for _, seen := range writes {
			if seen.Path == path {
				return
			}
		}
		writes = append(writes, BashWrite{Path: path, Shape: shape})
	}

	for _, segment := range allCommandSegments(cmd) {
		command, targets := splitSegmentRedirects(segment)
		for _, target := range targets {
			add("redirect", target)
		}
		tokens := skipCommandPrefix(tokenize(command))
		if len(tokens) == 0 {
			continue
		}
		primary := baseCommandName(tokens[0])
		switch {
		case primary == "tee":
			for _, token := range positionalTokens(dropInputRedirects(tokens)) {
				add("tee", token)
			}
		case inPlaceEditors[primary] && hasInPlaceFlag(tokens):
			// Every operand is rewritten, not just the last one.
			for _, token := range positionalTokens(dropInputRedirects(tokens)) {
				add(primary+" -i", token)
			}
		}
	}
	// A script write lives in the text an interpreter is asked to run, never
	// in the rest of the command: a path named in a commit message or quoted
	// inside a document being written is not a mutation.
	for _, script := range interpreterScripts(cmd) {
		for _, pattern := range bashWriteOpenPatterns {
			for _, match := range pattern.FindAllStringSubmatch(script, -1) {
				add("script write", match[1])
			}
		}
	}
	return writes
}

// interpreterScripts returns the text an interpreter is asked to run: each
// statement whose command is an interpreter, plus any heredoc payload such a
// statement opened.
func interpreterScripts(cmd string) []string {
	if !strings.ContainsRune(cmd, '(') {
		return nil // every write-open pattern needs a call
	}
	var scripts []string
	for _, segment := range allCommandSegments(cmd) {
		if namesInterpreter(segment) {
			scripts = append(scripts, segment)
		}
	}
	_, heredocs := splitHeredocs(cmd)
	for _, heredoc := range heredocs {
		if namesInterpreter(heredoc.opener) {
			scripts = append(scripts, heredoc.body)
		}
	}
	return scripts
}

// namesInterpreter reports whether any statement in text runs an interpreter.
func namesInterpreter(text string) bool {
	for _, segment := range allCommandSegments(text) {
		command, _ := splitSegmentRedirects(segment)
		tokens := skipCommandPrefix(tokenize(command))
		if len(tokens) > 0 && scriptInterpreters[baseCommandName(tokens[0])] {
			return true
		}
	}
	return false
}

// splitSegmentRedirects separates a segment's output redirections from the
// rest of it, returning the command with them removed and the paths they
// write. The scan is quote-aware and runs on the raw segment because
// tokenizing first strips the quotes: `grep -n '>' x.go` would then present a
// bare `>` operator token, and a plain read would be answered as a write.
func splitSegmentRedirects(segment string) (string, []string) {
	if !strings.ContainsRune(segment, '>') {
		return segment, nil
	}
	command := make([]byte, 0, len(segment))
	var targets []string
	inSingle, inDouble := false, false
	for i := 0; i < len(segment); {
		c := segment[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			command = append(command, c)
			i++
		case c == '"' && !inSingle:
			inDouble = !inDouble
			command = append(command, c)
			i++
		case inSingle || inDouble || c != '>':
			command = append(command, c)
			i++
		default:
			command = trimRedirectPrefix(command)
			i++ // the `>` itself
			for _, following := range []byte{'>', '|'} {
				if i < len(segment) && segment[i] == following {
					i++
				}
			}
			for i < len(segment) && (segment[i] == ' ' || segment[i] == '\t') {
				i++
			}
			// `>&2` / `>&-` duplicates or closes a descriptor and names no file.
			if i < len(segment) && segment[i] == '&' {
				for i++; i < len(segment) && (segment[i] == '-' || (segment[i] >= '0' && segment[i] <= '9')); i++ {
				}
				continue
			}
			target, next := readWord(segment, i)
			i = next
			if target != "" {
				targets = append(targets, target)
			}
		}
	}
	return string(command), targets
}

// trimRedirectPrefix removes a file-descriptor or `&` prefix belonging to the
// redirect operator rather than to the command, so `go build 2>err.log` keeps
// `go build` while `echo 2 > f` keeps its `2`.
func trimRedirectPrefix(command []byte) []byte {
	end := len(command)
	if end > 0 && command[end-1] == '&' {
		end--
	}
	for end > 0 && command[end-1] >= '0' && command[end-1] <= '9' {
		end--
	}
	if end == len(command) {
		return command
	}
	if end == 0 || command[end-1] == ' ' || command[end-1] == '\t' {
		return command[:end]
	}
	return command
}

// readWord reads one shell word starting at i, stripping quotes and stopping
// at whitespace or the next operator. It returns the word and the index after
// it.
func readWord(text string, i int) (string, int) {
	var word []byte
	inSingle, inDouble := false, false
	for i < len(text) {
		c := text[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case !inSingle && !inDouble && strings.IndexByte(" \t\n;|&<>", c) >= 0:
			return string(word), i
		default:
			word = append(word, c)
		}
		i++
	}
	return string(word), i
}

// bashHeredoc is one heredoc payload and the line that opened it.
type bashHeredoc struct {
	opener string
	body   string
}

// splitHeredocs separates a command's heredoc payloads from its commands,
// returning the command text with each payload removed alongside the payloads
// themselves. What an agent writes into a file is data: classifying it as
// commands is how a documented shell snippet becomes a phantom mutation. The
// payloads are handed back rather than dropped because the interpreter shapes
// write from inside them.
//
// A delimiter that is never closed swallows the rest of the command, which
// costs recall and never precision — the right direction for a guard.
func splitHeredocs(cmd string) (string, []bashHeredoc) {
	if !strings.Contains(cmd, "<<") {
		return cmd, nil
	}
	lines := strings.Split(cmd, "\n")
	commands := make([]string, 0, len(lines))
	var heredocs []bashHeredoc
	var body []string
	inSingle, inDouble := false, false
	delimiter, opener := "", ""
	stripTabs := false
	closeBody := func() {
		heredocs = append(heredocs, bashHeredoc{opener: opener, body: strings.Join(body, "\n")})
		delimiter, opener, body = "", "", nil
	}
	for _, line := range lines {
		if delimiter != "" {
			if heredocTerminates(line, delimiter, stripTabs) {
				closeBody()
			} else {
				body = append(body, line)
			}
			continue
		}
		commands = append(commands, line)
		delimiter, stripTabs, inSingle, inDouble = heredocOpener(line, inSingle, inDouble)
		if delimiter != "" {
			opener = line
		}
	}
	if delimiter != "" {
		closeBody()
	}
	return strings.Join(commands, "\n"), heredocs
}

// heredocOpener reports the delimiter a line opens a heredoc with, along with
// whether `<<-` tab stripping applies and the quote state the next line
// inherits. The scan carries quote state in from earlier lines so that a `<<`
// inside a quoted argument — `grep 'cap = 1 << 20'`, a C++ stream insert — is
// never mistaken for an operator, which would delete the rest of the command
// from classification. A `<<$VAR` delimiter is taken literally because bash
// does not expand it.
func heredocOpener(line string, inSingle, inDouble bool) (string, bool, bool, bool) {
	delimiter, stripTabs := "", false
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			i++
			continue
		case c == '"' && !inSingle:
			inDouble = !inDouble
			i++
			continue
		case inSingle || inDouble || c != '<' || i+1 >= len(line) || line[i+1] != '<':
			i++
			continue
		}
		if i+2 < len(line) && line[i+2] == '<' {
			i += 3 // a <<< herestring opens no payload
			continue
		}
		i += 2
		strip := false
		if i < len(line) && line[i] == '-' {
			strip = true
			i++
		}
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		word, next := readWord(line, i)
		i = next
		if word != "" {
			delimiter, stripTabs = word, strip
		}
	}
	return delimiter, stripTabs, inSingle, inDouble
}

// heredocTerminates reports whether a line closes the payload. Bash wants the
// delimiter alone on the line, with leading tabs allowed only for `<<-`;
// accepting an indented delimiter would end the payload early and hand the
// rest of the file's contents to the classifier as commands.
func heredocTerminates(line, delimiter string, stripTabs bool) bool {
	if stripTabs {
		line = strings.TrimLeft(line, "\t")
	}
	return strings.TrimRight(line, "\r") == delimiter
}

// hasInPlaceFlag reports whether an editor invocation carries an in-place
// rewrite flag.
func hasInPlaceFlag(tokens []string) bool {
	for _, token := range tokens[1:] {
		if token == "--in-place" || strings.HasPrefix(token, "--in-place=") {
			return true
		}
		if strings.HasPrefix(token, "--") {
			continue
		}
		if inPlaceFlagRE.MatchString(token) {
			return true
		}
	}
	return false
}

// positionalTokens returns the non-flag arguments of an invocation.
func positionalTokens(tokens []string) []string {
	var out []string
	for _, token := range tokens[1:] {
		if strings.HasPrefix(token, "-") && token != "-" {
			continue
		}
		out = append(out, token)
	}
	return out
}

// dropInputRedirects removes `<` / `<<` operands so a command that reads a
// file on stdin is not credited with writing it.
func dropInputRedirects(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		if !strings.HasPrefix(tokens[i], "<") {
			out = append(out, tokens[i])
			continue
		}
		if strings.TrimLeft(tokens[i], "<") == "" {
			i++ // the operand is the next token
		}
	}
	return out
}

// baseCommandName strips any directory prefix so `/usr/bin/sed` classifies
// like `sed`.
func baseCommandName(token string) string {
	if i := strings.LastIndexByte(token, '/'); i >= 0 {
		return token[i+1:]
	}
	return token
}
