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
	reader graph.Reader
	byFile map[string][]*graph.Node
}

func newLocalizationFileDeclarationCache(reader graph.Reader) *localizationFileDeclarationCache {
	return &localizationFileDeclarationCache{
		reader: reader,
		byFile: make(map[string][]*graph.Node),
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
	definitions := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil || isNonDefinitionNode(node.Kind) {
			continue
		}
		definitions = append(definitions, node)
	}
	cache.byFile[file] = definitions
	return definitions
}
