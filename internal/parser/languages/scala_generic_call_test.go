package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// A call query that pins the callee to a plain identifier node drops every
// call spelling explicit type arguments, because those grammars wrap the
// callee in a different node. The call then emits NO edge — not an
// unresolved stub, nothing — so `query callers` answers total_edges: 0 with
// the likely_unused caveat and a resolution failure is indistinguishable
// from dead code.
//
// Each test below pairs the plain and generic spellings of one call shape.
// The pair is the assertion: adding type arguments must not cost the edge.

// genericCallTargets returns the unresolved targets of every EdgeCalls in
// the result, keyed by source line.
func genericCallTargets(res *parser.ExtractionResult) map[int][]string {
	byLine := map[int][]string{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			byLine[e.Line] = append(byLine[e.Line], e.To)
		}
	}
	return byLine
}

func TestScalaExtractor_GenericCallEmitsEdge(t *testing.T) {
	src := []byte(`package demo
object Runner {
  def run(obj: Helper): Unit = {
    plainCall()
    genericCall[String]()
    genericTwo[String, Int](1)
    obj.methodPlain()
    obj.methodGeneric[String](1)
  }
}
`)
	res, err := NewScalaExtractor().Extract("Runner.scala", src)
	require.NoError(t, err)
	byLine := genericCallTargets(res)

	assert.Contains(t, byLine[4], "unresolved::*.plainCall", "control")
	assert.Contains(t, byLine[5], "unresolved::*.genericCall",
		"a generic_function callee must not cost the edge")
	assert.Contains(t, byLine[6], "unresolved::*.genericTwo")
	assert.Contains(t, byLine[7], "unresolved::*.methodPlain", "control")
	assert.Contains(t, byLine[8], "unresolved::*.methodGeneric",
		"a generic member call is still a member call")
}
