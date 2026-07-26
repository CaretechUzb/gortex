package doctor

import (
	"fmt"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/hooks"
)

// Severity orders findings by how much they cost the user.
type Severity string

const (
	// SeverityBlocker means the integration is not working. Doctor exits
	// non-zero when one is present so CI can gate on it.
	SeverityBlocker Severity = "BLOCKER"
	// SeverityWarn means it works but is degraded or half-wired.
	SeverityWarn Severity = "WARN"
	SeverityOK   Severity = "OK"
	SeverityInfo Severity = "INFO"
)

// Finding is one diagnosis. Remedy is the exact command or step that fixes
// it — a finding the reader cannot act on is a complaint, not a diagnosis.
type Finding struct {
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary"`
	Remedy   string   `json:"remedy,omitempty"`
}

// agentActivity is one harness's slice of the invocation log, so the finding
// rules below cannot accidentally read another agent's rows.
type agentActivity struct {
	events map[string]hooks.EventActivity
}

// AgentHooks is what an adapter's config declares, per lifecycle event.
type AgentHooks struct {
	// Agent is the adapter name ("codex").
	Agent string
	// Configured counts declared hook entries per event name.
	Configured map[string]int
	// RequiresTrust marks agents that gate hook execution behind an explicit
	// per-hook approval, where "declared but never ran" is the expected
	// signature of an unapproved hook rather than a broken install.
	RequiresTrust bool
	// TrustRemedy is how the user approves them.
	TrustRemedy string
	// InstructionsPath is the agent's user-level rules file, and
	// InstructionsWired reports whether Gortex's block is in it.
	InstructionsPath  string
	InstructionsWired bool
	// InstructionsShadowed is set when the agent will read a different file
	// instead, making the block above dead text.
	InstructionsShadowed string
}

// Diagnose turns the collected evidence into ordered findings, most
// actionable first. It takes plain data so the whole decision table is
// testable without a machine that has Codex, hooks, or a daemon on it.
func Diagnose(agent AgentHooks, summary hooks.EffectivenessSummary, adoption Adoption, now time.Time) []Finding {
	var findings []Finding
	// Scope the evidence to this harness. On a machine running more than one
	// agent the union would let a busy Claude Code session vouch for hooks
	// Codex is skipping — exactly the failure doctor exists to catch.
	activity := agentActivity{events: summary.ForAgent(agent.Agent)}
	add := func(sev Severity, remedy, format string, args ...any) {
		findings = append(findings, Finding{Severity: sev, Summary: fmt.Sprintf(format, args...), Remedy: remedy})
	}

	var configured int
	events := make([]string, 0, len(agent.Configured))
	for event, count := range agent.Configured {
		configured += count
		if count > 0 {
			events = append(events, event)
		}
	}
	sort.Strings(events)

	var ran int
	for _, event := range events {
		ran += activity.events[event].Runs
	}

	switch {
	case configured == 0:
		add(SeverityWarn, "gortex install",
			"%s has no Gortex lifecycle hooks configured.", agent.Agent)
	case ran == 0 && agent.RequiresTrust:
		// The headline case. Every declared hook is on disk and none has
		// run, which for a trust-gating agent means the hooks were never
		// approved — or were re-approved away by an upgrade that changed
		// their definitions, since trust is recorded against the hook hash.
		add(SeverityBlocker, agent.TrustRemedy,
			"%d %s hook(s) are configured but none has run in this window — %s skips new or changed hooks until they are trusted.",
			configured, agent.Agent, agent.Agent)
	case ran == 0:
		add(SeverityBlocker, "check the agent's hook configuration and that `gortex` is on its PATH",
			"%d %s hook(s) are configured but none has run in this window.", configured, agent.Agent)
	}

	// A per-event zero while other events ran is a narrower version of the
	// same problem: one hook's definition changed, so only its trust lapsed.
	if ran > 0 {
		for _, event := range events {
			stats := activity.events[event]
			if stats.Runs > 0 {
				continue
			}
			remedy := "re-run `gortex install`"
			if agent.RequiresTrust {
				remedy = agent.TrustRemedy
			}
			add(SeverityBlocker, remedy,
				"%s is configured but never ran, while other hooks did%s.", event, lastSeenSuffix(stats.LastSeen, now))
		}
	}

	for _, event := range events {
		stats := activity.events[event]
		if stats.Runs == 0 {
			continue
		}
		if stats.DaemonKnown > 0 && stats.DaemonUp == 0 {
			add(SeverityBlocker, "gortex daemon start --detach",
				"%s ran %d time(s) and never reached the daemon.", event, stats.Runs)
			continue
		}
		if stats.Emitted == 0 {
			add(SeverityWarn, "",
				"%s ran %d time(s) and injected context 0 times — it fires but has nothing to say.", event, stats.Runs)
		}
	}

	if agent.InstructionsShadowed != "" {
		add(SeverityWarn, fmt.Sprintf("move the Gortex block into %s, or remove it", agent.InstructionsShadowed),
			"%s reads %s instead of %s, so any rule block in the latter is ignored.",
			agent.Agent, agent.InstructionsShadowed, agent.InstructionsPath)
	} else if agent.InstructionsPath != "" && !agent.InstructionsWired {
		add(SeverityWarn, "gortex install",
			"No Gortex rule block in %s — the agent loads that file into every session, so without it a skipped or silent hook leaves the session with no standing rule.",
			agent.InstructionsPath)
	}

	switch share := adoption.GortexShare(); {
	case adoption.FilesFound == 0:
		add(SeverityInfo, "", "No %s session transcripts found — adoption could not be measured.", agent.Agent)
	case share < 0:
		add(SeverityInfo, "", "No tool calls in the window — adoption could not be measured.")
	case adoption.GortexCalls == 0:
		add(SeverityBlocker, "gortex install",
			"Across %d session(s) the model made %d shell call(s) and zero Gortex tool calls.",
			adoption.InWindow, adoption.ShellCalls)
	case share < 50:
		add(SeverityWarn, "",
			"Gortex tools are %.0f%% of tool calls (%d Gortex vs %d shell, %d read/search shaped).",
			share, adoption.GortexCalls, adoption.ShellCalls, adoption.ShellReads)
	default:
		add(SeverityOK, "",
			"Gortex tools are %.0f%% of tool calls (%d Gortex vs %d shell).",
			share, adoption.GortexCalls, adoption.ShellCalls)
	}

	sort.SliceStable(findings, func(i, j int) bool {
		return severityRank(findings[i].Severity) < severityRank(findings[j].Severity)
	})
	return findings
}

// HasBlocker reports whether any finding is a blocker — the command's exit
// status.
func HasBlocker(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityBlocker {
			return true
		}
	}
	return false
}

func severityRank(s Severity) int {
	switch s {
	case SeverityBlocker:
		return 0
	case SeverityWarn:
		return 1
	case SeverityInfo:
		return 2
	default:
		return 3
	}
}

// lastSeenSuffix turns a never/stale timestamp into a clause, so "never ran"
// can distinguish "not in this window" from "not once, ever".
func lastSeenSuffix(last, now time.Time) string {
	if last.IsZero() {
		return " (not once, ever)"
	}
	days := int(now.Sub(last).Hours() / 24)
	if days <= 0 {
		return ""
	}
	return fmt.Sprintf(" (last ran %dd ago)", days)
}
