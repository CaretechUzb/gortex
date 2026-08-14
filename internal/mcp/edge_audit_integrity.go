package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

type edgeAuditIntegritySinceOpen struct {
	Status              string                                 `json:"status"`
	Totals              graph.StructuralIntegrityTotals        `json:"totals"`
	Attribution         []graph.StructuralIntegrityAttribution `json:"attribution,omitempty"`
	Samples             []graph.StructuralIntegritySample      `json:"samples,omitempty"`
	AttributionOverflow uint64                                 `json:"attribution_overflow,omitempty"`
	SampleOverflow      uint64                                 `json:"sample_overflow,omitempty"`
	RepoTotalsOverflow  uint64                                 `json:"repo_totals_overflow,omitempty"`
	TotalsLowerBound    bool                                   `json:"totals_lower_bound,omitempty"`
	SamplesTruncated    bool                                   `json:"samples_truncated,omitempty"`
}

type edgeAuditGraphIntegrity struct {
	SinceOpen edgeAuditIntegritySinceOpen         `json:"since_open"`
	Persisted graph.StructuralIntegrityExactAudit `json:"persisted"`
}

func (s *Server) edgeAuditGraphIntegrity(ctx context.Context, sampleLimit int, scopeActive bool, repoPrefixes []string) (edgeAuditGraphIntegrity, error) {
	out := edgeAuditGraphIntegrity{
		SinceOpen: edgeAuditIntegritySinceOpen{Status: graph.StructuralAuditUnsupported},
		Persisted: graph.StructuralIntegrityExactAudit{Status: graph.StructuralAuditUnsupported},
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if sampleLimit <= 0 {
		sampleLimit = 10
	}
	if sampleLimit > 100 {
		sampleLimit = 100
	}
	if snapshotter, ok := s.graph.(graph.StructuralIntegritySnapshotter); ok {
		snapshot := snapshotter.StructuralIntegritySnapshot(graph.StructuralIntegritySnapshotOptions{
			RepoPrefixes:       repoPrefixes,
			RepoScopeActive:    scopeActive,
			IncludeAttribution: true,
			IncludeSamples:     true,
		})
		out.SinceOpen = edgeAuditIntegritySinceOpen{
			Status:              graph.StructuralAuditSupported,
			Totals:              snapshot.Totals,
			Attribution:         snapshot.Attribution,
			Samples:             snapshot.Samples,
			AttributionOverflow: snapshot.AttributionOverflow,
			SampleOverflow:      snapshot.SampleOverflow,
			RepoTotalsOverflow:  snapshot.RepoTotalsOverflow,
			TotalsLowerBound:    snapshot.TotalsLowerBound,
		}
		if len(out.SinceOpen.Samples) > sampleLimit {
			out.SinceOpen.Samples = out.SinceOpen.Samples[:sampleLimit]
			out.SinceOpen.SamplesTruncated = true
		}
	}
	if auditor, ok := s.graph.(graph.StructuralIntegrityAuditor); ok {
		audit, err := auditor.AuditStructuralIntegrity(ctx, graph.StructuralIntegrityAuditOptions{
			RepoPrefixes:    repoPrefixes,
			RepoScopeActive: scopeActive,
			SampleLimit:     sampleLimit,
		})
		if err != nil {
			return out, err
		}
		out.Persisted = audit
	}
	return out, nil
}
