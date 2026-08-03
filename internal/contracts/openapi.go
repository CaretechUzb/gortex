package contracts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// OpenAPIExtractor detects OpenAPI/Swagger spec definitions (providers).
type OpenAPIExtractor struct{}

var (
	// Detect OpenAPI YAML structure: paths with HTTP methods.
	openapiPathRe   = regexp.MustCompile(`(?m)^  (\/\S+)\s*:`)
	openapiMethodRe = regexp.MustCompile(`(?m)^\s{4}(get|post|put|patch|delete|head|options)\s*:`)

	// Detect OpenAPI JSON structure.
	openapiJSONPathRe   = regexp.MustCompile(`"(\/[^"]+)"\s*:\s*\{`)
	openapiJSONMethodRe = regexp.MustCompile(`"(get|post|put|patch|delete|head|options)"\s*:\s*\{`)

	// Schema-shape extraction — these run over a single operation's
	// block to pull out request / response type names. Full OpenAPI
	// parsing is overkill for what downstream validation needs:
	// a reference to the `components.schemas.<Name>` block is enough.
	//
	// The optional `['"]?` after `\$ref` handles the JSON form where
	// the key is `"$ref"` with a trailing quote before the colon;
	// in YAML the key is a bare `$ref:` with nothing between the
	// identifier and the colon.
	openapiRefRe = regexp.MustCompile(`\$ref['"]?\s*:\s*['"]?#/components/schemas/(\w+)['"]?`)
	// YAML: `requestBody:` block anchor, then everything up to the
	// next sibling key or the end of the operation.
	openapiRequestBodyRe = regexp.MustCompile(`(?s)requestBody\s*:(.*?)(?:\n      \w|\z)`)
	// YAML: `responses:` block; within it, a status-code entry followed
	// by a `$ref`. We capture both the status and the ref.
	openapiResponseStatusRe = regexp.MustCompile(`(?m)^\s{8}['"]?(\d{3})['"]?\s*:`)
	// JSON status-code extraction. Brace-balanced slicing (see
	// jsonObjectSlice below) handles the request/response object
	// boundaries so no greedy regex fight with nested braces.
	openapiJSONStatusRe = regexp.MustCompile(`"(\d{3})"\s*:`)
)

func (e *OpenAPIExtractor) SupportedLanguages() []string {
	return []string{"yaml", "json"}
}

func (e *OpenAPIExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	// Most JSON/YAML files are not API specifications. Keep this rejection
	// path on the caller-owned byte slice: converting a multi-megabyte source
	// to string and lower-casing it made two full-size copies merely to look
	// for these ASCII keys.
	if !containsOpenAPIMarker(src) {
		return nil
	}

	if strings.HasSuffix(filePath, ".json") {
		return e.extractJSON(filePath, src)
	}
	return e.extractYAML(filePath, src)
}

func containsOpenAPIMarker(src []byte) bool {
	const (
		openapi = "openapi"
		swagger = "swagger"
	)
	for start, c := range src {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		switch c {
		case 'o':
			if hasFoldASCIIPrefix(src[start:], openapi) {
				return true
			}
		case 's':
			if hasFoldASCIIPrefix(src[start:], swagger) {
				return true
			}
		}
	}
	return false
}

func hasFoldASCIIPrefix(src []byte, keyword string) bool {
	if len(src) < len(keyword) {
		return false
	}
	for i := 0; i < len(keyword); i++ {
		c := src[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != keyword[i] {
			return false
		}
	}
	return true
}

func (e *OpenAPIExtractor) extractYAML(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	// Find the "paths:" section.
	pathsIdx := strings.Index(text, "\npaths:")
	if pathsIdx < 0 {
		if strings.HasPrefix(text, "paths:") {
			pathsIdx = 0
		} else {
			return nil
		}
	}
	pathsSection := text[pathsIdx:]

	// Find each path entry.
	pathMatches := openapiPathRe.FindAllStringSubmatchIndex(pathsSection, -1)
	for i, pm := range pathMatches {
		path := pathsSection[pm[2]:pm[3]]
		// Determine the sub-section for this path (up to next path or end).
		end := len(pathsSection)
		if i+1 < len(pathMatches) {
			end = pathMatches[i+1][0]
		}
		pathBlock := pathsSection[pm[0]:end]

		// Iterate method matches with explicit indices so we can
		// slice each operation's block from the method line to the
		// next sibling method / path — that's the window we scan
		// for requestBody / responses $refs.
		methodLocs := openapiMethodRe.FindAllStringSubmatchIndex(pathBlock, -1)
		methodMatches := openapiMethodRe.FindAllStringSubmatch(pathBlock, -1)
		for mi, mm := range methodMatches {
			method := strings.ToUpper(mm[1])
			absOffset := pathsIdx + pm[0]
			normPath := NormalizeHTTPPath(path)

			// Block for this operation: start at the method line,
			// end at the next operation or end-of-path.
			opStart := methodLocs[mi][0]
			opEnd := len(pathBlock)
			if mi+1 < len(methodLocs) {
				opEnd = methodLocs[mi+1][0]
			}
			opBlock := pathBlock[opStart:opEnd]

			meta := map[string]any{"method": method, "path": normPath}
			reqType, respType, statusCodes := openapiExtractSchemas(opBlock)
			if reqType != "" {
				meta["request_type"] = reqType
			}
			if respType != "" {
				meta["response_type"] = respType
			}
			if len(statusCodes) > 0 {
				meta["status_codes"] = statusCodes
			}
			if reqType != "" || respType != "" {
				meta["schema_source"] = "extracted"
			} else {
				meta["schema_source"] = "none"
			}

			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("http::%s::%s", method, normPath),
				Type:       ContractOpenAPI,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       lineNumber(lines, absOffset),
				Meta:       meta,
				Confidence: 0.95,
			})
		}
	}

	return contracts
}

func (e *OpenAPIExtractor) extractJSON(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	pathMatches := openapiJSONPathRe.FindAllStringSubmatchIndex(text, -1)
	for i, pm := range pathMatches {
		path := text[pm[2]:pm[3]]
		if !strings.HasPrefix(path, "/") {
			continue
		}
		end := len(text)
		if i+1 < len(pathMatches) {
			end = pathMatches[i+1][0]
		}
		pathBlock := text[pm[0]:end]

		methodLocs := openapiJSONMethodRe.FindAllStringSubmatchIndex(pathBlock, -1)
		methodMatches := openapiJSONMethodRe.FindAllStringSubmatch(pathBlock, -1)
		for mi, mm := range methodMatches {
			method := strings.ToUpper(mm[1])
			normPath := NormalizeHTTPPath(path)
			opStart := methodLocs[mi][0]
			opEnd := len(pathBlock)
			if mi+1 < len(methodLocs) {
				opEnd = methodLocs[mi+1][0]
			}
			opBlock := pathBlock[opStart:opEnd]

			meta := map[string]any{"method": method, "path": normPath}
			reqType, respType, statusCodes := openapiExtractSchemasJSON(opBlock)
			if reqType != "" {
				meta["request_type"] = reqType
			}
			if respType != "" {
				meta["response_type"] = respType
			}
			if len(statusCodes) > 0 {
				meta["status_codes"] = statusCodes
			}
			if reqType != "" || respType != "" {
				meta["schema_source"] = "extracted"
			} else {
				meta["schema_source"] = "none"
			}
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("http::%s::%s", method, normPath),
				Type:       ContractOpenAPI,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       lineNumber(lines, pm[0]),
				Meta:       meta,
				Confidence: 0.9,
			})
		}
	}

	return contracts
}

// openapiExtractSchemas pulls the request / response body type names
// (from `$ref: '#/components/schemas/<X>'` entries) and the declared
// response status codes out of a single operation's YAML block.
// Returns bare schema names — the module-wide post-pass upgrades
// them to symbol IDs of the matching type nodes in the same spec
// file's components block.
func openapiExtractSchemas(opBlock string) (requestType, responseType string, statusCodes []int) {
	// Request body — scope the $ref search to the requestBody block
	// so a $ref under `parameters` or `responses` doesn't bleed in.
	if m := openapiRequestBodyRe.FindStringSubmatch(opBlock); len(m) > 1 {
		if r := openapiRefRe.FindStringSubmatch(m[1]); len(r) > 1 {
			requestType = r[1]
		}
	}
	// Responses — walk status entries in order, first $ref wins for
	// response_type (typically the 2xx body) and we record every
	// declared status code.
	respIdx := strings.Index(opBlock, "responses:")
	if respIdx >= 0 {
		respBlock := opBlock[respIdx:]
		for _, m := range openapiResponseStatusRe.FindAllStringSubmatch(respBlock, -1) {
			if n, ok := parseStatusExpr(m[1]); ok {
				statusCodes = append(statusCodes, n)
			}
		}
		if r := openapiRefRe.FindStringSubmatch(respBlock); len(r) > 1 {
			responseType = r[1]
		}
	}
	return
}

// openapiExtractSchemasJSON is the JSON counterpart. Regex-based
// capture of `"requestBody": { ... }` would misbehave on nested
// braces — instead we locate the key, brace-balance forward to find
// the matching close, and scan that slice for a $ref.
func openapiExtractSchemasJSON(opBlock string) (requestType, responseType string, statusCodes []int) {
	if slice := jsonObjectSlice(opBlock, `"requestBody"`); slice != "" {
		if r := openapiRefRe.FindStringSubmatch(slice); len(r) > 1 {
			requestType = r[1]
		}
	}
	if slice := jsonObjectSlice(opBlock, `"responses"`); slice != "" {
		for _, sm := range openapiJSONStatusRe.FindAllStringSubmatch(slice, -1) {
			if n, ok := parseStatusExpr(sm[1]); ok {
				statusCodes = append(statusCodes, n)
			}
		}
		if r := openapiRefRe.FindStringSubmatch(slice); len(r) > 1 {
			responseType = r[1]
		}
	}
	return
}

// jsonObjectSlice returns the textual body of the JSON object that
// follows `key` in src — i.e. the substring between the opening `{`
// and the matching `}`, with braces balanced. Returns empty string
// when the key isn't present or the object is malformed. Comments
// and strings aren't specially handled (JSON has no comments and
// `}` doesn't appear unescaped inside strings in any sane OpenAPI
// spec).
func jsonObjectSlice(src, key string) string {
	idx := strings.Index(src, key)
	if idx < 0 {
		return ""
	}
	// Skip to the first `{` after the key.
	open := strings.Index(src[idx:], "{")
	if open < 0 {
		return ""
	}
	open += idx
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open+1 : i]
			}
		}
	}
	return ""
}
