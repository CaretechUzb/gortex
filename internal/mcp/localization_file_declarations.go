package mcp

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// localizationFileDeclarationCache enumerates declarations through the
// node-only Reader path and remembers both hits and misses for one localization
// request. Outlines never need file adjacency, so decoding edges here is pure
// overhead and can multiply across the page's distinct files.
type localizationFileDeclarationCache struct {
	reader          graph.Reader
	byFile          map[string][]*graph.Node
	definitionLimit int
}

func newLocalizationFileDeclarationCache(reader graph.Reader) *localizationFileDeclarationCache {
	return newBoundedLocalizationFileDeclarationCache(reader, 0)
}

func newBoundedLocalizationFileDeclarationCache(reader graph.Reader, definitionLimit int) *localizationFileDeclarationCache {
	return &localizationFileDeclarationCache{
		reader:          reader,
		byFile:          make(map[string][]*graph.Node),
		definitionLimit: max(definitionLimit, 0),
	}
}

func (cache *localizationFileDeclarationCache) definitions(file string) []*graph.Node {
	file = strings.TrimSpace(file)
	if cache == nil || cache.reader == nil || file == "" {
		return nil
	}
	if nodes, cached := cache.byFile[file]; cached {
		return nodes
	}
	nodes := cache.reader.GetFileNodes(file)
	capacity := len(nodes)
	if cache.definitionLimit > 0 {
		capacity = min(capacity, cache.definitionLimit)
	}
	definitions := make([]*graph.Node, 0, capacity)
	for _, node := range nodes {
		if node == nil || isNonDefinitionNode(node.Kind) {
			continue
		}
		definitions = append(definitions, node)
		if cache.definitionLimit > 0 && len(definitions) >= cache.definitionLimit {
			break
		}
	}
	cache.byFile[file] = definitions
	return definitions
}
