package profiles

import (
	"fmt"
	"strings"
)

// The instruction bodies are composed from the shared sections below —
// no profile hand-authors its own copy of a section. Body text uses §
// as a backtick placeholder (bt renders it) so markdown tables stay
// readable inside Go string literals.

// bt renders § as a backtick.
func bt(s string) string { return strings.ReplaceAll(s, "§", "`") }

// sectionHeader renders the mandatory-rule opener every profile
// keeps: the MUST-prefer statement and the deny-hook warning are the
// two positioning cues that buy tool adoption, so they survive even
// the lean rendering — lean only condenses the prose around them. The
// heading doubles as the idempotency sentinel other adapters check
// (agents.InstructionsSentinel).
func sectionHeader(lean bool) string {
	if lean {
		return bt(`## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A machine-wide §gortex§ MCP server indexes this code. You MUST prefer graph queries over file reads — PreToolUse hooks deny §Read§ / §Grep§ / §Glob§ on indexed source, and the deny message names the right tool.

`)
	}
	return bt(`## MANDATORY: Use Gortex MCP tools instead of Read/Grep/Glob

A Gortex daemon is configured machine-wide via the §gortex§ MCP server. Whenever you operate on indexed source (any repo the daemon tracks — check §gortex daemon status§), you MUST prefer graph queries over file reads. PreToolUse hooks deny §Read§ / §Grep§ / §Glob§ against indexed source — the deny message names the right tool.

`)
}

// sectionFullRuleTable is the classic nine-row instead-of table used
// by the core and full profiles.
var sectionFullRuleTable = bt(`| Instead of...                       | Use...                                   |
|-------------------------------------|------------------------------------------|
| §Grep§ / §grep§ / §rg§ for a symbol | §search_symbols§ (BM25 + camelCase-aware) |
| §Grep§ for references               | §find_usages§ (zero false positives)     |
| Reading / grepping to find callers  | §get_callers§ / §get_call_chain§         |
| §Glob§ over source files            | §get_repo_outline§ / §search_symbols§    |
| §Read§ a file for one symbol        | §get_symbol_source§ (§compress_bodies:true§ for the signature only) |
| §Read§ to understand a file         | §get_file_summary§ / §get_editing_context§ |
| §Read§ a non-indexed / raw file     | §read_file§                              |
| Multiple reads to explore a task    | §smart_context§ (one call)               |
| §Edit§ / §Write§ source             | §edit_file§ / §write_file§ / §edit_symbol§ / §rename_symbol§ / §batch_edit§ |

`)

// sectionCLIFallback is the shell fallback line. Load-bearing for
// harnesses that mount no MCP tools — present in every profile.
var sectionCLIFallback = bt(`**CLI fallback (no MCP):** every tool above is reachable from a shell as §gortex call <tool> --arg k=v§ (e.g. §gortex call read_file --arg path=<file>§) — there is no bare §gortex <tool>§ verb.

`)

// sectionReadDiscipline is the scope-vs-fidelity paragraph.
var sectionReadDiscipline = bt(`Graph queries *narrow scope*; they do not replace reading the implementation. For the symbol you are about to change or depend on — especially behavior-critical code (migrations, retry / fallback / error-recovery paths, concurrency, compatibility shims) — read the real body with §get_symbol_source§ and do NOT pass §compress_bodies:true§, which elides the branches that carry the risk. §format:"gcx"§ (compact wire, ~27% fewer tokens) and §compress_bodies:true§ (body-eliding) exist on the read / list tools; the parameter legend is in the MCP server instructions.

`)

// sectionMemoryFull is the full memory-workflow section (core / full).
var sectionMemoryFull = bt(`## MANDATORY: Session + development memory

The graph remembers code; these tools remember **why you made a call**. They are behavior-critical — run each at its trigger, not "optionally":

- **Session start / after a compaction** — call §distill_session§ first: prior top symbols, pinned notes, decisions, recent excerpts. Seed your mental model before reading any file.
- **Immediately after §smart_context§** — call §surface_memories task:"<task>" symbol_ids:"<top hits>"§: cross-session invariants / gotchas / decisions anchored to your working set. If it returns nothing, don't probe further.
- **At every decision** — pick an approach, reject an alternative, hit a non-obvious constraint, or commit to an invariant → §save_note tags:"decision" body:"<what+why>"§. Mention symbol IDs (§pkg/foo.go::Bar§) in the body for auto-linking; §pinned:true§ for load-bearing notes.
- **When you learn a durable fact worth teaching the team** — §store_memory kind:"<invariant|gotcha|convention|decision|constraint|incident>" body:"<what+why>" symbol_ids:"<id>" importance:5§. §save_note§ is the per-session scratchpad; §store_memory§ is the workspace-wide store every future agent inherits. Supersede a stale memory with §supersedes:"<old-id>"§.
- **Before editing a symbol you've touched before** — §query_notes symbol_id:"<id>"§ / §query_memories symbol_id:"<id>"§ surface prior decisions and "do not change this without …" warnings.

**Save / store:** decisions, non-obvious constraints, invariants, follow-ups, incident learnings, bug reproductions. **Skip:** play-by-play the diff already shows, anything derivable from the graph, content already in CLAUDE.md.

`)

// sectionMemoryLean is the one-paragraph memory trio for the lean
// profile — the triggers survive, the elaboration moves to the guide.
var sectionMemoryLean = bt(`**Memory (mandatory):** §distill_session§ at session start; §surface_memories task:"<task>"§ right after §smart_context§; §save_note tags:"decision" body:"<what+why>"§ at every decision; §store_memory§ for durable invariants / gotchas the team should inherit; §query_notes§ / §query_memories§ before re-touching a symbol.

`)

// switchBullet renders the instruction-profile discovery line — the
// line every profile carries so a switched-down machine can always
// find its way back. The lean rendering keeps the verb and the
// next-session caveat, dropping only the roster prose.
func switchBullet(active string, lean bool) string {
	if lean {
		return bt(fmt.Sprintf(`- **Profiles:** active §%s§. Broader guidance: §gortex instructions switch core§ (or §full§; §list§ shows all) — NEW sessions only.
`, active))
	}
	return bt(fmt.Sprintf(`- **Instruction profiles** — this block is the active §%s§ profile. §gortex instructions list§ shows the others (§core§ balanced default · §localization§ lean · §full§ maximum guidance); switch with §gortex instructions switch <name>§ — applies to NEW sessions only (instructions, tools/list, and skills all load at session start).
`, active))
}

// sectionDiscovery builds the reference-and-discovery section shared
// by core and full; surfaceLine describes what ships eagerly.
func sectionDiscovery(active, surfaceLine string) string {
	return bt(`## Reference and discovery

- **§gortex://guide§ resource** (or the §gortex guide [topic]§ CLI) is the full reference: LLM-provider matrix, capabilities catalog, analyze / search_ast catalogs, token-economy detail, MCP resources, session-start checklist. Read it on demand — it is not pre-paid here.
- `) + surfaceLine + bt(`- The SessionStart hook injects daemon status. "daemon is not running" → run §gortex daemon start --detach§; "cwd is not covered by any tracked repo" → graph tools are unavailable there.
`) + switchBullet(active, false)
}

var coreSurfaceLine = bt(`**§tools_search§** — the server publishes a lean tool preset eagerly and defers the rest; call §tools_search§ to discover and load any tool by keyword (every tool stays callable by name).
`)

var fullSurfaceLine = bt(`**§tools_search§** — under this profile the server publishes the full documented dev-cycle preset (~34 workhorse tools) eagerly; the long tail still loads by keyword via §tools_search§ (every tool stays callable by name).
`)

// coreBody is the balanced default: today's slim policy core plus the
// profile-switch discovery line.
func coreBody() string {
	return sectionHeader(false) +
		sectionFullRuleTable +
		sectionCLIFallback +
		sectionReadDiscipline +
		sectionMemoryFull +
		sectionDiscovery("core", coreSurfaceLine)
}

// fullBody differs from core only in the eager-surface description —
// the ToolPreset column of the table is what actually widens the
// surface.
func fullBody() string {
	return sectionHeader(false) +
		sectionFullRuleTable +
		sectionCLIFallback +
		sectionReadDiscipline +
		sectionMemoryFull +
		sectionDiscovery("full", fullSurfaceLine)
}

// localizationRow maps an eager tool to its instead-of table row.
// Tools cued outside the table (the one-shot opener and the liveness
// check) are listed in localizationNonTableTools; the body test
// asserts every eager tool is covered by one of the two so the preset
// and the body cannot drift.
type localizationRow struct {
	tool, instead, use string
}

var localizationRows = []localizationRow{
	{"search_symbols", "§Grep§ / §rg§ for a symbol", "§search_symbols§ (BM25 + camelCase-aware)"},
	{"search_text", "§Grep§ for a literal / regex", "§search_text§ (trigram-indexed)"},
	{"find_usages", "§Grep§ for references", "§find_usages§ (zero false positives)"},
	{"get_callers", "Reading to find callers", "§get_callers§"},
	{"find_implementations", "Hunting interface implementors", "§find_implementations§"},
	{"get_symbol_source", "§Read§ a file for one symbol", "§get_symbol_source§"},
	{"get_file_summary", "§Read§ to understand a file", "§get_file_summary§"},
	{"read_file", "§Read§ a non-indexed / raw file", "§read_file§"},
}

// localizationNonTableTools are eager tools cued in prose rather than
// table rows.
var localizationNonTableTools = map[string]bool{
	"smart_context": true, // the open-with-one-call cue
	"index_health":  true, // the liveness line in discovery
}

func localizationTable() string {
	var sb strings.Builder
	sb.WriteString("| Instead of...                  | Use...                          |\n")
	sb.WriteString("|--------------------------------|---------------------------------|\n")
	for _, r := range localizationRows {
		fmt.Fprintf(&sb, "| %s | %s |\n", bt(r.instead), bt(r.use))
	}
	sb.WriteString("\n")
	return sb.String()
}

// localizationBody is the lean profile: every positioning cue (MUST
// rule, deny warning, one-shot opener, memory triggers, discovery /
// switch-back path) survives; reference elaboration moves to
// gortex://guide.
func localizationBody() string {
	return sectionHeader(true) +
		bt(`**Open every localization task with one call:** §smart_context task:"<what you are looking for>"§ — candidate symbols, source, and callers in one shot. Go targeted from its results instead of re-searching.

`) +
		localizationTable() +
		bt(`Read the real body (§get_symbol_source§, without §compress_bodies§) before you change or depend on a symbol. **No MCP mounted?** Any tool works from a shell: §gortex call <tool> --arg k=v§.

`) +
		sectionMemoryLean +
		bt(`**Discovery:** lean §localization§ profile — the tools above plus §index_health§ (liveness) ship eagerly; anything else loads by name via §tools_search§. Full reference: §gortex://guide§.
`) + switchBullet("localization", true)
}
