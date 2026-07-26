package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/doctor"
	"github.com/zzet/gortex/internal/hooks"
	"github.com/zzet/gortex/internal/savings"
)

// doctor_runtime.go is the half of `gortex doctor` that reads evidence rather
// than configuration: did the hooks run, is the agent calling Gortex tools,
// and what did the savings ledger record. Everything here is read-only and
// degrades to "no data" — doctor has to work on exactly the broken machines
// that make people run it.

// errDoctorBlocker makes the command exit non-zero when a blocker is found.
// SilenceUsage/SilenceErrors on the command keep cobra from printing it: the
// findings block is the message.
var errDoctorBlocker = errors.New("doctor found a blocking problem")

type doctorRuntime struct {
	Window   string                     `json:"window"`
	Since    time.Time                  `json:"since"`
	Redacted bool                       `json:"redacted"`
	Codex    codex.InstallState         `json:"codex"`
	Hooks    hooks.EffectivenessSummary `json:"hooks"`
	Adoption doctor.Adoption            `json:"adoption"`
	Savings  doctorSavings              `json:"savings"`
	Findings []doctor.Finding           `json:"findings"`
}

type doctorSavings struct {
	Available bool             `json:"available"`
	Error     string           `json:"error,omitempty"`
	Events    int              `json:"events"`
	Saved     int64            `json:"saved"`
	Returned  int64            `json:"returned"`
	NoClient  int              `json:"events_without_client"`
	NoModel   int              `json:"events_without_model"`
	ByTool    []doctorDimTotal `json:"by_tool,omitempty"`
	ByClient  []doctorDimTotal `json:"by_client,omitempty"`
	ByModel   []doctorDimTotal `json:"by_model,omitempty"`
}

type doctorDimTotal struct {
	Name  string `json:"name"`
	Calls int64  `json:"calls"`
	Saved int64  `json:"saved"`
}

func collectRuntime(home string, days int) doctorRuntime {
	if days < 1 {
		days = 1
	}
	now := time.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)

	state := codex.Inspect(home)
	if doctorRedact {
		state.ConfigPath = doctorPath(state.ConfigPath)
		state.InstructionsPath = doctorPath(state.InstructionsPath)
		state.ShadowedBy = doctorPath(state.ShadowedBy)
	}
	activity, err := hooks.ReadEffectiveness(since)
	if err != nil {
		// A log we cannot read is not fatal; the other sections still carry
		// signal, and the empty summary renders as "no hook has ever run".
		activity.Path = hooks.EffectivenessLogPath()
	}
	adoption := doctor.ScanCodexSessions(doctor.CodexHome(), since, 10)

	out := doctorRuntime{
		Window:   fmt.Sprintf("last %dd", days),
		Since:    since,
		Redacted: doctorRedact,
		Codex:    state,
		Hooks:    activity,
		Adoption: adoption,
		Savings:  collectSavings(since),
	}
	out.Findings = doctor.Diagnose(doctor.AgentHooks{
		Agent:                codex.Name,
		Configured:           state.Hooks,
		RequiresTrust:        true,
		TrustRemedy:          codex.TrustRemedy,
		InstructionsPath:     state.InstructionsPath,
		InstructionsWired:    state.InstructionsWired,
		InstructionsShadowed: state.ShadowedBy,
	}, activity, adoption, now)

	if doctorRedact {
		out.Adoption = redactAdoption(out.Adoption)
		out.Hooks.Path = doctorPath(out.Hooks.Path)
	}
	return out
}

func collectSavings(since time.Time) doctorSavings {
	out := doctorSavings{}
	store, err := savings.Open(savings.DefaultDBPath())
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer store.Close()
	out.Available = true

	events, err := store.EventsSince(since)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Events = len(events)
	for _, e := range events {
		out.Saved += e.Saved
		out.Returned += e.Returned
		if e.Client == "" {
			out.NoClient++
		}
		if e.Model == "" {
			out.NoModel++
		}
	}
	if tools, err := store.ToolTotals(since); err == nil {
		for _, t := range tools {
			out.ByTool = append(out.ByTool, doctorDimTotal{Name: t.Tool, Calls: t.CallsCounted, Saved: t.TokensSaved})
		}
	}
	if clients, err := store.ClientTotals(since); err == nil {
		for _, c := range clients {
			out.ByClient = append(out.ByClient, doctorDimTotal{Name: c.Name, Calls: c.CallsCounted, Saved: c.TokensSaved})
		}
	}
	if models, err := store.ModelTotals(since); err == nil {
		for _, m := range models {
			out.ByModel = append(out.ByModel, doctorDimTotal{Name: m.Name, Calls: m.CallsCounted, Saved: m.TokensSaved})
		}
	}
	return out
}

// redactAdoption hashes the workspace identifiers so the report can be pasted
// into an issue. Tool names, counts, and timings are never sensitive and are
// always left intact — redacting them would defeat the point of sharing it.
func redactAdoption(a doctor.Adoption) doctor.Adoption {
	a.Root = doctorPath(a.Root)
	for i := range a.Sessions {
		a.Sessions[i].CWD = doctorPath(a.Sessions[i].CWD)
		a.Sessions[i].Branch = redactName(a.Sessions[i].Branch)
	}
	return a
}

// redactName hashes a value that is not a path — a branch name can carry a
// ticket id or a customer name just as readily as a repo path can.
func redactName(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "<" + hex.EncodeToString(sum[:])[:8] + ">"
}

func printDoctorRuntime(w io.Writer, r doctorRuntime) {
	fmt.Fprintf(w, "Gortex doctor — runtime evidence (%s):\n\n", r.Window)

	fmt.Fprintln(w, "  codex install")
	if !r.Codex.ConfigPresent {
		fmt.Fprintf(w, "    %s %s missing\n", glyphCross, r.Codex.ConfigPath)
	} else {
		fmt.Fprintf(w, "    %s mcp_servers.gortex\n", mark(r.Codex.MCPServer))
		for _, event := range codex.HookEvents {
			fmt.Fprintf(w, "    %s hook %-17s %d configured\n", mark(r.Codex.Hooks[event] > 0), event, r.Codex.Hooks[event])
		}
	}
	fmt.Fprintf(w, "    %s rule block in %s\n", mark(r.Codex.InstructionsWired), r.Codex.InstructionsPath)
	if r.Codex.ShadowedBy != "" {
		fmt.Fprintf(w, "    %s shadowed by %s\n", glyphCross, r.Codex.ShadowedBy)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "  hook activity   (did %s's hook processes actually run?)\n", codex.Name)
	if !r.Hooks.Present {
		fmt.Fprintf(w, "    no %s — no hook has ever run on this machine\n", r.Hooks.Path)
	} else {
		scoped := r.Hooks.ForAgent(codex.Name)
		fmt.Fprintf(w, "    %-18s %7s %9s %10s %8s  %s\n", "event", "runs", "injected", "daemon-up", "unattrib", "last seen")
		for _, event := range doctorEventOrder(r.Hooks) {
			stats := scoped[event]
			daemon := "n/a"
			if stats.DaemonKnown > 0 {
				daemon = fmt.Sprintf("%d/%d", stats.DaemonUp, stats.DaemonKnown)
			}
			// A zero in `runs` is only alarming when `unattrib` is zero too;
			// showing them side by side is what keeps the reader from
			// reaching the conclusion the findings deliberately withheld.
			ambiguous := r.Hooks.AmbiguousRuns(event)
			seen := stats.LastSeen
			if seen.IsZero() {
				// The event did run, we just cannot say for whom — dating it
				// "never" would contradict the count beside it.
				seen = r.Hooks.Unattributed[event].LastSeen
			}
			last := "never"
			if !seen.IsZero() {
				last = seen.Format(time.RFC3339)[:19]
			}
			fmt.Fprintf(w, "    %-18s %7d %9d %10s %8d  %s\n", event, stats.Runs, stats.Emitted, daemon, ambiguous, last)
		}
		fmt.Fprintf(w, "    %d row(s) in window, %d in the whole log\n", r.Hooks.WindowRows, r.Hooks.TotalRows)
		if agents := r.Hooks.AgentNames(); len(agents) > 0 {
			fmt.Fprintf(w, "    agents seen                %s\n", joinComma(agents))
		}
		if r.Hooks.UnattributedRows > 0 {
			// Rows written before the agent field existed cannot be assigned
			// to anyone. They are held out of every agent's counts rather
			// than shared into all of them, and a `runs` of 0 beside a
			// non-zero `unattrib` means "cannot tell", not "did not run".
			fmt.Fprintf(w, "    note: %d row(s) predate per-agent attribution (the unattrib column).\n", r.Hooks.UnattributedRows)
			fmt.Fprintln(w, "          They can neither confirm nor rule out an agent's hooks, so a 0 in")
			fmt.Fprintln(w, "          runs beside a non-zero unattrib is withheld from the findings.")
			fmt.Fprintln(w, "          They clear as the window rolls past the upgrade.")
		}
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "  codex adoption  (what the model actually called)")
	if r.Adoption.FilesFound == 0 {
		fmt.Fprintf(w, "    no session transcripts under %s\n", r.Adoption.Root)
	} else {
		fmt.Fprintf(w, "    sessions with tool calls   %d of %d on disk\n", r.Adoption.InWindow, r.Adoption.FilesFound)
		fmt.Fprintf(w, "    gortex MCP calls           %d\n", r.Adoption.GortexCalls)
		fmt.Fprintf(w, "    other MCP calls            %d\n", r.Adoption.OtherMCP)
		fmt.Fprintf(w, "    shell calls                %d (%d read/search shaped)\n", r.Adoption.ShellCalls, r.Adoption.ShellReads)
		if len(r.Adoption.Tools) > 0 {
			fmt.Fprintf(w, "    gortex tools used          %s\n", topCounts(r.Adoption.Tools, 8))
		}
		if len(r.Adoption.Models) > 0 {
			fmt.Fprintf(w, "    models                     %s\n", topCounts(r.Adoption.Models, 4))
		}
	}
	fmt.Fprintln(w, "    note: Codex does not persist hook-injected context into transcripts, so")
	fmt.Fprintln(w, "          this section cannot prove a hook fired — hook activity above does.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "  savings ledger")
	switch {
	case !r.Savings.Available:
		fmt.Fprintf(w, "    unavailable: %s\n", r.Savings.Error)
	case r.Savings.Events == 0:
		fmt.Fprintln(w, "    no recorded events in window")
	default:
		baseline := r.Savings.Saved + r.Savings.Returned
		pct := 0.0
		if baseline > 0 {
			pct = 100 * float64(r.Savings.Saved) / float64(baseline)
		}
		fmt.Fprintf(w, "    events                     %d\n", r.Savings.Events)
		fmt.Fprintf(w, "    saved / baseline           %d / %d (%.1f%%)\n", r.Savings.Saved, baseline, pct)
		fmt.Fprintf(w, "    without client / model     %d / %d\n", r.Savings.NoClient, r.Savings.NoModel)
		for label, rows := range map[string][]doctorDimTotal{
			"by tool": r.Savings.ByTool, "by client": r.Savings.ByClient, "by model": r.Savings.ByModel,
		} {
			for i, row := range rows {
				if i >= 6 {
					break
				}
				fmt.Fprintf(w, "    %-10s %-24s %6d calls  %10d saved\n", label, doctorTruncate(row.Name, 24), row.Calls, row.Saved)
				label = ""
			}
		}
	}
	fmt.Fprintln(w, "    note: only the read-family tools write here (saved = whole-file tokens −")
	fmt.Fprintln(w, "          returned). explore / search / relations / trace record nothing, and")
	fmt.Fprintln(w, "          reading a whole file records 0 — so this describes those calls only.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "  findings")
	for _, f := range r.Findings {
		glyph := "·"
		switch f.Severity {
		case doctor.SeverityBlocker:
			glyph = glyphCross
		case doctor.SeverityWarn:
			glyph = "!"
		case doctor.SeverityOK:
			glyph = glyphCheck
		}
		fmt.Fprintf(w, "    %s %-7s %s\n", glyph, f.Severity, f.Summary)
		if f.Remedy != "" {
			fmt.Fprintf(w, "              → %s\n", f.Remedy)
		}
	}
	if !r.Redacted {
		fmt.Fprintln(w, "\n  sharing this? re-run with --redact to hash repo paths and branch names.")
	}
	fmt.Fprintln(w)
}

// doctorEventOrder lists the adapter's own events first, in lifecycle order,
// then anything else the log saw — so the events a reader is looking for are
// never buried under LocalizationTerminal rows.
func doctorEventOrder(s hooks.EffectivenessSummary) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(s.Events)+len(codex.HookEvents))
	for _, event := range codex.HookEvents {
		out = append(out, event)
		seen[event] = true
	}
	for _, event := range s.EventNames() {
		if !seen[event] {
			out = append(out, event)
		}
	}
	return out
}

func topCounts(counts map[string]int, limit int) string {
	type kv struct {
		name string
		n    int
	}
	rows := make([]kv, 0, len(counts))
	for name, n := range counts {
		rows = append(rows, kv{name, n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("%s×%d", row.name, row.n))
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func doctorTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func mark(ok bool) string {
	if ok {
		return glyphCheck
	}
	return glyphCross
}

// silenceDoctorErrors keeps the blocker exit status from printing a second
// error line under the findings block.
func silenceDoctorErrors(cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}
}
