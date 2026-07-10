package mcp

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
)

// ToolDescriptor is the unified, machine-readable record for one
// registered MCP tool — the single descriptor shape the CLI consumes
// over the socket so it never has to re-derive per-tool metadata. It
// joins the four independent classifications a tool carries: its
// functional Category (tool_categories.go), whether it Mutates state
// (daemon.MutatingTools), which builtin Presets publish it eagerly
// (tool_presets.go), and a one-line Summary distilled from its MCP
// Description.
type ToolDescriptor struct {
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Mutating    bool     `json:"mutating"`
	Presets     []string `json:"presets"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
}

// ToolDescriptors enumerates EVERY registered tool — both live (in the
// current tools/list) and deferred (reachable only via tools_search) —
// and returns one ToolDescriptor each, sorted by Name. The set is the
// authoritative tool catalog: it is independent of the active preset's
// visibility filtering (a deferred / hidden tool still appears here),
// so the CLI sees the whole surface and can decide what to expose.
func (s *Server) ToolDescriptors() []ToolDescriptor {
	// Collect the raw registered names from both halves of the surface.
	// ListTools() is the live MCP server's map; DeferredNames() is the
	// cold catalog. A name can only live in one of the two, but dedupe
	// defensively so a future overlap never double-counts.
	type entry struct {
		desc string
		seen bool
	}
	names := make(map[string]*entry)

	for name, st := range s.mcpServer.ListTools() {
		desc := ""
		if st != nil {
			desc = st.Tool.Description
		}
		names[name] = &entry{desc: desc, seen: true}
	}
	if s.lazy != nil {
		for _, name := range s.lazy.DeferredNames() {
			if _, ok := names[name]; ok {
				continue
			}
			desc := ""
			if t, ok := s.lazy.DeferredTool(name); ok {
				desc = t.Description
			}
			names[name] = &entry{desc: desc, seen: true}
		}
	}

	out := make([]ToolDescriptor, 0, len(names))
	for name, e := range names {
		out = append(out, ToolDescriptor{
			Name:        name,
			Category:    toolCategory(name),
			Mutating:    daemon.IsMutating(name),
			Presets:     presetsContaining(name),
			Summary:     firstSentence(e.desc),
			Description: strings.TrimSpace(e.desc),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// presetsContaining returns the builtin preset labels that publish the
// named tool eagerly. The explicit roster presets (core / edit / nav)
// are reported by membership in their allow-list; "readonly" is implicit
// for any non-mutating tool (it tracks daemon.MutatingTools, not a
// list). The "full" preset publishes everything and so is intentionally
// omitted — it adds no information. The result is sorted for stable
// output.
func presetsContaining(name string) []string {
	var presets []string
	if toolSetContains(corePresetTools, name) {
		presets = append(presets, "core")
	}
	if toolSetContains(editPresetTools, name) {
		presets = append(presets, "edit")
	}
	if toolSetContains(navPresetTools, name) {
		presets = append(presets, "nav")
	}
	if toolSetContains(localizationPresetTools, name) {
		presets = append(presets, "localization")
	}
	if !daemon.IsMutating(name) {
		presets = append(presets, "readonly")
	}
	sort.Strings(presets)
	return presets
}

// toolSetContains reports whether names includes name.
func toolSetContains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// firstSentence trims a tool Description down to its leading sentence —
// the text up to the first ". " (period-space) boundary, with the
// terminating period kept. An empty or single-sentence description is
// returned unchanged (trimmed of surrounding space).
func firstSentence(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	if i := strings.Index(desc, ". "); i >= 0 {
		return strings.TrimSpace(desc[:i+1])
	}
	return desc
}
