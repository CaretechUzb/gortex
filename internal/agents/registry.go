package agents

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry holds the set of adapters `gortex init` will run. Callers
// Register each adapter explicitly (from cmd/gortex/init.go) rather
// than via sub-package init() hooks so that unit tests can build a
// registry with a controlled subset without a global side-effect.
//
// Methods are safe for concurrent reads; Register should only be
// called during startup.
type Registry struct {
	mu       sync.RWMutex
	adapters []Adapter
	byName   map[string]Adapter
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Adapter)}
}

// Register adds an adapter. Panics on duplicate names — duplicates
// indicate a programming error (two packages registering the same
// name), not a user mistake, so a panic is appropriate.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := a.Name()
	if _, dup := r.byName[name]; dup {
		panic(fmt.Sprintf("agents.Registry: duplicate adapter name %q", name))
	}
	r.byName[name] = a
	r.adapters = append(r.adapters, a)
}

// All returns adapters in registration order. Callers should not
// mutate the returned slice.
func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, len(r.adapters))
	copy(out, r.adapters)
	return out
}

// Filter returns the subset of adapters selected by allow/skip lists.
// allowCSV:
//   - "" (empty) or "auto" selects every registered adapter (the
//     default: let Detect decide which ones run)
//   - comma-separated list of names selects only those
//   - any unknown name produces an error
//
// skipCSV removes names from the selection after allowCSV applies.
// Unknown skip names are also hard errors — silent skips mask typos.
//
// The returned slice preserves registration order so the user sees a
// stable run sequence.
func (r *Registry) Filter(allowCSV, skipCSV string) ([]Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	allow := parseCSV(allowCSV)
	skip := parseCSV(skipCSV)

	// Validate names up front; we'd rather report all typos at once.
	var unknown []string
	for _, n := range allow {
		if n == "auto" {
			continue
		}
		if _, ok := r.byName[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	for _, n := range skip {
		if _, ok := r.byName[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown agent name(s): %s; available: %s",
			strings.Join(unknown, ", "),
			strings.Join(r.sortedNamesLocked(), ", "))
	}

	// Build the allowed set. Empty allowCSV or the "auto" sentinel
	// means "every registered adapter" — Detect() then decides which
	// ones actually run.
	var allowSet map[string]struct{}
	autoMode := len(allow) == 0
	for _, n := range allow {
		if n == "auto" {
			autoMode = true
			continue
		}
	}
	if !autoMode {
		allowSet = make(map[string]struct{}, len(allow))
		for _, n := range allow {
			allowSet[n] = struct{}{}
		}
	}

	skipSet := make(map[string]struct{}, len(skip))
	for _, n := range skip {
		skipSet[n] = struct{}{}
	}

	out := make([]Adapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		name := a.Name()
		if allowSet != nil {
			if _, ok := allowSet[name]; !ok {
				continue
			}
		}
		if _, skipped := skipSet[name]; skipped {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (r *Registry) sortedNamesLocked() []string {
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// parseCSV splits a comma-separated list and trims whitespace. Empty
// input and all-whitespace input yield a nil slice.
func parseCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
