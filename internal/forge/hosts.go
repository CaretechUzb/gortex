package forge

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/indexer"
)

// ProviderKind names a forge backend. The zero value is meaningless; every
// resolution path returns one of the constants below.
type ProviderKind string

const (
	// ProviderGitHub is github.com and GitHub Enterprise.
	ProviderGitHub ProviderKind = "github"
	// ProviderGitLab is gitlab.com and any self-hosted GitLab.
	ProviderGitLab ProviderKind = "gitlab"
)

// remote is the resolved remote identity of a repository: the host that serves
// it and the project path beneath that host.
//
// Path keeps EVERY component under the host, not just the trailing two. GitHub
// is always owner/repo, but GitLab supports nested subgroups
// (group/subgroup/project), and collapsing those to the last two components
// addresses the wrong project.
type remote struct {
	Host string // "github.com", "gitlab.caretech.uz"
	Path string // "owner/repo", "group/subgroup/project"
}

// remoteCache memoizes resolveRemote per repoDir.
//
// Availability is now checked PER PR inside the triage fan-out
// (mcp/tools_prs.go → resolvePRFiles → fetchPRFiles), and resolving a remote
// costs an indexer.DetectIdentity plus, on a miss, a `git remote get-url origin`
// SUBPROCESS. Uncached, a 20-PR triage paid up to 20 process forks for an answer
// that cannot change within a run. The cached value is the remote URL, which is
// repo configuration rather than per-call state, so a stale entry would require
// someone to re-point origin mid-process.
var remoteCache sync.Map // repoDir → remoteCacheEntry

type remoteCacheEntry struct {
	remote remote
	err    error
}

// resolveRemote derives the host and project path for repoDir. It asks the
// indexed repo identity first (indexer.DetectIdentity → NormalizeRemoteURL
// canonical form "host/owner/repo"), then falls back to
// `git remote get-url origin` through the git chokepoint — the same order
// resolveSlug uses, so both agree on which remote is authoritative.
//
// Results (including failures) are memoized per repoDir; see remoteCache.
func resolveRemote(ctx context.Context, repoDir string) (remote, error) {
	if v, ok := remoteCache.Load(repoDir); ok {
		e := v.(remoteCacheEntry)
		return e.remote, e.err
	}
	r, err := resolveRemoteUncached(ctx, repoDir)
	if err != nil {
		// Deliberately NOT cached. The justification for caching is that a
		// repo's remote cannot change within a run — true of a success, false
		// of a failure. A transient `git remote get-url` error (a repo mid-
		// clone, a worktree not yet materialised during warmup) would otherwise
		// pin that repo as forge-unavailable until the daemon restarts. Only
		// the success path carries the subprocess cost the cache exists to kill.
		return r, err
	}
	remoteCache.Store(repoDir, remoteCacheEntry{remote: r})
	return r, nil
}

// resetRemoteCache drops every memoized remote. Tests that build a repo per
// case call it so one t.TempDir()'s answer cannot leak into another.
func resetRemoteCache() { remoteCache = sync.Map{} }

// resolveRemoteUncached is resolveRemote's actual work, split out so the cache
// wrapper stays readable.
func resolveRemoteUncached(ctx context.Context, repoDir string) (remote, error) {
	// An empty repoDir is a caller saying "no resolvable working tree", not a
	// request to use the process's cwd. filepath.Abs("") and a bare `git` both
	// silently resolve to the DAEMON's own directory, which answered the
	// provider, the token hint, and the review URL from an unrelated repository
	// — and remoteCache then pinned that answer under the key "".
	if strings.TrimSpace(repoDir) == "" {
		return remote{}, fmt.Errorf("forge: no repository path given")
	}
	if id, err := indexer.DetectIdentity(repoDir); err == nil && id != nil {
		if r, ok := remoteFrom(id.CanonicalID); ok {
			return r, nil
		}
		if r, ok := remoteFrom(indexer.NormalizeRemoteURL(id.RemoteURL)); ok {
			return r, nil
		}
	}
	raw, err := gitcmd.Output(ctx, repoDir, "remote", "get-url", "origin")
	if err != nil {
		return remote{}, fmt.Errorf("forge: resolving remote for %s: %w", repoDir, err)
	}
	if r, ok := remoteFrom(indexer.NormalizeRemoteURL(raw)); ok {
		return r, nil
	}
	return remote{}, fmt.Errorf("forge: could not derive host/project from remote %q", strings.TrimSpace(raw))
}

// remoteFrom splits a normalized "host/owner/repo" canonical remote into its
// host and project path. A canonical form with no host component (a bare
// "owner/repo") yields ok=false: without a host there is nothing to route on.
//
// Segment COUNT alone is not enough to recognise a remote. indexer.DetectIdentity
// returns a path-shaped CanonicalID for a directory that has no git remote, and
// "/var/folders/x/y/001" has plenty of segments — accepting it produced a
// fabricated "https://var/folders/x/y/001/pull/7" and, worse, made every
// unresolvable-remote fallback in this package unreachable. So the first segment
// must actually look like a host.
func remoteFrom(canonical string) (remote, bool) {
	canonical = strings.TrimSpace(canonical)
	// A leading slash means a filesystem path, never a canonical remote.
	if strings.HasPrefix(canonical, "/") {
		return remote{}, false
	}
	canonical = strings.TrimSuffix(canonical, ".git")
	canonical = strings.Trim(canonical, "/")
	if canonical == "" {
		return remote{}, false
	}
	parts := strings.Split(canonical, "/")
	if len(parts) < 3 {
		// "owner/repo" with no host, or a single component — not routable.
		return remote{}, false
	}
	host := strings.ToLower(parts[0])
	path := strings.Join(parts[1:], "/")
	if path == "" || !looksLikeHost(host) {
		return remote{}, false
	}
	return remote{Host: host, Path: path}, true
}

// hostLabels matches a dotted DNS name with an optional numeric port. Each
// label is alphanumeric with interior hyphens.
var hostLabels = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]{1,5})?$`)

// looksLikeHost reports whether s is a syntactically valid network host: a
// dotted name (github.com, gitlab.caretech.uz), a host:port, or "localhost".
//
// This validates the CHARSET, it does not merely test for the presence of a dot.
// The difference is a credential-exfiltration bug: a presence test accepts
// "gitlab.com@evil.example", which carries a "gitlab" label (so it routes to the
// GitLab backend) and concatenates into "https://gitlab.com@evil.example/api/v4/"
// — where "gitlab.com" is USERINFO and the request, carrying the user's token,
// actually goes to evil.example. The host reaches here straight from a
// repository's own git remote, so it is attacker-controlled input in any repo
// the user did not write.
//
// A bare single-label host such as "gitlab" on a corporate network is still
// rejected. That is the safe direction to be wrong in: a rejected remote
// degrades to the GitHub default with an actionable hint, and the host can be
// named explicitly through the forge config or GITLAB_API_URL.
func looksLikeHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	if strings.ContainsAny(s, "@/?#\\ \t\r\n") {
		return false
	}
	if !hostLabels.MatchString(s) {
		return false
	}
	if s == "localhost" || strings.HasPrefix(s, "localhost:") {
		return true
	}
	// Require a dot beyond the port, so a single-label host stays rejected.
	return strings.Contains(strings.SplitN(s, ":", 2)[0], ".")
}

// providerFor decides which backend serves repoDir.
//
// Detection is by HOST ALONE and never consults a token. That separation is
// deliberate: if a missing token could change the answer, a GitLab repo with no
// GITLAB_TOKEN would silently resolve to the GitHub backend and report
// "set GH_TOKEN" — the exact misdiagnosis this layer exists to prevent.
//
// Order:
//  1. GORTEX_FORGE_PROVIDER — explicit escape hatch for one run.
//  2. A `forge.hosts.<host>.provider` declaration in the GLOBAL config —
//     the durable answer for a vanity domain nothing else can recognise.
//  3. Public GitHub.
//  4. GITHUB_API_URL / GH_HOST naming this host — GitHub Enterprise.
//  5. A glab-cli entry for this host — the user already authenticated there.
//  6. A "gitlab" label in the host name.
//  7. GitHub, preserving the historical default.
func providerFor(ctx context.Context, repoDir string) (ProviderKind, remote, error) {
	r, err := resolveRemote(ctx, repoDir)
	if err != nil {
		return "", remote{}, err
	}
	return providerForRemote(r), r, nil
}

// providerForRemote is the pure host→provider decision, split out so it is
// table-testable without a git repository.
func providerForRemote(r remote) ProviderKind {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_FORGE_PROVIDER"))) {
	case string(ProviderGitHub):
		return ProviderGitHub
	case string(ProviderGitLab):
		return ProviderGitLab
	}
	// A declared host wins over every inference below, but NOT over the env
	// override above: config is a durable statement about a host, the env var is
	// a deliberate one-run escape hatch, and the escape hatch has to be able to
	// override the file or it is not an escape hatch.
	if h, ok := declaredHost(r.Host); ok {
		switch strings.ToLower(strings.TrimSpace(h.Provider)) {
		case string(ProviderGitHub):
			return ProviderGitHub
		case string(ProviderGitLab):
			return ProviderGitLab
		}
		// An unrecognised provider string falls through to inference rather than
		// addressing the host with an arbitrarily-chosen API.
	}
	if isPublicGitHub(r.Host) {
		return ProviderGitHub
	}
	if base := enterpriseBase(); base != "" && hostOf(base) == r.Host {
		return ProviderGitHub
	}
	if _, ok := glabHost(r.Host); ok {
		return ProviderGitLab
	}
	if hasHostLabel(r.Host, "gitlab") {
		return ProviderGitLab
	}
	return ProviderGitHub
}

// hasHostLabel reports whether any dot-separated label of host equals label.
// It matches "gitlab.example.com" and "example.gitlab.io" but NOT
// "notgitlab.com" or "gitlabby.example.com", so a substring coincidence in an
// unrelated domain cannot misroute a repository.
func hasHostLabel(host, label string) bool {
	for _, part := range strings.Split(strings.ToLower(host), ".") {
		if part == label {
			return true
		}
	}
	return false
}

// glabConfig is the subset of glab-cli's config.yml this package reads.
type glabConfig struct {
	Hosts map[string]glabHostEntry `yaml:"hosts"`
}

// glabHostEntry is one host block of glab-cli's config.yml.
type glabHostEntry struct {
	Token       string `yaml:"token"`
	APIHost     string `yaml:"api_host"`
	APIProtocol string `yaml:"api_protocol"`
	Subfolder   string `yaml:"subfolder"`
}

// glabConfigPaths returns the candidate locations of glab-cli's config.yml, in
// probe order. glab stores it under the platform config dir, which is
// ~/Library/Application Support on macOS and ~/.config elsewhere; GLAB_CONFIG_DIR
// overrides both.
func glabConfigPaths() []string {
	if dir := strings.TrimSpace(os.Getenv("GLAB_CONFIG_DIR")); dir != "" {
		return []string{filepath.Join(dir, "config.yml")}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var paths []string
	if runtime.GOOS == "darwin" {
		paths = append(paths, filepath.Join(home, "Library", "Application Support", "glab-cli", "config.yml"))
	}
	paths = append(paths, filepath.Join(home, ".config", "glab-cli", "config.yml"))
	return paths
}

// glabHost returns the glab-cli entry for host, if the user has one. It is the
// zero-setup path: someone already signed in with `glab auth login` needs no
// GITLAB_TOKEN for Gortex to reach the same instance.
//
// A parse failure is reported as "no entry" rather than an error — glab's config
// is not Gortex's to validate, and a malformed one must not break a run that an
// environment token could still serve.
func glabHost(host string) (glabHostEntry, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return glabHostEntry{}, false
	}
	// Memoized: routing consults this up to three times per PR inside the
	// triage fan-out, and each miss was an os.ReadFile plus a yaml.Unmarshal.
	glabMu.Lock()
	if !glabLoaded {
		glabHosts, glabLoaded = loadGlabHosts(), true
	}
	entry, ok := glabHosts[host]
	glabMu.Unlock()
	return entry, ok
}

var (
	glabMu     sync.Mutex
	glabLoaded bool
	glabHosts  map[string]glabHostEntry
)

// resetGlabHosts forces the next lookup to re-read glab's config. Tests that
// swap GLAB_CONFIG_DIR mid-run call it.
func resetGlabHosts() {
	glabMu.Lock()
	glabHosts, glabLoaded = nil, false
	glabMu.Unlock()
}

// loadGlabHosts parses glab-cli's config once into a lowercased host map.
func loadGlabHosts() map[string]glabHostEntry {
	out := map[string]glabHostEntry{}
	for _, path := range glabConfigPaths() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg glabConfig
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			continue
		}
		for name, entry := range cfg.Hosts {
			if k := strings.ToLower(strings.TrimSpace(name)); k != "" {
				if _, seen := out[k]; !seen {
					out[k] = entry
				}
			}
		}
	}
	return out
}

// resolveGitLabToken returns the access token for host, or "" when none is
// resolvable.
//
// Order: the host's own declared variable, then its glab-cli entry, and only
// then the shared environment variables — and those LAST ones are host-scoped
// (see trustedForSharedToken).
//
// The scoping is the security-critical part. The destination URL is derived
// from a repository's own git remote, so it is attacker-controlled in any repo
// the user did not write. An unscoped GITLAB_TOKEN meant that cloning a repo
// whose origin is "https://gitlab.attacker.example/g/p.git" and running
// `gortex prs` sent the user's real gitlab.com token to the attacker's server.
// The GitHub backend was never exposed to this, because it derives its base URL
// only from GITHUB_API_URL / GH_HOST and never from the remote.
//
// Per-host credentials are also checked before shared ones so an operator with
// two instances gets each host's own token rather than whichever is in the
// environment.
func resolveGitLabToken(host string) string {
	if h, ok := declaredHost(host); ok {
		if env := strings.TrimSpace(h.TokenEnv); env != "" {
			if t := strings.TrimSpace(os.Getenv(env)); t != "" {
				return t
			}
		}
	}
	if entry, ok := glabHost(host); ok {
		if t := strings.TrimSpace(entry.Token); t != "" {
			return t
		}
	}
	if !trustedForSharedToken(host) {
		return ""
	}
	for _, env := range []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN"} {
		if t := strings.TrimSpace(os.Getenv(env)); t != "" {
			return t
		}
	}
	// CI_JOB_TOKEN is scoped harder still: it belongs to the instance running
	// the job, which CI_SERVER_HOST names.
	if t := strings.TrimSpace(os.Getenv("CI_JOB_TOKEN")); t != "" {
		if ciHost := strings.ToLower(strings.TrimSpace(os.Getenv("CI_SERVER_HOST"))); ciHost != "" && ciHost == strings.ToLower(host) {
			return t
		}
	}
	return ""
}

// trustedForSharedToken reports whether a shared, non-host-specific credential
// (GITLAB_TOKEN and friends) may be sent to host.
//
// A host earns that trust only by the user having named it: gitlab.com, a host
// declared in the global forge config, or a host they have already logged into
// with `glab auth login`. Every other host — including one that merely carries a
// "gitlab" label and therefore ROUTES to this backend — gets nothing, and the
// caller surfaces errNoGitLabToken naming the host.
func trustedForSharedToken(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if h == "gitlab.com" || strings.HasSuffix(h, ".gitlab.com") {
		return true
	}
	if _, ok := declaredHost(h); ok {
		return true
	}
	if _, ok := glabHost(h); ok {
		return true
	}
	return false
}

// gitlabAPIBase returns the REST v4 base URL for host, with a trailing slash.
// GITLAB_API_URL overrides everything; otherwise the glab-cli entry's api_host /
// api_protocol / subfolder are honoured, falling back to https://<host>/api/v4/.
func gitlabAPIBase(host string) string {
	if v := strings.TrimSpace(os.Getenv("GITLAB_API_URL")); v != "" {
		if base, ok := vettedAPIBase(v, host); ok {
			return base
		}
	}
	if h, ok := declaredHost(host); ok {
		if base, ok := vettedAPIBase(strings.TrimSpace(h.APIBase), host); ok {
			return base
		}
	}
	scheme, apiHost, subfolder := "https", host, ""
	if entry, ok := glabHost(host); ok {
		if p := strings.TrimSpace(entry.APIProtocol); p != "" {
			scheme = p
		}
		if h := strings.TrimSpace(entry.APIHost); h != "" {
			apiHost = h
		}
		subfolder = strings.Trim(strings.TrimSpace(entry.Subfolder), "/")
	}
	if !allowInsecureScheme(scheme, apiHost) {
		scheme = "https"
	}
	// url.URL rather than concatenation: it puts apiHost in the Host field, so a
	// value carrying userinfo or a path cannot smuggle the request to another
	// origin the way string-building allowed.
	u := url.URL{Scheme: scheme, Host: apiHost, Path: "/api/v4"}
	if subfolder != "" {
		u.Path = "/" + subfolder + "/api/v4"
	}
	return ensureTrailingSlash(u.String())
}

// vettedAPIBase validates an operator-supplied API base before a token is sent
// to it, returning ok=false when the base must be ignored in favour of the
// host-derived default.
//
// Both callers used to be returned VERBATIM and ahead of every check, which
// defeated two guarantees one layer down: the https floor never ran on them, and
// neither was keyed on the host. A single global GITLAB_API_URL therefore
// redirected EVERY GitLab repo's request — carrying whichever credential
// resolveGitLabToken picked for that repo's own host — to one unrelated origin.
//
// The rule: the base must parse, must be https (loopback excepted), and must
// either name the same host as the remote or be declared for that host by the
// operator. GITLAB_API_URL naming a different host is honoured only when that
// host is declared, since a global env var cannot express per-host intent.
func vettedAPIBase(raw, host string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", false
	}
	if !allowInsecureScheme(u.Scheme, u.Host) {
		return "", false
	}
	if !sameHost(u.Host, host) {
		if _, declared := declaredHost(hostOnly(u.Host)); !declared {
			return "", false
		}
	}
	return ensureTrailingSlash(u.String()), true
}

// sameHost compares two authorities ignoring case and any port.
func sameHost(a, b string) bool {
	return strings.EqualFold(hostOnly(a), hostOnly(b))
}

// hostOnly strips a port from an authority.
func hostOnly(h string) string { return strings.ToLower(strings.SplitN(h, ":", 2)[0]) }

// allowInsecureScheme reports whether a non-https scheme may carry a token to
// apiHost. Only loopback qualifies: everywhere else, a PRIVATE-TOKEN over
// cleartext hands the credential to anyone on the path.
func allowInsecureScheme(scheme, apiHost string) bool {
	if strings.EqualFold(scheme, "https") {
		return true
	}
	h := strings.ToLower(strings.SplitN(apiHost, ":", 2)[0])
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// errNoGitLabToken builds the host-aware not-authenticated error for a GitLab
// remote. It wraps ErrNotAuthenticated so errors.Is still identifies the class,
// while the message names the variable that would actually help — the whole
// point of routing by host before reporting a credential problem.
func errNoGitLabToken(host string) error {
	return fmt.Errorf("%w: no GitLab token for %s: set GITLAB_TOKEN (or run `glab auth login --hostname %s`)",
		ErrNotAuthenticated, host, host)
}

// loadForgeHosts reads the global forge host table. It is a package var so a
// test can swap it — the same func-var seam ghrest.go uses for makeGitHubClient
// — rather than reaching through config.Load on every run.
var loadForgeHosts = func() config.ForgeConfig {
	cfg, err := config.Load(config.DefaultGlobalConfigPath())
	if err != nil || cfg == nil {
		return config.ForgeConfig{}
	}
	return cfg.Forge
}

var (
	declaredMu     sync.Mutex
	declaredLoaded bool
	declaredHosts  config.ForgeConfig
)

// declaredHost returns the configured forge settings for host, if any.
//
// Only the GLOBAL config is consulted, never a repo's own `.gortex.yaml`. A
// repo-local file is attacker-supplied content in any repo you did not write —
// letting it name the API base and the token env var for its own host would let
// a cloned repo redirect where a credential is sent. Declaring a host is an
// operator decision, so it lives with the operator's config.
func declaredHost(host string) (config.ForgeHostConfig, bool) {
	declaredMu.Lock()
	if !declaredLoaded {
		declaredHosts = loadForgeHosts()
		declaredLoaded = true
	}
	table := declaredHosts
	declaredMu.Unlock()
	return table.ForgeHost(host)
}

// resetDeclaredHosts installs a host table directly and marks it loaded, so the
// lazy read never runs over it. Tests use it to detach from the developer's
// real ~/.gortex/config.yaml.
func resetDeclaredHosts(hosts config.ForgeConfig) {
	declaredMu.Lock()
	declaredHosts = hosts
	declaredLoaded = true
	declaredMu.Unlock()
}
