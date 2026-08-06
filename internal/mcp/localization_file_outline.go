package mcp

import (
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// Leading-file outline.
//
// A bounded localization page shows ten signature rows, and per-file
// diversification spends most of those slots on distinct files. When the answer
// is a declaration in the file the page already leads with, but not one of the
// rows that file won, nothing on the page says the file declares it at all —
// so the caller searches again for something it is already looking at. The
// outline is that leading file's declaration index: name and line, no
// signatures and no bodies. Only a page that still asks the caller to choose
// carries one; a terminal page's caller is answering, not choosing.

const (
	// localizationOutlineRowCap bounds the rows one outline may serialize.
	localizationOutlineRowCap = 40
	// localizationOutlineHeadRows splits an elided outline between the file's
	// opening declarations and its closing ones.
	localizationOutlineHeadRows = localizationOutlineRowCap / 2
)

type localizationOutlineRow struct {
	Name string `json:"name"`
	Line int    `json:"line"`
	// Kind is a one-rune hint. The row is a locator, so a coarse hint costs a
	// byte and the name still decides.
	Kind string `json:"kind,omitempty"`
}

// localizationFileOutline is the bounded declaration index of a page's leading
// file. Declared counts what the file declares; Elided counts the rows the cap
// dropped from the middle.
type localizationFileOutline struct {
	File     string                   `json:"file"`
	Declared int                      `json:"declared"`
	Elided   int                      `json:"elided,omitempty"`
	Rows     []localizationOutlineRow `json:"rows"`
}

// localizationPageAcceptsOutline admits the outline on the states whose caller
// still has to choose where to look.
func localizationPageAcceptsOutline(state string) bool {
	switch state {
	case localizationStateNeedsRefinement, localizationStateNeedsRecovery,
		localizationStateNeedsExactRead:
		return true
	}
	return false
}

// localizationLeadingFileOutlineProvider defers the file enumeration until a
// page is known to still be refining, and performs it at most once per page.
// A nil enumerator disables the outline entirely.
func localizationLeadingFileOutlineProvider(
	pool []*rerank.Candidate,
	targets []exploreTarget,
	enumerate func(string) []*graph.Node,
) func() *localizationFileOutline {
	if enumerate == nil {
		return nil
	}
	var (
		outline *localizationFileOutline
		built   bool
	)
	return func() *localizationFileOutline {
		if built {
			return outline
		}
		built = true
		file := localizationOutlineLeadingFile(pool, targets)
		if file == "" {
			return nil
		}
		nodes := enumerate(file)
		if len(nodes) == 0 {
			// Nothing to enumerate leaves the nodes this page already fetched,
			// which is the whole of what it knows about that file.
			nodes = localizationOutlineFetchedNodes(pool, targets)
		}
		outline = newLocalizationFileOutline(file, nodes)
		return outline
	}
}

// localizationOutlineLeadingFile reuses the ranked leading-file notion depth
// allocation settled on, falling back to the page's own order for the query
// classes that never diversify by file and so retain no pre-diversification
// pool.
func localizationOutlineLeadingFile(pool []*rerank.Candidate, targets []exploreTarget) string {
	if file := exploreLeadingRankedFile(pool); file != "" {
		return file
	}
	ranked := make([]*rerank.Candidate, 0, len(targets))
	for _, target := range targets {
		if target.node == nil {
			continue
		}
		ranked = append(ranked, &rerank.Candidate{Node: target.node})
	}
	return exploreLeadingRankedFile(ranked)
}

func localizationOutlineFetchedNodes(pool []*rerank.Candidate, targets []exploreTarget) []*graph.Node {
	nodes := make([]*graph.Node, 0, len(pool)+len(targets))
	for _, candidate := range pool {
		if candidate != nil && candidate.Node != nil {
			nodes = append(nodes, candidate.Node)
		}
	}
	for _, target := range targets {
		if target.node != nil {
			nodes = append(nodes, target.node)
		}
	}
	return nodes
}

// newLocalizationFileOutline projects one file's declarations into file order.
// Nodes from another file are dropped, so a candidate pool is as valid an input
// as a file enumeration.
func newLocalizationFileOutline(file string, nodes []*graph.Node) *localizationFileOutline {
	file = strings.TrimSpace(file)
	if file == "" {
		return nil
	}
	rows := make([]localizationOutlineRow, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		if node == nil || nodeDisplayPath(node) != file || isNonDefinitionNode(node.Kind) {
			continue
		}
		name := strings.TrimSpace(node.QualName)
		if name == "" {
			name = strings.TrimSpace(node.Name)
		}
		if name == "" {
			continue
		}
		key := node.ID
		if key == "" {
			key = name
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		rows = append(rows, localizationOutlineRow{
			Name: compactLocalizationField(name, localizationMaxNameRunes),
			Line: node.StartLine,
			Kind: localizationOutlineKindLetter(node.Kind),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.SliceStable(rows, func(first, second int) bool {
		if rows[first].Line != rows[second].Line {
			return rows[first].Line < rows[second].Line
		}
		return rows[first].Name < rows[second].Name
	})
	outline := &localizationFileOutline{File: file, Declared: len(rows), Rows: rows}
	if len(rows) > localizationOutlineRowCap {
		// Both ends of a file carry declarations a caller may want; the middle
		// is what a bounded index can give back.
		kept := make([]localizationOutlineRow, 0, localizationOutlineRowCap)
		kept = append(kept, rows[:localizationOutlineHeadRows]...)
		kept = append(kept, rows[len(rows)-(localizationOutlineRowCap-localizationOutlineHeadRows):]...)
		outline.Elided = len(rows) - localizationOutlineRowCap
		outline.Rows = kept
	}
	return outline
}

func localizationOutlineKindLetter(kind graph.NodeKind) string {
	for _, letter := range string(kind) {
		return string(unicode.ToLower(letter))
	}
	return ""
}
