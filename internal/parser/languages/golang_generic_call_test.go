package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoExtractor_GenericCallEmitsEdge(t *testing.T) {
	src := []byte(`package demo

type Box struct{}

func run(obj Box) {
	Plain(1)
	Zero[int]()
	OneArg[int](1)
	TwoArgs[int](1, 2)
	MultiT[string, int](1, 2)
	obj.Method[int]()
	obj.Single[int](1)
}
`)
	res, err := NewGoExtractor().Extract("demo.go", src)
	require.NoError(t, err)
	byLine := genericCallTargets(res)

	assert.Contains(t, byLine[6], "unresolved::Plain", "control")
	assert.Contains(t, byLine[7], "unresolved::Zero",
		"one type argument and no value argument parses as an index_expression")
	assert.Contains(t, byLine[8], "unresolved::OneArg",
		"one type argument and exactly one value argument parses as a type conversion")
	assert.Contains(t, byLine[9], "unresolved::TwoArgs")
	assert.Contains(t, byLine[10], "unresolved::MultiT",
		"control: two type arguments always parsed as an ordinary call")
	assert.Contains(t, byLine[11], "unresolved::*.Method")
	assert.Contains(t, byLine[12], "unresolved::*.Single",
		"the qualified-type conversion form is still a member call")
}
