package resolver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/frameworkgate"
)

// Framework-registry run options.
//
// The allow-list arrives as an option rather than as a positional
// parameter for two reasons. First, internal/resolver deliberately does
// not import internal/config (see cross_repo.go), so the decision has to
// be handed in already resolved — frameworkgate is the shared leaf
// package both sides can name. Second, the four public entry points have
// callers in this repository and upstream; a variadic option leaves every
// one of them compiling unchanged. This mirrors the option shape already
// used by internal/analyzer's synthesizer report.

// frameworkSynthOptions is the resolved option set for one registry run.
// The zero value runs every registered pass.
type frameworkSynthOptions struct {
	allowed frameworkgate.Set
	// allowedByRepo enforces each repository's own allow-list on the
	// shared pass. allowed (the union) decides whether a pass runs;
	// this decides which repositories its edges may land in. See
	// framework_repo_gate.go for why the two differ.
	allowedByRepo map[string]frameworkgate.Set
	// selected is the operator's configured synthesizer allow-list
	// (index.framework_synthesizers), carried separately from allowed
	// because the two answer different questions and must INTERSECT.
	//
	// allowed is derived from the repositories in scope, so it widens as
	// scope widens; selected is a fixed operator choice. Folding them into
	// one field means whichever option is applied last silently discards
	// the other — which is exactly what a single `allowed` field did when
	// upstream's selection first landed here.
	selected frameworkgate.Set
}

// admits reports whether a pass clears BOTH gates and may therefore execute.
// An unset Set on either side allows everything, so the common case is two
// cheap unconfigured checks.
func (o frameworkSynthOptions) admits(name string) bool {
	return o.allowed.Allows(name) && o.selected.Allows(name)
}

// The two gates are reported DIFFERENTLY, and the difference is deliberate.
//
//	selected  the operator's configured allow-list. A pass left out of it is
//	          not part of this deployment at all, so it is omitted from the
//	          run report entirely — listing it would invite the reader to ask
//	          why something they switched off is still being mentioned.
//
//	allowed   the scope-derived union of the repositories in play. A pass left
//	          out of it IS installed and does run elsewhere; it is simply not
//	          wanted for this scope. It stays in the report with Disabled set,
//	          so `analyze kind=synthesizers` can distinguish "gated off here"
//	          from "ran and found nothing" — which is the whole reason the
//	          Disabled column exists.
//
// Collapsing the two costs one of those properties whichever way it is done.

// selects reports whether the configured selection admits the pass, i.e.
// whether it belongs in the report at all.
func (o frameworkSynthOptions) selects(name string) bool { return o.selected.Allows(name) }

// selectsNothing reports whether the configured selection is explicitly empty,
// which disables the whole pipeline and produces no report rows. It is NOT the
// same as an allow-list of none: that one still reports every pass, disabled.
func (o frameworkSynthOptions) selectsNothing() bool { return o.selected.AllowsNothing() }

// FrameworkSynthOption configures a framework-registry run.
type FrameworkSynthOption func(*frameworkSynthOptions)

// WithAllowedFrameworks restricts this run to the named passes. It
// applies to the dispatch synthesizers and to the claiming resolvers
// alike, so a framework whose synthesizer and claimer share a name — as
// django and django-descriptor do — is admitted by one entry. An unset
// Set allows every registered pass.
func WithAllowedFrameworks(s frameworkgate.Set) FrameworkSynthOption {
	return func(o *frameworkSynthOptions) { o.allowed = s }
}

// WithFrameworkAllowByRepo enforces per-repository allow-lists on a run
// that spans several repositories. Keys are repository prefixes; a repo
// absent from the map, or carrying an unset Set, admits every pass.
//
// It composes with WithAllowedFrameworks rather than replacing it: the
// union still decides whether a pass executes at all — an excluded pass
// must stay free, not run and have its output discarded — while this map
// decides which repositories the surviving passes may write into.
func WithFrameworkAllowByRepo(byRepo map[string]frameworkgate.Set) FrameworkSynthOption {
	return func(o *frameworkSynthOptions) { o.allowedByRepo = byRepo }
}

func resolveFrameworkSynthOptions(opts []FrameworkSynthOption) frameworkSynthOptions {
	var o frameworkSynthOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// RegisteredFrameworkSynthesizerNames returns every registered dispatch
// synthesizer name, in run order. It is the inventory behind the
// `analyze kind=frameworks` listing and behind the unknown-name warning
// for index.frameworks.allow.
func RegisteredFrameworkSynthesizerNames() []string {
	synths := defaultFrameworkSynthesizers()
	out := make([]string, 0, len(synths))
	for _, s := range synths {
		out = append(out, s.Name())
	}
	return out
}

// RegisteredClaimingResolverNames returns every registered claiming
// resolver name, in run order.
func RegisteredClaimingResolverNames() []string {
	resolvers := defaultClaimingResolvers()
	out := make([]string, 0, len(resolvers))
	for _, r := range resolvers {
		out = append(out, r.Name())
	}
	return out
}

// FrameworkSynthesizerSelection is upstream's compiled allow-list, adopted in
// the 2026-09-04 merge. It is a WRAPPER over frameworkgate.Set rather than the
// second admission mechanism upstream shipped, because two mechanisms deciding
// "may this pass run" is exactly the arrangement that lets them disagree — and
// this fork's per-repository gate (WithFrameworkAllowByRepo) composes with the
// Set, not with a parallel map.
//
// The zero value allows every pass, matching both AllFrameworkSynthesizers and
// the unset Set. A selection built from an empty list disables the whole
// pipeline, which frameworkgate spells AllowNone — never an empty New(), which
// resolves back to "allow everything".
type FrameworkSynthesizerSelection struct {
	set frameworkgate.Set
}

// AllFrameworkSynthesizers returns the legacy, all-enabled selection.
func AllFrameworkSynthesizers() FrameworkSynthesizerSelection {
	return FrameworkSynthesizerSelection{}
}

// FrameworkSynthesizerNames returns every configurable registry name in
// canonical execution order — dispatch synthesizers followed by claiming
// resolvers. Correctness-only tail gates are intentionally not configurable
// independently.
//
// It is deliberately wider than RegisteredFrameworkSynthesizerNames, which
// lists synthesizers alone for the `analyze kind=frameworks` inventory.
func FrameworkSynthesizerNames() []string {
	synthesizers := defaultFrameworkSynthesizers()
	claimers := defaultClaimingResolvers()
	names := make([]string, 0, len(synthesizers)+len(claimers))
	for _, synthesizer := range synthesizers {
		names = append(names, synthesizer.Name())
	}
	for _, claimer := range claimers {
		names = append(names, claimer.Name())
	}
	return names
}

// NewFrameworkSynthesizerSelection validates and compiles an explicit
// allow-list. Passing an empty slice is distinct from AllFrameworkSynthesizers
// and disables the complete framework pipeline.
//
// Names are validated exactly, with no wildcard handling: this is the
// configuration surface for an explicit list, and a caller that wants the
// pattern semantics frameworkgate offers goes through WithAllowedFrameworks.
func NewFrameworkSynthesizerSelection(names []string) (FrameworkSynthesizerSelection, error) {
	validNames := FrameworkSynthesizerNames()
	valid := make(map[string]struct{}, len(validNames))
	for _, name := range validNames {
		valid[name] = struct{}{}
	}

	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := seen[name]; exists {
			return FrameworkSynthesizerSelection{}, fmt.Errorf("duplicate framework synthesizer %q", name)
		}
		if _, exists := valid[name]; !exists {
			allowed := append([]string(nil), validNames...)
			sort.Strings(allowed)
			return FrameworkSynthesizerSelection{}, fmt.Errorf(
				"unknown framework synthesizer %q (valid: %s)",
				name, strings.Join(allowed, ", "),
			)
		}
		seen[name] = struct{}{}
	}
	if len(names) == 0 {
		return FrameworkSynthesizerSelection{set: frameworkgate.AllowNone()}, nil
	}
	return FrameworkSynthesizerSelection{set: frameworkgate.New(names)}, nil
}

// ValidateFrameworkSynthesizers validates a configured allow-list.
func ValidateFrameworkSynthesizers(names []string) error {
	_, err := NewFrameworkSynthesizerSelection(names)
	return err
}

// WithFrameworkSelection lowers a selection onto the option the registry
// actually consults, so upstream's selection callers and this fork's option
// callers converge on one gate.
func WithFrameworkSelection(sel FrameworkSynthesizerSelection) FrameworkSynthOption {
	return func(o *frameworkSynthOptions) { o.selected = sel.set }
}
