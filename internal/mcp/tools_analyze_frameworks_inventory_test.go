package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/resolver"
)

func inventoryRow(t *testing.T, rows []frameworkInventoryRow, name string) frameworkInventoryRow {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no inventory row for %q", name)
	return frameworkInventoryRow{}
}

// The inventory is the answer to "what may I put in
// index.frameworks.allow", so it has to cover all three registries — the
// gap that made analyze route_frameworks and analyze synthesizers
// insufficient on their own.
func TestFrameworkInventory_CoversAllThreeRegistries(t *testing.T) {
	rows := (&Server{}).frameworkInventory()
	require.NotEmpty(t, rows)

	byName := map[string]frameworkInventoryRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	for _, p := range contracts.RegisteredFrameworkRoutePasses() {
		assert.Contains(t, byName, p.Name(), "route pass missing from inventory")
	}
	for _, n := range resolver.RegisteredFrameworkSynthesizerNames() {
		assert.Contains(t, byName, n, "synthesizer missing from inventory")
	}
	for _, n := range resolver.RegisteredClaimingResolverNames() {
		assert.Contains(t, byName, n, "claiming resolver missing from inventory")
	}

	assert.Contains(t, inventoryRow(t, rows, "django").Layers, frameworkLayerRoute)
	assert.Contains(t, inventoryRow(t, rows, resolver.SynthMyBatis).Layers, frameworkLayerSynthesizer)
	assert.Contains(t, inventoryRow(t, rows, resolver.SynthDjangoDescriptor).Layers, frameworkLayerClaiming)
}

// A name registered in more than one registry is one allow-list entry, so
// it must be one row carrying both layers rather than two rows.
func TestFrameworkInventory_SharedNameIsOneRow(t *testing.T) {
	rows := (&Server{}).frameworkInventory()

	seen := map[string]int{}
	for _, r := range rows {
		seen[r.Name]++
	}
	for name, n := range seen {
		assert.Equal(t, 1, n, "framework %q must appear exactly once", name)
	}

	django := inventoryRow(t, rows, resolver.SynthDjangoDescriptor)
	assert.Contains(t, django.Layers, frameworkLayerClaiming)
}

// With no ConfigManager wired (embedded single-repo mode, tests) every
// pass is active — the "unset allows everything" rule.
func TestFrameworkInventory_NoConfigManagerMeansEverythingActive(t *testing.T) {
	for _, r := range (&Server{}).frameworkInventory() {
		assert.True(t, r.Active, "pass %q must be active with no config", r.Name)
		assert.Empty(t, r.AllowedIn)
	}
}

func TestFrameworkAdmission_UnionAcrossRepos(t *testing.T) {
	perRepo := map[string]frameworkgate.Set{
		"repo-a": frameworkgate.New([]string{"odoo"}),
		"repo-b": frameworkgate.New([]string{"django"}),
	}

	// The union admits both, matching the shared synthesis pass.
	active, allowedIn := frameworkAdmission("odoo", perRepo)
	assert.True(t, active)
	assert.Equal(t, []string{"repo-a"}, allowedIn,
		"when repos disagree the admitting repo must be named")

	active, allowedIn = frameworkAdmission("django", perRepo)
	assert.True(t, active)
	assert.Equal(t, []string{"repo-b"}, allowedIn)

	active, allowedIn = frameworkAdmission("drupal", perRepo)
	assert.False(t, active, "a pass no repo allows must be excluded")
	assert.Empty(t, allowedIn)
}

// A single repository with no allow-list re-admits the whole registry —
// the safe direction, since it never opted out of anything.
func TestFrameworkAdmission_UnconfiguredRepoReadmitsEverything(t *testing.T) {
	perRepo := map[string]frameworkgate.Set{
		"repo-a": frameworkgate.New([]string{"odoo"}),
		"repo-b": {},
	}
	active, _ := frameworkAdmission("drupal", perRepo)
	assert.True(t, active, "an unconfigured repo must re-admit every framework")
}

// When every repository agrees, per-repo attribution is noise.
func TestFrameworkAdmission_AgreeingReposReportNoAttribution(t *testing.T) {
	perRepo := map[string]frameworkgate.Set{
		"repo-a": frameworkgate.New([]string{"odoo"}),
		"repo-b": frameworkgate.New([]string{"odoo"}),
	}
	active, allowedIn := frameworkAdmission("odoo", perRepo)
	assert.True(t, active)
	assert.Empty(t, allowedIn, "unanimous admission needs no per-repo breakdown")
}
