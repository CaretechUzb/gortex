package graph

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	structuralIntegrityMaxRepos      = 256
	structuralIntegrityMaxDimensions = 256
	structuralIntegrityMaxSamples    = 64
	structuralIntegrityOriginLimit   = 48
	structuralIntegrityDetailLimit   = 512
)

type StructuralDropDirection string

const (
	StructuralDropWrite StructuralDropDirection = "write_rejection"
	StructuralDropRead  StructuralDropDirection = "read_suppression"
)

type StructuralDropPath string

const (
	StructuralPathUnknown         StructuralDropPath = "unknown"
	StructuralPathGraphAddEdge    StructuralDropPath = "graph_add_edge"
	StructuralPathGraphAddBatch   StructuralDropPath = "graph_add_batch"
	StructuralPathShadowCold      StructuralDropPath = "shadow_cold_batch"
	StructuralPathShadowStreaming StructuralDropPath = "shadow_streaming_batch"
	StructuralPathSQLiteAddEdge   StructuralDropPath = "sqlite_add_edge"
	StructuralPathSQLiteAddBatch  StructuralDropPath = "sqlite_add_batch"
	StructuralPathSQLiteFullRead  StructuralDropPath = "sqlite_full"
	StructuralPathSQLiteLightRead StructuralDropPath = "sqlite_light"
)

type StructuralDropReason string

const (
	StructuralReasonParameterTarget StructuralDropReason = "parameter_target"
	StructuralReasonLocalTarget     StructuralDropReason = "local_target"
)

type StructuralIntegrityEvent struct {
	Direction StructuralDropDirection
	Path      StructuralDropPath
	Repo      string
	Kind      EdgeKind
	Reason    StructuralDropReason
	Origin    string
	From      string
	To        string
	FilePath  string
	Line      int
}

type StructuralIntegrityTotals struct {
	WriteRejected  uint64 `json:"write_rejected,omitempty"`
	ReadSuppressed uint64 `json:"read_suppressed,omitempty"`
}

func (t StructuralIntegrityTotals) Empty() bool {
	return t.WriteRejected == 0 && t.ReadSuppressed == 0
}

type StructuralIntegrityAttribution struct {
	Direction StructuralDropDirection `json:"direction"`
	Path      StructuralDropPath      `json:"path"`
	Repo      string                  `json:"repo"`
	Kind      EdgeKind                `json:"kind"`
	Reason    StructuralDropReason    `json:"reason"`
	Origin    string                  `json:"origin"`
	Count     uint64                  `json:"count"`
}

type StructuralIntegritySample struct {
	Direction StructuralDropDirection `json:"direction"`
	Path      StructuralDropPath      `json:"path"`
	Repo      string                  `json:"repo"`
	Kind      EdgeKind                `json:"kind"`
	Reason    StructuralDropReason    `json:"reason"`
	Origin    string                  `json:"origin"`
	From      string                  `json:"from,omitempty"`
	To        string                  `json:"to,omitempty"`
	FilePath  string                  `json:"file_path,omitempty"`
	Line      int                     `json:"line,omitempty"`
}

type StructuralIntegritySnapshotOptions struct {
	RepoPrefixes       []string
	RepoScopeActive    bool
	IncludeAttribution bool
	IncludeSamples     bool
}

type StructuralIntegritySnapshot struct {
	Totals              StructuralIntegrityTotals        `json:"totals"`
	Attribution         []StructuralIntegrityAttribution `json:"attribution,omitempty"`
	Samples             []StructuralIntegritySample      `json:"samples,omitempty"`
	AttributionOverflow uint64                           `json:"attribution_overflow,omitempty"`
	SampleOverflow      uint64                           `json:"sample_overflow,omitempty"`
	RepoTotalsOverflow  uint64                           `json:"repo_totals_overflow,omitempty"`
	TotalsLowerBound    bool                             `json:"totals_lower_bound,omitempty"`
}

type StructuralIntegrityAuditOptions struct {
	RepoPrefixes    []string
	RepoScopeActive bool
	SampleLimit     int
}

type StructuralIntegrityExactGroup struct {
	Repo   string               `json:"repo"`
	Kind   EdgeKind             `json:"kind"`
	Reason StructuralDropReason `json:"reason"`
	Origin string               `json:"origin"`
	Count  uint64               `json:"count"`
}

type StructuralIntegrityExactSample struct {
	Repo     string               `json:"repo"`
	Kind     EdgeKind             `json:"kind"`
	Reason   StructuralDropReason `json:"reason"`
	Origin   string               `json:"origin"`
	From     string               `json:"from"`
	To       string               `json:"to"`
	FilePath string               `json:"file_path,omitempty"`
	Line     int                  `json:"line,omitempty"`
}

type StructuralIntegrityExactAudit struct {
	Status    string                           `json:"status"`
	TotalRows uint64                           `json:"total_rows"`
	Groups    []StructuralIntegrityExactGroup  `json:"groups,omitempty"`
	Samples   []StructuralIntegrityExactSample `json:"samples,omitempty"`
	Truncated bool                             `json:"truncated,omitempty"`
}

const (
	StructuralAuditSupported   = "supported"
	StructuralAuditUnsupported = "unsupported"
)

type StructuralIntegrityEventRecorder interface {
	RecordStructuralIntegrityEvent(StructuralIntegrityEvent)
}

type StructuralIntegritySnapshotter interface {
	StructuralIntegritySnapshot(StructuralIntegritySnapshotOptions) StructuralIntegritySnapshot
}

type StructuralIntegrityAuditor interface {
	AuditStructuralIntegrity(context.Context, StructuralIntegrityAuditOptions) (StructuralIntegrityExactAudit, error)
}

type structuralIntegrityKey struct {
	direction StructuralDropDirection
	path      StructuralDropPath
	repo      string
	kind      EdgeKind
	reason    StructuralDropReason
	origin    string
}

type structuralIntegritySampleKey struct {
	structuralIntegrityKey
	from     string
	to       string
	filePath string
	line     int
}

type StructuralIntegrityMeter struct {
	writeRejected  atomic.Uint64
	readSuppressed atomic.Uint64

	mu                  sync.Mutex
	repoTotals          map[string]StructuralIntegrityTotals
	attribution         map[structuralIntegrityKey]uint64
	samples             map[structuralIntegritySampleKey]StructuralIntegritySample
	repoTotalsOverflow  uint64
	attributionOverflow uint64
	sampleOverflow      uint64
}

func normalizeStructuralRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "unknown"
	}
	return boundedStructuralString(repo, 128)
}

func normalizeStructuralOrigin(origin string) string {
	origin = strings.ToLower(strings.TrimSpace(origin))
	if origin == "" {
		return "unknown"
	}
	return boundedStructuralString(origin, structuralIntegrityOriginLimit)
}

func boundedStructuralString(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func normalizeStructuralPath(path StructuralDropPath) StructuralDropPath {
	switch path {
	case StructuralPathGraphAddEdge, StructuralPathGraphAddBatch,
		StructuralPathShadowCold, StructuralPathShadowStreaming,
		StructuralPathSQLiteAddEdge, StructuralPathSQLiteAddBatch,
		StructuralPathSQLiteFullRead, StructuralPathSQLiteLightRead:
		return path
	default:
		return StructuralPathUnknown
	}
}

func validStructuralKind(kind EdgeKind) bool {
	switch kind {
	case EdgeImplements, EdgeExtends, EdgeOverrides, EdgeInstantiates, EdgeMemberOf:
		return true
	default:
		return false
	}
}

func structuralReasonForTarget(target string) (StructuralDropReason, bool) {
	switch {
	case strings.Contains(target, "#param:"):
		return StructuralReasonParameterTarget, true
	case strings.Contains(target, "#local:"):
		return StructuralReasonLocalTarget, true
	default:
		return "", false
	}
}

func StructuralIntegrityEventForEdge(direction StructuralDropDirection, path StructuralDropPath, repo string, edge *Edge) (StructuralIntegrityEvent, bool) {
	if edge == nil || !validStructuralKind(edge.Kind) {
		return StructuralIntegrityEvent{}, false
	}
	reason, ok := structuralReasonForTarget(edge.To)
	if !ok {
		return StructuralIntegrityEvent{}, false
	}
	return StructuralIntegrityEvent{
		Direction: direction,
		Path:      normalizeStructuralPath(path),
		Repo:      normalizeStructuralRepo(repo),
		Kind:      edge.Kind,
		Reason:    reason,
		Origin:    edge.Origin,
		From:      edge.From,
		To:        edge.To,
		FilePath:  edge.FilePath,
		Line:      edge.Line,
	}, true
}

func (m *StructuralIntegrityMeter) Record(event StructuralIntegrityEvent) {
	if m == nil || !validStructuralKind(event.Kind) {
		return
	}
	if event.Direction != StructuralDropWrite && event.Direction != StructuralDropRead {
		return
	}
	if event.Reason != StructuralReasonParameterTarget && event.Reason != StructuralReasonLocalTarget {
		return
	}
	event.Path = normalizeStructuralPath(event.Path)
	event.Repo = normalizeStructuralRepo(event.Repo)
	event.Origin = normalizeStructuralOrigin(event.Origin)
	event.From = boundedStructuralString(event.From, structuralIntegrityDetailLimit)
	event.To = boundedStructuralString(event.To, structuralIntegrityDetailLimit)
	event.FilePath = boundedStructuralString(event.FilePath, structuralIntegrityDetailLimit)

	key := structuralIntegrityKey{
		direction: event.Direction,
		path:      event.Path,
		repo:      event.Repo,
		kind:      event.Kind,
		reason:    event.Reason,
		origin:    event.Origin,
	}
	sampleKey := structuralIntegritySampleKey{
		structuralIntegrityKey: key,
		from:                   event.From, to: event.To, filePath: event.FilePath, line: event.Line,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if event.Direction == StructuralDropWrite {
		m.writeRejected.Add(1)
	} else {
		m.readSuppressed.Add(1)
	}
	if m.repoTotals == nil {
		m.repoTotals = make(map[string]StructuralIntegrityTotals)
	}
	if totals, exists := m.repoTotals[event.Repo]; exists || len(m.repoTotals) < structuralIntegrityMaxRepos {
		if event.Direction == StructuralDropWrite {
			totals.WriteRejected++
		} else {
			totals.ReadSuppressed++
		}
		m.repoTotals[event.Repo] = totals
	} else {
		m.repoTotalsOverflow++
	}
	if m.attribution == nil {
		m.attribution = make(map[structuralIntegrityKey]uint64)
	}
	if _, exists := m.attribution[key]; exists || len(m.attribution) < structuralIntegrityMaxDimensions {
		m.attribution[key]++
	} else {
		m.attributionOverflow++
	}
	if m.samples == nil {
		m.samples = make(map[structuralIntegritySampleKey]StructuralIntegritySample)
	}
	if _, exists := m.samples[sampleKey]; exists {
		return
	}
	if len(m.samples) >= structuralIntegrityMaxSamples {
		m.sampleOverflow++
		return
	}
	m.samples[sampleKey] = StructuralIntegritySample(event)
}

func (m *StructuralIntegrityMeter) Snapshot(opts StructuralIntegritySnapshotOptions) StructuralIntegritySnapshot {
	if m == nil {
		return StructuralIntegritySnapshot{}
	}
	allowed := make(map[string]struct{}, len(opts.RepoPrefixes))
	for _, repo := range opts.RepoPrefixes {
		allowed[normalizeStructuralRepo(repo)] = struct{}{}
	}
	scoped := opts.RepoScopeActive || len(opts.RepoPrefixes) > 0
	matches := func(repo string) bool {
		if !scoped {
			return true
		}
		_, ok := allowed[repo]
		return ok
	}

	out := StructuralIntegritySnapshot{}
	m.mu.Lock()
	out.AttributionOverflow = m.attributionOverflow
	out.SampleOverflow = m.sampleOverflow
	out.RepoTotalsOverflow = m.repoTotalsOverflow
	if opts.IncludeAttribution {
		for key, count := range m.attribution {
			if !matches(key.repo) {
				continue
			}
			out.Attribution = append(out.Attribution, StructuralIntegrityAttribution{
				Direction: key.direction,
				Path:      key.path,
				Repo:      key.repo,
				Kind:      key.kind,
				Reason:    key.reason,
				Origin:    key.origin,
				Count:     count,
			})
		}
	}
	if !scoped {
		out.Totals.WriteRejected = m.writeRejected.Load()
		out.Totals.ReadSuppressed = m.readSuppressed.Load()
	} else {
		for repo := range allowed {
			totals := m.repoTotals[repo]
			out.Totals.WriteRejected += totals.WriteRejected
			out.Totals.ReadSuppressed += totals.ReadSuppressed
		}
		out.TotalsLowerBound = m.repoTotalsOverflow > 0
	}
	if opts.IncludeSamples {
		for _, sample := range m.samples {
			if matches(sample.Repo) {
				out.Samples = append(out.Samples, sample)
			}
		}
	}
	m.mu.Unlock()

	sort.Slice(out.Attribution, func(i, j int) bool {
		a, b := out.Attribution[i], out.Attribution[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Origin < b.Origin
	})
	sort.Slice(out.Samples, func(i, j int) bool {
		a, b := out.Samples[i], out.Samples[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		if a.Origin != b.Origin {
			return a.Origin < b.Origin
		}
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		return a.Line < b.Line
	})
	return out
}

func (g *Graph) RecordStructuralIntegrityEvent(event StructuralIntegrityEvent) {
	g.structuralIntegrity.Record(event)
}

func (g *Graph) StructuralIntegritySnapshot(opts StructuralIntegritySnapshotOptions) StructuralIntegritySnapshot {
	return g.structuralIntegrity.Snapshot(opts)
}

func (g *Graph) recordStructuralRejections(path StructuralDropPath, edges []*Edge, nodes []*Node) {
	if len(edges) == 0 {
		return
	}
	if g.structuralIntegrityPath != "" {
		path = g.structuralIntegrityPath
	}
	var sink StructuralIntegrityEventRecorder = g
	if g.structuralIntegritySink != nil {
		sink = g.structuralIntegritySink
	}
	configuredRepo := g.structuralIntegrityRepo
	inputRepos := make(map[string]string)
	if configuredRepo == "" || configuredRepo == "unknown" {
		for _, node := range nodes {
			if node != nil && node.ID != "" && node.RepoPrefix != "" {
				inputRepos[node.ID] = node.RepoPrefix
			}
		}
	}
	for _, edge := range edges {
		repo := configuredRepo
		if repo == "" || repo == "unknown" {
			repo = inputRepos[edge.From]
			if repo == "" {
				if source := g.GetNode(edge.From); source != nil {
					repo = source.RepoPrefix
				}
			}
		}
		if event, ok := StructuralIntegrityEventForEdge(StructuralDropWrite, path, repo, edge); ok {
			sink.RecordStructuralIntegrityEvent(event)
		}
	}
}

func NewStructuralIntegrityShadow(owner Store, repo string, path StructuralDropPath) *Graph {
	g := New()
	if sink, ok := owner.(StructuralIntegrityEventRecorder); ok {
		g.structuralIntegritySink = sink
	}
	g.structuralIntegrityRepo = normalizeStructuralRepo(repo)
	g.structuralIntegrityPath = normalizeStructuralPath(path)
	return g
}
