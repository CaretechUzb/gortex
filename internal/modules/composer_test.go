package modules

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const composerManifest = `{
  "name": "acme/store",
  "require": {
    "php": ">=8.1",
    "ext-json": "*",
    "lib-openssl": "*",
    "composer-runtime-api": "^2.0",
    "monolog/monolog": "^3.0",
    "symfony/console": "~6.4"
  },
  "require-dev": {
    "phpunit/phpunit": "^10.5"
  },
  "autoload": {
    "psr-4": {
      "Acme\\Store\\": "src/",
      "Acme\\Shared\\": ["lib/", "./vendor-src/shared"]
    },
    "psr-0": {
      "Legacy_": "legacy/"
    }
  },
  "autoload-dev": {
    "psr-4": { "Acme\\Store\\Tests\\": "tests/" }
  }
}`

func TestParseComposerJSON_SkipsPlatformRequirements(t *testing.T) {
	specs := ParseComposerJSON([]byte(composerManifest))
	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}
	require.Contains(t, got, "monolog/monolog")
	assert.Equal(t, "composer", got["monolog/monolog"].Ecosystem)
	assert.Equal(t, "^3.0", got["monolog/monolog"].Version, "version constraints stay verbatim")
	assert.False(t, got["monolog/monolog"].Indirect)

	require.Contains(t, got, "symfony/console")

	require.Contains(t, got, "phpunit/phpunit")
	assert.True(t, got["phpunit/phpunit"].Indirect, "require-dev is not a production dependency")
	assert.Equal(t, "dev", got["phpunit/phpunit"].Replace)

	for _, platform := range []string{"php", "ext-json", "lib-openssl", "composer-runtime-api"} {
		assert.NotContains(t, got, platform, "%s names the runtime, not an installable package", platform)
	}
}

func TestParseComposerJSON_EmptyAndMalformed(t *testing.T) {
	assert.Nil(t, ParseComposerJSON(nil))
	assert.Nil(t, ParseComposerJSON([]byte("")))
	assert.Nil(t, ParseComposerJSON([]byte("{not json")))
}

func TestParseComposerLock_ResolvedVersions(t *testing.T) {
	specs := ParseComposerLock([]byte(`{
      "packages": [
        {"name": "monolog/monolog", "version": "3.5.0"},
        {"name": "php", "version": "8.2.0"}
      ],
      "packages-dev": [
        {"name": "phpunit/phpunit", "version": "10.5.11"}
      ]
    }`))
	got := map[string]Spec{}
	for _, s := range specs {
		got[s.Path] = s
	}
	require.Contains(t, got, "monolog/monolog")
	assert.Equal(t, "3.5.0", got["monolog/monolog"].Version)
	assert.NotContains(t, got, "php")
	require.Contains(t, got, "phpunit/phpunit")
	assert.True(t, got["phpunit/phpunit"].Indirect)
}

func TestComposerAutoloadRoots(t *testing.T) {
	roots := ComposerAutoloadRoots([]byte(composerManifest))
	require.NotNil(t, roots)
	assert.Equal(t, []string{"src"}, roots[`Acme\Store`], "the trailing namespace separator is trimmed")
	assert.Equal(t, []string{"lib", "vendor-src/shared"}, roots[`Acme\Shared`],
		"an autoload value may be an array of directories")
	assert.Equal(t, []string{"tests"}, roots[`Acme\Store\Tests`], "autoload-dev counts too")
	assert.Equal(t, []string{"legacy"}, roots["Legacy_"], "psr-0 counts too")
}

func TestComposerAutoloadRoots_EmptyPrefixSkipped(t *testing.T) {
	roots := ComposerAutoloadRoots([]byte(`{"autoload":{"psr-4":{"":"src/"}}}`))
	assert.Nil(t, roots, "the catch-all prefix claims every namespace and is not a root")
}

func TestComposerOwnName(t *testing.T) {
	assert.Equal(t, "acme/store", ComposerOwnName([]byte(composerManifest)))
	assert.Empty(t, ComposerOwnName([]byte(`{}`)))
	assert.Empty(t, ComposerOwnName(nil))
}

func TestEcosystemLanguage_Composer(t *testing.T) {
	assert.Equal(t, "php", ecosystemLanguage("composer"))
}
