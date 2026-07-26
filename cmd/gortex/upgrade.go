package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/zzet/gortex/internal/daemon"
)

// InstallMethod is how this gortex binary was installed, inferred from where it
// lives on disk. Each method maps to the right self-update command, so
// `gortex upgrade` upgrades the way the user installed rather than clobbering a
// package-manager-owned binary with a raw download.
type InstallMethod string

const (
	InstallBrew      InstallMethod = "brew"       // Homebrew Cellar
	InstallScoop     InstallMethod = "scoop"      // Scoop apps dir (Windows)
	InstallGoInstall InstallMethod = "go-install" // $GOPATH/bin or ~/go/bin
	InstallScript    InstallMethod = "script"     // get.gortex.dev installer → ~/.local/bin
	InstallUnknown   InstallMethod = "unknown"    // manual download / packaged elsewhere
)

const upgradeRepoURL = "https://github.com/zzet/gortex"

// detectInstallMethod infers the install method from the binary's path. brew
// and scoop are recognised by their well-known directory anchors; a binary
// under the Go bin dir is a `go install`; one under ~/.local/bin is the
// installer script's target. Everything else is unknown (the user gets the
// release-page fallback). Paths are slash-normalised so the same logic works on
// Windows.
func detectInstallMethod(execPath, goBinDir, homeDir string) InstallMethod {
	// Normalise backslashes explicitly rather than via filepath.ToSlash, which
	// only converts on Windows — a Windows path can be classified on any OS.
	p := strings.ReplaceAll(execPath, "\\", "/")
	switch {
	case strings.Contains(p, "/Cellar/") || strings.Contains(p, "/homebrew/"):
		return InstallBrew
	case strings.Contains(p, "/scoop/apps/"):
		return InstallScoop
	case goBinDir != "" && underDir(p, goBinDir):
		return InstallGoInstall
	case homeDir != "" && underDir(p, filepath.Join(homeDir, ".local", "bin")):
		return InstallScript
	default:
		return InstallUnknown
	}
}

// underDir reports whether slash-path p sits directly under dir.
func underDir(p, dir string) bool {
	d := strings.ReplaceAll(dir, "\\", "/")
	return p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/")
}

// upgradeInstructions returns the command that updates gortex for the detected
// install method, honouring a version pin where the method supports it, and
// whether the upgrade is a manual step (no scripted command). cosign + SHA256
// verification is preserved: brew/scoop/the installer script all verify, and
// `go install` builds from the verified module proxy.
func upgradeInstructions(m InstallMethod, pinVersion string) (command string, manual bool) {
	switch m {
	case InstallBrew:
		return "brew upgrade gortex", false
	case InstallScoop:
		return "scoop update gortex", false
	case InstallGoInstall:
		v := pinVersion
		if v == "" {
			v = "latest"
		}
		return "go install " + upgradeModulePath + "@" + v, false
	case InstallScript:
		return "curl -fsSL https://get.gortex.dev | sh", false
	default:
		return "", true
	}
}

const upgradeModulePath = "github.com/zzet/gortex/cmd/gortex"

// tagFromReleaseLocation extracts the version tag from a GitHub
// /releases/latest redirect Location (…/releases/tag/v0.49.0 → v0.49.0).
// Returns "" when the location is not a tag URL.
func tagFromReleaseLocation(location string) string {
	const marker = "/releases/tag/"
	i := strings.Index(location, marker)
	if i < 0 {
		return ""
	}
	tag := location[i+len(marker):]
	if j := strings.IndexAny(tag, "?#"); j >= 0 {
		tag = tag[:j]
	}
	return strings.TrimSpace(tag)
}

// latestReleaseVersion resolves the newest release tag via the
// /releases/latest redirect — the redirect target carries the tag, so we read
// it without the 60-request/hour unauthenticated API limit.
func latestReleaseVersion() (string, error) {
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // capture the redirect, don't follow it
		},
	}
	resp, err := client.Get(upgradeRepoURL + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	tag := tagFromReleaseLocation(resp.Header.Get("Location"))
	if tag == "" {
		return "", fmt.Errorf("could not read latest release tag from %s", upgradeRepoURL)
	}
	return tag, nil
}

// normalizeSemver ensures a leading "v" so golang.org/x/mod/semver accepts it.
func normalizeSemver(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

var upgradeRun bool

var upgradeCmd = &cobra.Command{
	Use:   "upgrade [version]",
	Short: "Update gortex to the latest release using the method it was installed with",
	Long: "Detects how this gortex binary was installed (Homebrew, Scoop, go install, or the " +
		"installer script) and runs the matching update command. Pass a version (or set " +
		"GORTEX_VERSION) to pin a specific release. By default the command is printed; pass --run to " +
		"execute it. cosign + SHA256 verification is preserved by every supported method.",
	Args: cobra.MaximumNArgs(1),
	RunE: runUpgrade,
}

func init() {
	upgradeCmd.Flags().BoolVar(&upgradeRun, "run", false, "execute the detected upgrade command instead of printing it")
	upgradeCmd.Flags().BoolVar(&upgradeNoMigrate, "no-migrate", false, "skip refreshing agent config after the upgrade")
	rootCmd.AddCommand(upgradeCmd)
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()

	pin := os.Getenv("GORTEX_VERSION")
	if len(args) == 1 {
		pin = args[0]
	}

	execPath, err := os.Executable()
	if err != nil {
		execPath = ""
	}
	method := detectInstallMethod(execPath, goBinDir(), homeDirOrEmpty())

	current := normalizeSemver(version)
	target := normalizeSemver(pin)
	if target == "" {
		if latest, lerr := latestReleaseVersion(); lerr == nil {
			target = normalizeSemver(latest)
		} else {
			fmt.Fprintf(out, "could not check the latest version (%v); proceeding with the upgrade command\n", lerr)
		}
	}

	// Already current? Only short-circuit when we actually resolved a target
	// and the user did not pin a (possibly older/specific) version.
	if pin == "" && semver.IsValid(current) && semver.IsValid(target) && semver.Compare(current, target) >= 0 {
		fmt.Fprintf(out, "gortex %s is already the latest release.\n", version)
		return nil
	}

	command, manual := upgradeInstructions(method, pin)
	if manual {
		fmt.Fprintf(out, "Installed via an unrecognised method — download the latest release from:\n  %s/releases/latest\n", upgradeRepoURL)
		return nil
	}

	if target != "" {
		fmt.Fprintf(out, "Upgrading gortex %s → %s (install method: %s)\n", version, target, method)
	} else {
		fmt.Fprintf(out, "Upgrading gortex (install method: %s)\n", method)
	}

	migrated := false
	if !upgradeRun {
		fmt.Fprintf(out, "Run:\n  %s\n", command)
	} else {
		fmt.Fprintf(out, "$ %s\n", command)
		run := func() error {
			ex := upgradeExecCommand(cmd.Context(), command)
			ex.Stdout, ex.Stderr, ex.Stdin = out, cmd.ErrOrStderr(), os.Stdin
			return ex.Run()
		}
		if rerr := runUpgradeCommand(out, cmd.ErrOrStderr(), defaultUpgradeDaemonOps(cmd), run); rerr != nil {
			return fmt.Errorf("upgrade command failed: %w", rerr)
		}
		// Config shapes drift between releases, and the moment the new binary
		// lands is when they should be brought current — telling the user to
		// go run `gortex install` afterwards hands back a chore the upgrade
		// can do itself. Runs from the new binary; see upgrade_migrate.go.
		if upgradeNoMigrate {
			upgradeMigrationSkipped(out)
		} else {
			migrateAgentConfigs(cmd.Context(), out, cmd.ErrOrStderr())
			migrated = true
		}
	}

	// A new binary may carry newer per-language extractors, so the indexed
	// graph for an affected language is stale until reindexed.
	upgradeFollowUps(out, upgradeRun, migrated)
	return nil
}

// upgradeExecCommand builds the command that --run executes. The installer
// script is a shell pipeline (`curl … | sh`), so it must run through `sh -c`;
// splitting it on whitespace would hand `|` and `sh` to curl as extra
// hostnames (issue #281). Plain-argv methods (go install / brew / scoop) keep
// direct execution so no shell is spawned when one isn't needed.
func upgradeExecCommand(ctx context.Context, command string) *exec.Cmd {
	if upgradeCommandNeedsShell(command) {
		return exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // fixed installer template, needs shell for pipe
	}
	parts := strings.Fields(command)
	return exec.CommandContext(ctx, parts[0], parts[1:]...) //nolint:gosec // command is one of the fixed install-method templates
}

// upgradeCommandNeedsShell reports whether the command contains shell
// metacharacters that only a shell interprets (a pipe, redirection, expansion,
// quoting) — the signal that it must not be split into a raw argv vector.
func upgradeCommandNeedsShell(command string) bool {
	return strings.ContainsAny(command, "|&;<>()$`\\\"'")
}

// upgradeDaemonOps captures the daemon-lifecycle hooks runUpgradeCommand uses
// to bounce the daemon around the binary swap. Defaults wire to the real
// daemon; tests substitute fakes to assert the stop→swap→restart ordering
// without a live daemon or an OS supervisor.
type upgradeDaemonOps struct {
	isRunning    func() bool
	supervised   func() bool
	stop         func() error
	start        func() error
	superRestart func(io.Writer) error
}

// defaultUpgradeDaemonOps wires upgradeDaemonOps to the real daemon lifecycle.
func defaultUpgradeDaemonOps(cmd *cobra.Command) upgradeDaemonOps {
	return upgradeDaemonOps{
		isRunning:  daemon.IsRunning,
		supervised: serviceActive,
		stop: func() error {
			// Suppress the stop-intent marker (as `daemon restart` does) so a
			// failed upgrade can't leave autostart permanently disabled.
			daemonRestartActive = true
			defer func() { daemonRestartActive = false }()
			return runDaemonStop(cmd, nil)
		},
		start: func() error {
			daemonDetach = true
			return runDaemonStart(cmd, nil)
		},
		superRestart: serviceRestart,
	}
}

// runUpgradeCommand executes the install command (run) with the daemon bounced
// around it, so the upgraded binary is what serves afterwards and the old
// process isn't holding the store lock during an in-place replace. A daemon
// owned by an OS supervisor is bounced THROUGH the supervisor (keeping its
// ownership): the binary is swapped in place first — the running daemon keeps
// the old inode — then the supervisor re-execs the new binary. A manually
// started daemon is stopped before the swap and restarted after. The daemon is
// always brought back, even when the install command fails, so a failed
// upgrade never leaves the user with the daemon down.
func runUpgradeCommand(out, errw io.Writer, ops upgradeDaemonOps, run func() error) error {
	running := ops.isRunning()
	supervised := running && ops.supervised()

	switch {
	case supervised:
		if err := run(); err != nil {
			return err
		}
		fmt.Fprintln(out, "Restarting daemon via service supervisor…")
		if err := ops.superRestart(errw); err != nil {
			fmt.Fprintf(errw, "warning: upgrade succeeded but service restart failed: %v\n", err)
		}
		return nil
	case running:
		fmt.Fprintln(out, "Stopping daemon before upgrade…")
		if err := ops.stop(); err != nil {
			return fmt.Errorf("stop daemon before upgrade: %w", err)
		}
		runErr := run()
		fmt.Fprintln(out, "Restarting daemon…")
		if err := ops.start(); err != nil {
			fmt.Fprintf(errw, "warning: daemon restart after upgrade failed: %v\n", err)
		}
		return runErr
	default:
		return run()
	}
}

// goBinDir returns the directory `go install` drops binaries into — $GOBIN, or
// $GOPATH/bin, or ~/go/bin — for install-method detection.
func goBinDir() string {
	if b := strings.TrimSpace(os.Getenv("GOBIN")); b != "" {
		return b
	}
	if gp := strings.TrimSpace(os.Getenv("GOPATH")); gp != "" {
		// GOPATH may be a list; the first entry owns `go install` output.
		if i := strings.IndexByte(gp, os.PathListSeparator); i >= 0 {
			gp = gp[:i]
		}
		return filepath.Join(gp, "bin")
	}
	if home := homeDirOrEmpty(); home != "" {
		return filepath.Join(home, "go", "bin")
	}
	return ""
}

func homeDirOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
