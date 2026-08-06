package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// A `TiledAtlas.fromTiledMap(...)` call binds to the named factory constructor
// on TiledAtlas — not to the same-named constructor on a decoy class. The
// Capitalized chain head is the only evidence that separates them, so the bind
// proves both halves: the constructor is a real member symbol, and the call
// carries the receiver type the resolver matches it against.
func TestDartConstructorCall_BindsViaCapitalizedReceiver(t *testing.T) {
	src := `class Decoy {
  Decoy.fromTiledMap(int m);
}

class TiledAtlas {
  factory TiledAtlas.fromTiledMap(int m) {
    return TiledAtlas.fromKey('x');
  }

  TiledAtlas.fromKey(String k);
}

void load() {
  var a = TiledAtlas.fromTiledMap(1);
}
`
	res, err := languages.NewDartExtractor().Extract("atlas.dart", []byte(src))
	require.NoError(t, err)

	g := graph.New()
	loadExtract(t, g, res)
	New(g).ResolveAll()

	assert.Equal(t, "atlas.dart::TiledAtlas.fromTiledMap", dartCallTarget(g, "atlas.dart::load"),
		"the call must land on TiledAtlas' factory constructor, not the decoy's")

	// The factory body's own `TiledAtlas.fromKey(...)` binds too — a
	// constructor is now an enclosing owner, so the calls it makes are
	// attributed instead of dropped.
	assert.Equal(t, "atlas.dart::TiledAtlas.fromKey",
		dartCallTarget(g, "atlas.dart::TiledAtlas.fromTiledMap"),
		"a call made inside a factory constructor must attribute to that constructor")
}

// dartCallTarget returns the single outgoing call target of a symbol.
func dartCallTarget(g graph.Store, from string) string {
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == graph.EdgeCalls {
			return e.To
		}
	}
	return ""
}
