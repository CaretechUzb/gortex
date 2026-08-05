package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

// mockExtractor implements the Extractor interface for testing.
type mockExtractor struct {
	lang string
	exts []string
}

func (m *mockExtractor) Language() string     { return m.lang }
func (m *mockExtractor) Extensions() []string { return m.exts }
func (m *mockExtractor) Extract(filePath string, src []byte) (*ExtractionResult, error) {
	return &ExtractionResult{
		Nodes: []*graph.Node{{ID: filePath + "::mock", Name: "mock", Kind: graph.KindFunction}},
	}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "go", exts: []string{".go"}})

	e, ok := r.GetByLanguage("go")
	assert.True(t, ok)
	assert.Equal(t, "go", e.Language())

	e, ok = r.GetByExtension(".go")
	assert.True(t, ok)
	assert.Equal(t, "go", e.Language())
}

func TestRegistry_DetectLanguage(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "go", exts: []string{".go"}})
	r.Register(&mockExtractor{lang: "typescript", exts: []string{".ts", ".tsx"}})

	lang, ok := r.DetectLanguage("pkg/foo.go")
	assert.True(t, ok)
	assert.Equal(t, "go", lang)

	lang, ok = r.DetectLanguage("src/app.tsx")
	assert.True(t, ok)
	assert.Equal(t, "typescript", lang)
}

func TestRegistry_UnknownExtension(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "go", exts: []string{".go"}})

	_, ok := r.GetByExtension(".txt")
	assert.False(t, ok)

	_, ok = r.DetectLanguage("readme.md")
	assert.False(t, ok)
}

// TestRegistry_BasenameStem covers the `Dockerfile.<suffix>` convention:
// a per-purpose build file must detect as the stem's language, while the
// bare basename and the dotted-extension form keep working.
func TestRegistry_BasenameStem(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "dockerfile", exts: []string{
		".dockerfile", "Dockerfile", "Containerfile",
		"Dockerfile.*", "Containerfile.*",
	}})

	for _, path := range []string{
		"Dockerfile",
		"deploy/Dockerfile",
		"Dockerfile.perf",
		"build/Dockerfile.ci",
		"Containerfile.dev",
		"svc.dockerfile",
	} {
		lang, ok := r.DetectLanguage(path)
		assert.True(t, ok, "%s must be detected", path)
		assert.Equal(t, "dockerfile", lang, "path %s", path)
	}
}

// TestRegistry_BasenameStemYieldsToKnownExtension pins the ordering that
// keeps the stem rule safe: a suffix that is itself a registered
// extension wins, so `Dockerfile.md` is markdown, not a build file.
func TestRegistry_BasenameStemYieldsToKnownExtension(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "dockerfile", exts: []string{"Dockerfile", "Dockerfile.*"}})
	r.Register(&mockExtractor{lang: "markdown", exts: []string{".md"}})

	lang, ok := r.DetectLanguage("Dockerfile.md")
	assert.True(t, ok)
	assert.Equal(t, "markdown", lang)

	lang, ok = r.DetectLanguage("Dockerfile.perf")
	assert.True(t, ok)
	assert.Equal(t, "dockerfile", lang)
}

// TestRegistry_BasenameStemDoesNotMatchDotfile guards the `idx > 0`
// bound: a leading-dot basename has an empty stem and must not bind to
// a registered stem entry.
func TestRegistry_BasenameStemDoesNotMatchDotfile(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "dockerfile", exts: []string{"Dockerfile.*"}})
	r.Register(&mockExtractor{lang: "go", exts: []string{".go"}})

	_, ok := r.DetectLanguage(".dockerignore")
	assert.False(t, ok, "a dotfile must not match an empty stem")

	_, ok = r.DetectLanguage("Dockerfilex.perf")
	assert.False(t, ok, "the stem must match the whole segment, not a prefix")
}

func TestRegistry_SupportedLanguages(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockExtractor{lang: "go", exts: []string{".go"}})
	r.Register(&mockExtractor{lang: "python", exts: []string{".py"}})

	langs := r.SupportedLanguages()
	assert.Len(t, langs, 2)
	assert.Contains(t, langs, "go")
	assert.Contains(t, langs, "python")
}
