package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A request written in prose and the identifier the contract prescribes
// tokenize differently: prose separates what an identifier joins, so a term
// requirement can never be satisfied by the very symbol the engine chose.
// These fixtures pin that acceptance now matches across that join, and that
// closing it does not lower the bar for evidence that is merely wordy.
func TestReservedReadAcceptsAnIdentifierThatJoinsWhatTheRequestSeparates(t *testing.T) {
	// The request writes the subject as prose; the symbol spells it as one
	// word. Tokenization alone made those disjoint, so the contract refused the
	// very symbol it had just prescribed.
	const task = "TraceKit stays enabled in production builds of the storage middleware"
	const lead = "TraceKit stays enabled in production"
	row := localizationDigestRow{
		ID: "repo/src/middleware/tracekit.ts::tracekitImpl", Name: "tracekitImpl",
		QualName: "middleware.tracekitImpl", Kind: "function",
		File: "src/middleware/tracekit.ts", Line: 40,
	}

	// Pin the asymmetry the fix exists for: no term is shared outright.
	leadTerms := exploreTerminalTerms(lead)
	rowTerms := exploreTerminalTerms(row.ID + " " + row.Name + " " + row.QualName + " " + row.File)
	for term := range rowTerms {
		_, exact := leadTerms[term]
		require.False(t, exact && term != "production", "fixture must rely on the join, not a direct hit")
	}
	require.True(t, localizationTermMatchesAcrossJoin("tracekit", leadTerms),
		"a joined identifier matches the separated prose terms")

	require.True(t, localizationReservedReadEvidenceAlignedWithLead(task, lead, "", []localizationDigestRow{row}),
		"the prescribed symbol is accepted once the join is closed")
	require.True(t, localizationRecoveryEvidenceAlignedWithLead(task, lead, "", "read.source", []localizationDigestRow{row}),
		"the recovery path applies the same join")
}

func TestJoinMatchingDoesNotAcceptASymptomEcho(t *testing.T) {
	// A long descriptive name that merely restates the symptom must still be
	// refused — the join closes a tokenization gap, it does not lower the bar.
	const task = "Storage commits stall during flush"
	echo := localizationDigestRow{
		ID: "repo/storage/report.go::Reporter.ReportStorageFailureAsPending",
		Name: "ReportStorageFailureAsPending", QualName: "Reporter.ReportStorageFailureAsPending",
		Kind: "method", File: "repo/storage/report.go",
		Signature: "storage writes stall during commit",
	}
	require.False(t, localizationRecoveryEvidenceAlignedWithLead(
		task, localizationTaskLead(task), "storage", "search.text", []localizationDigestRow{echo}),
		"a symptom-echoing name cannot borrow confidence from the join")
}

func TestReservedReadAcceptanceStillNeedsTwoTaskTerms(t *testing.T) {
	const task = "TraceKit logging stays enabled in production builds of the storage middleware"
	unrelated := localizationDigestRow{
		ID: "repo/build/bundle.config.js::createBundle", Name: "createBundle",
		QualName: "createBundle", Kind: "function", File: "build/bundle.config.js", Line: 7,
	}
	require.False(t, localizationReservedReadEvidenceAlignedWithLead(task, "", "", []localizationDigestRow{unrelated}),
		"a row sharing nothing with the request is still refused")
	require.False(t, localizationRecoveryEvidenceAlignedWithLead(task, "", "", "read.source", []localizationDigestRow{unrelated}))
}

func TestAcceptanceSurvivesARequestWithNoUsableLeadClause(t *testing.T) {
	// A request opening with a short label leaves no lead terms at all; that
	// must not by itself refuse otherwise aligned evidence.
	const task = "Bug: the storage middleware keeps its logging enabled in production"
	row := localizationDigestRow{
		ID: "repo/src/middleware/logging.ts::loggingMiddleware", Name: "loggingMiddleware",
		QualName: "middleware.loggingMiddleware", Kind: "function",
		File: "src/middleware/logging.ts", Line: 12,
	}
	require.True(t, localizationReservedReadEvidenceAlignedWithLead(task, "", "", []localizationDigestRow{row}))
}

func TestCitationStillAcceptsImmediately(t *testing.T) {
	const task = `the failure is raised inside DiskStorage.Load when the path is empty`
	row := localizationDigestRow{
		ID: "repo/storage/disk.go::DiskStorage.Load", Name: "DiskStorage.Load",
		QualName: "storage.DiskStorage.Load", Kind: "method", File: "storage/disk.go", Line: 12,
	}
	require.True(t, localizationReservedReadEvidenceAlignedWithLead(task, "", "DiskStorage.Load", nil) ||
		localizationReservedReadEvidenceAlignedWithLead(task, "", "", []localizationDigestRow{row}),
		"a request that names the symbol is accepted on citation alone")
}
