package mcp

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// localizationFileDeclarations carries a bounded declaration page together with
// the count observed while scanning the file. DeclaredKnown distinguishes a
// complete count from fallback evidence, while Truncated says Nodes intentionally
// retains only a prefix of that known declaration set.
type localizationFileDeclarations struct {
	Nodes         []*graph.Node
	Declared      int
	DeclaredKnown bool
	Truncated     bool
}

// localizationFileDeclarationCache enumerates declarations through the
// node-only Reader path and remembers both hits and misses for one localization
// request. Outlines never need file adjacency, so decoding edges here is pure
// overhead and can multiply across the page's distinct files.
type localizationFileDeclarationCache struct {
	reader          graph.Reader
	byFile          map[string]localizationFileDeclarations
	definitionLimit int
}

func newLocalizationFileDeclarationCache(reader graph.Reader) *localizationFileDeclarationCache {
	return newBoundedLocalizationFileDeclarationCache(reader, 0)
}

func newBoundedLocalizationFileDeclarationCache(reader graph.Reader, definitionLimit int) *localizationFileDeclarationCache {
	return &localizationFileDeclarationCache{
		reader:          reader,
		byFile:          make(map[string]localizationFileDeclarations),
		definitionLimit: max(definitionLimit, 0),
	}
}

func (cache *localizationFileDeclarationCache) definitions(file string) localizationFileDeclarations {
	file = strings.TrimSpace(file)
	if cache == nil || cache.reader == nil || file == "" {
		return localizationFileDeclarations{}
	}
	if declarations, cached := cache.byFile[file]; cached {
		return declarations
	}
	declarations := cache.readDefinitions(file, cache.definitionLimit)
	cache.byFile[file] = declarations
	return declarations
}

// boundedDefinitions avoids retaining a generated file's entire declaration
// set for callers that need only bounded supporting evidence. A cached outline
// page can be sliced safely; an uncached file is filtered into a short-lived
// bounded slice and deliberately does not poison the outline cache.
func (cache *localizationFileDeclarationCache) boundedDefinitions(file string, limit int) []*graph.Node {
	file = strings.TrimSpace(file)
	if cache == nil || cache.reader == nil || file == "" || limit <= 0 {
		return nil
	}
	if declarations, cached := cache.byFile[file]; cached {
		return declarations.Nodes[:min(len(declarations.Nodes), limit)]
	}
	return cache.readDefinitions(file, limit).Nodes
}

func (cache *localizationFileDeclarationCache) readDefinitions(file string, limit int) localizationFileDeclarations {
	nodes := cache.reader.GetFileNodes(file)
	capacity := len(nodes)
	if limit > 0 {
		capacity = min(capacity, limit)
	}
	definitions := make([]*graph.Node, 0, capacity)
	declared := 0
	for _, node := range nodes {
		if node == nil || isNonDefinitionNode(node.Kind) {
			continue
		}
		declared++
		if limit <= 0 || len(definitions) < limit {
			definitions = append(definitions, node)
		}
	}
	return localizationFileDeclarations{
		Nodes:         definitions,
		Declared:      declared,
		DeclaredKnown: true,
		Truncated:     declared > len(definitions),
	}
}
