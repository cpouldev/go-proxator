# Repository Guidelines

## Working principles

- Clarify requirements that would materially change architecture or product behavior. For small gaps, choose the simplest safe interpretation and record it.
- Preserve unrelated work in this fresh worktree. Do not reset, overwrite, or broadly reformat files outside the task.
- Prefer small, typed, testable changes. Keep external side effects behind durable local state and idempotent/retry-safe boundaries.
- Treat eligibility, consent, billing, authentication, and account deletion as correctness-critical. Fail closed when source evidence or state is ambiguous.
- Never commit plaintext credentials, local certificates, database dumps, or production environment files.

## Project Structure & Module Organization

The root `proxator` package holds the entire exported API and its white-box tests, so that every type, field, and method is documented on pkg.go.dev: routing and lifecycle in `client.go`, `pool.go`, and `endpoint.go`; configuration and classification in `config.go`, `detector.go`, and `transient.go`; transport contracts and adapters in `transport*.go`; package documentation in `doc.go`; runnable API examples in `example_test.go`. Only genuinely private state lives under `internal/` — currently `internal/domain/` for domain-aware routing state and its tests. The executable getting-started example is `examples/basic/main.go`. Broader usage guidance is in `README.md`, while CI and lint configuration live in `.github/workflows/` and `.golangci.yml`.

## Build, Test, and Development Commands

- `go build ./...` compiles the module.
- `go test ./...` runs unit, integration, and example tests.
- `go test -race -coverprofile=coverage.txt ./...` matches CI's race and coverage run.
- `go vet ./...` performs Go static checks.
- `gofmt -w .` formats Go files; `gofmt -l .` should then print nothing.
- `golangci-lint run` applies the configured linters.
- `go test -run 'TestPool_GetNextEndpoint' .` runs a focused test group.
- `go test -run '^$' -bench '^BenchmarkPool_GetNextEndpointForDomain$' -benchmem .` measures the routing hot path.

## Coding Style & Naming Conventions

Use Go 1.24-compatible syntax and let `gofmt` control layout and imports. Keep files focused and use lowercase underscore names such as `transport_http.go`. Exported identifiers use PascalCase, internal identifiers use camelCase, and exported APIs require Go doc comments. Wrap inspectable errors with `%w`. Preserve concurrency safety and verify shared-state changes with the race detector.

## Testing Guidelines

Tests use Go's `testing` package, table-driven subtests, and `t.Parallel()` when cases are isolated. Name tests by subject and scenario, for example `TestEndpoint_RecordLatency_Concurrent`. Add regression coverage beside affected code. Keep tests deterministic and offline; prefer synthetic URLs, in-memory factories, and local proxy fixtures over live services.

## Commit & Pull Request Guidelines

Use Conventional Commit subjects such as `feat: add transport adapter`, `fix: unblock closed sessions`, or `docs: clarify retries`. Keep commits focused and explain API or concurrency tradeoffs in the body. Pull requests should describe behavior changes, link issues, list verification commands, and call out public API changes. Include screenshots only for visual changes. Update `README.md` or `doc.go` when usage changes.

## Security & Configuration Tips

Proxy URLs may embed credentials. Never commit real usernames, passwords, endpoints, or captured responses; use clearly synthetic values in tests and documentation.

### Use Serena MCP for Semantic Code Analysis instead of regular code search and editing

Serena MCP is available for advanced code retrieval and editing capabilities.

**When to use Serena:**
- Symbol-based code navigation (find definitions, references, implementations)
- Precise code manipulation in structured codebases
- Prefer symbol-based operations over file-based grep/sed when available

**Key tools:**
- `find_symbol` - Find symbol by name across the codebase
- `find_referencing_symbols` - Find all symbols that reference a given symbol
- `get_symbols_overview` - Get overview of top-level symbols in a file
- `read_file` - Read file content within the project directory

**Usage notes:**
- Memory files can be manually reviewed/edited in `.serena/memories/`
