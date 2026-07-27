package doctor

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sessions_claude.go reads Claude Code transcripts, the counterpart to the
// Codex rollout scanner. Same output type, same question — did the model reach
// for the graph or for the filesystem — so doctor can report both agents side
// by side instead of describing one and leaving the other blank.
//
// The formats differ enough to keep the parsers apart: Codex writes flat
// `function_call` payloads with a `namespace`, Claude writes assistant
// messages whose `content` array carries `tool_use` parts, and the tool name
// alone says whether it went through MCP.

// claudeNativeReadTools are the built-in tools whose job the graph tools are
// meant to take over. Bash is counted separately: only a read- or
// search-shaped command competes with a graph query.
var claudeNativeReadTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "NotebookRead": true,
}

// ClaudeHome resolves Claude Code's config root, honouring the same override
// the adapter uses so a non-default profile is not silently missed.
func ClaudeHome() string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// ScanClaudeSessions tallies tool calls across Claude Code transcripts
// (<claude home>/projects/<slug>/<uuid>.jsonl, one JSON object per line).
//
// Transcripts accumulate per project and can number in the thousands, so the
// newest are read and the rest skipped — a diagnostic nobody waits for does
// not get run. Sessions with no tool calls are excluded: they say nothing
// about tool choice and would dilute the ratio.
func ScanClaudeSessions(claudeHome string, since time.Time, limit int) Adoption {
	root := filepath.Join(claudeHome, "projects")
	out := Adoption{
		Root:        root,
		Tools:       map[string]int{},
		Models:      map[string]int{},
		CLIVersions: map[string]int{},
	}
	if claudeHome == "" {
		return out
	}

	type candidate struct {
		path string
		mod  time.Time
	}
	var files []candidate
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if info.ModTime().Before(since) {
			return nil // cheap reject before the file is opened
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
		row, ok := scanClaudeTranscript(file.path)
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
		for name, n := range row.tools {
			out.Tools[name] += n
		}
		if len(out.Sessions) < limit {
			out.Sessions = append(out.Sessions, row.SessionRow)
		}
	}
	return out
}

func scanClaudeTranscript(path string) (rollupRow, bool) {
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
			Type      string `json:"type"`
			CWD       string `json:"cwd"`
			GitBranch string `json:"gitBranch"`
			Version   string `json:"version"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Model   string `json:"model"`
				Content []struct {
					Type  string          `json:"type"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		if rec.CWD != "" && row.CWD == "" {
			row.CWD = rec.CWD
			row.Branch = rec.GitBranch
			row.CLIVersion = rec.Version
			row.Started = rec.Timestamp
		}
		if rec.Message.Model != "" {
			row.Model = rec.Message.Model
		}
		for _, part := range rec.Message.Content {
			if part.Type != "tool_use" || part.Name == "" {
				continue
			}
			switch {
			case strings.HasPrefix(part.Name, "mcp__gortex__"):
				row.Gortex++
				row.tools[strings.TrimPrefix(part.Name, "mcp__gortex__")]++
			case strings.HasPrefix(part.Name, "mcp__"):
				row.OtherMCP++
			case part.Name == "Bash":
				row.Shell++
				if readLike.Match(part.Input) {
					row.ShellRead++
				}
			case claudeNativeReadTools[part.Name]:
				// A native Read/Grep/Glob is the same competing behaviour a
				// shell read is, and belongs in the same denominator.
				row.Shell++
				row.ShellRead++
			}
		}
	}
	if row.Gortex == 0 && row.Shell == 0 && row.OtherMCP == 0 {
		return rollupRow{}, false
	}
	return row, true
}
