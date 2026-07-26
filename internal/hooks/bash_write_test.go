package hooks

import "testing"

func TestClassifyBashWriteTargets(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		wantPaths []string
		wantShape string
	}{
		// --- shell redirects ---
		{"redirect separated", `echo 'package x' > internal/x.go`, []string{"internal/x.go"}, "redirect"},
		{"redirect attached", `printf 'package x' >internal/x.go`, []string{"internal/x.go"}, "redirect"},
		{"append redirect", `echo more >> internal/x.go`, []string{"internal/x.go"}, "redirect"},
		{"clobber redirect", `echo x >| internal/x.go`, []string{"internal/x.go"}, "redirect"},
		{"heredoc into source", "cat > internal/x.go <<'EOF'\npackage x\nEOF", []string{"internal/x.go"}, "redirect"},
		{"redirect after &&", `go build ./... && echo done > internal/x.go`, []string{"internal/x.go"}, "redirect"},
		{"second statement writes", "cat internal/a.go\necho x > internal/b.go", []string{"internal/b.go"}, "redirect"},

		// --- tee, including after a pipe (a read classification drops it) ---
		{"tee after pipe", `go run ./gen | tee internal/x.go`, []string{"internal/x.go"}, "tee"},
		{"tee append", `go run ./gen | tee -a internal/x.go`, []string{"internal/x.go"}, "tee"},
		{"tee non-source only", `go test ./... | tee /tmp/out.log`, nil, ""},

		// --- in-place editors ---
		{"sed -i", `sed -i 's/a/b/' internal/x.go`, []string{"internal/x.go"}, "sed -i"},
		{"sed -i over several operands", `sed -i 's/a/b/' internal/a.go internal/b.go`, []string{"internal/a.go", "internal/b.go"}, "sed -i"},
		{"sed -i with a trailing non-source operand", `sed -i 's/a/b/' internal/a.go README.md`, []string{"internal/a.go"}, "sed -i"},
		{"sed -i over an unexpanded glob", `sed -i 's/a/b/' internal/*.go`, nil, ""},
		{"sed -i with suffix", `sed -i.bak 's/a/b/' internal/x.go`, []string{"internal/x.go"}, "sed -i"},
		{"sed BSD empty suffix", `sed -i '' -e 's/a/b/' internal/x.go`, []string{"internal/x.go"}, "sed -i"},
		{"sed --in-place", `sed --in-place 's/a/b/' internal/x.go`, []string{"internal/x.go"}, "sed -i"},
		{"gsed -i", `gsed -i 's/a/b/' internal/x.go`, []string{"internal/x.go"}, "gsed -i"},
		{"absolute sed path", `/usr/bin/sed -i 's/a/b/' internal/x.go`, []string{"internal/x.go"}, "sed -i"},
		{"perl -pi", `perl -pi -e 's/a/b/' internal/x.go`, []string{"internal/x.go"}, "perl -i"},
		{"ruby -i", `ruby -i -pe 'gsub(/a/,"b")' internal/x.go`, []string{"internal/x.go"}, "ruby -i"},
		{"sed bounded read is not a write", `sed -n '20,80p' internal/x.go`, nil, ""},
		{"sed extended regex without -i", `sed -E -e 's/a/b/' internal/x.go`, nil, ""},
		{"perl module flag containing i", `perl -MList::Util -e 'print' internal/x.go`, nil, ""},
		{"grep -i is not an editor", `grep -i handleFoo internal/x.go`, nil, ""},

		// --- inline interpreter scripts ---
		{
			"python heredoc write",
			"python3 - <<'PY'\np = 'internal/x.go'\nopen('internal/x.go','w').write('package x')\nPY",
			[]string{"internal/x.go"}, "script write",
		},
		{
			"python -c write",
			`python3 -c "open('internal/x.go', 'a').write('x')"`,
			[]string{"internal/x.go"}, "script write",
		},
		{
			"python pathlib write",
			"python3 - <<'PY'\nPath('internal/x.go').write_text('package x')\nPY",
			[]string{"internal/x.go"}, "script write",
		},
		{
			"python read-only heredoc",
			"python3 - <<'PY'\nprint(open('internal/x.go').read())\nPY",
			nil, "",
		},
		{
			"python writes elsewhere",
			"python3 - <<'PY'\nsrc = open('internal/x.go').read()\nopen('/tmp/out.txt','w').write(src)\nPY",
			nil, "",
		},
		{
			"node writeFileSync",
			`node -e "require('fs').writeFileSync('src/app.ts', 'x')"`,
			[]string{"src/app.ts"}, "script write",
		},
		{
			"perl three-arg open",
			`perl -e "open(my $fh, '>', 'internal/x.go');"`,
			[]string{"internal/x.go"}, "script write",
		},
		{
			"echo of write-shaped text is not a write",
			`echo "open('internal/x.go','w')"`,
			nil, "",
		},

		// --- a newline separates statements, a heredoc payload is data ---
		{"newline then in-place edit", "cd /repo\nsed -i 's/a/b/' internal/x.go", []string{"internal/x.go"}, "sed -i"},
		{
			"documented command inside a payload is not a write",
			"cat > docs/notes.md <<'EOF'\nRun: sed -i 's/a/b/' internal/x.go\nEOF",
			nil, "",
		},
		{
			"redirect inside a payload is not a write",
			"cat > docs/notes.md <<'EOF'\necho x > internal/x.go\nEOF",
			nil, "",
		},
		{
			"payload delimiter with tab stripping",
			"cat > docs/notes.md <<-EOF\n\tsed -i 's/a/b/' internal/x.go\nEOF",
			nil, "",
		},
		{
			"command after the payload is still classified",
			"cat > docs/notes.md <<'EOF'\nplain text\nEOF\nsed -i 's/a/b/' internal/x.go",
			[]string{"internal/x.go"}, "sed -i",
		},
		{
			"multi-line quoted argument is not split",
			"git commit -m 'line one\n> internal/x.go line two'",
			nil, "",
		},

		// --- a quoted operator is data, not a redirect ---
		{"quoted redirect pattern", `grep -c '>' internal/x.go`, nil, ""},
		{"quoted append pattern", `rg '>>' internal/x.go`, nil, ""},
		{"double-quoted redirect pattern", `grep -n ">" internal/x.go`, nil, ""},
		{"shift operator inside a quoted pattern", "grep -rn 'cap = 1 << 20' internal/\nsed -i 's/a/b/' internal/x.go", []string{"internal/x.go"}, "sed -i"},
		{"quoted << does not swallow the next statement", "git commit -m 'use << shift'\necho x > internal/x.go", []string{"internal/x.go"}, "redirect"},

		// --- an indented delimiter does not close a plain heredoc ---
		{
			"indented delimiter keeps the payload data",
			"cat > docs/notes.md <<'EOF'\nexample:\n  EOF\nsed -i 's/a/b/' internal/x.go\nmore text\nEOF",
			nil, "",
		},
		{
			"tab-indented delimiter closes a <<- heredoc",
			"cat > docs/notes.md <<-EOF\npayload\n\tEOF\nsed -i 's/a/b/' internal/x.go",
			[]string{"internal/x.go"}, "sed -i",
		},

		// --- a continued command is one command ---
		{"line continuation keeps operands together", "sed -i 's/a/b/' \\\n  internal/x.go", []string{"internal/x.go"}, "sed -i"},
		{"line continuation before a redirect", "echo x \\\n  > internal/x.go", []string{"internal/x.go"}, "redirect"},

		// --- interpreter scans stay inside the interpreter's own text ---
		{
			"write-shaped text in another statement is not a write",
			`python3 scripts/check.py && git commit -m "stop using open('internal/x.go','w')"`,
			nil, "",
		},
		{
			"write-shaped text in a payload another command opened is not a write",
			"cat > docs/howto.md <<'EOF'\nWe used to do open('internal/x.go','w')\nEOF\npython3 scripts/check.py",
			nil, "",
		},

		// --- negatives: real commands that must not be read as writes ---
		{"build output flag", `go build -o gortex ./cmd/gortex/`, nil, ""},
		{"tee reading a source file on stdin", `tee /tmp/copy.log < internal/x.go`, nil, ""},
		{"test log redirect", `go test -race ./... > /tmp/test.log 2>&1`, nil, ""},
		{"stderr merge", `go build ./... 2>&1 | head -30`, nil, ""},
		{"devnull", `go vet ./... > /dev/null`, nil, ""},
		{"markdown redirect", `gortex guide > docs/notes.md`, nil, ""},
		{"quoted comparison in pattern", `grep -rn 'a>b' internal/x.go`, nil, ""},
		{"process substitution read", `diff <(gofmt -d internal/x.go) internal/x.go`, nil, ""},
		{"plain read", `cat internal/x.go`, nil, ""},
		{"empty", ``, nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writes := classifyBashWriteTargets(tt.cmd)
			paths := writePaths(writes)
			if len(paths) != len(tt.wantPaths) {
				t.Fatalf("paths = %v, want %v", paths, tt.wantPaths)
			}
			for i := range paths {
				if paths[i] != tt.wantPaths[i] {
					t.Errorf("paths[%d] = %q, want %q", i, paths[i], tt.wantPaths[i])
				}
			}
			shape := ""
			if len(writes) > 0 {
				shape = writes[0].Shape
			}
			if shape != tt.wantShape {
				t.Errorf("shape = %q, want %q", shape, tt.wantShape)
			}
		})
	}
}

func writePaths(writes []BashWrite) []string {
	var paths []string
	for _, write := range writes {
		paths = append(paths, write.Path)
	}
	return paths
}

func TestClassifyBashWriteTargets_CollectsEveryCandidateUpToLimit(t *testing.T) {
	// One statement can name several targets; the first indexed one is what
	// the enrichment answers on, so all of them have to survive classification.
	cmd := `echo a > internal/a.go; echo b > internal/b.go; echo c > internal/c.go; echo d > internal/d.go`
	writes := classifyBashWriteTargets(cmd)
	if len(writes) != bashWriteProbeLimit {
		t.Fatalf("writes = %v, want %d candidates", writePaths(writes), bashWriteProbeLimit)
	}
	for i, want := range []string{"internal/a.go", "internal/b.go", "internal/c.go"} {
		if writes[i].Path != want || writes[i].Shape != "redirect" {
			t.Errorf("writes[%d] = %+v, want %q via redirect", i, writes[i], want)
		}
	}
}

func TestClassifyBashWriteTargets_DedupesRepeatedTarget(t *testing.T) {
	writes := classifyBashWriteTargets(`echo a > internal/x.go && echo b >> internal/x.go`)
	if len(writes) != 1 || writes[0].Path != "internal/x.go" {
		t.Fatalf("writes = %v, want one internal/x.go", writePaths(writes))
	}
}

func TestClassifyBashWriteTargets_ShapeTravelsWithItsPath(t *testing.T) {
	// The message names the shape of the path it reports, so a command with
	// two shapes must not attribute one path's write to the other's construct.
	writes := classifyBashWriteTargets(`sed -i 's/a/b/' /tmp/scratch.go && echo x > internal/x.go`)
	want := []BashWrite{
		{Path: "/tmp/scratch.go", Shape: "sed -i"},
		{Path: "internal/x.go", Shape: "redirect"},
	}
	if len(writes) != len(want) {
		t.Fatalf("writes = %+v, want %+v", writes, want)
	}
	for i := range want {
		if writes[i] != want[i] {
			t.Errorf("writes[%d] = %+v, want %+v", i, writes[i], want[i])
		}
	}
}

func TestAllCommandSegmentsKeepsPipedSegments(t *testing.T) {
	got := allCommandSegments(`go run ./gen | tee internal/x.go`)
	want := []string{"go run ./gen", "tee internal/x.go"}
	if len(got) != len(want) {
		t.Fatalf("segments = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("segment %d = %q, want %q", i, got[i], want[i])
		}
	}
	if primary := primarySegments(`go run ./gen | tee internal/x.go`); len(primary) != 1 {
		t.Errorf("primarySegments must still drop the piped segment, got %v", primary)
	}
}

func TestSplitSegmentRedirects(t *testing.T) {
	tests := []struct {
		segment     string
		wantCommand string
		wantTargets []string
	}{
		{`echo x > internal/x.go`, "echo x ", []string{"internal/x.go"}},
		{`echo x >internal/x.go`, "echo x ", []string{"internal/x.go"}},
		{`echo x >> internal/x.go`, "echo x ", []string{"internal/x.go"}},
		{`echo x >| internal/x.go`, "echo x ", []string{"internal/x.go"}},
		{`go build ./... 2> err.log`, "go build ./... ", []string{"err.log"}},
		{`go build ./... 2>err.log`, "go build ./... ", []string{"err.log"}},
		{`go test ./... &> out.log`, "go test ./... ", []string{"out.log"}},
		{`go test ./... 2>&1`, "go test ./... ", nil},
		{`echo 2 > internal/x.go`, "echo 2 ", []string{"internal/x.go"}},
		{`cat internal/x.go`, "cat internal/x.go", nil},
		{`cat internal/a.go > /tmp/out.go`, "cat internal/a.go ", []string{"/tmp/out.go"}},
		{`cat > internal/x.go <<'EOF'`, "cat  <<'EOF'", []string{"internal/x.go"}},
		// A quoted operator is a pattern, not a redirect.
		{`grep -c '>' internal/x.go`, `grep -c '>' internal/x.go`, nil},
		{`grep -n ">>" internal/x.go`, `grep -n ">>" internal/x.go`, nil},
		{`tee /tmp/copy.log < internal/x.go`, `tee /tmp/copy.log < internal/x.go`, nil},
	}
	for _, tt := range tests {
		command, targets := splitSegmentRedirects(tt.segment)
		if command != tt.wantCommand {
			t.Errorf("splitSegmentRedirects(%q) command = %q, want %q", tt.segment, command, tt.wantCommand)
		}
		if len(targets) != len(tt.wantTargets) {
			t.Errorf("splitSegmentRedirects(%q) targets = %v, want %v", tt.segment, targets, tt.wantTargets)
			continue
		}
		for i := range targets {
			if targets[i] != tt.wantTargets[i] {
				t.Errorf("splitSegmentRedirects(%q) targets[%d] = %q, want %q", tt.segment, i, targets[i], tt.wantTargets[i])
			}
		}
	}
}

func TestSplitHeredocs(t *testing.T) {
	tests := []struct {
		name         string
		cmd          string
		wantCommands string
		wantBodies   []string
	}{
		{
			"payload removed and handed back",
			"cat > x.md <<'EOF'\nline one\nline two\nEOF\necho done",
			"cat > x.md <<'EOF'\necho done",
			[]string{"line one\nline two"},
		},
		{
			"interpreter payload is attributed to its opener",
			"python3 - <<PY\nprint(1)\nPY",
			"python3 - <<PY",
			[]string{"print(1)"},
		},
		{
			"quoted << opens nothing",
			"grep -rn 'cap = 1 << 20' internal/\necho done",
			"grep -rn 'cap = 1 << 20' internal/\necho done",
			nil,
		},
		{
			"herestring opens nothing",
			"grep foo <<< \"$body\"\necho done",
			"grep foo <<< \"$body\"\necho done",
			nil,
		},
		{
			"unclosed delimiter swallows the rest",
			"cat > x.md <<'EOF'\nline one\necho done",
			"cat > x.md <<'EOF'",
			[]string{"line one\necho done"},
		},
		{
			"expanded delimiter is taken literally",
			"cat > x.md <<$D\npayload\n$D\necho done",
			"cat > x.md <<$D\necho done",
			[]string{"payload"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commands, heredocs := splitHeredocs(tt.cmd)
			if commands != tt.wantCommands {
				t.Errorf("commands = %q, want %q", commands, tt.wantCommands)
			}
			if len(heredocs) != len(tt.wantBodies) {
				t.Fatalf("got %d heredocs, want %d: %+v", len(heredocs), len(tt.wantBodies), heredocs)
			}
			for i := range heredocs {
				if heredocs[i].body != tt.wantBodies[i] {
					t.Errorf("heredocs[%d].body = %q, want %q", i, heredocs[i].body, tt.wantBodies[i])
				}
			}
		})
	}
}
