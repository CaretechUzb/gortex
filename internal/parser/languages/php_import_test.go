package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// phpImports returns every import edge keyed by target, with its meta.
func phpImports(t *testing.T, src string) map[string]map[string]any {
	t.Helper()
	res, err := NewPHPExtractor().Extract("imp.php", []byte(src))
	require.NoError(t, err)
	out := map[string]map[string]any{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeImports {
			out[e.To] = e.Meta
		}
	}
	return out
}

func TestPHPImport_PlainUseCarriesFQNAndSymbol(t *testing.T) {
	got := phpImports(t, `<?php
use Monolog\Handler\StreamHandler;
`)
	meta := got["unresolved::import::Monolog/Handler/StreamHandler"]
	require.NotNil(t, meta, "got %v", got)
	assert.Equal(t, `Monolog\Handler\StreamHandler`, meta["fqn"])
	assert.Equal(t, "StreamHandler", meta["symbol"])
}

// A braced group shares one prefix. Before, only the prefix survived and every
// imported member was dropped.
func TestPHPImport_GroupedUseExpandsEachMember(t *testing.T) {
	got := phpImports(t, `<?php
use Monolog\Handler\{StreamHandler, RotatingFileHandler, NullHandler};
`)
	for _, want := range []string{
		"unresolved::import::Monolog/Handler/StreamHandler",
		"unresolved::import::Monolog/Handler/RotatingFileHandler",
		"unresolved::import::Monolog/Handler/NullHandler",
	} {
		assert.Contains(t, got, want, "got %v", got)
	}
	assert.NotContains(t, got, "unresolved::import::Monolog/Handler",
		"the shared prefix is not itself an import")
}

func TestPHPImport_AliasRecorded(t *testing.T) {
	got := phpImports(t, `<?php
use Monolog\Handler\StreamHandler as Stream;
`)
	meta := got["unresolved::import::Monolog/Handler/StreamHandler"]
	require.NotNil(t, meta, "got %v", got)
	assert.Equal(t, "Stream", meta["alias"])
}

func TestPHPImport_FunctionAndConstQualifiers(t *testing.T) {
	got := phpImports(t, `<?php
use function Monolog\Utils\jsonEncode;
use const Monolog\Level\DEBUG;
`)
	fn := got["unresolved::import::Monolog/Utils/jsonEncode"]
	require.NotNil(t, fn, "got %v", got)
	assert.Equal(t, "function", fn["import_kind"])

	c := got["unresolved::import::Monolog/Level/DEBUG"]
	require.NotNil(t, c, "got %v", got)
	assert.Equal(t, "const", c["import_kind"])
}

// A group may qualify individual members, which overrides the declaration's
// own qualifier.
func TestPHPImport_GroupedMixedQualifiers(t *testing.T) {
	got := phpImports(t, `<?php
use Monolog\{Logger, function Utils\jsonEncode, const Level\DEBUG};
`)
	require.Contains(t, got, "unresolved::import::Monolog/Logger")
	assert.Nil(t, got["unresolved::import::Monolog/Logger"]["import_kind"],
		"a class member of a mixed group is not a function import")
	assert.Equal(t, "function", got["unresolved::import::Monolog/Utils/jsonEncode"]["import_kind"])
	assert.Equal(t, "const", got["unresolved::import::Monolog/Level/DEBUG"]["import_kind"])
}

func TestPHPImport_GroupedAlias(t *testing.T) {
	got := phpImports(t, `<?php
use App\Log\{Handler as LogHandler, Formatter};
`)
	meta := got["unresolved::import::App/Log/Handler"]
	require.NotNil(t, meta, "got %v", got)
	assert.Equal(t, "LogHandler", meta["alias"])
	assert.Contains(t, got, "unresolved::import::App/Log/Formatter")
}
