package resolver

import "github.com/zzet/gortex/internal/frameworkgate"

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
}

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
