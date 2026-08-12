package mcp

import "strings"

func exploreQualifiedIdentifierLeaf(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if separator := strings.LastIndex(identifier, "::"); separator >= 0 {
		identifier = identifier[separator+2:]
	}
	if separator := strings.LastIndexAny(identifier, ".\\/"); separator >= 0 {
		identifier = identifier[separator+1:]
	}
	return identifier
}
