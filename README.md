# go-proxator

[![Go Reference](https://pkg.go.dev/badge/github.com/cpouldev/go-proxator.svg)](https://pkg.go.dev/github.com/cpouldev/go-proxator) [![CI](https://github.com/cpouldev/go-proxator/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/cpouldev/go-proxator/actions/workflows/ci.yml) [![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE.md)

`go-proxator` is a proxy-pool client for Go with performance-weighted routing, per-domain block memory, retries, and automatic endpoint recovery. It supports a browser-like TLS transport by default, a standard-library HTTP transport, and custom session implementations.

## Features

- Routes more traffic through fast, reliable endpoints without starving the rest.
- Applies an immediate weight penalty only for the target domain that blocked an endpoint.
- Reuses transport sessions and supports per-endpoint rate limits.
- Escalates cooldown after repeated failures and recovers endpoints automatically.
- Exposes pool statistics, structured logging, and manual endpoint controls.

## Requirements and installation

The module requires Go 1.24 or newer. From an existing Go module, run:

```bash
go get github.com/cpouldev/go-proxator
```

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/cpouldev/go-proxator"
)

func main() {
	client, err := proxator.New(proxator.Config{
		Pools: []proxator.PoolConfig{{
			Name:            "main",
			Endpoints:       []string{"http://user:pass@gate.example.net:8000"},
			SessionPoolSize: 1,
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Get(ctx, "main", "https://example.com")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Status, len(resp.Body))
}
```

Replace the sample proxy URL with your provider's endpoint. To try it without writing a file, run the maintained example against a real proxy; `TARGET_URL` is optional and defaults to `https://example.com`:

```bash
PROXY_URL='http://user:pass@proxy.example:8080' go run ./examples/basic
```

See [`examples/basic/main.go`](./examples/basic/main.go) for the complete setup, timeout, request, and cleanup flow.

## Configure endpoints

Every `PoolConfig` needs a unique, non-empty `Name` and at least one complete proxy URL in `Endpoints`, such as `http://user:pass@gate.example.net:8000`.

For providers that encode sticky sessions in the username, use `StickyEndpoints`. Its default username format is `%s-%d`, producing `user-1`, `user-2`, and so on. Set `UsernameFormat` to match your provider, and set `Scheme` when it is not `http`. The runnable [`ExampleStickyEndpoints`](./example_test.go) demonstrates authenticated SOCKS5 endpoints.

Proxy scheme support depends on the selected transport. Do not commit real proxy credentials; use environment variables or a secret manager.

## Use one or multiple pools

Pool names can represent providers, regions, or proxy tiers. Pass the configured name to `Get`, `Post`, `Do`, and other request methods. When exactly one pool is configured, those methods may receive `""` and resolve that pool automatically; `PoolConfig.Name` itself is still required.

The [`ExampleClient_multiplePools`](./example_test.go) example routes requests through separate `eu` and `us` pools.

## Choose a transport

| Configuration | Proxy schemes | Use when |
|---|---|---|
| `SessionFactory: nil` | HTTP, HTTPS, SOCKS5 | You want the default Azure TLS adapter with a Chrome fingerprint. |
| `SessionFactory: proxator.HTTPFactory{}` | HTTP, HTTPS | Standard `net/http` behavior is sufficient. |
| Custom `SessionFactory` and `Session` | Implementation-defined | You need another protocol, client, or test double. |

The client creates `SessionPoolSize` sessions for each endpoint during `New` and closes owned sessions in `Client.Close`. Always defer `client.Close()` after successful construction.

## Routing, retries, and blocking

Endpoint selection combines success rate and average latency, with a warmup period for new endpoints. Blocking responses and recognized proxy errors lower only that endpoint's weight for the target domain. Repeated failures move the endpoint into an escalating cooldown; a later success resets the escalation.

`RetryConfig.MaxAttempts` defaults to 3 total attempts, including the initial request. The pool selects an endpoint again before each retry. Retries have no separate elapsed-time budget, so bound the whole operation with `context.WithTimeout` or a deadline. Cancellation stops further attempts; custom `RequestFunc` callbacks must pass that context to `Session.Do`, and custom sessions must honor it.

## Common pool settings

| Setting | Default | Behavior |
|---|---:|---|
| `SessionPoolSize` | `15` | Concurrent reusable sessions per endpoint; also the rate-limit burst size. |
| `RequestsPerSecond` | `0` | Sustained per-endpoint limit; zero disables limiting. |
| `DomainBlockTTL` | `5m` | How long a target-domain penalty remains active. |
| `DomainBlockPenalty` | `0.1` | Weight multiplier for a recently blocked endpoint/domain pair. |
| `HealthCheckTimeout` | `15s` | Timeout for one enabled health check. |

Health checks stay disabled unless `HealthCheckInterval` is positive. When enabled, `HealthCheckURL` is required; choose an endpoint you control or are comfortable polling.

## Send requests

Use `Get`, `GetWithHeaders`, `Post`, or `PostWithHeaders` for common requests; these methods derive the target domain from the URL. Use `DoForDomain` with a `RequestFunc` when a custom request should retain domain-specific penalties, or `Do` when no target domain is available. The complete [`ExampleClient_Do`](./example_test.go) shows a custom POST with ordered headers.

The built-in transports send `string`, `[]byte`, and `io.Reader` bodies as raw data and JSON-encode other supported values. Recreate one-shot readers inside the callback when a request can be retried.

When a blocking response exhausts the configured attempts, these methods return **both** that last response and a non-nil error, so inspect both values:

```go
resp, err := client.Get(ctx, "main", targetURL)
if err != nil {
	if resp != nil {
		log.Printf("giving up on %s, last status %s", targetURL, resp.Status)
	}
	return err
}
```

## Observe and control pools

`Client.Stats()` reports alive, cooldown, and dead counts plus per-endpoint latency, success rate, session availability, and cooldown tier. Operational events use `Config.Logger`, which defaults to `slog.Default()`.

Use `MarkEndpointDead(pool, index)` to retire an endpoint and `ResetEndpoint(pool, index)` to restore it. Endpoint indexes are available in `Stats`; see [`ExampleClient_Stats`](./example_test.go).

## Repository layout

```text
.
├── client.go, pool.go, endpoint.go   # routing, pooling, and endpoint lifecycle
├── config.go, detector.go, transient.go
├── transport*.go                     # session contracts and built-in adapters
├── doc.go, example_test.go           # package docs and runnable examples
├── examples/basic/                   # executable getting-started example
├── internal/domain/                  # per-domain block tracking and tests
└── .github/workflows/ci.yml          # build, test, vet, format, and lint checks
```

The exported API lives in the root package so that every type, field, and method is documented on pkg.go.dev. Only genuinely private state lives under `internal/`.

## Development

```bash
go mod verify
go build ./...
go test ./...
go test -race -coverprofile=coverage.txt ./...
go vet ./...
test -z "$(gofmt -l .)"
golangci-lint run
```

CI runs dependency verification, formatting, vet, and race-enabled tests with coverage on Go 1.24 and 1.25. A separate job runs `golangci-lint`.

## Status

The API may change before v1. See the [Go Reference](https://pkg.go.dev/github.com/cpouldev/go-proxator) for exported types and methods.

## License

[MIT](./LICENSE.md)
