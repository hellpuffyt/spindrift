# Changelog

Format: [Keep a Changelog](https://keepachangelog.com). Versions: SemVer.
Changes to the pipeline language are called out explicitly.

## [Unreleased]

## [0.1.0] — 2026-09-06

First release.

### Added
- `http1`: strict HTTP/1.1 request parser (Content-Length and chunked bodies,
  trailers, pipelining, keep-alive rules, 100-continue, byte budgets) and
  response writer (fixed, chunked, close-delimited, HEAD).
- Pipeline language: `listen`, `log`, `max_body`, `max_header`,
  `read_timeout`, `idle_timeout`, `route`, `middleware`; actions `respond`,
  `redirect`, `files`, `proxy`, `stream`, `header`, `require`, `ratelimit`,
  `cors`; conditions and `{{…}}` templates over request fields and env.
- Engine: per-connection handling with timeouts, router with `:param` and
  `/*`, traversal-safe static files, streaming reverse proxy, SSE streams,
  per-IP token buckets, Prometheus metrics, JSON/text logs, atomic
  `SIGHUP` reload, graceful drain.
- CLI: `serve`, `check`, `bench`.
