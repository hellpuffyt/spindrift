# Security

Spindrift is a network-facing server; the request parser is its trust
boundary. This document lists what it defends against, with the test that
demonstrates each claim, and what it does not.

## Defended (tested)

| Threat | Defence | Test |
|---|---|---|
| Header flooding / huge request lines | Byte budget over request line + headers (`max_header`, default 64 KiB); a single endless line is also bounded → 431 | `http1.TestHeaderTooLarge`, `server.TestRawProtocolBehaviours` |
| Body bombs | `max_body` (default 8 MiB) checked on `Content-Length` before reading and on decoded chunked bytes → 413 | `http1.TestChunkedBodyLimitAndTruncation`, `TestMalformedInputs` |
| Request smuggling | `Transfer-Encoding` wins, only a lone `chunked` is accepted, and a TE+CL request closes the connection after its response; TE on HTTP/1.0, conflicting `Content-Length` values, duplicate `Host`, bare LF (headers, chunk lines, trailers), obs-fold, signed/padded/over-long chunk sizes and `CONNECT` are rejected; a body whose framing breaks or exceeds `max_body` closes the connection instead of parsing the rest as a new request | `http1.TestMalformedInputs`, `http1.TestSmugglingVectorsAreRejected`, `http1.TestHeaderLevelSmugglingVectors`, `http1.TestTEPlusCLClosesConnection`, `server.TestMalformedChunkedBodyClosesConnection`, `server.TestTEPlusCLRequestClosesAfterResponse` |
| Trailer spoofing | Chunked trailers go to `Request.Trailer`, never into the header map (RFC 9110 §6.5.1); the trailer section is byte-bounded | `http1.TestTrailersAreNotMergedIntoHeaders` |
| Slowloris | The idle-timeout deadline also covers reading the request line and headers; read timeout while reading a body → 408 and close | `server.TestSlowlorisHeaderTimeout` |
| Path traversal in `files` | Path is unescaped and cleaned once at dispatch; the join is checked lexically and again after symlink resolution (including a directory's `index.html`); anything outside the root is 404 | `server.TestStaticFilesAndTraversal` (`..`, `%2e%2e`, `..%2f`, mixed), `server.TestFilesRefuseSymlinksOutOfRoot` |
| Serving non-files | Only regular files; directories map to `index.html`; devices/sockets are 404 | same |
| Unbounded pipelined bodies | Leftover body drained up to 1 MiB, else the connection is closed | `serveConn` |
| Header injection via templates | Templates can read decoded query parameters, which may contain CR/LF. The response writer replaces CR, LF and NUL in header values with spaces and drops header names that are not tokens, so `header "X" "{{query.q}}"` or `redirect 302 "{{query.next}}"` cannot add header lines or split a response | `http1.TestResponseHeaderInjectionIsNeutralised`, `server.TestTemplatedHeadersCannotSplitResponses` |
| Secrets in config | Conditions and templates can read `env.NAME`; the config file itself can stay secret-free. An unset variable reads as `""`, so token gates must be written `header.X exists && header.X == env.NAME`; `serve` and `check` warn when a `require` reads an empty variable | `config.TestTemplatesAndConditions`, `config.TestRequireEnvListsGateVariables`, `server.TestDocumentedTokenGateFailsClosedWhenEnvUnset` |
| Proxy loops and header leakage | Hop-by-hop headers, and headers named in `Connection`, stripped both ways; `Via` added; redirects from upstream are passed through, not followed | `server.TestProxyStreamsBothWays`, `server.TestProxyForwardsRoutedPathAndStripsConnectionListedHeaders` |
| Proxy path confusion | The upstream receives the cleaned path that routing and middleware saw (re-escaped, trailing slash kept), not the raw request-target | same |
| Metrics memory growth | Non-standard methods are counted under `OTHER`, so clients cannot mint new series | `server.TestMetricsMethodLabelIsBounded` |
| Accidental exposure | Without `listen`, the server binds `127.0.0.1:8080` | `config.TestDefaultListenIsLoopback` |
| Data races | `go test -race ./...` is clean; the pipeline is swapped atomically | CI |
| Memory safety | Go; no `unsafe`, no cgo, no dependencies | CI grep |

## Not defended

- **TLS.** Spindrift speaks plaintext HTTP/1.1. Terminate TLS in front of
  it or wait for the roadmap item. Do not expose it directly on the
  internet with secrets in `require` conditions until then.
- **Distributed denial of service.** Per-connection goroutines and per-IP
  token buckets help against a single abusive client, not a botnet. There
  is no global connection cap yet (`ROADMAP.md`).
- **Slow readers.** There is no write timeout: a client that stops reading
  holds its connection goroutine until it disconnects; on shutdown the
  drain gives up after 10 s.
- **Regex denial of service.** `matches` uses Go's RE2 engine, which is
  linear-time, so pathological patterns are not a concern — but a huge
  pattern set on a hot path is still CPU.
- **`proxy` to arbitrary upstreams (SSRF).** The upstream URL comes from
  the config file, never from the request; the cleaned request path is
  appended. If you point `proxy` at an internal service,
  every request path under that rule reaches it — scope the pattern.
- **Authentication beyond shared secrets.** `require` compares strings; it
  is not constant-time. Use it for internal tokens, not passwords.
- **Output size.** A `respond` template can grow with query input (up to the
  8 KiB target limit); `stream` runs until `count` or disconnect.

## Reporting

Open a private security advisory at
https://github.com/hellpuffyt/spindrift/security/advisories/new.
Include the config and a raw request reproducing the issue. Acknowledgement
within 7 days; only the latest release receives fixes.
