package hooks

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// Reading side of the effectiveness log. It lives next to the writer so the
// record shape has exactly one definition — a diagnostic that silently
// mis-parses its own telemetry is worse than no diagnostic.
//
// The log is the only evidence that separates "hook installed" from "hook
// running": every invocation appends a row whether or not it had anything to
// inject, so an event with zero rows did not run. That distinction is
// invisible in an agent's config file, which is what makes an untrusted or
// disabled hook look like a working one.

// EffectivenessLogPath is where hook invocations are recorded.
func EffectivenessLogPath() string { return hookEffectivenessPath() }

// EventActivity is one event's rollup over the requested window.
type EventActivity struct {
	Event string `json:"event"`
	// Runs counts invocations; Emitted counts the subset that injected
	// context. Runs>0 with Emitted==0 is a hook that fires and has nothing
	// to say — a different problem from one that never fires.
	Runs    int `json:"runs"`
	Emitted int `json:"emitted"`
	// DaemonKnown counts rows that reported reachability at all; hooks doing
	// no daemon I/O omit it rather than report a false "down".
	DaemonKnown int `json:"daemon_known"`
	DaemonUp    int `json:"daemon_up"`
	// TotalMS across Runs, for a mean the caller can render.
	TotalMS  int64     `json:"total_ms"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// EffectivenessSummary is the whole log reduced to per-event rollups.
// LastSeen is tracked across the entire log, not just the window, so a
// caller can say "last ran 9 days ago" instead of only "not in window".
type EffectivenessSummary struct {
	Path       string `json:"path"`
	Present    bool   `json:"present"`
	WindowRows int    `json:"window_rows"`
	TotalRows  int    `json:"total_rows"`
	// Events is the union across every agent — the right denominator for
	// "is anything running on this machine at all".
	Events map[string]EventActivity `json:"events"`
	// ByAgent partitions the same rows by harness, so a machine running two
	// agents can be asked about one of them.
	ByAgent map[string]map[string]EventActivity `json:"by_agent,omitempty"`
	// Attributed is true once any row in the window carries an agent. Rows
	// written before the field existed do not, and a summary of only those
	// cannot answer per-agent questions at all.
	Attributed bool `json:"attributed"`
	// UnattributedRows counts window rows with no agent, so a caller can say
	// how much of its evidence predates attribution.
	UnattributedRows int `json:"unattributed_rows"`
}

// Ran reports whether the event was invoked at least once in the window,
// across every agent.
func (s EffectivenessSummary) Ran(event string) bool {
	return s.Events[event].Runs > 0
}

// ForAgent returns the rollups attributable to one harness.
//
// The fallback is deliberately asymmetric, because the two unknowns are not
// equally safe. When the log carries no attribution at all — every row
// predates the agent field — the union is returned: it over-reports this
// agent, but a caller that concludes "these hooks never ran" from a log that
// simply cannot say would be manufacturing a blocker out of missing metadata.
// When the log *is* attributed and this agent has no rows, that is real
// evidence of silence and is returned as such.
func (s EffectivenessSummary) ForAgent(agent string) map[string]EventActivity {
	if !s.Attributed {
		return s.Events
	}
	if events, ok := s.ByAgent[agent]; ok {
		return events
	}
	return map[string]EventActivity{}
}

// accumulate folds one record into a rollup. Shared by the union and the
// per-agent partition so the two can never disagree about what a row means.
func accumulate(into *EventActivity, rec hookEffectiveness) {
	into.Runs++
	into.TotalMS += rec.DurationMS
	if rec.EmittedContext {
		into.Emitted++
	}
	if rec.DaemonReachable != nil {
		into.DaemonKnown++
		if *rec.DaemonReachable {
			into.DaemonUp++
		}
	}
}

// AgentNames lists the harnesses seen in the window, sorted.
func (s EffectivenessSummary) AgentNames() []string {
	out := make([]string, 0, len(s.ByAgent))
	for name := range s.ByAgent {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// EventNames lists the events present in the summary, sorted.
func (s EffectivenessSummary) EventNames() []string {
	out := make([]string, 0, len(s.Events))
	for name := range s.Events {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ReadEffectiveness aggregates the log, counting rows with ts >= since. A
// missing log is not an error: it means no hook has ever run on this machine,
// which is itself the answer the caller is looking for.
//
// Malformed lines are skipped rather than failing the read — the log is
// append-only from short-lived processes, so a torn final line during a crash
// must not blind the diagnostic to the 40,000 good rows before it.
func ReadEffectiveness(since time.Time) (EffectivenessSummary, error) {
	path := EffectivenessLogPath()
	summary := EffectivenessSummary{
		Path:    path,
		Events:  map[string]EventActivity{},
		ByAgent: map[string]map[string]EventActivity{},
	}
	if path == "" {
		return summary, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return summary, nil
		}
		return summary, err
	}
	summary.Present = true

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec hookEffectiveness
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		summary.TotalRows++
		stamp, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if err != nil {
			continue
		}
		event := rec.Event
		if event == "" {
			event = "?"
		}
		activity := summary.Events[event]
		activity.Event = event
		if stamp.After(activity.LastSeen) {
			activity.LastSeen = stamp
		}
		if !stamp.Before(since) {
			summary.WindowRows++
			accumulate(&activity, rec)
			if rec.Agent == "" {
				summary.UnattributedRows++
			} else {
				summary.Attributed = true
				perAgent := summary.ByAgent[rec.Agent]
				if perAgent == nil {
					perAgent = map[string]EventActivity{}
					summary.ByAgent[rec.Agent] = perAgent
				}
				scoped := perAgent[event]
				scoped.Event = event
				if stamp.After(scoped.LastSeen) {
					scoped.LastSeen = stamp
				}
				accumulate(&scoped, rec)
				perAgent[event] = scoped
			}
		}
		summary.Events[event] = activity
	}
	return summary, nil
}
