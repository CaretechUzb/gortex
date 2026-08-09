package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func firstLocalizationPrimaryLine(t *testing.T, page string) string {
	t.Helper()
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "- PRIMARY — ") {
			return line
		}
	}
	t.Fatalf("page carries no primary row:\n%s", page)
	return ""
}

// A name and a file locate a declaration; they do not show it. The excerpt is
// the cheapest proof that the row names the declaration the caller wants.
func TestPrimaryRowsCarryTheirDeclarationExcerpt(t *testing.T) {
	rows := []localizationDigestRow{
		{
			ID: "repo/storage/disk.go::DiskStorage.Load", Name: "Load", Kind: "method",
			File: "repo/storage/disk.go", Line: 42,
			Signature: "func (s *DiskStorage) Load(key string) ([]byte, error)",
		},
		{
			ID: "repo/storage/cache.go::Cache.Warm", Name: "Warm", Kind: "method",
			File: "repo/storage/cache.go", Line: 8,
			Signature: "func (c *Cache) Warm(ctx context.Context) error",
		},
		{
			ID: "repo/storage/index.go::Index.Rebuild", Name: "Rebuild", Kind: "method",
			File: "repo/storage/index.go", Line: 3,
			Signature: "func (i *Index) Rebuild() error",
		},
		{
			ID: "repo/storage/meta.go::Meta.Stamp", Name: "Stamp", Kind: "method",
			File: "repo/storage/meta.go", Line: 90,
			Signature: "func (m *Meta) Stamp(at time.Time)",
		},
	}
	page := renderLocalizationFinalResponse(rows)

	require.Contains(t, page,
		"- PRIMARY — repo/storage/disk.go:42 — repo/storage/disk.go::DiskStorage.Load\n"+
			"  func (s *DiskStorage) Load(key string) ([]byte, error)\n")
	require.NotContains(t, page, "func (m *Meta) Stamp(at time.Time)",
		"a supporting row is a pointer, not a declaration the answer rests on")
	require.LessOrEqual(t, len(page), localizationFinalResponseMaxBytes)
}

// An excerpt proves which declaration a row names; a row is the answer itself.
// Under the byte cap the proof goes before the identity it proves.
func TestDeclarationExcerptsShedBeforeLocatedRows(t *testing.T) {
	var envelope localizationExploreEnvelope
	for index := 0; index < 4; index++ {
		envelope.Evidence = append(envelope.Evidence, localizationEvidence{
			Rank: index + 1,
			ID:   fmt.Sprintf("repo/pkg/file%d.go::Handler%d.Run", index, index),
			Name: fmt.Sprintf("Handler%d.Run", index), Kind: "method",
			File: fmt.Sprintf("pkg/file%d.go", index), Line: index + 1,
			Signature: "func (h *Handler) Run(" + strings.Repeat("option string, ", 260) + ") error",
		})
	}
	digest := newLocalizationEvidenceDigestForTask("run the handler", envelope)

	require.Len(t, digest.Evidence, len(envelope.Evidence))
	for _, row := range envelope.Evidence {
		require.Contains(t, digest.finalResponse, row.ID)
	}
	require.NotContains(t, digest.finalResponse, "option string")
	require.LessOrEqual(t, len(digest.finalResponse), localizationFinalResponseMaxBytes)
}

// Two declarations sharing a bare name are the one case where naming the right
// symbol is not enough. The provenance chain already knows which file the
// answer is about; the leading row has to agree with it.
func TestHomonymAnswerLeadsWithTheProvenanceLinkedFile(t *testing.T) {
	rows := []localizationDigestRow{
		{ID: "repo/storage/disk.go::DiskStorage.Load", Name: "Load", Kind: "method", File: "repo/storage/disk.go", Line: 42},
		{ID: "repo/storage/cloud.go::CloudStorage.Load", Name: "Load", Kind: "method", File: "repo/storage/cloud.go", Line: 17},
		{
			ID: "repo/storage/cloud.go::CloudStorage", Name: "CloudStorage", Kind: "type",
			File: "repo/storage/cloud.go", Line: 5, Provenance: localizationProvenanceDivergentDefaultType,
		},
	}
	page := renderLocalizationFinalResponse(rows)
	require.Contains(t, firstLocalizationPrimaryLine(t, page), "repo/storage/cloud.go::CloudStorage.Load")

	// With no provenance flag the top-ranked row's file is the only thing the
	// page knows, so the homonym in that file leads instead.
	unanchored := renderLocalizationFinalResponse(rows[:2])
	require.Contains(t, firstLocalizationPrimaryLine(t, unanchored), "repo/storage/disk.go::DiskStorage.Load")
}

func TestSameNamedRowsRenderFileQualified(t *testing.T) {
	page := renderLocalizationFinalResponse([]localizationDigestRow{
		{ID: "DiskStorage.Load", Name: "Load", Kind: "method", File: "repo/storage/disk.go", Line: 42},
		{ID: "CloudStorage.Load", Name: "Load", Kind: "method", File: "repo/storage/cloud.go", Line: 17},
	})
	require.Contains(t, page, "- PRIMARY — repo/storage/disk.go:42 — repo/storage/disk.go::DiskStorage.Load")
	require.Contains(t, page, "- PRIMARY — repo/storage/cloud.go:17 — repo/storage/cloud.go::CloudStorage.Load")

	distinct := renderLocalizationFinalResponse([]localizationDigestRow{
		{ID: "DiskStorage.Load", Name: "Load", Kind: "method", File: "repo/storage/disk.go", Line: 42},
		{ID: "Cache.Warm", Name: "Warm", Kind: "method", File: "repo/storage/cache.go", Line: 8},
	})
	require.Contains(t, distinct, "- PRIMARY — repo/storage/disk.go:42 — DiskStorage.Load")
}

// The claim rule belongs on the page that ends the session. An unconfirmed
// page prescribes a next step instead, and must not carry it.
func TestTerminalDirectiveBindsClaimsToTheEvidence(t *testing.T) {
	digest := provisionalTestDigest(t, provisionalTestRow())
	require.Contains(t, localizationAnswerReadyDirective, localizationAnswerClaimDiscipline)
	require.Contains(t, digest.finalResponse, localizationAnswerClaimDiscipline)
	require.NotContains(t, digest.provisionalResponse, localizationAnswerClaimDiscipline)
	require.LessOrEqual(t, len(digest.finalResponse), localizationFinalResponseMaxBytes)
}
