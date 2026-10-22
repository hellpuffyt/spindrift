# Changelog

Format: [Keep a Changelog](https://keepachangelog.com). Versions: SemVer.
Changes to the pipeline language are called out explicitly.

## [Unreleased]

### Security
- Request smuggling: a chunked body whose framing broke (or exceeded
  `max_body`) while being drained left the connection open and the
  remaining bytes were parsed as a new request. The connection now closes.
- RFC 9112 strictness: chunk sizes must be plain hex (no sign, no leading
  whitespace) on CRLF-terminated, bounded lines; trailer sections are
  bounded, CRLF-only and fold-free; TE+CL requests close the connection;
  TE on HTTP/1.0, duplicate `Host` and control bytes in the target or field
  values are rejected.
- Trailers are exposed as `Request.Trailer` and no longer merged into the
  request headers (behaviour change for embedders).
- Response header values have CR/LF/NUL replaced, so templated `header`
  and `redirect` values cannot split responses.
- `files`: a symlinked `index.html` could serve a file outside the root.
- `proxy` forwards the cleaned, routed path instead of the raw target and
  strips `Connection`-listed headers.
- Metrics method labels are bounded (`OTHER`).
- Default `listen` is now `127.0.0.1:8080` (was `:8080`).
- `serve`/`check` warn when a `require` condition reads an empty env var;
  the README's token-gate example now guards with `exists`.

### Fixed
- `Expect: 100-continue` is ignored for HTTP/1.0; empty lines before a
  request-line are skipped; absolute-form targets without a path set Host.

### Changed
- README benchmark numbers re-measured on the audit machine (the previous
  table came from a machine this audit could not reproduce).

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
