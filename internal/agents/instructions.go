// Instructions shared across every doc-aware adapter. Centralising the
// body here avoids per-adapter drift: Cursor's .cursor/rules file,
// Copilot's .github/copilot-instructions.md, Codex's AGENTS.md, and
// Claude Code's CLAUDE.md all read from the same constant, so when the
// "prefer Gortex over Read/Grep" story evolves we update it once and
// every agent sees the change on the next `gortex init`.
//
// The claudecode adapter extends this body with its own slash-commands
// appendix — that part is Claude-Code-specific and lives in
// claudecode/content.go, keyed off the same sentinel so idempotency
// checks line up across adapters.
package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/profiles"
)

// InstructionsSentinel is the substring every doc-aware adapter checks
// for when deciding whether to append the instructions block. If it's
// already present (wherever it came from — a prior `gortex init`, a
// user-copied block, another adapter writing to a shared rules file
// like AGENTS.md) we skip to stay idempotent.
const InstructionsSentinel = "## MANDATORY: Use Gortex MCP tools"

// CommunitiesStartMarker / CommunitiesEndMarker fence the generated
// community-routing block that `gortex init` writes into per-repo
// instructions files. Fenced (not just start-only) because this block
// is regenerated on every `init` re-run as the codebase evolves, so
// we need to identify and overwrite it precisely without clobbering
// user edits around it.
const (
	CommunitiesStartMarker = "<!-- gortex:communities:start -->"
	CommunitiesEndMarker   = "<!-- gortex:communities:end -->"
)

// GlobalRulesStartMarker / GlobalRulesEndMarker fence the rule block
// that `gortex install` merges into ~/.claude/CLAUDE.md. The block is
// idempotent (re-running install replaces it in place) and removable
// (user can delete the marked region by hand without other side
// effects). Distinct from the communities markers above because this
// block lives at user level and survives every project init.
const (
	GlobalRulesStartMarker = "<!-- gortex:rules:start -->"
	GlobalRulesEndMarker   = "<!-- gortex:rules:end -->"
)

// GlobalPointerBody renders the thin machine-level rule block
// `gortex install` merges into ~/.claude/CLAUDE.md. The rule content
// itself lives in <instructionsDir>/active.md — an atomic byte copy of
// the selected instruction profile (internal/profiles) — and is pulled
// in through an @-include, so switching guidance depth never rewrites
// CLAUDE.md. The heading stays here as the idempotency sentinel and as
// a functional minimum for readers that do not expand @-includes.
func GlobalPointerBody(instructionsDir string) string {
	active := filepath.Join(instructionsDir, profiles.ActiveFileName)
	return "## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob\n\n" +
		"The machine-wide Gortex rules load from the active instruction profile, imported below:\n\n" +
		"@" + active + "\n\n" +
		"Switch guidance depth with `gortex instructions switch <core|localization|full>` (`list` shows all) — applies to NEW sessions only.\n"
}

// InstructionsBody is the shared rule block every adapter writes to
// its agent's instructions file. Tool names in the tables (Read, Grep)
// are Claude-Code-specific flavour; models outside Claude Code read
// them as "any file-reading tool" — the principle stays the same so
// we keep one body rather than branch by agent.
const InstructionsBody = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep

Gortex runs as an MCP server for this repository. You MUST prefer graph queries over file reads on every task — PreToolUse hooks deny ` + "`" + `Read` + "`" + ` / ` + "`" + `Grep` + "`" + ` / ` + "`" + `Glob` + "`" + ` against indexed source, and the deny message names the right tool.

| Instead of...                       | Use...                                   |
|-------------------------------------|------------------------------------------|
| ` + "`" + `Grep` + "`" + ` for a symbol                 | ` + "`" + `search_symbols` + "`" + ` (BM25 + camelCase-aware) |
| ` + "`" + `Grep` + "`" + ` for references               | ` + "`" + `find_usages` + "`" + ` (zero false positives)     |
| Reading / grepping to find callers  | ` + "`" + `get_callers` + "`" + ` / ` + "`" + `get_call_chain` + "`" + `         |
| ` + "`" + `Read` + "`" + ` a file for one symbol        | ` + "`" + `get_symbol_source` + "`" + ` (` + "`" + `compress_bodies:true` + "`" + ` for the signature only) |
| ` + "`" + `Read` + "`" + ` to understand a file         | ` + "`" + `get_file_summary` + "`" + ` / ` + "`" + `get_editing_context` + "`" + ` |
| ` + "`" + `Read` + "`" + ` a non-indexed / raw file     | ` + "`" + `read_file` + "`" + `                              |
| 5-10 reads to explore a task        | ` + "`" + `smart_context` + "`" + ` (one call)               |
| ` + "`" + `Edit` + "`" + ` / ` + "`" + `Write` + "`" + ` source             | ` + "`" + `edit_file` + "`" + ` / ` + "`" + `write_file` + "`" + ` / ` + "`" + `edit_symbol` + "`" + ` / ` + "`" + `rename_symbol` + "`" + ` / ` + "`" + `batch_edit` + "`" + ` |

**CLI fallback (no MCP):** every tool above is reachable from a shell as ` + "`" + `gortex call <tool> --arg k=v` + "`" + ` (e.g. ` + "`" + `gortex call read_file --arg path=<file>` + "`" + `) — there is no bare ` + "`" + `gortex <tool>` + "`" + ` verb.

The graph narrows scope; read the real body with ` + "`" + `get_symbol_source` + "`" + ` before you change or depend on a symbol — especially behavior-critical code (migrations, retry / fallback, concurrency), where ` + "`" + `compress_bodies:true` + "`" + ` would elide the risky branches. ` + "`" + `format:"gcx"` + "`" + ` and ` + "`" + `compress_bodies:true` + "`" + ` exist on the read / list tools — the parameter legend is in the MCP server instructions.

**Memory workflow** (behavior-critical; the full triggers live in the global ` + "`" + `~/.claude/CLAUDE.md` + "`" + ` policy and in ` + "`" + `gortex://guide` + "`" + `): ` + "`" + `distill_session` + "`" + ` at session start; ` + "`" + `surface_memories` + "`" + ` right after ` + "`" + `smart_context` + "`" + `; ` + "`" + `save_note tags:"decision"` + "`" + ` at each decision; ` + "`" + `store_memory` + "`" + ` for durable invariants / gotchas the team should inherit; ` + "`" + `query_notes` + "`" + ` / ` + "`" + `query_memories` + "`" + ` before re-touching a symbol.

**Reference:** ` + "`" + `gortex://guide` + "`" + ` (or ` + "`" + `gortex guide [topic]` + "`" + `) carries the full detail — provider matrix, capabilities, analyze / search_ast catalogs, token-economy, MCP resources. The server publishes a lean tool preset eagerly; call ` + "`" + `tools_search` + "`" + ` to discover and load any other tool by keyword.
`

// AppendInstructions appends body to path, creating the file if
// missing. Idempotent: when `sentinel` is already present anywhere in
// the file we skip with ActionSkip and log the reason. Callers pass
// the adapter's ApplyOpts through so --dry-run / --global / --force
// all flow to the right FileAction status.
//
// Not atomic. Rules files are plaintext a human edits, matching the
// historical CLAUDE.md append behaviour — a concurrent external writer
// during init is extraordinarily unlikely and atomic rename of a file
// a human is editing would fight their editor.
func AppendInstructions(w io.Writer, path, body, sentinel string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	if existed && strings.Contains(string(existing), sentinel) {
		if w != nil {
			fmt.Fprintf(w, "[gortex init] skip %s (Gortex block already present)\n", path)
		}
		return FileAction{Path: path, Action: ActionSkip, Reason: "block-present"}, nil
	}

	if opts.DryRun {
		action := ActionWouldMerge
		if !existed {
			action = ActionWouldCreate
		}
		return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
	}

	// Two blank lines between existing content and the block so the
	// appended section reads as a separate document and doesn't glue
	// onto the last paragraph the user wrote.
	prefix := ""
	if existed && len(existing) > 0 {
		prefix = "\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return FileAction{}, err
	}
	defer f.Close()
	if _, err := f.WriteString(prefix + body); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		fmt.Fprintf(w, "[gortex init] appended Gortex block to %s\n", path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
}

// CursorMDCFrontmatter wraps the instructions body in the YAML
// frontmatter Cursor expects for MDC rules files. Cursor reads
// `alwaysApply: true` rules on every chat turn — which is what we
// want for the MANDATORY-prefer-Gortex block.
//
// Kept separate from AppendInstructions because MDC files are
// one-rule-per-file (Cursor owns the filename, not the content), so
// they use WriteIfNotExists semantics, not append.
func CursorMDCFrontmatter(body string) string {
	return `---
description: Gortex code intelligence — prefer graph tools over file reads
alwaysApply: true
---

` + body
}

// UpsertMarkedBlock writes `body` into `path` between `startMarker`
// and `endMarker`. Unlike AppendInstructions, this is idempotent AND
// regeneratable: if the markers already exist the block between them
// is replaced; otherwise the block is appended with a blank-line gap
// to existing content. If `body` is empty and the markers exist, the
// block is removed (migration use case). Creates the file if missing.
//
// Designed for the per-repo community-routing block which regenerates
// on every `gortex init` run as the graph evolves.
func UpsertMarkedBlock(w io.Writer, path, body, startMarker, endMarker string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	text := ""
	if existed {
		text = string(existing)
	}

	hasBlock := existed && strings.Contains(text, startMarker) && strings.Contains(text, endMarker)
	empty := strings.TrimSpace(body) == ""

	// Nothing to do: empty body and no existing block.
	if empty && !hasBlock {
		return FileAction{Path: path, Action: ActionSkip, Reason: "no-communities"}, nil
	}

	fenced := startMarker + "\n" + body + "\n" + endMarker + "\n"

	var next string
	switch {
	case hasBlock:
		start := strings.Index(text, startMarker)
		end := strings.Index(text, endMarker) + len(endMarker)
		// Trim trailing newline after the end marker so we don't
		// accumulate blank lines on repeated re-runs.
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if empty {
			next = text[:start] + text[end:]
		} else {
			next = text[:start] + fenced + text[end:]
		}
	case !existed:
		next = fenced
	default:
		prefix := ""
		if len(text) > 0 {
			if !strings.HasSuffix(text, "\n") {
				prefix = "\n\n"
			} else if !strings.HasSuffix(text, "\n\n") {
				prefix = "\n"
			}
		}
		next = text + prefix + fenced
	}

	// Skip when the file would end up byte-identical to what's
	// already there — important for AssertIdempotent semantics and
	// for avoiding spurious mtime bumps on `gortex init` re-runs
	// when the graph hasn't changed.
	if existed && next == text {
		return FileAction{Path: path, Action: ActionSkip, Reason: "unchanged"}, nil
	}

	if opts.DryRun {
		switch {
		case !existed:
			return FileAction{Path: path, Action: ActionWouldCreate, Keys: []string{"communities-block"}}, nil
		case hasBlock:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		default:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		verb := "updated"
		if !existed {
			verb = "wrote"
		}
		fmt.Fprintf(w, "[gortex init] %s %s (communities block)\n", verb, path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"communities-block"}}, nil
}
