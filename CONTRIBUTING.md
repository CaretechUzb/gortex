# Contributing to Gortex

Thank you for considering contributing to Gortex! This guide will help you get started.

## Licensing of contributions

Gortex is released under the [Apache License, Version 2.0](LICENSE.md).
By submitting a contribution (a pull request, patch, or other work) you
agree that it is licensed to the project under the same Apache 2.0 terms,
as described in section 5 of the License. You retain copyright in your
contribution; the project retains a perpetual, worldwide, royalty-free
license to use, modify, and redistribute it as part of Gortex.

Contributors are listed in [CONTRIBUTORS.md](CONTRIBUTORS.md). Add yourself
to that file in the same PR if you'd like to be credited.

## Getting Started

### Prerequisites

- Go 1.21+
- CGO enabled (required for tree-sitter C bindings)
- Git

### Building

```bash
git clone https://github.com/zzet/gortex.git
cd gortex
go build -o gortex ./cmd/gortex/
```

### Running Tests

```bash
go test -race ./...
```

### Running Benchmarks

```bash
go test -bench=. -benchmem ./internal/parser/languages/
go test -bench=. -benchmem ./internal/query/
go test -bench=. -benchmem ./internal/indexer/
```

## How to Contribute

### Reporting Bugs

- Open an issue with a clear description
- Include the output of `gortex version` and `go version`
- Provide a minimal reproduction if possible

### Suggesting Features

- Open an issue describing the feature and its use case
- For language support requests, mention if the tree-sitter grammar is available in `github.com/smacker/go-tree-sitter`

### Submitting Code

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Write tests for your changes
4. Ensure all tests pass (`go test -race ./...`)
5. Commit with a clear message
6. Open a pull request

### Adding a New Language Extractor

This is one of the most impactful contributions. Follow these steps:

1. Check if the tree-sitter grammar exists in `github.com/smacker/go-tree-sitter`
2. Create `internal/parser/languages/<language>.go` implementing the `parser.Extractor` interface
3. Create `internal/parser/languages/<language>_test.go` with at least 3 tests
4. Register it in `internal/parser/languages/register.go`
5. Debug the AST first — tree-sitter node types vary between grammars

**What to extract (in priority order):**
- Functions/methods with `EdgeMemberOf` to their class/type
- Classes/types/interfaces
- Interface method specs in `Meta["methods"]` (enables IMPLEMENTS inference)
- Imports
- Call sites
- Variables/constants

**Reference implementations:**
- `golang.go` — the most complete extractor
- `python.go` — simple OOP language
- `rust.go` — systems language with impl blocks
- `yaml.go` — simple config extractor

### Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- No unnecessary abstractions — three similar lines is better than a premature helper
- Tests should be self-contained with inline source snippets
- Extractor test helpers (`nodesOfKind`, `edgesOfKind`) are shared across test files

## Project Structure

```
cmd/gortex/          CLI entry point and commands
internal/
  analysis/          Community detection, process discovery, impact analysis
  claudemd/          CLAUDE.md generator
  config/            Configuration loading
  graph/             Core graph data structure (Node, Edge, Graph)
  indexer/           Directory walker, file watcher
  mcp/               MCP server and tool handlers
  parser/            Extractor interface, tree-sitter helpers
    languages/       Per-language extractors (one file each)
  query/             Query engine (BFS traversal, SubGraph)
  resolver/          Cross-file reference resolution, IMPLEMENTS inference
  web/               Web visualization server (Sigma.js)
pkg/gortex/          Public API for embedding
```

## Public API (`pkg/gortex`)

`New` returns `(*Engine, error)` — it opens a SQLite graph store, which can
fail. Pass `WithStorePath` to keep the store at a path of your choosing and
reuse the index on the next run; without it the store lives in a temp
directory.

Every `Engine` must be closed. `Close` checkpoints the write-ahead log, closes
the database handle, and removes the temp directory when `New` created one —
skipping it leaks a file handle and a background goroutine.

## Questions?

Open an issue or start a discussion. We're happy to help!
