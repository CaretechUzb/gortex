package hermes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/agents"
)

// installRoutingSkills seeds a Hermes skills tree the way Apply would,
// so the sync tests start from a real install rather than a fixture
// that could drift from what the adapter writes.
func installRoutingSkills(t *testing.T, home string) {
	t.Helper()
	routing := RoutingSkills()
	for _, name := range RoutingSkillNames() {
		path := skillPath(home, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(routing[name]), 0o644))
	}
}

// TestSyncSkillsNarrowsToProfileSubset is the behaviour that was missing:
// a profile switch shrank Claude Code's skill set and left Hermes at full
// size, so the same machine enforced two different profiles depending on
// which agent the user happened to open.
func TestSyncSkillsNarrowsToProfileSubset(t *testing.T) {
	home := t.TempDir()
	installRoutingSkills(t, home)

	allowed := []string{"gortex-explore", "gortex-debug"}
	_, err := SyncSkills(nil, home, allowed, agents.ApplyOpts{})
	require.NoError(t, err)

	for _, name := range RoutingSkillNames() {
		_, statErr := os.Stat(skillPath(home, name))
		if name == "gortex-explore" || name == "gortex-debug" {
			require.NoError(t, statErr, "%s is in the profile and must survive", name)
			continue
		}
		require.True(t, os.IsNotExist(statErr), "%s is outside the profile and should be pruned", name)
	}
}

// TestSyncSkillsKeepsTheMasterSkill pins the one skill that must outlive
// every profile: the native `gortex` master is not a routing skill, and
// pruning it would leave Hermes with no statement of what Gortex is.
func TestSyncSkillsKeepsTheMasterSkill(t *testing.T) {
	home := t.TempDir()
	installRoutingSkills(t, home)

	master := skillPath(home, SkillName)
	require.NoError(t, os.MkdirAll(filepath.Dir(master), 0o755))
	require.NoError(t, os.WriteFile(master, []byte("---\nname: gortex\n---\nmaster\n"), 0o644))

	_, err := SyncSkills(nil, home, []string{"gortex-explore"}, agents.ApplyOpts{})
	require.NoError(t, err)

	body, readErr := os.ReadFile(master)
	require.NoError(t, readErr)
	require.Equal(t, "---\nname: gortex\n---\nmaster\n", string(body))
}

// TestSyncSkillsKeepsACustomisedSkill holds the rule that decides whether
// a user loses work on an upgrade: a skill the user edited is kept even
// when the profile excludes it.
func TestSyncSkillsKeepsACustomisedSkill(t *testing.T) {
	home := t.TempDir()
	installRoutingSkills(t, home)

	edited := skillPath(home, "gortex-rename")
	require.NoError(t, os.WriteFile(edited, []byte("---\nname: gortex-rename\n---\nmine\n"), 0o644))

	_, err := SyncSkills(nil, home, []string{"gortex-explore"}, agents.ApplyOpts{})
	require.NoError(t, err)

	body, readErr := os.ReadFile(edited)
	require.NoError(t, readErr)
	require.Equal(t, "---\nname: gortex-rename\n---\nmine\n", string(body))
}

// TestSyncSkillsWithoutAnInstalledRootIsANoOp keeps a profile switch from
// conjuring a Hermes surface on a machine that never ran `gortex install`.
func TestSyncSkillsWithoutAnInstalledRootIsANoOp(t *testing.T) {
	home := t.TempDir()

	actions, err := SyncSkills(nil, home, []string{"gortex-explore"}, agents.ApplyOpts{})
	require.NoError(t, err)
	require.Empty(t, actions)

	_, statErr := os.Stat(filepath.Join(hermesDir(home), "skills"))
	require.True(t, os.IsNotExist(statErr))
}
