package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSwiftExtractor_GenericCallEmitsEdge(t *testing.T) {
	src := []byte(`class Runner {
  func run(obj: Runner) {
    upperPlain()
    lowerGeneric<Int>()
    obj.doThing()
    obj.doThing<Int>()
    let _ = MyType<Int>(x: 1)
  }
}
`)
	res, err := NewSwiftExtractor().Extract("demo.swift", src)
	require.NoError(t, err)
	byLine := genericCallTargets(res)

	assert.Contains(t, byLine[3], "unresolved::upperPlain", "control")
	assert.Contains(t, byLine[4], "unresolved::lowerGeneric",
		"a generic free call reroutes to constructor_expression but is still a call")
	assert.Contains(t, byLine[5], "unresolved::*.doThing", "control")
	assert.Contains(t, byLine[6], "unresolved::*.doThing",
		"the grammar folds the receiver into the constructed type path")
	assert.Contains(t, byLine[7], "unresolved::MyType",
		"a generic initializer is recorded like its non-generic spelling")
}

// Swift's member-call branch was gated to factory chains on the belief that
// the bare-identifier pattern covered ordinary obj.method(). It does not:
// that pattern needs a simple_identifier as a DIRECT child of the call, and
// a member call nests it under a navigation_expression. So every plain
// obj.method() and self.method() emitted nothing.
func TestSwiftExtractor_PlainMemberCallEmitsEdge(t *testing.T) {
	src := []byte(`class Runner {
  func free() {}
  func run(h: Helper) {
    free()
    h.work()
    self.free()
  }
}
`)
	res, err := NewSwiftExtractor().Extract("demo.swift", src)
	require.NoError(t, err)
	byLine := genericCallTargets(res)

	assert.Contains(t, byLine[4], "unresolved::free", "control: a free call always worked")
	assert.Contains(t, byLine[5], "unresolved::*.work",
		"an ordinary member call is a call")
	assert.Contains(t, byLine[6], "unresolved::*.free",
		"a self-qualified call is a call")
}
