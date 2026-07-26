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
	Path       string                   `json:"path"`
	Present    bool                     `json:"present"`
	WindowRows int                      `json:"window_rows"`
	TotalRows  int                      `json:"total_rows"`
	Events     map[string]EventActivity `json:"events"`
}

// Ran reports whether the event was invoked at least once in the window.
func (s EffectivenessSummary) Ran(event string) bool {
	return s.Events[event].Runs > 0
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
	summary := EffectivenessSummary{Path: path, Events: map[string]EventActivity{}}
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
			activity.Runs++
			activity.TotalMS += rec.DurationMS
			if rec.EmittedContext {
				activity.Emitted++
			}
			if rec.DaemonReachable != nil {
				activity.DaemonKnown++
				if *rec.DaemonReachable {
					activity.DaemonUp++
				}
			}
		}
		summary.Events[event] = activity
	}
	return summary, nil
}
