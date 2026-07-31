package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
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

// Keeping FilePath on the registration site must not cost the handler's own
// schema enrichment. That enrichment reads the handler body and looks its
// bindings up against compiler facts stored under the handler's real source
// file, so anything deriving a source path from Contract.FilePath would, for a
// cross-file route, consult the registration file instead — silently losing
// the response type or matching an unrelated binding on the same line.
//
// This is the two-file form of TestContracts_ResponseFromMakeSlice: the
// registration lives in router.go, the handler and its type in actions.go.
func TestContracts_CrossFileHandlerStillResolvesResponseType(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "router.go"), []byte(`package main

import "net/http"

type Handler struct{ tools map[string]any }

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tools", h.handleListTools)
}
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "actions.go"), []byte(`package main

import "net/http"

type toolInfo struct {
	Name        string `+"`json:\"name\"`"+`
	Description string `+"`json:\"description\"`"+`
}

func (h *Handler) handleListTools(w http.ResponseWriter, _ *http.Request) {
	result := make([]toolInfo, 0, len(h.tools))
	WriteJSON(w, http.StatusOK, result)
}

func WriteJSON(w http.ResponseWriter, code int, body any) {}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	cr := idx.ContractRegistry()
	require.NotNil(t, cr)

	var found contracts.Contract
	for _, c := range cr.All() {
		if c.Type == contracts.ContractHTTP && strings.Contains(c.ID, "/v1/tools") {
			found = c
			break
		}
	}
	require.NotEmpty(t, found.ID, "expected an HTTP contract for /v1/tools")

	// The enrichment that reads the handler body must still land, even though
	// the contract's own FilePath now names the registration file.
	rt, _ := found.Meta["response_type"].(string)
	assert.Contains(t, rt, "toolInfo",
		"response_type must resolve from the handler's file, not the registration file")
	assert.True(t, found.Meta["response_repeated"] == true,
		"response_repeated must survive cross-file enrichment")

	// And the location itself still describes the registration site.
	assert.Equal(t, "router.go", found.FilePath,
		"FilePath must name the registration site")
	assert.Equal(t, "actions.go::Handler.handleListTools", found.SymbolID,
		"SymbolID must still point at the resolved cross-file handler")
}

// bindingSiteRecorder captures the semantic binding sites the contract
// type-resolution pass queries, so a test can assert which file:line pair the
// lookup is built from without needing a live compiler-facts provider (which
// does not run in unit tests).
type bindingSiteRecorder struct {
	graph.Store
	sites []graph.SemanticBindingSite
}

func (r *bindingSiteRecorder) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	r.sites = append(r.sites, sites...)
	return nil, nil
}

// The binding line comes from the handler's body and the compiler bindings it
// is matched against are stored under the handler's own source file. Pairing
// that line with the contract's FilePath — the registration site, once a route
// is registered cross-file — queries a file:line that describes nothing, and
// either loses the type or matches an unrelated binding on the same line.
func TestContractBindingSitesUseTheHandlersOwnFile(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "router.go"), []byte(`package main

import "net/http"

type Handler struct{}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tools", h.handleListTools)
}
`), 0o644))

	// The response value is bound from a method call, which the literal
	// fallback cannot type — so resolution has to go through a semantic
	// binding site, which is the path under test.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "actions.go"), []byte(`package main

import "net/http"

func (h *Handler) handleListTools(w http.ResponseWriter, _ *http.Request) {
	result := h.buildResult()
	WriteJSON(w, http.StatusOK, result)
}

func WriteJSON(w http.ResponseWriter, code int, body any) {}
`), 0o644))

	rec := &bindingSiteRecorder{Store: graph.New()}
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(rec, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	require.NotEmpty(t, rec.sites,
		"expected the contract pass to query at least one semantic binding site")
	for _, s := range rec.sites {
		assert.Equal(t, "actions.go", s.FilePath,
			"binding site must name the handler's file, since its Line comes from the handler body")
	}
}
