package parser

import (
	"testing"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

func TestPutMatchScratchClearsFullBackingSlab(t *testing.T) {
	s := &matchScratch{
		captures: map[string]*CapturedNode{},
		nodes:    make([]CapturedNode, 1, 8),
	}
	full := s.nodes[:cap(s.nodes)]
	for i := range full {
		full[i] = CapturedNode{Text: "retained source", Node: &sitter.Node{}}
	}
	s.captures["last"] = &full[0]

	putMatchScratch(s)

	if len(s.captures) != 0 {
		t.Fatalf("captures retained %d entries", len(s.captures))
	}
	if len(s.nodes) != 0 || cap(s.nodes) != len(full) {
		t.Fatalf("pooled node slab len/cap = %d/%d, want 0/%d", len(s.nodes), cap(s.nodes), len(full))
	}
	for i, node := range s.nodes[:cap(s.nodes)] {
		if node.Text != "" || node.Node != nil {
			t.Fatalf("backing slot %d retained captured data", i)
		}
	}
}

func TestPutMatchScratchDropsPathologicalSlab(t *testing.T) {
	s := &matchScratch{
		captures: map[string]*CapturedNode{},
		nodes:    make([]CapturedNode, 1, 257),
	}
	full := s.nodes[:cap(s.nodes)]
	for i := range full {
		full[i] = CapturedNode{Text: "retained source", Node: &sitter.Node{}}
	}

	putMatchScratch(s)

	if s.nodes != nil {
		t.Fatalf("pathological node slab retained len/cap %d/%d", len(s.nodes), cap(s.nodes))
	}
	for i, node := range full {
		if node.Text != "" || node.Node != nil {
			t.Fatalf("discarded backing slot %d was not cleared", i)
		}
	}
}
