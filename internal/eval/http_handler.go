package eval

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/server"
	"go.uber.org/zap"
)

// Handler extends server.Handler with eval-specific endpoints.
// It inherits every /v1/* route from the base handler and adds
// POST /v1/augment for the eval augment pipeline.
type Handler struct {
	*server.Handler
}

// NewHandler creates an eval HTTP handler that dispatches to MCP tools.
// It provides all server /v1/* endpoints plus POST /v1/augment.
func NewHandler(mcpServer *mcpserver.MCPServer, g graph.Store, version string, logger *zap.Logger) *Handler {
	base := server.NewHandler(mcpServer, g, version, logger)
	h := &Handler{Handler: base}
	base.Mux().HandleFunc("POST /v1/augment", h.handleAugment)
	return h
}

// --- /augment (eval-specific) ---

type augmentRequest struct {
	Pattern string `json:"pattern"`
}

type augmentResponse struct {
	Pattern string          `json:"pattern"`
	Symbols []augmentSymbol `json:"symbols"`
}

type augmentSymbol struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	File      string   `json:"file"`
	Kind      string   `json:"kind"`
	Callers   []string `json:"callers,omitempty"`
	Callees   []string `json:"callees,omitempty"`
	CallChain []string `json:"call_chain,omitempty"`
}

func (h *Handler) handleAugment(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		server.WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req augmentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		server.WriteJSONError(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}

	if req.Pattern == "" {
		server.WriteJSONError(w, http.StatusBadRequest, "missing 'pattern' field")
		return
	}

	ctx := r.Context()

	searchResults := h.CallTool(ctx, "search_symbols", map[string]any{
		"query":   req.Pattern,
		"compact": true,
	})

	symbolIDs := extractSymbolIDs(searchResults)

	var symbols []augmentSymbol
	for _, id := range symbolIDs {
		sym := augmentSymbol{ID: id}

		if parts := strings.SplitN(id, "::", 2); len(parts) == 2 {
			sym.File = parts[0]
			sym.Name = parts[1]
		} else {
			sym.Name = id
		}

		if node := h.Graph().GetNode(id); node != nil {
			sym.Kind = string(node.Kind)
		}

		usageResults := h.CallTool(ctx, "find_usages", map[string]any{
			"id":      id,
			"compact": true,
		})
		sym.Callers = extractLines(usageResults)

		chainResults := h.CallTool(ctx, "get_call_chain", map[string]any{
			"function_id": id,
			"compact":     true,
			"depth":       float64(2),
		})
		sym.CallChain = extractLines(chainResults)

		symbols = append(symbols, sym)
	}

	resp := augmentResponse{
		Pattern: req.Pattern,
		Symbols: symbols,
	}
	server.WriteJSON(w, http.StatusOK, resp)
}

// --- Helpers ---

// extractSymbolIDs parses symbol IDs from compact search_symbols output.
func extractSymbolIDs(text string) []string {
	if text == "" {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 3 {
			name := parts[1]
			fileLine := parts[2]
			file := fileLine
			if idx := strings.LastIndex(fileLine, ":"); idx > 0 {
				file = fileLine[:idx]
			}
			id := file + "::" + name
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// extractLines splits text into non-empty trimmed lines.
func extractLines(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
