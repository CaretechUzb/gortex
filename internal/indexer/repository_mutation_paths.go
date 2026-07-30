package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/pathkey"
)

var errEmptyRepositoryMutationPath = errors.New("repository mutation path is empty")

// canonicalRepositoryMutationPaths validates one caller's explicit scope before
// it can join a coalesced coordinator generation. Returned paths are
// repository-relative, slash-separated NFC keys, so absolute/relative and
// Unicode-normalization aliases occupy one coordinator slot. A nil/empty input
// remains the sole spelling of a full-tree request.
func canonicalRepositoryMutationPaths(root string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if root == "" {
		return nil, errors.New("repository mutation root is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("repository mutation root %q: %w", root, err)
	}
	absRoot = filepath.Clean(absRoot)

	canonical := make(map[string]struct{}, len(paths))
	full := false
	for _, path := range paths {
		if path == "" {
			return nil, errEmptyRepositoryMutationPath
		}
		absPath := path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(absRoot, filepath.FromSlash(path))
		}
		absPath, err = filepath.Abs(absPath)
		if err != nil {
			return nil, fmt.Errorf("repository mutation path %q: %w", path, err)
		}
		absPath = filepath.Clean(absPath)
		rel, relErr := filepath.Rel(absRoot, absPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if relErr != nil {
				return nil, fmt.Errorf("repository mutation path %q relative to %q: %w", path, absRoot, relErr)
			}
			return nil, fmt.Errorf("repository mutation path %q is outside repository root %q", path, absRoot)
		}
		// A missing path is a valid deletion scope. Other stat failures (invalid
		// bytes, permissions, I/O) are caller-local errors and must not poison a
		// generation shared with otherwise valid waiters.
		if _, statErr := os.Stat(absPath); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("repository mutation stat %q: %w", path, statErr)
		}
		if rel == "." {
			full = true
			continue
		}
		canonical[pathkey.Normalize(filepath.ToSlash(rel))] = struct{}{}
	}
	if full {
		return nil, nil
	}
	result := make([]string, 0, len(canonical))
	for path := range canonical {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func canonicalRepositoryMutationPath(root, path string) (string, error) {
	paths, err := canonicalRepositoryMutationPaths(root, []string{path})
	if err != nil {
		return "", err
	}
	if len(paths) != 1 {
		return "", fmt.Errorf("repository mutation path %q resolves to the repository root", path)
	}
	return paths[0], nil
}

func validateRepositoryMutationRegularFile(root, relPath string) error {
	absPath := filepath.Join(root, filepath.FromSlash(relPath))
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("index file %q: %w", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("index file %q is not a regular file", relPath)
	}
	return nil
}

// repositoryMutationRootPath returns the authoritative root used to validate a
// public point mutation. A stale MultiIndexer-owned Indexer consults current
// prefix metadata rather than admitting a path against its construction-time
// root.
func (idx *Indexer) repositoryMutationRootPath() (string, error) {
	if idx == nil {
		return "", errors.New("repository mutation indexer is nil")
	}
	if owner := idx.repositoryMutationOwner; owner != nil && idx.repoPrefix != "" {
		owner.mu.RLock()
		meta := owner.repos[idx.repoPrefix]
		root := ""
		if meta != nil {
			root = meta.RootPath
		}
		owner.mu.RUnlock()
		if root == "" {
			return "", fmt.Errorf("repository mutation root is unavailable for %q", idx.repoPrefix)
		}
		return root, nil
	}
	if idx.rootPath == "" {
		return "", errors.New("repository mutation root is unavailable")
	}
	return idx.rootPath, nil
}
