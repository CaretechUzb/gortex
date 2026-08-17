package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/agents"
)

// fakeAdapter is a minimal agents.Adapter used to seed the wizard's checklist
// without depending on every real adapter's Detect side-effects. detected
// controls what Detect returns.
type fakeAdapter struct {
	name     string
	detected bool
}

func (a *fakeAdapter) Name() string                            { return a.name }
func (a *fakeAdapter) DocsURL() string                         { return "" }
func (a *fakeAdapter) Detect(_ agents.Env) (bool, error)       { return a.detected, nil }
func (a *fakeAdapter) Plan(_ agents.Env) (*agents.Plan, error) { return &agents.Plan{}, nil }
func (a *fakeAdapter) Apply(_ agents.Env, _ agents.ApplyOpts) (*agents.Result, error) {
	return &agents.Result{Name: a.name, Detected: a.detected, Configured: a.detected}, nil
}

func sampleRegistered() []agents.Adapter {
	return []agents.Adapter{
		&fakeAdapter{name: "claude-code", detected: true},
		&fakeAdapter{name: "cursor", detected: true},
		&fakeAdapter{name: "aider", detected: false},
		&fakeAdapter{name: "codex", detected: false},
	}
}

func sampleDetected() map[string]bool {
	return map[string]bool{
		"claude-code": true,
		"cursor":      true,
		"aider":       false,
		"codex":       false,
	}
}

func defaultDefaults() initDefaults {
	return initDefaults{hooks: true, hookMode: "deny", analyze: false, skills: true}
}

// pressKey simulates one key press and returns the model after Update.
func pressKey(m *initWizardModel, key string) *initWizardModel {
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	out, _ := updated.(*initWizardModel)
	return out
}

func pressSpecial(m *initWizardModel, key tea.KeyType) *initWizardModel {
	updated, _ := m.Update(tea.KeyMsg{Type: key})
	out, _ := updated.(*initWizardModel)
	return out
}

func TestInitWizard_DetectedAdaptersStartChecked(t *testing.T) {
	m := newInitWizardModel(".", sampleRegistered(), sampleDetected(), defaultDefaults())

	picked := m.collectPickedLabels()
	assert.Contains(t, picked, "Claude Code", "Claude Code should always be pre-checked")
	assert.Contains(t, picked, "Cursor", "detected adapters should be pre-checked")
	assert.NotContains(t, picked, "Aider", "undetected adapters should be unchecked")
	assert.NotContains(t, picked, "Codex CLI", "undetected adapters should be unchecked")
}

func TestInitWizard_ClaudeCodeAlwaysCheckedEvenIfUndetected(t *testing.T) {
	registered := []agents.Adapter{
		&fakeAdapter{name: "claude-code", detected: false},
		&fakeAdapter{name: "cursor", detected: false},
	}
	detected := map[string]bool{"claude-code": false, "cursor": false}

	m := newInitWizardModel(".", registered, detected, defaultDefaults())

	picked := m.collectPickedLabels()
	assert.Contains(t, picked, "Claude Code", "Claude Code must default-on even when Detect returns false — it owns the load-bearing hook surface")
	assert.NotContains(t, picked, "Cursor")
}

func TestInitWizard_QuitOnEsc(t *testing.T) {
	m := newInitWizardModel(".", sampleRegistered(), sampleDetected(), defaultDefaults())
	m = pressSpecial(m, tea.KeyEsc)
	assert.True(t, m.cancelled, "Esc must cancel")
	assert.Equal(t, stepCancelled, m.step)
}

func TestInitWizard_AdvancesThroughStepsAndCollectsChoices(t *testing.T) {
	m := newInitWizardModel(".", sampleRegistered(), sampleDetected(), defaultDefaults())

	// Step 1: agents — toggle codex on, then advance.
	m = pressKey(m, "j") // cursor → cursor
	m = pressKey(m, "j") // → aider
	m = pressKey(m, "j") // → codex
	m = pressKey(m, " ") // pick codex
	m = pressSpecial(m, tea.KeyEnter)
	assert.Equal(t, stepOptions, m.step)

	// Step 2: options — flip "Codebase analysis" on.
	m = pressKey(m, "j") // cursor on row 1 → row 2 (analyze)
	m = pressKey(m, " ") // toggle analyze
	m = pressKey(m, "N") // shift-N: advance
	assert.Equal(t, stepConfirm, m.step)

	// Step 3: confirm.
	m = pressSpecial(m, tea.KeyEnter)
	assert.True(t, m.confirmed)

	assert.Contains(t, m.pickedAgents, "claude-code")
	assert.Contains(t, m.pickedAgents, "cursor")
	assert.Contains(t, m.pickedAgents, "codex")
	assert.True(t, m.analyze, "analyze must be set by the toggle")
	assert.True(t, m.hooks, "hooks default carried through")
	assert.Equal(t, "deny", m.hookMode)
}

func TestInitWizard_PickNoneFallsBackToClaudeCode(t *testing.T) {
	m := newInitWizardModel(".", sampleRegistered(), sampleDetected(), defaultDefaults())

	m = pressKey(m, "n")              // pick none
	m = pressSpecial(m, tea.KeyEnter) // advance — should auto-pick claude-code

	pickedLabels := m.collectPickedLabels()
	assert.Equal(t, []string{"Claude Code"}, pickedLabels, "advancing with nothing picked must snap to Claude Code")
	assert.Equal(t, stepOptions, m.step)
}

func TestInitWizard_PickAllAndPickInvert(t *testing.T) {
	m := newInitWizardModel(".", sampleRegistered(), sampleDetected(), defaultDefaults())

	m = pressKey(m, "a")
	assert.Equal(t, 4, m.checklist.PickedCount(), "a picks all")

	m = pressKey(m, "i")
	assert.Equal(t, 0, m.checklist.PickedCount(), "i inverts all-picked → none")

	m = pressKey(m, "i")
	assert.Equal(t, 4, m.checklist.PickedCount(), "i again inverts none → all")
}

func TestInitWizard_HookModeSelectCycles(t *testing.T) {
	m := newInitWizardModel(".", sampleRegistered(), sampleDetected(), defaultDefaults())

	// jump to confirm-ready: agents → options.
	m = pressSpecial(m, tea.KeyEnter)
	// step 2 has 3 toggles + 1 select; cursor must reach select row (index 3).
	m = pressKey(m, "j") // → toggle 1
	m = pressKey(m, "j") // → toggle 2
	m = pressKey(m, "j") // → select (hook posture)

	// Right arrow cycles forward.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m, _ = updated.(*initWizardModel)
	assert.Equal(t, "enrich", m.options.Selects[0].Value(), "right cycles deny → enrich")

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = updated.(*initWizardModel)
	assert.Equal(t, "deny", m.options.Selects[0].Value(), "left cycles back")
}

// TestInitWizard_CopilotCLISurvivesLabelRoundTrip pins the silent-drop
// trap in collectChoices: the wizard renders a checklist of *labels* and
// maps them back to slugs through agentLabels, so a slug that is
// registered but missing from that table still renders (title-cased) and
// then vanishes on confirmation — the user's pick never runs. It also
// pins that the CLI adapter's label is distinguishable from the VS Code
// one, since two identical labels would collide in the same reverse map.
func TestInitWizard_CopilotCLISurvivesLabelRoundTrip(t *testing.T) {
	registered := []agents.Adapter{
		&fakeAdapter{name: "claude-code", detected: true},
		&fakeAdapter{name: "copilot-cli", detected: true},
		&fakeAdapter{name: "vscode", detected: true},
	}
	detected := map[string]bool{"claude-code": true, "copilot-cli": true, "vscode": true}

	m := newInitWizardModel(".", registered, detected, defaultDefaults())
	assert.Contains(t, m.collectPickedLabels(), "GitHub Copilot CLI")
	assert.NotEqual(t, agentLabel("vscode"), agentLabel("copilot-cli"),
		"the CLI and the VS Code host must not share a label — the label→slug map is keyed by label")

	m.collectChoices()
	assert.Contains(t, m.pickedAgents, "copilot-cli",
		"copilot-cli must survive the label→slug round-trip; a missing agentLabels entry drops it silently")
	assert.Contains(t, m.pickedAgents, "vscode")
}

// TestInitWizard_EveryRegisteredAdapterHasALabel generalises the trap
// above: any adapter reachable from buildRegistry must be in agentLabels,
// or the wizard will offer it and then discard the pick.
func TestInitWizard_EveryRegisteredAdapterHasALabel(t *testing.T) {
	for _, a := range buildRegistry().All() {
		if _, ok := agentLabels[a.Name()]; !ok {
			t.Errorf("adapter %q is registered but missing from agentLabels — collectChoices would drop the user's pick", a.Name())
		}
	}
}

func TestInitWizard_ViewIncludesBannerAndStepLine(t *testing.T) {
	m := newInitWizardModel("/tmp/demo", sampleRegistered(), sampleDetected(), defaultDefaults())

	view := m.View()
	assert.Contains(t, view, "gortex init", "banner title must appear")
	assert.Contains(t, view, "/tmp/demo", "subtitle must echo the repo path")
	assert.Contains(t, view, "Step 1", "step line must reflect current step")
	assert.Contains(t, view, "Claude Code", "checklist must render adapter labels")
}
