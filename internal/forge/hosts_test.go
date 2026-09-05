package forge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
)

// isolateForgeEnv detaches a test from every ambient source of forge routing.
//
// XDG_CONFIG_HOME is the one that bites: declaredHost reads the GLOBAL gortex
// config, so a developer with a real `forge:` block in ~/.gortex/config.yaml
// silently reroutes these tests (a `{host: github.com, provider: gitlab}` entry
// makes the GitHub dispatch case return *glClient). A test that depends on the
// machine it runs on is not a test.
func isolateForgeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GORTEX_FORGE_PROVIDER", "")
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GH_HOST", "")
	t.Setenv("GITLAB_API_URL", "")
	resetDeclaredHosts(config.ForgeConfig{})
	resetRemoteCache()
	resetGlabHosts()
	t.Cleanup(func() {
		resetDeclaredHosts(config.ForgeConfig{})
		resetRemoteCache()
		resetGlabHosts()
	})
}

func TestRemoteFrom(t *testing.T) {
	tests := []struct {
		name      string
		canonical string
		wantHost  string
		wantPath  string
		wantOK    bool
	}{
		{"github", "github.com/octo/gortex", "github.com", "octo/gortex", true},
		{"gitlab self-hosted", "gitlab.caretech.uz/his/his", "gitlab.caretech.uz", "his/his", true},
		{
			// The whole reason Path is not "the last two components": collapsing
			// a nested group would address group "sub", project "project" — a
			// different project, or none at all.
			name:      "gitlab nested subgroups keeps every component",
			canonical: "gitlab.com/group/sub/deeper/project",
			wantHost:  "gitlab.com",
			wantPath:  "group/sub/deeper/project",
			wantOK:    true,
		},
		{"trailing .git stripped", "github.com/octo/gortex.git", "github.com", "octo/gortex", true},
		{"host uppercased is normalized", "GitHub.com/Octo/Gortex", "github.com", "Octo/Gortex", true},
		{"no host component", "octo/gortex", "", "", false},
		{"single component", "gortex", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := remoteFrom(tc.canonical)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Host != tc.wantHost || got.Path != tc.wantPath {
				t.Errorf("got %q/%q, want %q/%q", got.Host, got.Path, tc.wantHost, tc.wantPath)
			}
		})
	}
}

func TestProviderForRemote(t *testing.T) {
	// Isolate from the developer's real glab config and forge env.
	isolateForgeEnv(t)

	tests := []struct {
		name string
		host string
		want ProviderKind
	}{
		{"public github", "github.com", ProviderGitHub},
		{"gitlab.com", "gitlab.com", ProviderGitLab},
		{"self-hosted gitlab by label", "gitlab.caretech.uz", ProviderGitLab},
		{"gitlab as a middle label", "code.gitlab.example.com", ProviderGitLab},
		// A substring match would misroute both of these to GitLab.
		{"notgitlab is not gitlab", "notgitlab.com", ProviderGitHub},
		{"gitlabby is not gitlab", "gitlabby.example.com", ProviderGitHub},
		{"unknown host defaults to github", "git.example.com", ProviderGitHub},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerForRemote(remote{Host: tc.host, Path: "a/b"}); got != tc.want {
				t.Errorf("providerForRemote(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

func TestProviderForRemote_EnvOverride(t *testing.T) {
	isolateForgeEnv(t)

	t.Setenv("GORTEX_FORGE_PROVIDER", "gitlab")
	if got := providerForRemote(remote{Host: "github.com", Path: "a/b"}); got != ProviderGitLab {
		t.Errorf("override to gitlab ignored: got %q", got)
	}
	t.Setenv("GORTEX_FORGE_PROVIDER", "github")
	if got := providerForRemote(remote{Host: "gitlab.com", Path: "a/b"}); got != ProviderGitHub {
		t.Errorf("override to github ignored: got %q", got)
	}
}

// TestProviderForRemote_EnterpriseWins guards the GHE regression: a GitHub
// Enterprise host is arbitrary, so nothing in its name says "github". Without
// the enterpriseBase check it would fall through to the default — and if it
// were ever named with a "gitlab" label it would be routed to the wrong API.
func TestProviderForRemote_EnterpriseWins(t *testing.T) {
	isolateForgeEnv(t)
	t.Setenv("GH_HOST", "gitlab-named-ghe.example.com")

	if got := providerForRemote(remote{Host: "gitlab-named-ghe.example.com", Path: "a/b"}); got != ProviderGitHub {
		t.Errorf("GHE host routed to %q, want github", got)
	}
}

// writeGlabConfig writes a minimal glab-cli config.yml into dir and points
// GLAB_CONFIG_DIR at it.
func writeGlabConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing glab config: %v", err)
	}
	t.Setenv("GLAB_CONFIG_DIR", dir)
	// The host table is memoized, so a mid-test swap must invalidate it.
	resetGlabHosts()
	t.Cleanup(resetGlabHosts)
}

func TestGlabHostAndToken(t *testing.T) {
	writeGlabConfig(t, `
hosts:
    gitlab.caretech.uz:
        token: glab-token-abc
        api_host: gitlab.caretech.uz
        api_protocol: https
`)
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_ACCESS_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")
	t.Setenv("GITLAB_API_URL", "")

	if got := resolveGitLabToken("gitlab.caretech.uz"); got != "glab-token-abc" {
		t.Errorf("token from glab config = %q, want glab-token-abc", got)
	}
	if got := resolveGitLabToken("gitlab.other.example"); got != "" {
		t.Errorf("token for an unconfigured host = %q, want empty", got)
	}
	// A glab entry is itself evidence the host is a GitLab, even though the
	// host name carries no "gitlab" label.
	writeGlabConfig(t, "hosts:\n    code.internal.example:\n        token: t\n")
	if got := providerForRemote(remote{Host: "code.internal.example", Path: "a/b"}); got != ProviderGitLab {
		t.Errorf("glab-configured host routed to %q, want gitlab", got)
	}
}

// TestResolveGitLabToken_HostSpecificBeatsShared pins the credential ordering.
//
// A glab-cli entry is HOST-SPECIFIC; GITLAB_TOKEN is shared across every GitLab
// host. The specific one wins, so an operator with two instances gets each
// host's own credential rather than whichever happens to be in the environment.
// (This inverted an earlier ordering, where the shared variable won — that made
// a second instance silently receive the first instance's token.)
func TestResolveGitLabToken_HostSpecificBeatsShared(t *testing.T) {
	isolateForgeEnv(t)
	writeGlabConfig(t, "hosts:\n    gitlab.example.com:\n        token: from-config\n")
	t.Setenv("GITLAB_TOKEN", "from-env")
	if got := resolveGitLabToken("gitlab.example.com"); got != "from-config" {
		t.Errorf("token = %q, want the host-specific glab token to win", got)
	}
	// A host with no entry of its own falls back to the shared variable only
	// because the glab config names it — see trustedForSharedToken.
	writeGlabConfig(t, "hosts:\n    gitlab.example.com:\n        token: \"\"\n")
	if got := resolveGitLabToken("gitlab.example.com"); got != "from-env" {
		t.Errorf("token = %q, want the shared variable when the entry has none", got)
	}
}

func TestGitlabAPIBase(t *testing.T) {
	t.Setenv("GITLAB_API_URL", "")

	writeGlabConfig(t, "hosts: {}\n")
	if got := gitlabAPIBase("gitlab.example.com"); got != "https://gitlab.example.com/api/v4/" {
		t.Errorf("default base = %q", got)
	}

	writeGlabConfig(t, `
hosts:
    gitlab.example.com:
        api_host: api.gitlab.example.com
        api_protocol: http
        subfolder: /gitlab/
`)
	// api_protocol: http is UPGRADED to https for a non-loopback host — a token
	// on a cleartext connection is readable by anyone on the path. See
	// TestGitlabAPIBase_HTTPSFloor for the loopback exception.
	if got := gitlabAPIBase("gitlab.example.com"); got != "https://api.gitlab.example.com/gitlab/api/v4/" {
		t.Errorf("configured base = %q", got)
	}

	// A cross-host override is REFUSED unless the operator declared that host —
	// a global env var cannot express per-host intent, and honouring it sent the
	// repo's own credential to an unrelated origin. See
	// TestVettedAPIBase_HostBoundAndHTTPS.
	t.Setenv("GITLAB_API_URL", "https://override.example/api/v4")
	if got := gitlabAPIBase("gitlab.example.com"); got == "https://override.example/api/v4/" {
		t.Errorf("undeclared cross-host override accepted: %q", got)
	}
}

// TestErrNoGitLabTokenIsNotAuthenticated is the regression guard for the bug
// this whole change exists to fix: a GitLab repo must still satisfy
// errors.Is(err, ErrNotAuthenticated) for existing callers, while the MESSAGE
// names GITLAB_TOKEN rather than sending the user after GH_TOKEN.
func TestErrNoGitLabTokenIsNotAuthenticated(t *testing.T) {
	err := errNoGitLabToken("gitlab.caretech.uz")
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("errors.Is(err, ErrNotAuthenticated) = false")
	}
	msg := err.Error()
	if !strings.Contains(msg, "GITLAB_TOKEN") {
		t.Errorf("message %q does not name GITLAB_TOKEN", msg)
	}
	if strings.Contains(msg, "GH_TOKEN") {
		t.Errorf("message %q still points at GH_TOKEN", msg)
	}
	if !strings.Contains(msg, "gitlab.caretech.uz") {
		t.Errorf("message %q does not name the host", msg)
	}
}

// TestRemoteFrom_RejectsFilesystemPaths is the regression guard for a shipped
// bug: remoteFrom accepted ANY >=3-segment string, so indexer.DetectIdentity's
// path-shaped CanonicalID for a non-git directory parsed as a remote.
// resolveRemote then never failed, which made every documented
// unresolvable-remote fallback in this package unreachable, and PRWebURL
// invented URLs like "https://var/folders/x/y/001/pull/7".
func TestRemoteFrom_RejectsFilesystemPaths(t *testing.T) {
	for _, in := range []string{
		"/var/folders/sp/abc/T/001",
		"/Users/commeta/projects/gortex",
		"var/folders/sp/abc/T/001",
		"tmp/xyz/abc",
		"home/user/code/repo",
	} {
		if r, ok := remoteFrom(in); ok {
			t.Errorf("remoteFrom(%q) accepted a filesystem path as host=%q path=%q", in, r.Host, r.Path)
		}
	}
}

func TestLooksLikeHost(t *testing.T) {
	for in, want := range map[string]bool{
		"github.com":         true,
		"gitlab.caretech.uz": true,
		"localhost":          true,
		"localhost:8080":     true,
		"git.example.com":    true,
		"var":                false,
		"users":              false,
		"tmp":                false,
		"gitlab":             false, // bare single-label host: rejected on purpose
		"":                   false,
	} {
		if got := looksLikeHost(in); got != want {
			t.Errorf("looksLikeHost(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestResolveRemote_FailsOnBareDirectory pins the contract the fallbacks depend
// on: a directory with no git remote must produce an ERROR, not a fabricated
// remote. Without this, Available / MissingTokenHint / PRWebURL / defaultNewClient
// all silently take their success paths on garbage input.
func TestResolveRemote_FailsOnBareDirectory(t *testing.T) {
	resetRemoteCache()
	t.Cleanup(resetRemoteCache)
	dir := t.TempDir()
	r, err := resolveRemote(context.Background(), dir)
	if err == nil {
		t.Fatalf("resolveRemote(%q) = %+v, want an error for a dir with no remote", dir, r)
	}
}

// gitRepoWithRemote makes a real git repo whose origin is remoteURL, so the
// host-routing helpers can be exercised end to end instead of only through
// their pure inner functions (which is how PRWebURL / MissingTokenHint /
// ProviderName all sat at 0% coverage while looking tested).
func gitRepoWithRemote(t *testing.T, remoteURL string) string {
	t.Helper()
	resetRemoteCache()
	t.Cleanup(resetRemoteCache)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", remoteURL},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed (%v): %s", args, err, out)
		}
	}
	return dir
}

func TestPRWebURL_PerProviderShape(t *testing.T) {
	isolateForgeEnv(t)
	ctx := context.Background()

	gh := gitRepoWithRemote(t, "https://github.com/octo/gortex.git")
	if got := PRWebURL(ctx, gh, 7); got != "https://github.com/octo/gortex/pull/7" {
		t.Errorf("github PRWebURL = %q", got)
	}
	// GitLab uses /-/merge_requests/, and the "/-/" separator is what keeps a
	// nested group path distinguishable from the route.
	gl := gitRepoWithRemote(t, "https://gitlab.example.com/group/sub/proj.git")
	if got := PRWebURL(ctx, gl, 6413); got != "https://gitlab.example.com/group/sub/proj/-/merge_requests/6413" {
		t.Errorf("gitlab PRWebURL = %q", got)
	}
	// No remote, and a non-positive number, both yield "".
	if got := PRWebURL(ctx, t.TempDir(), 7); got != "" {
		t.Errorf("PRWebURL with no remote = %q, want empty", got)
	}
	if got := PRWebURL(ctx, gh, 0); got != "" {
		t.Errorf("PRWebURL(num=0) = %q, want empty", got)
	}
}

// TestMissingTokenHint_NamesTheRightToken is the whole point of the change:
// a GitLab remote must never be told to set GH_TOKEN.
func TestMissingTokenHint_NamesTheRightToken(t *testing.T) {
	isolateForgeEnv(t)
	ctx := context.Background()

	gh := gitRepoWithRemote(t, "https://github.com/octo/gortex.git")
	if got := MissingTokenHint(ctx, gh); !strings.Contains(got, "GH_TOKEN") || strings.Contains(got, "GITLAB_TOKEN") {
		t.Errorf("github hint = %q", got)
	}
	gl := gitRepoWithRemote(t, "https://gitlab.example.com/his/his.git")
	got := MissingTokenHint(ctx, gl)
	if !strings.Contains(got, "GITLAB_TOKEN") {
		t.Errorf("gitlab hint %q does not name GITLAB_TOKEN", got)
	}
	if strings.Contains(got, "GH_TOKEN") {
		t.Errorf("gitlab hint %q still points at GH_TOKEN", got)
	}
	if !strings.Contains(got, "gitlab.example.com") {
		t.Errorf("gitlab hint %q does not name the host", got)
	}
	// An unresolvable remote falls back to the GitHub wording — and it must
	// actually REACH that fallback (see TestResolveRemote_FailsOnBareDirectory).
	if got := MissingTokenHint(ctx, t.TempDir()); !strings.Contains(got, "GH_TOKEN") {
		t.Errorf("fallback hint = %q, want the GitHub wording", got)
	}
}

func TestProviderName_FromRemote(t *testing.T) {
	isolateForgeEnv(t)
	ctx := context.Background()

	if got := ProviderName(ctx, gitRepoWithRemote(t, "https://github.com/o/r.git")); got != "github" {
		t.Errorf("ProviderName(github) = %q", got)
	}
	if got := ProviderName(ctx, gitRepoWithRemote(t, "git@gitlab.example.com:g/p.git")); got != "gitlab" {
		t.Errorf("ProviderName(gitlab ssh) = %q", got)
	}
	if got := ProviderName(ctx, t.TempDir()); got != "github" {
		t.Errorf("ProviderName(no remote) = %q, want the github default", got)
	}
}

// TestAvailable_GitLabRemoteUsesGitLabToken pins the branch that was dead
// before the remoteFrom fix: on a GitLab remote, availability is decided by the
// GitLab credential, not by GH_TOKEN.
func TestAvailable_GitLabRemoteUsesGitLabToken(t *testing.T) {
	isolateForgeEnv(t)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "gh-tok-present")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_ACCESS_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")
	ctx := context.Background()

	// The host must be one the user has named, or the shared token is
	// deliberately withheld — see TestResolveGitLabToken_IsHostScoped.
	declareHosts(t, []config.ForgeHostConfig{{Host: "gitlab.example.com", Provider: "gitlab"}})
	gl := gitRepoWithRemote(t, "https://gitlab.example.com/his/his.git")
	if Available(ctx, gl) {
		t.Error("Available = true on a GitLab remote with only a GitHub token set")
	}
	t.Setenv("GITLAB_TOKEN", "gl-tok")
	if !Available(ctx, gl) {
		t.Error("Available = false on a GitLab remote with GITLAB_TOKEN set")
	}
}

// TestDefaultNewClient_RoutesByHost is the plan's core verification item: the
// seam must actually hand back a GitLab client for a GitLab remote. Every other
// test exercises the pure decision helpers; this one pins the dispatch itself,
// which is the one thing that makes the backend reachable at all.
func TestDefaultNewClient_RoutesByHost(t *testing.T) {
	isolateForgeEnv(t)
	t.Setenv("GH_TOKEN", "gh-tok")
	t.Setenv("GITLAB_TOKEN", "gl-tok")
	declareHosts(t, []config.ForgeHostConfig{{Host: "gitlab.example.com", Provider: "gitlab"}})
	ctx := context.Background()

	gh, err := defaultNewClient(ctx, gitRepoWithRemote(t, "https://github.com/octo/gortex.git"))
	if err != nil {
		t.Fatalf("github dispatch: %v", err)
	}
	if _, ok := gh.(*ghClient); !ok {
		t.Errorf("github remote produced %T, want *ghClient", gh)
	}

	gl, err := defaultNewClient(ctx, gitRepoWithRemote(t, "https://gitlab.example.com/group/sub/proj.git"))
	if err != nil {
		t.Fatalf("gitlab dispatch: %v", err)
	}
	glc, ok := gl.(*glClient)
	if !ok {
		t.Fatalf("gitlab remote produced %T, want *glClient", gl)
	}
	// The nested group path must survive URL-escaped into the project selector;
	// collapsing it to the last two segments would address a different project.
	if glc.project != "group%2Fsub%2Fproj" {
		t.Errorf("project = %q, want group%%2Fsub%%2Fproj", glc.project)
	}
	if glc.base != "https://gitlab.example.com/api/v4/" {
		t.Errorf("base = %q", glc.base)
	}

	// A directory with no remote now FAILS rather than producing a client
	// pointed at a fabricated slug. Before the host-shape guard, the
	// path-shaped CanonicalID "/var/folders/x/y/001" parsed as owner "y",
	// repo "001", so every call went to a repository that does not exist and
	// the error arrived from the forge as a confusing 404.
	if c, err := defaultNewClient(ctx, t.TempDir()); err == nil {
		t.Errorf("no-remote dispatch produced %T, want an error naming the unreadable remote", c)
	}
}

// TestNewGLClient_RequiresGitLabToken pins the credential half of the dispatch:
// a GitLab remote with no GitLab token must fail naming GITLAB_TOKEN, even when
// a GitHub token is sitting right there in the environment.
func TestNewGLClient_RequiresGitLabToken(t *testing.T) {
	isolateForgeEnv(t)
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_ACCESS_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")
	t.Setenv("GH_TOKEN", "gh-tok-present")

	_, err := newGLClient(context.Background(), gitRepoWithRemote(t, "https://gitlab.example.com/his/his.git"))
	if err == nil {
		t.Fatal("newGLClient succeeded with no GitLab token")
	}
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("err = %v, want errors.Is ErrNotAuthenticated", err)
	}
	if !strings.Contains(err.Error(), "GITLAB_TOKEN") || strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("err = %q, want it to name GITLAB_TOKEN and not GH_TOKEN", err)
	}
}

// declareHosts installs a forge host table for the duration of a test.
func declareHosts(t *testing.T, hosts []config.ForgeHostConfig) {
	t.Helper()
	resetDeclaredHosts(config.ForgeConfig{Hosts: hosts})
	t.Cleanup(func() { resetDeclaredHosts(config.ForgeConfig{}) })
}

// TestDeclaredHost_RoutesAVanityDomain is the case automatic routing cannot
// reach: a GitLab whose hostname says nothing about GitLab and which the user
// has never run `glab auth login` against.
func TestDeclaredHost_RoutesAVanityDomain(t *testing.T) {
	isolateForgeEnv(t)

	vanity := remote{Host: "code.internal.corp", Path: "team/app"}
	if got := providerForRemote(vanity); got != ProviderGitHub {
		t.Fatalf("undeclared vanity host = %q, want the github default", got)
	}
	declareHosts(t, []config.ForgeHostConfig{
		{Host: "code.internal.corp", Provider: "gitlab"},
	})
	if got := providerForRemote(vanity); got != ProviderGitLab {
		t.Errorf("declared vanity host = %q, want gitlab", got)
	}
	// Host matching is case-insensitive.
	if got := providerForRemote(remote{Host: "CODE.Internal.Corp", Path: "a/b"}); got != ProviderGitLab {
		t.Errorf("case-insensitive match failed: got %q", got)
	}
}

// TestDeclaredHost_EnvStillWins pins the precedence: the env var is a one-run
// escape hatch and must be able to override a durable config declaration.
func TestDeclaredHost_EnvStillWins(t *testing.T) {
	isolateForgeEnv(t)
	declareHosts(t, []config.ForgeHostConfig{
		{Host: "code.internal.corp", Provider: "gitlab"},
	})
	t.Setenv("GORTEX_FORGE_PROVIDER", "github")
	if got := providerForRemote(remote{Host: "code.internal.corp", Path: "a/b"}); got != ProviderGitHub {
		t.Errorf("env override lost to config: got %q, want github", got)
	}
}

// TestDeclaredHost_BadProviderFallsThrough: a typo must not pick an arbitrary
// backend — addressing a GitLab with the GitHub API is worse than inferring.
func TestDeclaredHost_BadProviderFallsThrough(t *testing.T) {
	isolateForgeEnv(t)
	declareHosts(t, []config.ForgeHostConfig{
		{Host: "gitlab.example.com", Provider: "gitlabb"}, // typo
	})
	// Falls through to inference, which still recognises the gitlab label.
	if got := providerForRemote(remote{Host: "gitlab.example.com", Path: "a/b"}); got != ProviderGitLab {
		t.Errorf("typo'd provider = %q, want inference to still say gitlab", got)
	}
}

func TestDeclaredHost_APIBaseAndTokenEnv(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GITLAB_API_URL", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_ACCESS_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")
	declareHosts(t, []config.ForgeHostConfig{{
		Host:     "code.internal.corp",
		Provider: "gitlab",
		APIBase:  "https://code.internal.corp/gitlab/api/v4",
		TokenEnv: "CORP_GITLAB_TOKEN",
	}})

	if got := gitlabAPIBase("code.internal.corp"); got != "https://code.internal.corp/gitlab/api/v4/" {
		t.Errorf("declared api_base = %q", got)
	}
	if got := resolveGitLabToken("code.internal.corp"); got != "" {
		t.Errorf("token = %q with CORP_GITLAB_TOKEN unset, want empty", got)
	}
	t.Setenv("CORP_GITLAB_TOKEN", "corp-tok")
	if got := resolveGitLabToken("code.internal.corp"); got != "corp-tok" {
		t.Errorf("token = %q, want the declared env var's value", got)
	}
	// A per-host declaration beats the generic variable — two instances need
	// two credentials, which one shared GITLAB_TOKEN cannot express.
	t.Setenv("GITLAB_TOKEN", "generic-tok")
	if got := resolveGitLabToken("code.internal.corp"); got != "corp-tok" {
		t.Errorf("token = %q, want the host-specific one to win", got)
	}
	// An undeclared host gets NOTHING, even with the generic variable set.
	// Sending it would hand the user's token to whatever host a cloned repo's
	// remote happened to name.
	if got := resolveGitLabToken("other.example.com"); got != "" {
		t.Errorf("undeclared host token = %q, want empty (shared tokens are host-scoped)", got)
	}
	// GITLAB_API_URL overrides a declared base only for the SAME host.
	t.Setenv("GITLAB_API_URL", "https://code.internal.corp/alt/api/v4")
	if got := gitlabAPIBase("code.internal.corp"); got != "https://code.internal.corp/alt/api/v4/" {
		t.Errorf("same-host env override = %q, want it to win", got)
	}
	// A cross-host one is refused and the declaration stands.
	t.Setenv("GITLAB_API_URL", "https://elsewhere.example/api/v4")
	if got := gitlabAPIBase("code.internal.corp"); got != "https://code.internal.corp/gitlab/api/v4" && got != "https://code.internal.corp/gitlab/api/v4/" {
		t.Errorf("cross-host env override = %q, want the declared base to stand", got)
	}
}

// TestLooksLikeHost_RejectsUserinfoSmuggling is the regression guard for a
// credential-exfiltration bug. The original check was a presence test
// (strings.ContainsAny(s, ".:")), so "gitlab.com@evil.example" passed it,
// matched hasHostLabel(...,"gitlab"), routed to the GitLab backend, and built
// "https://gitlab.com@evil.example/api/v4/" — where gitlab.com is USERINFO and
// the token-bearing request actually went to evil.example. The host comes
// straight from a repo's git remote, so it is attacker-controlled in any repo
// the user did not write.
func TestLooksLikeHost_RejectsUserinfoSmuggling(t *testing.T) {
	for _, bad := range []string{
		"gitlab.com@evil.example",
		"gitlab.com@evil.example:443",
		"gitlab.com/evil.example",
		"gitlab.com?x=1",
		"gitlab.com#frag",
		"gitlab.com\\evil.example",
		"gitlab .com",
		"gitlab.com\nevil.example",
		"-leading.hyphen.com",
		"trailing.hyphen-.com",
		"host.com:notaport",
	} {
		if looksLikeHost(bad) {
			t.Errorf("looksLikeHost(%q) = true, want false", bad)
		}
	}
	// Through remoteFrom, only the forms that could smuggle an authority are
	// checked. A "/" is NOT one of them: "gitlab.com/evil.example/g/p" simply
	// parses as host gitlab.com with a longer project path, which addresses the
	// real gitlab.com and redirects nothing.
	for _, bad := range []string{
		"gitlab.com@evil.example",
		"gitlab.com@evil.example:443",
		"gitlab .com",
		"-leading.hyphen.com",
	} {
		if r, ok := remoteFrom(bad + "/g/p"); ok {
			t.Errorf("remoteFrom accepted %q as host=%q", bad, r.Host)
		}
	}
	for _, good := range []string{
		"github.com", "gitlab.caretech.uz", "code.internal.corp",
		"localhost", "localhost:8080", "gitlab.example.com:8443",
	} {
		if !looksLikeHost(good) {
			t.Errorf("looksLikeHost(%q) = false, want true", good)
		}
	}
}

// TestResolveGitLabToken_IsHostScoped is the regression guard for credential
// redirection. The shared GITLAB_TOKEN used to be returned for ANY host that
// routed to GitLab, while the destination URL came from the repo's own remote —
// so cloning a repo whose origin was gitlab.attacker.example and running
// `gortex prs` sent the user's real gitlab.com token to the attacker.
func TestResolveGitLabToken_IsHostScoped(t *testing.T) {
	isolateForgeEnv(t)
	t.Setenv("GITLAB_TOKEN", "REAL-GITLAB-COM-PAT")
	t.Setenv("GITLAB_ACCESS_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")
	t.Setenv("CI_SERVER_HOST", "")
	resetDeclaredHosts(config.ForgeConfig{})

	// gitlab.com is named by the variable itself — trusted.
	if got := resolveGitLabToken("gitlab.com"); got != "REAL-GITLAB-COM-PAT" {
		t.Errorf("gitlab.com token = %q, want the shared token", got)
	}
	// An arbitrary gitlab-labelled host is NOT.
	for _, host := range []string{
		"gitlab.attacker.example",
		"gitlab.internal.corp",
		"evil.gitlab.io",
	} {
		if got := resolveGitLabToken(host); got != "" {
			t.Errorf("resolveGitLabToken(%q) = %q — the shared token leaked to an unnamed host", host, got)
		}
	}
	// Declaring the host is how a user opts in.
	declareHosts(t, []config.ForgeHostConfig{{Host: "gitlab.internal.corp", Provider: "gitlab"}})
	if got := resolveGitLabToken("gitlab.internal.corp"); got != "REAL-GITLAB-COM-PAT" {
		t.Errorf("declared host token = %q, want the shared token", got)
	}
}

// TestResolveGitLabToken_CIJobTokenBoundToCIServerHost: a job token belongs to
// the instance running the job, and CI_SERVER_HOST names it.
func TestResolveGitLabToken_CIJobTokenBoundToCIServerHost(t *testing.T) {
	isolateForgeEnv(t)
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_ACCESS_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "job-tok")
	t.Setenv("CI_SERVER_HOST", "gitlab.company.com")
	declareHosts(t, []config.ForgeHostConfig{
		{Host: "gitlab.company.com", Provider: "gitlab"},
		{Host: "gitlab.other.com", Provider: "gitlab"},
	})

	if got := resolveGitLabToken("gitlab.company.com"); got != "job-tok" {
		t.Errorf("CI host token = %q, want job-tok", got)
	}
	if got := resolveGitLabToken("gitlab.other.com"); got != "" {
		t.Errorf("job token leaked to %q: %q", "gitlab.other.com", got)
	}
}

// TestGitlabAPIBase_HTTPSFloor: a token must not ride cleartext except to loopback.
func TestGitlabAPIBase_HTTPSFloor(t *testing.T) {
	t.Setenv("GITLAB_API_URL", "")
	resetDeclaredHosts(config.ForgeConfig{})
	writeGlabConfig(t, `
hosts:
    gitlab.example.com:
        api_protocol: http
    localhost:
        api_protocol: http
`)
	if got := gitlabAPIBase("gitlab.example.com"); !strings.HasPrefix(got, "https://") {
		t.Errorf("base = %q, want https forced for a remote host", got)
	}
	if got := gitlabAPIBase("localhost"); !strings.HasPrefix(got, "http://") {
		t.Errorf("base = %q, want http allowed for loopback", got)
	}
}

// TestDeclaredHost_IgnoresRepoLocalConfig pins the credential-redirect guard
// that declaredHost's doc comment claims but nothing enforced.
//
// A repo's own .gortex.yaml is attacker-supplied content in any repo the user
// did not write. If routing honoured it, a cloned repo could name its own
// api_base and token_env and redirect the user's credential to a host of its
// choosing. Only the operator's global config may declare a host.
//
// This test exists because internal/config's parse test writes the same block
// into a repo-local .gortex.yaml and asserts it PARSES — which reads as the
// opposite guarantee. Parsing is not honouring.
func TestDeclaredHost_IgnoresRepoLocalConfig(t *testing.T) {
	isolateForgeEnv(t)
	dir := gitRepoWithRemote(t, "https://gitlab.example.com/his/his.git")
	hostile := "forge:\n" +
		"  hosts:\n" +
		"    - host: gitlab.example.com\n" +
		"      provider: gitlab\n" +
		"      api_base: https://attacker.example/api/v4\n" +
		"      token_env: GITLAB_TOKEN\n"
	if err := os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(hostile), 0o600); err != nil {
		t.Fatalf("writing repo-local config: %v", err)
	}
	t.Setenv("GITLAB_TOKEN", "VICTIM-TOKEN")

	if got := gitlabAPIBase("gitlab.example.com"); strings.Contains(got, "attacker.example") {
		t.Fatalf("a repo-local .gortex.yaml redirected the API base to %q", got)
	}
	if got := gitlabAPIBase("gitlab.example.com"); got != "https://gitlab.example.com/api/v4/" {
		t.Errorf("api base = %q, want the host's own", got)
	}
	// And the repo-local declaration must not make the host trusted for the
	// shared token either.
	if got := resolveGitLabToken("gitlab.example.com"); got != "" {
		t.Errorf("repo-local declaration leaked the shared token: %q", got)
	}
	if _, ok := declaredHost("gitlab.example.com"); ok {
		t.Error("declaredHost honoured a repo-local .gortex.yaml")
	}
}

// TestDeclaredHost_GitHubBeatsGitlabLabel proves a declaration beats INFERENCE,
// not merely the default. A GitHub Enterprise at a gitlab-labelled vanity
// domain is exactly the case the config block advertises.
func TestDeclaredHost_GitHubBeatsGitlabLabel(t *testing.T) {
	isolateForgeEnv(t)
	// A genuine "gitlab" label — "gitlab-ghe" would not match, since
	// hasHostLabel compares whole dot-separated labels.
	ghe := remote{Host: "gitlab.corp.example", Path: "team/app"}
	if got := providerForRemote(ghe); got != ProviderGitLab {
		t.Fatalf("control: undeclared gitlab-labelled host = %q, want gitlab", got)
	}
	declareHosts(t, []config.ForgeHostConfig{{Host: "gitlab.corp.example", Provider: "github"}})
	if got := providerForRemote(ghe); got != ProviderGitHub {
		t.Errorf("declared provider:github lost to label inference: got %q", got)
	}
}

// TestResolveSlug_SharesRemoteResolution pins the dedup: resolveSlug is now
// expressed over resolveRemote, so the GitHub path inherits BOTH the host-shape
// guard and remoteCache. Before, it ran its own copy of the resolution ladder —
// which meant it accepted a path-shaped CanonicalID that remoteFrom rejects, and
// it forked `git remote get-url` on every client construction, bypassing the
// cache added precisely to stop that in the triage fan-out.
func TestResolveSlug_SharesRemoteResolution(t *testing.T) {
	isolateForgeEnv(t)
	ctx := context.Background()
	dir := gitRepoWithRemote(t, "https://github.com/octo/gortex.git")

	owner, repo, err := resolveSlug(ctx, dir)
	if err != nil {
		t.Fatalf("resolveSlug: %v", err)
	}
	if owner != "octo" || repo != "gortex" {
		t.Errorf("slug = %q/%q, want octo/gortex", owner, repo)
	}

	// The resolution is now memoized, so the second call must not re-fork git.
	// Proven by removing the repo's git dir: a cached answer still resolves.
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("removing .git: %v", err)
	}
	owner2, repo2, err := resolveSlug(ctx, dir)
	if err != nil {
		t.Fatalf("resolveSlug after cache: %v — the GitHub path is not sharing remoteCache", err)
	}
	if owner2 != owner || repo2 != repo {
		t.Errorf("cached slug = %q/%q, want %q/%q", owner2, repo2, owner, repo)
	}

	// And a directory with no remote errors rather than inventing a slug from
	// its filesystem path.
	if o, r, err := resolveSlug(ctx, t.TempDir()); err == nil {
		t.Errorf("resolveSlug on a bare dir = %q/%q, want an error", o, r)
	}
}

// TestVettedAPIBase_HostBoundAndHTTPS is the regression guard for a fix that
// defeated itself: GITLAB_API_URL and a declared api_base were returned
// VERBATIM and ahead of every check, so the https floor never ran on them and
// neither was keyed on the host. One global env var redirected every GitLab
// repo's token-bearing request to a single unrelated origin.
func TestVettedAPIBase_HostBoundAndHTTPS(t *testing.T) {
	isolateForgeEnv(t)

	// Cleartext is refused, so the host-derived https default is used instead.
	t.Setenv("GITLAB_API_URL", "http://gitlab.example.com/api/v4")
	if got := gitlabAPIBase("gitlab.example.com"); got != "https://gitlab.example.com/api/v4/" {
		t.Errorf("cleartext override accepted: %q", got)
	}

	// A different host is refused unless the operator declared it.
	t.Setenv("GITLAB_API_URL", "https://internal.corp/api/v4")
	if got := gitlabAPIBase("gitlab.example.com"); got != "https://gitlab.example.com/api/v4/" {
		t.Errorf("cross-host override accepted: %q — a token would go to internal.corp", got)
	}
	// Declaring it is how an operator opts in.
	declareHosts(t, []config.ForgeHostConfig{{Host: "internal.corp", Provider: "gitlab"}})
	if got := gitlabAPIBase("gitlab.example.com"); got != "https://internal.corp/api/v4/" {
		t.Errorf("declared cross-host override rejected: %q", got)
	}
	// Same host over https is always fine.
	t.Setenv("GITLAB_API_URL", "https://gitlab.example.com/gitlab/api/v4")
	if got := gitlabAPIBase("gitlab.example.com"); got != "https://gitlab.example.com/gitlab/api/v4/" {
		t.Errorf("same-host https override = %q", got)
	}
	// Userinfo is refused outright.
	t.Setenv("GITLAB_API_URL", "https://gitlab.example.com@evil.example/api/v4")
	if got := gitlabAPIBase("gitlab.example.com"); strings.Contains(got, "evil.example") {
		t.Errorf("userinfo smuggled through the override: %q", got)
	}
}

// TestResolveRemote_RefusesEmptyRepoDir: an empty path means "no resolvable
// working tree", never "use the process's cwd". Before this, filepath.Abs("")
// resolved to the DAEMON's own directory, so an unresolvable scope answered
// from an unrelated repository — and remoteCache pinned that under the key "".
func TestResolveRemote_RefusesEmptyRepoDir(t *testing.T) {
	isolateForgeEnv(t)
	for _, in := range []string{"", "   "} {
		if r, err := resolveRemote(context.Background(), in); err == nil {
			t.Errorf("resolveRemote(%q) = %+v, want an error", in, r)
		}
	}
	if _, ok := remoteCache.Load(""); ok {
		t.Error(`the empty key was cached`)
	}
}

// TestEmptyRepoDir_NeverAnswersFromAnotherRepo is the load-bearing half of the
// empty-scope fix.
//
// The MCP handlers deliberately do NOT pre-check for an empty root — they are
// documented to degrade, and an early return there broke the rate-limit path.
// So the guarantee has to hold HERE: with an empty path, nothing may answer
// from the process's own working directory, and nothing may be cached under "".
func TestEmptyRepoDir_NeverAnswersFromAnotherRepo(t *testing.T) {
	isolateForgeEnv(t)
	ctx := context.Background()

	if _, err := resolveRemote(ctx, ""); err == nil {
		t.Fatal("resolveRemote(\"\") succeeded — it resolved some other repository")
	}
	if _, ok := remoteCache.Load(""); ok {
		t.Error("the empty key was cached, pinning a wrong answer for the process lifetime")
	}
	// The derived answers fall back to their provider-neutral defaults rather
	// than describing whatever repo the process happens to be sitting in.
	if got := ProviderName(ctx, ""); got != string(ProviderGitHub) {
		t.Errorf("ProviderName(\"\") = %q, want the github default", got)
	}
	if got := PRWebURL(ctx, "", 7); got != "" {
		t.Errorf("PRWebURL(\"\", 7) = %q, want empty — that URL named another repo", got)
	}
	if got := MissingTokenHint(ctx, ""); !strings.Contains(got, "GH_TOKEN") {
		t.Errorf("MissingTokenHint(\"\") = %q, want the neutral GitHub wording", got)
	}
	// And a slug cannot be invented from the process cwd either.
	if o, r, err := resolveSlug(ctx, ""); err == nil {
		t.Errorf("resolveSlug(\"\") = %q/%q, want an error", o, r)
	}
}
