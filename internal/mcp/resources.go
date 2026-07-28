package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerResources() {
	// Session state: survives context compaction.
	s.addResource(
		mcp.NewResource(
			"gortex://session",
			"Session State",
			mcp.WithResourceDescription("Recently viewed symbols, modified files, and search queries. Read after context compaction to restore working memory without re-calling tools."),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleResourceSession,
	)

	// Static resource: graph stats (session start orientation). Same
	// payload as the `graph_stats` tool — kept as a tool too for
	// back-compat with clients that don't speak resources.
	s.addResource(
		mcp.NewResource(
			"gortex://stats",
			"Graph Statistics",
			mcp.WithResourceDescription("Node/edge counts by kind and language, plus per-repo / token-savings / semantic-provider rollups. Read at session start to orient in the codebase. Updates push as `notifications/resources/updated` after each graph re-warm."),
			mcp.WithMIMEType("application/json"),
		),
		s.handleResourceStats,
	)

	// Static resource: graph schema reference.
	s.addResource(
		mcp.NewResource(
			"gortex://schema",
			"Graph Schema",
			mcp.WithResourceDescription("Node kinds, edge kinds, and their relationships. Reference for understanding graph query results."),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleResourceSchema,
	)

	// Static resource: the on-demand reference guide. The single home for
	// content that used to be pre-paid in the installed CLAUDE.md section —
	// the LLM-provider matrix, capabilities catalog, token-economy detail,
	// resources list, and pointers into the analyze / search_ast catalogs.
	s.addResource(
		mcp.NewResource(
			"gortex://guide",
			"Gortex Guide",
			mcp.WithResourceDescription("On-demand reference: LLM-provider matrix, capabilities catalog, token-economy deep-dive, MCP resources, analyze/search_ast catalogs, session-start checklist. Section-addressable via gortex://guide/{topic}."),
			mcp.WithMIMEType("text/markdown"),
		),
		s.handleResourceGuide,
	)

	// Template resource: a single guide section by topic keyword.
	s.addResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://guide/{topic}",
			"Gortex Guide Section",
			mcp.WithTemplateDescription("One section of the guide by topic: providers, capabilities, tokens, analyze, search_ast, resources, workflow."),
			mcp.WithTemplateMIMEType("text/markdown"),
		),
		s.handleResourceGuideSection,
	)

	// Bootstrap-state resources: read-only, no args, every session
	// hits these at startup. Same payloads as the corresponding
	// tools; tools stay registered for back-compat.
	s.registerBootstrapResources()

	// Analyzer-backed rollup resources: long-form summaries whose
	// only "argument" is the current state of the indexed code.
	s.registerAnalyzerResources()

	// Template resources: communities and processes (dynamic, parameterized).
	s.addResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://communities",
			"Communities",
			mcp.WithTemplateDescription("Functional clusters discovered by community detection with cohesion scores."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceCommunities,
	)

	s.addResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://community/{id}",
			"Community Detail",
			mcp.WithTemplateDescription("Members, files, and cohesion score for a specific community."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceCommunity,
	)

	s.addResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://processes",
			"Processes",
			mcp.WithTemplateDescription("Discovered execution flows — call chains starting from entry points."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceProcesses,
	)

	s.addResourceTemplate(
		mcp.NewResourceTemplate(
			"gortex://process/{id}",
			"Process Detail",
			mcp.WithTemplateDescription("Step-by-step call chain for a specific execution flow."),
			mcp.WithTemplateMIMEType("application/json"),
		),
		s.handleResourceProcess,
	)
}

func (s *Server) handleResourceStats(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return jsonResource(req.Params.URI, s.buildGraphStatsPayload(ctx))
}

// jsonResource marshals payload as JSON and wraps it in the single-entry
// ResourceContents slice every read-resource handler returns.
func jsonResource(uri string, payload any) ([]mcp.ResourceContents, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleResourceSession(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	snap := s.sessionFor(ctx).snapshot()

	var b strings.Builder
	b.WriteString("# Gortex Session State\n\n")

	if files, ok := snap["modified_files"].([]string); ok && len(files) > 0 {
		b.WriteString("## Modified Files\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}

	if symbols, ok := snap["viewed_symbols"].([]string); ok && len(symbols) > 0 {
		b.WriteString("## Recently Viewed Symbols\n")
		for _, s := range symbols {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		b.WriteString("\n")
	}

	if files, ok := snap["viewed_files"].([]string); ok && len(files) > 0 {
		b.WriteString("## Recently Viewed Files\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}

	if queries, ok := snap["recent_searches"].([]string); ok && len(queries) > 0 {
		b.WriteString("## Recent Searches\n")
		for _, q := range queries {
			fmt.Fprintf(&b, "- \"%s\"\n", q)
		}
		b.WriteString("\n")
	}

	if b.Len() <= len("# Gortex Session State\n\n") {
		b.WriteString("No activity recorded yet.\n")
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "text/plain",
			Text:     b.String(),
		},
	}, nil
}

func (s *Server) handleResourceSchema(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	schema := `# Gortex Graph Schema

## Node Kinds
- file      — source file
- function  — top-level function or free function
- method    — method belonging to a type (has EdgeMemberOf)
- type      — struct, class, enum, module, table
- interface — interface, trait, protocol, service
- variable  — variable, constant, field, property
- import    — resolved or unresolved import target
- package   — package, namespace, module
- doc       — a heading-delimited Markdown prose section; Name is the
              breadcrumb heading path, Meta["section_text"] holds the
              section body. Searchable via search_symbols corpus:docs.
- contract  — an API contract record (HTTP route, gRPC/Thrift method,
              topic, env var, …); ID is the canonical contract key,
              Meta carries type/role/symbol_id.
- contract_bridge — one matched provider↔consumer contract group
              (route / RPC method / topic) spanning every participating
              repo; ID is bridge::<contract-id>, Meta carries
              canonical_key, repos, provider_count, consumer_count.
              Queried via the contracts tool's action=bridge.

## Edge Kinds
- calls        — function/method A calls function/method B
- imports      — file A imports file/package B
- re_exports   — barrel-file re-export forwarding (export {x} / export * from "mod"); distinct from imports so a dependency walk tells forwarding from consumption
- defines      — file/package A defines symbol B
- implements   — type A implements interface B (structural inference)
- extends      — class A extends class B
- references   — symbol A references type/variable B
- member_of    — method/field A belongs to type B
- instantiates — function A creates instance of type B
- similar_to   — function/method A is a near-duplicate (clone) of B
- provides     — symbol A provides contract B; consumes is the inverse role
- matches      — consumer symbol A resolves to provider symbol B across services
- bridges      — contract_bridge A groups contract B (edge Meta["side"] =
                 provider|consumer|both)
- package_workspace_member — package-manager workspace root A owns member package B
- cross_repo_calls      — calls edge whose target lives in another repo
- cross_repo_implements — implements edge crossing a repo boundary
- cross_repo_extends    — extends edge crossing a repo boundary

## Node ID Format
  file_path::SymbolName
  file_path::TypeName.MethodName

## Meta Fields
- signature  — function/method signature string
- receiver   — method receiver type name
- methods    — interface/trait method names ([]string, for IMPLEMENTS inference)
- proto_type — protobuf: "message", "enum"
- sql_type   — SQL: "table", "view", "index", "trigger"
- schema     — SQL: schema qualifier of a schema-qualified object (Name stays the bare object name)
- qualified_name — SQL: "schema.object", set only when the declaration was qualified
- indexes    — SQL: qualified name of the table a CREATE INDEX covers
- visibility — "private" for unexported symbols
- temporal_role — Temporal node role: activity / workflow / activity_interface / workflow_interface / signal / query / update

## Edge Meta: via
Synthesized framework-dispatch edges carry a "via" tag on the calls edge.
Temporal (with temporal_kind + temporal_name):
- temporal.register   — worker registration (provider; activity/workflow)
- temporal.stub       — workflow→activity/child-workflow dispatch (resolved)
- temporal.start      — service→workflow start, ExecuteWorkflow/SignalWithStartWorkflow (resolved)
- temporal.handler    — workflow exposes query/signal/update handler (provider)
- temporal.signal-send / temporal.query-call — sender→running workflow (consumer)

## analyze kinds
` + analyzeCatalogText() + `
## search_ast detectors
` + searchASTDetectorCatalogText()
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "text/plain",
			Text:     schema,
		},
	}, nil
}

func (s *Server) handleResourceGuide(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "text/markdown",
			Text:     GuideText(""),
		},
	}, nil
}

func (s *Server) handleResourceGuideSection(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	topic := extractURIParam(req.Params.URI, "gortex://guide/")
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "text/markdown",
			Text:     GuideText(topic),
		},
	}, nil
}

func (s *Server) handleResourceCommunities(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	rows, next, err := s.analysisCommunitySummaries(100, "")
	if err != nil || len(rows) == 0 {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     `{"communities":[],"message":"no communities detected yet"}`,
			},
		}, nil
	}

	type summary struct {
		ID       string   `json:"id"`
		Label    string   `json:"label"`
		Size     int      `json:"size"`
		Files    []string `json:"files"`
		Cohesion float64  `json:"cohesion"`
	}
	summaries := make([]summary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, summary{
			ID: row.ID, Label: row.Label, Size: row.Size,
			Files: row.Files, Cohesion: row.Cohesion,
		})
	}
	total := len(summaries)
	modularity := 0.0
	if _, header, ok := s.activeAnalysisQuery(); ok {
		total = header.CommunityCount
		modularity = header.Modularity
	}
	data, err := json.Marshal(map[string]any{
		"communities": summaries,
		"returned":    len(summaries),
		"total":       total,
		"modularity":  modularity,
		"truncated":   next != "",
		"next_cursor": next,
	})
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleResourceCommunity(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	id := extractURIParam(req.Params.URI, "gortex://community/")
	if id == "" {
		return nil, fmt.Errorf("missing community id in URI")
	}
	var found bool
	var label, hub, parentID string
	var size int
	var cohesion float64
	var files []string
	cursor := ""
	for !found {
		rows, next, err := s.analysisCommunitySummaries(analysisGenerationQueryPage, cursor)
		if err != nil {
			return nil, fmt.Errorf("no communities detected yet")
		}
		for _, row := range rows {
			if row.ID == id {
				found, label, hub, parentID = true, row.Label, row.Hub, row.ParentID
				size, cohesion, files = row.Size, row.Cohesion, row.Files
				break
			}
		}
		if found || next == "" || next == cursor || len(rows) == 0 {
			break
		}
		cursor = next
	}
	if !found {
		return nil, fmt.Errorf("community not found: %s", id)
	}
	members, next, err := s.analysisCommunityMembers(id, analysisGenerationQueryMax, "")
	if err != nil {
		return nil, err
	}
	memberIDs := make([]string, 0, len(members))
	for _, member := range members {
		memberIDs = append(memberIDs, member.NodeID)
	}
	data, err := json.Marshal(map[string]any{
		"id": id, "label": label, "hub": hub, "parent_id": parentID,
		"size": size, "cohesion": cohesion, "files": files, "members": memberIDs,
		"members_truncated": next != "", "next_member_cursor": next,
	})
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI: req.Params.URI, MIMEType: "application/json", Text: string(data),
		},
	}, nil
}

func (s *Server) handleResourceProcesses(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	rows, next, err := s.analysisProcessSummaries(100, "")
	if err != nil || len(rows) == 0 {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     `{"processes":[],"message":"no processes discovered yet"}`,
			},
		}, nil
	}

	type summary struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		EntryPoint string  `json:"entry_point"`
		StepCount  int     `json:"step_count"`
		FileCount  int     `json:"file_count"`
		Score      float64 `json:"score"`
	}
	summaries := make([]summary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, summary{
			ID: row.ID, Name: row.Name, EntryPoint: row.EntryPoint,
			StepCount: row.StepCount, FileCount: len(row.Files), Score: row.Score,
		})
	}
	total := len(summaries)
	if _, header, ok := s.activeAnalysisQuery(); ok {
		total = header.ProcessCount
	}
	data, err := json.Marshal(map[string]any{
		"processes":   summaries,
		"returned":    len(summaries),
		"total":       total,
		"truncated":   next != "",
		"next_cursor": next,
	})
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleResourceProcess(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	id := extractURIParam(req.Params.URI, "gortex://process/")
	if id == "" {
		return nil, fmt.Errorf("missing process id in URI")
	}
	var found bool
	var name, entryPoint string
	var stepCount int
	var score float64
	var truncated bool
	var files []string
	cursor := ""
	for !found {
		rows, next, err := s.analysisProcessSummaries(analysisGenerationQueryPage, cursor)
		if err != nil {
			return nil, fmt.Errorf("no processes discovered yet")
		}
		for _, row := range rows {
			if row.ID == id {
				found, name, entryPoint = true, row.Name, row.EntryPoint
				stepCount, score, truncated, files = row.StepCount, row.Score, row.Truncated, row.Files
				break
			}
		}
		if found || next == "" || next == cursor || len(rows) == 0 {
			break
		}
		cursor = next
	}
	if !found {
		return nil, fmt.Errorf("process not found: %s", id)
	}
	steps, next, err := s.analysisProcessSteps(id, analysisGenerationQueryMax, -1)
	if err != nil {
		return nil, err
	}
	type step struct {
		ID    string `json:"id"`
		Depth int    `json:"depth"`
	}
	outSteps := make([]step, 0, len(steps))
	for _, row := range steps {
		outSteps = append(outSteps, step{ID: row.NodeID, Depth: row.Depth})
	}
	data, err := json.Marshal(map[string]any{
		"id": id, "name": name, "entry_point": entryPoint,
		"steps": outSteps, "step_count": stepCount, "files": files,
		"score": score, "truncated": truncated || next >= 0,
		"next_step_cursor": next,
	})
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI: req.Params.URI, MIMEType: "application/json", Text: string(data),
		},
	}, nil
}

// extractURIParam extracts the parameter value after a URI prefix.
// e.g. extractURIParam("gortex://community/community-0", "gortex://community/") => "community-0"
func extractURIParam(uri, prefix string) string {
	if len(uri) > len(prefix) && uri[:len(prefix)] == prefix {
		return uri[len(prefix):]
	}
	return ""
}
