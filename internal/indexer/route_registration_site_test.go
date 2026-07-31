package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// A route registered in one file and handled in another must report a
// file:line pair that describes a single site. resolveProviderHandlers swaps
// the contract's FilePath to the handler's file so the enricher can read that
// file's tree, but Line keeps describing the registration — so leaving the
// swap in place pairs a file and a line from two different places and every
// consumer citing file:line lands on unrelated code.
// See https://github.com/zzet/gortex/issues/322.
func TestRouteContractKeepsRegistrationSiteCoherent(t *testing.T) {
	dir := t.TempDir()

	// The registration literal is handler.go:8.
	handlerGo := `package app

import "net/http"

type handlers struct{}

func (h handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/things/{id}/update", h.requestUpdate)
}
`
	// The handler body is actions.go:11 — deliberately a different line
	// number than the registration, so a mixed pair is detectable.
	actionsGo := `package app

import "net/http"

//
//
//
//
//
// requestUpdate applies a pending change.
func (h handlers) requestUpdate(w http.ResponseWriter, r *http.Request) {
	_ = w
	_ = r
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "handler.go"), []byte(handlerGo), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "actions.go"), []byte(actionsGo), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	const routeID = "http::POST::/api/things/{p1}/update"
	require.NotNil(t, g.GetNode(routeID), "expected a route contract node")

	var edge *graph.Edge
	for _, e := range g.GetInEdges(routeID) {
		if e.Kind == graph.EdgeHandlesRoute {
			edge = e
			break
		}
	}
	require.NotNil(t, edge, "expected an EdgeHandlesRoute into the route")

	// The cross-file handler resolution itself must keep working: that is the
	// whole point of the swap, and this pins the fix to restoring FilePath
	// rather than abandoning the re-point.
	assert.Equal(t, "actions.go::handlers.requestUpdate", edge.From,
		"handler must still resolve across files")

	assert.Equal(t, "handler.go", edge.FilePath,
		"file must name the registration site, which is what Line describes")
	assert.Equal(t, 8, edge.Line,
		"line must stay on the registration literal")
}
