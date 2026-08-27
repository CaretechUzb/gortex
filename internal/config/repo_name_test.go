package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRepoName(t *testing.T) {
	cases := []struct {
		name  string
		input string
		valid bool
		why   string
	}{
		{name: "empty means derive from path", input: "", valid: true},
		{name: "plain prefix", input: "gortex", valid: true},
		{name: "worktree instance prefix keeps @", input: "local@aurora-redesign", valid: true},
		{name: "ticket id tag", input: "local@DEV-1284", valid: true},
		{name: "dots and underscores", input: "my.repo_v2", valid: true},
		{name: "digits only", input: "6343", valid: true},

		// The case that motivated the guard: real branch names carry
		// slashes, and a prefix is the first segment of every node ID.
		{name: "forward slash", input: "local@feat/DEV-777", valid: false, why: "/"},
		{name: "backslash", input: `local@feat\DEV-777`, valid: false, why: `\`},
		{name: "colon collides with the id separator", input: "local::x", valid: false, why: ":"},
		{name: "space", input: "local branch", valid: false, why: " "},
		{name: "leading slash", input: "/local", valid: false, why: "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRepoName(tc.input)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			// strconv.Quote, not the raw string: the message renders the
			// name with %q, so a backslash arrives escaped.
			require.Contains(t, err.Error(), strconv.Quote(tc.input),
				"the error must quote the offending name so the operator can find it")
		})
	}
}

// AddRepo is the gate every programmatic tracker goes through — the CLI's
// `gortex track --name`, the MCP track_repository tool, and daemon reload.
func TestAddRepoRejectsAnUnusableName(t *testing.T) {
	gc := &GlobalConfig{}
	err := gc.AddRepo(RepoEntry{Path: t.TempDir(), Name: "local@feat/DEV-777"})
	require.Error(t, err)
	require.Empty(t, gc.Repos, "a rejected entry must not be appended")

	require.NoError(t, gc.AddRepo(RepoEntry{Path: t.TempDir(), Name: "local@DEV-777"}))
	require.Len(t, gc.Repos, 1)
}

// A name that is already on disk reached the config by a hand-edit, so it
// cannot be refused at the door. Rejecting the single entry keeps the daemon
// up and every other repository queryable — the alternative, failing the load,
// turns a naming mistake into a total outage.
func TestRejectInvalidRepoNamesDropsOnlyTheOffendingEntry(t *testing.T) {
	good, bad, projGood, projBad := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: good, Name: "local@DEV-1284"},
			{Path: bad, Name: "local@feat/DEV-777"},
		},
		Projects: map[string]ProjectConfig{
			"work": {Repos: []RepoEntry{
				{Path: projGood, Name: "api"},
				{Path: projBad, Name: "api:v2"},
			}},
		},
	}

	rejected := gc.RejectInvalidRepoNames()
	require.Len(t, rejected, 2)

	require.Len(t, gc.Repos, 1)
	require.Equal(t, "local@DEV-1284", gc.Repos[0].Name)
	require.Len(t, gc.Projects["work"].Repos, 1,
		"a project entry is rejected the same way as a top-level one")
	require.Equal(t, "api", gc.Projects["work"].Repos[0].Name)

	byName := map[string]RepoNameRejection{}
	for _, r := range rejected {
		byName[r.Name] = r
	}
	require.Contains(t, byName, "local@feat/DEV-777")
	require.Contains(t, byName, "api:v2")
	require.Equal(t, "", byName["local@feat/DEV-777"].Project)
	require.Equal(t, "work", byName["api:v2"].Project,
		"the report must name the project so the operator can find the line")
	for _, r := range rejected {
		require.NotEmpty(t, r.Path, "the report must name the path")
		require.True(t, strings.Contains(r.Reason, r.Name),
			"the reason must quote the offending name")
	}
}

func TestRejectInvalidRepoNamesIsANoOpOnACleanConfig(t *testing.T) {
	gc := &GlobalConfig{Repos: []RepoEntry{
		{Path: t.TempDir(), Name: "local"},
		{Path: t.TempDir()}, // derived from the path — always legal
	}}
	require.Empty(t, gc.RejectInvalidRepoNames())
	require.Len(t, gc.Repos, 2)
}

// ResolvePrefix returns Name verbatim, which is exactly why the name has to be
// validated before it ever gets stored. This pins the contract the guard
// protects rather than the guard itself.
func TestResolvePrefixReturnsAValidatedNameVerbatim(t *testing.T) {
	require.Equal(t, "local@DEV-1284",
		ResolvePrefix(RepoEntry{Path: "/tmp/whatever", Name: "local@DEV-1284"}))
	require.Equal(t, "whatever",
		ResolvePrefix(RepoEntry{Path: "/tmp/whatever"}))
}
