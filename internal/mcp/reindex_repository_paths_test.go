package mcp

import (
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/indexer"
)

func TestNormalizeReindexPaths(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		want    []string
		wantErr bool
	}{
		{name: "omitted means full repository"},
		{name: "explicit empty means full repository", paths: []string{}},
		{name: "all blank means full repository", paths: []string{"", " \t"}},
		{
			name:  "mixed blanks retain scoped paths",
			paths: []string{"", "internal/indexer", "  ", "README.md"},
			want:  []string{"internal/indexer", "README.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeReindexPaths(tt.paths)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeReindexPaths() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeReindexPaths() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestIncrementalReindexSucceeded(t *testing.T) {
	tests := []struct {
		name   string
		result *indexer.IndexResult
		err    error
		want   bool
	}{
		{name: "successful batch", result: &indexer.IndexResult{}, want: true},
		{name: "nil result", result: nil},
		{name: "batch error", result: &indexer.IndexResult{}, err: errors.New("boom")},
		{name: "isolated file failure", result: &indexer.IndexResult{FailedFiles: []string{"broken.go"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := incrementalReindexSucceeded(tt.result, tt.err); got != tt.want {
				t.Fatalf("incrementalReindexSucceeded() = %v, want %v", got, tt.want)
			}
		})
	}
}
