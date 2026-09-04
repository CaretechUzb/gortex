package forge

import (
	"testing"
	"time"
)

func TestClassifyStatus(t *testing.T) {
	now := time.Now()
	old := now.AddDate(0, 0, -45)
	recent := now.AddDate(0, 0, -2)

	tests := []struct {
		name         string
		pr           PR
		defaultBase  string
		wantState    string
		wantMismatch bool
		wantBlockers []string
	}{
		{
			name:      "draft wins over everything",
			pr:        PR{IsDraft: true, BaseRef: "feature", ReviewDecision: "CHANGES_REQUESTED", UpdatedAt: recent},
			wantState: "DRAFT",
			// base unset so no mismatch; blockers = draft + changes-requested
			wantBlockers: []string{"draft", "changes-requested"},
		},
		{
			name:         "base mismatch",
			pr:           PR{BaseRef: "develop", UpdatedAt: recent},
			defaultBase:  "main",
			wantState:    "BASE_MISMATCH",
			wantMismatch: true,
			wantBlockers: []string{"base-mismatch"},
		},
		{
			name:         "changes requested",
			pr:           PR{BaseRef: "main", ReviewDecision: "CHANGES_REQUESTED", UpdatedAt: recent},
			defaultBase:  "main",
			wantState:    "CHANGES_REQUESTED",
			wantBlockers: []string{"changes-requested"},
		},
		{
			name:        "approved",
			pr:          PR{BaseRef: "main", ReviewDecision: "APPROVED", UpdatedAt: recent},
			defaultBase: "main",
			wantState:   "APPROVED",
		},
		{
			name:        "stale by age",
			pr:          PR{BaseRef: "main", UpdatedAt: old},
			defaultBase: "main",
			wantState:   "STALE",
		},
		{
			name:        "ready",
			pr:          PR{BaseRef: "main", UpdatedAt: recent},
			defaultBase: "main",
			wantState:   "READY",
		},
		{
			name:         "ci failure adds a blocker",
			pr:           PR{BaseRef: "main", CIRollup: "FAILURE", UpdatedAt: recent},
			defaultBase:  "main",
			wantState:    "READY",
			wantBlockers: []string{"ci-failure"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyStatus(tt.pr, tt.defaultBase)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
			if got.BaseMismatch != tt.wantMismatch {
				t.Errorf("BaseMismatch = %v, want %v", got.BaseMismatch, tt.wantMismatch)
			}
			if tt.wantBlockers != nil {
				if !sameStringSet(got.Blockers, tt.wantBlockers) {
					t.Errorf("Blockers = %v, want %v", got.Blockers, tt.wantBlockers)
				}
			}
		})
	}
}

func TestRollupCI(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "NONE"},
		{"NONE", "NONE"},
		{"FAILURE", "FAILURE"},
		{"PENDING", "PENDING"},
		{"SUCCESS", "SUCCESS"},
		{"garbage", "NONE"},
	}
	for _, tt := range tests {
		if got := RollupCI(PR{CIRollup: tt.in}); got != tt.want {
			t.Errorf("RollupCI(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCollapseStates(t *testing.T) {
	tests := []struct {
		name   string
		states []string
		want   string
	}{
		{"empty", nil, "NONE"},
		{"all success", []string{"success", "success"}, "SUCCESS"},
		{"any failure", []string{"success", "failure"}, "FAILURE"},
		{"error counts as failure", []string{"success", "error"}, "FAILURE"},
		{"pending no failure", []string{"success", "pending"}, "PENDING"},
		{"in_progress is pending", []string{"in_progress"}, "PENDING"},
		{"failure beats pending", []string{"pending", "failure"}, "FAILURE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := collapseStates(tt.states); got != tt.want {
				t.Errorf("collapseStates(%v) = %q, want %q", tt.states, got, tt.want)
			}
		})
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestParseBases(t *testing.T) {
	tests := []struct {
		spec string
		want []string
	}{
		{"", nil},
		{"main", []string{"main"}},
		{"16.0,aurora-redesign", []string{"16.0", "aurora-redesign"}},
		{" 16.0 , aurora-redesign ", []string{"16.0", "aurora-redesign"}},
		{"16.0,,aurora-redesign", []string{"16.0", "aurora-redesign"}},
		{" , ", nil},
	}
	for _, tc := range tests {
		got := ParseBases(tc.spec)
		if len(got) != len(tc.want) {
			t.Errorf("ParseBases(%q) = %v, want %v", tc.spec, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ParseBases(%q)[%d] = %q, want %q", tc.spec, i, got[i], tc.want[i])
			}
		}
	}
}

// TestClassifyStatus_MultipleAcceptedBases is the regression guard for a repo
// with more than one live target branch. With a single accepted base, whichever
// branch is not the default has EVERY one of its PRs mislabelled BASE_MISMATCH
// — measured on a real repo: 31 MRs on 16.0 and 11 on aurora-redesign, so one
// of those groups was always wrong.
func TestClassifyStatus_MultipleAcceptedBases(t *testing.T) {
	release := PR{BaseRef: "16.0", UpdatedAt: time.Now()}
	redesign := PR{BaseRef: "aurora-redesign", UpdatedAt: time.Now()}
	stray := PR{BaseRef: "some/feature", UpdatedAt: time.Now()}

	// Single base: the other live branch is (wrongly, for this repo) flagged.
	if st := ClassifyStatus(redesign, "16.0"); !st.BaseMismatch {
		t.Errorf("single-base control: expected BaseMismatch for aurora-redesign")
	}

	// Both accepted: neither real target is flagged, the stray one still is.
	const bases = "16.0,aurora-redesign"
	if st := ClassifyStatus(release, bases); st.BaseMismatch {
		t.Errorf("16.0 flagged as mismatch against %q", bases)
	}
	if st := ClassifyStatus(redesign, bases); st.BaseMismatch {
		t.Errorf("aurora-redesign flagged as mismatch against %q", bases)
	}
	if st := ClassifyStatus(stray, bases); !st.BaseMismatch {
		t.Errorf("some/feature NOT flagged against %q — the check went inert", bases)
	}
	if st := ClassifyStatus(release, bases); st.State != "READY" {
		t.Errorf("State = %q, want READY", st.State)
	}
}

// TestClassifyStatus_EmptyBasesDisablesCheck pins the "unknown" case: no
// accepted bases must mean "do not check", never "everything is wrong".
func TestClassifyStatus_EmptyBasesDisablesCheck(t *testing.T) {
	pr := PR{BaseRef: "anything", UpdatedAt: time.Now()}
	if st := ClassifyStatus(pr, ""); st.BaseMismatch {
		t.Errorf("empty base spec flagged a mismatch")
	}
	if st := ClassifyStatusAgainst(pr, nil); st.BaseMismatch {
		t.Errorf("nil bases flagged a mismatch")
	}
}

// TestClassifyStatus_BaseMatchIsExact guards against a prefix/substring match
// sneaking in: "16.0" must not accept "16.0-hotfix".
func TestClassifyStatus_BaseMatchIsExact(t *testing.T) {
	pr := PR{BaseRef: "16.0-hotfix", UpdatedAt: time.Now()}
	if st := ClassifyStatus(pr, "16.0"); !st.BaseMismatch {
		t.Errorf("16.0-hotfix accepted against base 16.0 — match is not exact")
	}
}
