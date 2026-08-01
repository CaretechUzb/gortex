package main

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/indexer"
)

func TestWarmupDeltaFrontierPreservesExactFiles(t *testing.T) {
	frontier := newWarmupDeltaFrontier()
	frontier.record(&indexer.IndexResult{
		RepoPrefix: "repo-a",
		DerivedInvalidation: indexer.DerivedInvalidationPlan{
			Files:          []string{"repo-a/z.go", "repo-a/a.go"},
			LegacyFallback: true,
		},
	})
	frontier.record(&indexer.IndexResult{
		RepoPrefix: "repo-b",
		DerivedInvalidation: indexer.DerivedInvalidationPlan{
			Files: []string{"repo-b/b.go", "repo-a/a.go"},
		},
	})

	plans, files, exact := frontier.snapshot(2, false)
	if !exact {
		t.Fatal("complete incremental frontier was widened")
	}
	if len(plans) != 2 || !plans["repo-a"].LegacyFallback {
		t.Fatalf("plans were not preserved: %+v", plans)
	}
	wantFiles := []string{"repo-a/a.go", "repo-a/z.go", "repo-b/b.go"}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Fatalf("files = %v, want %v", files, wantFiles)
	}
}

func TestWarmupDeltaFrontierFailsClosed(t *testing.T) {
	tests := []struct {
		name         string
		result       *indexer.IndexResult
		changedRepos int
		scopeUnknown bool
		invalidate   bool
	}{
		{name: "full retrack", result: &indexer.IndexResult{RepoPrefix: "repo", FullRetrack: true}, changedRepos: 1},
		{name: "failed file", result: &indexer.IndexResult{RepoPrefix: "repo", FailedFiles: []string{"bad.go"}}, changedRepos: 1},
		{name: "missing prefix", result: &indexer.IndexResult{}, changedRepos: 1},
		{name: "scope unknown", result: &indexer.IndexResult{RepoPrefix: "repo"}, changedRepos: 1, scopeUnknown: true},
		{name: "missing result", result: &indexer.IndexResult{RepoPrefix: "repo"}, changedRepos: 2},
		{name: "explicit invalidation", result: &indexer.IndexResult{RepoPrefix: "repo"}, changedRepos: 1, invalidate: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frontier := newWarmupDeltaFrontier()
			frontier.record(tt.result)
			if tt.invalidate {
				frontier.invalidate()
			}
			_, _, exact := frontier.snapshot(tt.changedRepos, tt.scopeUnknown)
			if exact {
				t.Fatal("incomplete warm delta was accepted as exact")
			}
		})
	}
}
