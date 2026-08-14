package semantic

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSymbolMap(t *testing.T) {
	m := NewSymbolMap()

	m.Add("scip::Foo", "main.go::Foo")
	m.Add("scip::Bar", "lib.go::Bar")

	id, ok := m.GortexID("scip::Foo")
	assert.True(t, ok)
	assert.Equal(t, "main.go::Foo", id)

	id, ok = m.GortexID("scip::Bar")
	assert.True(t, ok)
	assert.Equal(t, "lib.go::Bar", id)

	_, ok = m.GortexID("scip::Unknown")
	assert.False(t, ok)
}
