// Package doctor holds the runtime half of `gortex doctor`: the probes that
// answer questions an agent's config file cannot.
//
// An install is two claims — "Gortex is wired in" and "the agent is using
// it" — and only the first is visible on disk. Worse, some agents make the
// first claim unverifiable too: Codex records trust against each hook's hash
// and skips new or changed hooks until the user approves them in /hooks, so a
// fully populated config.toml and a completely inert integration are
// byte-identical. The probes here read the evidence instead: the hook
// invocation log (internal/hooks) and the agent's own session transcripts.
package doctor

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// shellTools are the Codex tool names that run a command. A read- or
// search-shaped call here is the behaviour the Gortex rule exists to replace,
// so it is the denominator adoption is measured against.
var shellTools = map[string]bool{
	"exec_command":     true,
	"shell":            true,
	"local_shell_call": true,
	"container.exec":   true,
	"bash":             true,
}

// readLike matches the shell commands that read or search source. Deliberately
// generous: over-counting a `ls` as a read costs nothing, while missing a
// `rg` would hide the exact behaviour being diagnosed.
var readLike = regexp.MustCompile(`\b(rg|grep|egrep|fgrep|ag|ack|cat|head|tail|sed|awk|less|more|find|fd|ls|tree)\b`)

// SessionRow is one agent session's tool-call tally.
type SessionRow struct {
	Started    string `json:"started,omitempty"`
	Model      string `json:"model,omitempty"`
	CWD        string `json:"cwd,omitempty"`
	Branch     string `json:"branch,omitempty"`
	CLIVersion string `json:"cli_version,omitempty"`
	Gortex     int    `json:"gortex_calls"`
	OtherMCP   int    `json:"other_mcp_calls"`
	Shell      int    `json:"shell_calls"`
	ShellRead  int    `json:"shell_read_calls"`
}

// Adoption is what the agent's transcripts say about tool choice.
//
// It deliberately carries no "did the hook fire" signal: Codex does not
// persist hook-injected context into its rollouts, so an absent orientation
// block proves nothing and must not be reported as if it did. Hook liveness
// comes from the invocation log alone.
type Adoption struct {
	Root        string         `json:"root"`
	FilesFound  int            `json:"files_found"`
	InWindow    int            `json:"sessions_in_window"`
	GortexCalls int            `json:"gortex_calls"`
	OtherMCP    int            `json:"other_mcp_calls"`
	ShellCalls  int            `json:"shell_calls"`
	ShellReads  int            `json:"shell_read_calls"`
	Tools       map[string]int `json:"tools,omitempty"`
	Models      map[string]int `json:"models,omitempty"`
	CLIVersions map[string]int `json:"cli_versions,omitempty"`
	Sessions    []SessionRow   `json:"sessions,omitempty"`
}

// GortexShare is the fraction of graph-vs-shell tool calls that went to
// Gortex, in percent. Returns -1 when there were no calls to compare.
func (a Adoption) GortexShare() float64 {
	total := a.GortexCalls + a.ShellCalls
	if total == 0 {
		return -1
	}
	return 100 * float64(a.GortexCalls) / float64(total)
}

// CodexHome resolves Codex's config root the way Codex does.
func CodexHome() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// maxSessionFiles bounds the scan. Rollouts accumulate indefinitely and a
// single one can reach hundreds of megabytes, so doctor reads the newest
// files and stops — a diagnostic that takes a minute does not get run.
const maxSessionFiles = 200

// ScanCodexSessions tallies tool calls across Codex rollout transcripts
// (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl, one JSON object per line).
// Sessions with no tool calls are skipped: they say nothing about tool choice
// and would dilute the ratio.
//
// Every failure mode degrades to "less data", never an error: a missing
// directory means Codex was never run, and an unreadable or half-written
// rollout is skipped. Neither should take down a diagnostic whose whole job
// is to work on a broken machine.
func ScanCodexSessions(codexHome string, since time.Time, limit int) Adoption {
	root := filepath.Join(codexHome, "sessions")
	out := Adoption{
		Root:        root,
		Tools:       map[string]int{},
		Models:      map[string]int{},
		CLIVersions: map[string]int{},
	}
	if codexHome == "" {
		return out
	}

	type candidate struct {
		path string
		mod  time.Time
	}
	var files []candidate
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, don't abort
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		files = append(files, candidate{path: path, mod: info.ModTime()})
		return nil
	})
	out.FilesFound = len(files)
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > maxSessionFiles {
		files = files[:maxSessionFiles]
	}

	for _, file := range files {
		if file.mod.Before(since) {
			continue // rollouts are append-only: an old mtime means an old session
		}
		row, ok := scanCodexRollout(file.path)
		if !ok {
			continue
		}
		out.InWindow++
		out.GortexCalls += row.Gortex
		out.OtherMCP += row.OtherMCP
		out.ShellCalls += row.Shell
		out.ShellReads += row.ShellRead
		if row.Model != "" {
			out.Models[row.Model]++
		}
		if row.CLIVersion != "" {
			out.CLIVersions[row.CLIVersion]++
		}
		for name, n := range row.tools {
			out.Tools[name] += n
		}
		if len(out.Sessions) < limit {
			out.Sessions = append(out.Sessions, row.SessionRow)
		}
	}
	return out
}

type rollupRow struct {
	SessionRow
	tools map[string]int
}

// maxRolloutLine bounds a single transcript line. Tool results are inlined
// into rollouts, so one line can carry a whole file; bufio's default 64 KiB
// would silently truncate the scan mid-file.
const maxRolloutLine = 8 << 20

func scanCodexRollout(path string) (rollupRow, bool) {
	handle, err := os.Open(path)
	if err != nil {
		return rollupRow{}, false
	}
	defer handle.Close()

	row := rollupRow{tools: map[string]int{}}
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64<<10), maxRolloutLine)
	for scanner.Scan() {
		var rec struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
				// function_call
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				Arguments string `json:"arguments"`
				// session_meta
				CWD        string `json:"cwd"`
				CLIVersion string `json:"cli_version"`
				Timestamp  string `json:"timestamp"`
				Git        struct {
					Branch string `json:"branch"`
				} `json:"git"`
				// turn_context
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		payload := rec.Payload
		switch {
		case rec.Type == "session_meta":
			row.CWD = payload.CWD
			row.CLIVersion = payload.CLIVersion
			row.Started = payload.Timestamp
			row.Branch = payload.Git.Branch
		case rec.Type == "turn_context":
			if payload.Model != "" {
				row.Model = payload.Model
			}
		case payload.Type == "function_call":
			switch {
			case strings.Contains(payload.Namespace, "gortex") || strings.Contains(payload.Name, "gortex"):
				row.Gortex++
				name := payload.Name
				if name == "" {
					name = payload.Namespace
				}
				row.tools[name]++
			case payload.Namespace != "":
				row.OtherMCP++
			case shellTools[payload.Name]:
				row.Shell++
				if readLike.MatchString(payload.Arguments) {
					row.ShellRead++
				}
			}
		}
	}
	if row.Gortex == 0 && row.Shell == 0 && row.OtherMCP == 0 {
		return rollupRow{}, false
	}
	return row, true
}
