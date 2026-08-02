# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing has been tagged yet; this section describes the first release.

### Added

- Named rotating proxy pools with performance-weighted selection, warmup, per-domain block memory, and escalating self-healing cooldowns.
- Reusable per-endpoint sessions, rate limiting, optional health checks, pool statistics, and manual endpoint retirement and recovery.
- Configurable retry attempts, backoff, cooldown limits, domain-block lifetime, and domain-specific weight penalties.
- Transport-neutral request, response, session, and factory contracts, with certificate-verifying Azure TLS fingerprinting by default and a standard-library `HTTPFactory` alternative.
- HTTP helpers accepting ordered-header input, block detection, transient-error classification, and context-aware request cancellation.
- HTTP, HTTPS, and SOCKS5 proxy support through the default Azure TLS adapter; `HTTPFactory` supports HTTP and HTTPS proxy URLs.
- Runnable package examples, deterministic local proxy integration tests, race/coverage CI across Go 1.24 and 1.25, and golangci-lint checks.

### Behavior notes

- A nil field in `DetectorConfig` or `TransientConfig` selects the package default; an empty non-nil field disables that check. Clearing a `Default…` set therefore disables it rather than falling back to a built-in list.
- `New` returns an error, rather than panicking, when a replaced `Default…` pattern fails to compile.
- `IsTransient` reads the package defaults at call time, so it always agrees with `NewTransientClassifier`.
- Endpoint URL validation errors report the parse failure without echoing the endpoint URL, which may embed credentials.

[Unreleased]: https://github.com/cpouldev/go-proxator/commits/main
