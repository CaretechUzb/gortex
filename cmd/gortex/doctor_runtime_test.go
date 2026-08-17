package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/copilotcli"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/hooks"
	"github.com/zzet/gortex/internal/testenv"
)

// doctorAgentProbes is hand-maintained, and the failure mode of a
// hand-maintained list is silence: an adapter that grows an Inspect and is
// never added here reports nothing at all, which reads to the user as
// "Gortex has nothing to say about this agent" rather than "doctor forgot
// about it". Deriving the expectation from the source tree is what turns
// the omission into a red build instead.
var (
	inspectDecl   = regexp.MustCompile(`(?m)^func Inspect\(`)
	adapterNameRe = regexp.MustCompile(`(?m)^(?:const\s+)?[ \t]*Name\s*=\s*"([^"]+)"`)
)

func TestDoctorProbesEveryAdapterThatExposesInspect(t *testing.T) {
	adaptersDir := filepath.Join(repoRootForDocs(t), "internal", "agents")
	entries, err := os.ReadDir(adaptersDir)
	if err != nil {
		t.Fatalf("read %s: %v", adaptersDir, err)
	}

	// package directory → adapter Name, for every package with an Inspect.
	want := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := filepath.Glob(filepath.Join(adaptersDir, e.Name(), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		hasInspect, name := false, ""
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if inspectDecl.Match(body) {
				hasInspect = true
			}
			if name == "" {
				if m := adapterNameRe.FindSubmatch(body); m != nil {
					name = string(m[1])
				}
			}
		}
		if !hasInspect {
			continue
		}
		if name == "" {
			t.Fatalf("internal/agents/%s exposes Inspect but declares no Name constant", e.Name())
		}
		want[e.Name()] = name
	}

	// A scan that matched nothing would make the guard pass vacuously.
	for _, required := range []string{"claudecode", "codex", "copilotcli", "opencode"} {
		if _, ok := want[required]; !ok {
			t.Fatalf("the source scan missed internal/agents/%s — the guard would pass vacuously", required)
		}
	}

	probed := map[string]bool{}
	for _, probe := range doctorAgentProbes("") {
		probed[probe.agent] = true
	}
	for pkg, name := range want {
		if !probed[name] {
			t.Errorf("internal/agents/%s exposes Inspect but doctorAgentProbes has no probe for %q; "+
				"add one so `gortex doctor` can speak about it", pkg, name)
		}
	}
}

// TestDoctorProbeAgentNamesMatchTheHookWriter closes the loop the probe
// list cannot close on its own.
//
// The invocation log is partitioned per agent, and the writer stamps
// whatever `gortex hook --agent=<x>` resolves to through hooks.SetAgent.
// A probe that reads a key the writer never stamps sees zero runs forever
// — and doctor turns zero runs into a BLOCKER, so the failure presents as
// "your hooks never ran" on a machine where they run every turn.
func TestDoctorProbeAgentNamesMatchTheHookWriter(t *testing.T) {
	t.Cleanup(func() { hooks.SetAgent("") })
	for _, probe := range doctorAgentProbes("") {
		// SetAgent normalises the flag value; the probe's key must be a
		// fixed point of that normalisation.
		hooks.SetAgent(probe.agent)
		if got := hooks.ActiveAgent(); got != probe.agent {
			t.Errorf("probe reads %q but `gortex hook --agent=%s` records %q", probe.agent, probe.agent, got)
		}
	}
	// Claude Code is the one agent whose hook command carries no --agent
	// flag at all (telemetry.go: rewriting it would invalidate Codex-style
	// hook trust for no gain), so the empty default has to resolve to the
	// same key its probe reads.
	hooks.SetAgent("")
	if got := hooks.ActiveAgent(); got != hooks.AgentClaudeCode {
		t.Errorf("the empty --agent default records %q, want %q", got, hooks.AgentClaudeCode)
	}
}

// TestDoctorRuntimeReportsInstallStateForEveryHost runs the real
// ModeGlobal installs into a sandbox home and then asks doctor what it
// sees, so the probes are exercised against what the writers actually
// wrote rather than a hand-built fixture.
func TestDoctorRuntimeReportsInstallStateForEveryHost(t *testing.T) {
	home := t.TempDir()
	testenv.SandboxAt(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	claudecode.SetConfigDirOverride("")

	env := agents.Env{
		Root:                      t.TempDir(),
		Home:                      home,
		HookCommand:               "/tmp/test-gortex hook",
		Mode:                      agents.ModeGlobal,
		InstallHooks:              true,
		InstallGlobalInstructions: true,
		InstructionsDir:           filepath.Join(home, ".gortex", "instructions"),
	}
	for _, a := range []agents.Adapter{claudecode.New(), codex.New(), copilotcli.New(), opencode.New()} {
		if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
			t.Fatalf("%s apply: %v", a.Name(), err)
		}
	}

	runtime := collectRuntime(home, 7)
	byAgent := map[string]doctorAgentRuntime{}
	for _, a := range runtime.Agents {
		byAgent[a.Agent] = a
	}

	for _, tc := range []struct {
		agent     string
		wantMCP   bool
		hookProof string // the file that must name this agent's --agent value
	}{
		{codex.Name, true, filepath.Join(home, ".codex", "config.toml")},
		{hooks.AgentClaudeCode, true, ""},
		{copilotcli.Name, true, filepath.Join(home, ".copilot", "hooks", "gortex.json")},
		{opencode.Name, true, opencode.PluginPath(home)},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			got, ok := byAgent[tc.agent]
			if !ok {
				t.Fatalf("doctor reported nothing for %s", tc.agent)
			}
			if got.Install.MCPServer != tc.wantMCP {
				t.Errorf("MCPServer = %v, want %v", got.Install.MCPServer, tc.wantMCP)
			}
			if len(got.HookEvents) == 0 {
				t.Fatal("no hook events declared")
			}
			for _, event := range got.HookEvents {
				if got.Install.Hooks[event] == 0 {
					t.Errorf("hook %s reported 0 configured after a real install", event)
				}
			}
			if tc.hookProof == "" {
				return
			}
			// The other half of the agent-name contract: the adapter must
			// bake the very string the probe reads into its hook command,
			// or the probe reads an empty partition of the log forever.
			body, err := os.ReadFile(tc.hookProof)
			if err != nil {
				t.Fatalf("read %s: %v", tc.hookProof, err)
			}
			if !strings.Contains(string(body), "--agent="+tc.agent) {
				t.Errorf("%s does not invoke `gortex hook --agent=%s`", tc.hookProof, tc.agent)
			}
		})
	}

	// The stop-line: neither new host has a session transcript Gortex can
	// read, so their adoption slice stays at its zero value rather than
	// carrying an invented number.
	for _, agent := range []string{copilotcli.Name, opencode.Name} {
		if got := byAgent[agent].Adoption; got.FilesFound != 0 || got.GortexCalls != 0 {
			t.Errorf("%s reports adoption data it cannot have: %+v", agent, got)
		}
	}

	// --json has to carry all four; it is what CI consumers read.
	blob, err := json.Marshal(map[string]any{"runtime": runtime})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	for _, agent := range []string{codex.Name, hooks.AgentClaudeCode, copilotcli.Name, opencode.Name} {
		if !strings.Contains(string(blob), `"agent":"`+agent+`"`) {
			t.Errorf("the --json report never names %s", agent)
		}
	}
}
