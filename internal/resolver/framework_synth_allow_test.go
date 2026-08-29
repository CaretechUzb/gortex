package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/graph"
)

func rowByName(t *testing.T, rep FrameworkSynthReport, name string) SynthCount {
	t.Helper()
	for _, row := range rep.Per {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("no report row for %q", name)
	return SynthCount{}
}

// An unset allow-list must run every registered pass — the report keeps
// one row per synthesizer and none of them is marked excluded.
func TestFrameworkSynth_UnsetAllowListRunsEveryPass(t *testing.T) {
	g := graph.New()
	rep := RunFrameworkSynthesizers(g)

	require.NotEmpty(t, rep.Per)
	for _, row := range rep.Per {
		assert.False(t, row.Disabled, "pass %q must not be marked excluded", row.Name)
	}
}

// The report row count is stable regardless of configuration: the
// registry slice is never filtered, only the execution is gated, so a
// disabled pass stays visible as an explicit `disabled` row rather than
// vanishing from the report.
func TestFrameworkSynth_AllowListKeepsEveryReportRow(t *testing.T) {
	g := graph.New()
	full := RunFrameworkSynthesizers(g)
	narrowed := RunFrameworkSynthesizers(g, WithAllowedFrameworks(frameworkgate.New([]string{SynthMyBatis})))

	require.Equal(t, len(full.Per), len(narrowed.Per),
		"gating execution must not change the report's shape")

	assert.False(t, rowByName(t, narrowed, SynthMyBatis).Disabled,
		"the allowed pass must not be marked excluded")
	assert.True(t, rowByName(t, narrowed, SynthCelery).Disabled,
		"a pass absent from the allow-list must be marked excluded")
	assert.Zero(t, rowByName(t, narrowed, SynthCelery).Edges,
		"an excluded pass must land no edges")
}

// The gate must short-circuit before the pass function is entered, so an
// excluded framework costs nothing.
func TestFrameworkSynth_ExcludedPassIsNeverInvoked(t *testing.T) {
	var ran bool
	s := synthFunc{name: "test-only-pass", fn: func(graph.Store) int { ran = true; return 7 }}

	o := resolveFrameworkSynthOptions([]FrameworkSynthOption{
		WithAllowedFrameworks(frameworkgate.New([]string{"something-else"})),
	})
	require.False(t, o.allowed.Allows(s.Name()))
	if o.allowed.Allows(s.Name()) {
		s.Synthesize(graph.New())
	}
	assert.False(t, ran, "an excluded pass function must never be entered")

	o = resolveFrameworkSynthOptions([]FrameworkSynthOption{
		WithAllowedFrameworks(frameworkgate.New([]string{"test-only-pass"})),
	})
	if o.allowed.Allows(s.Name()) {
		s.Synthesize(graph.New())
	}
	assert.True(t, ran, "an allowed pass function must be entered")
}

// django-descriptor is a claiming resolver whose Name is the same
// constant as the synthesizer tier's, so one config entry governs both
// layers. Pinning it keeps that property from silently regressing.
func TestFrameworkSynth_ClaimingResolverSharesFrameworkName(t *testing.T) {
	assert.Equal(t, SynthDjangoDescriptor, DjangoDescriptorResolver{}.Name())
	assert.Contains(t, RegisteredClaimingResolverNames(), SynthDjangoDescriptor)
}

func TestFrameworkSynth_AllowListGatesClaimingResolvers(t *testing.T) {
	g := graph.New()
	rep := RunFrameworkSynthesizers(g, WithAllowedFrameworks(frameworkgate.New([]string{SynthMyBatis})))
	assert.True(t, rowByName(t, rep, SynthDjangoDescriptor).Disabled,
		"a claiming resolver absent from the allow-list must be marked excluded")

	rep = RunFrameworkSynthesizers(g, WithAllowedFrameworks(frameworkgate.New([]string{SynthDjangoDescriptor})))
	assert.False(t, rowByName(t, rep, SynthDjangoDescriptor).Disabled,
		"a named claiming resolver must run")
}

func TestFrameworkSynth_NoneSentinelExcludesEveryPass(t *testing.T) {
	g := graph.New()
	rep := RunFrameworkSynthesizers(g, WithAllowedFrameworks(frameworkgate.AllowNone()))

	require.NotEmpty(t, rep.Per)
	for _, row := range rep.Per {
		assert.True(t, row.Disabled, "pass %q must be excluded by the none sentinel", row.Name)
		assert.Zero(t, row.Edges, "pass %q must land no edges", row.Name)
	}
	assert.Zero(t, rep.Total)
}

// Every registered name across both resolver registries must be nameable
// in index.frameworks.allow, otherwise a user could not opt a pass back in.
func TestRegisteredFrameworkNames_CoverTheRunRegistries(t *testing.T) {
	synths := RegisteredFrameworkSynthesizerNames()
	require.Len(t, synths, len(defaultFrameworkSynthesizers()))
	assert.Contains(t, synths, SynthMyBatis)
	assert.Contains(t, synths, SynthCelery)

	claimers := RegisteredClaimingResolverNames()
	require.Len(t, claimers, len(defaultClaimingResolvers()))

	for _, name := range append(append([]string{}, synths...), claimers...) {
		assert.NotEmpty(t, name, "every registered pass must have a nameable identity")
		assert.True(t, frameworkgate.New([]string{name}).Allows(name),
			"pass %q must be admissible by its own name", name)
	}
}
