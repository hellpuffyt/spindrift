# Security

Spindrift is a network-facing server; the request parser is its trust
boundary. This document lists what it defends against, with the test that
demonstrates each claim, and what it does not.

## Defended (tested)

| Threat | Defence | Test |
|---|---|---|
| Header flooding / huge request lines | Byte budget over request line + headers (`max_header`, default 64 KiB); a single endless line is also bounded → 431 | `http1.TestHeaderTooLarge`, `server.TestRawProtocolBehaviours` |
| Body bombs | `max_body` (default 8 MiB) checked on `Content-Length` before reading and on decoded chunked bytes → 413 | `http1.TestChunkedBodyLimitAndTruncation`, `TestMalformedInputs` |
| Request smuggling | `Transfer-Encoding` wins and only a lone `chunked` is accepted; conflicting `Content-Length` values are rejected; bare LF and obs-fold rejected; `CONNECT` rejected | `http1.TestMalformedInputs` |
| Slowloris | Idle timeout while waiting for a request, read timeout while reading a body → 408 and close | `server.TestSlowlorisHeaderTimeout` |
| Path traversal in `files` | Path is unescaped and cleaned once at dispatch; the join is checked lexically and again after symlink resolution; anything outside the root is 404 | `server.TestStaticFilesAndTraversal` (`..`, `%2e%2e`, `..%2f`, mixed) |
| Serving non-files | Only regular files; directories map to `index.html`; devices/sockets are 404 | same |
| Unbounded pipelined bodies | Leftover body drained up to 1 MiB, else the connection is closed | `serveConn` |
| Header injection via templates | Response headers set through `Header.Set` are written verbatim; templates cannot inject CR/LF into the *request* fields they read (the parser rejects them), so `header "X" "{{query.q}}"` cannot split a response | `http1.TestMalformedInputs` (CR/LF in values) |
| Secrets in config | Conditions and templates can read `env.NAME`; the config file itself can stay secret-free | `config.TestTemplatesAndConditions` |
| Proxy loops and header leakage | Hop-by-hop headers stripped both ways; `Via` added; redirects from upstream are passed through, not followed | `server.TestProxyStreamsBothWays` |
| Data races | `go test -race ./...` is clean; the pipeline is swapped atomically | CI |
| Memory safety | Go; no `unsafe`, no cgo, no dependencies | CI grep |

## Not defended

- **TLS.** Spindrift speaks plaintext HTTP/1.1. Terminate TLS in front of
  it or wait for the roadmap item. Do not expose it directly on the
  internet with secrets in `require` conditions until then.
- **Distributed denial of service.** Per-connection goroutines and per-IP
  token buckets help against a single abusive client, not a botnet. There
  is no global connection cap yet (`ROADMAP.md`).
- **Regex denial of service.** `matches` uses Go's RE2 engine, which is
  linear-time, so pathological patterns are not a concern — but a huge
  pattern set on a hot path is still CPU.
- **`proxy` to arbitrary upstreams (SSRF).** The upstream URL comes from
  the config file, never from the request; the request path is appended
  verbatim after cleaning. If you point `proxy` at an internal service,
  every request path under that rule reaches it — scope the pattern.
- **Authentication beyond shared secrets.** `require` compares strings; it
  is not constant-time. Use it for internal tokens, not passwords.
- **Output size.** A `respond` template can grow with query input (up to the
  8 KiB target limit); `stream` runs until `count` or disconnect.

## Reporting

Open a security advisory on the GitHub repository or email the maintainer.
Include the config and a raw request reproducing the issue. Acknowledgement
within 7 days; only the latest release receives fixes.
