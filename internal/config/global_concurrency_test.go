package config

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestGlobalConfigConcurrentAddRepoPreservesUniqueEntries(t *testing.T) {
	gc := &GlobalConfig{}
	root := t.TempDir()
	const repoCount = 32

	var workers sync.WaitGroup
	errors := make(chan error, repoCount)
	for i := 0; i < repoCount; i++ {
		i := i
		workers.Add(1)
		go func() {
			defer workers.Done()
			errors <- gc.AddRepo(RepoEntry{Path: filepath.Join(root, fmt.Sprintf("repo-%02d", i))})
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(gc.Repos); got != repoCount {
		t.Fatalf("repositories = %d, want %d", got, repoCount)
	}

	seen := make(map[string]struct{}, repoCount)
	for _, repo := range gc.Repos {
		if _, duplicate := seen[repo.Path]; duplicate {
			t.Fatalf("duplicate repository path %q", repo.Path)
		}
		seen[repo.Path] = struct{}{}
	}
}
