# Production readiness — spindrift

Audit of the `audit/production-readiness` branch. Evidence only; the
classification at the end is a proposal for the maintainer to confirm.

- Tested commit: `ab6087a2090d468efaa1e2b17a913ccc474e501a` (this file was
  added in the next commit and changes no code)
- Date: 2026-10-22
- Host: Apple M5 (10 cores, 16 GB), macOS 27.0.1, Go 1.27.3. `go.mod`
  declares `go 1.24`.

## Scope

A plaintext HTTP/1.1 server with its own request parser and response
writer (`http1/`, no `net/http` on the server side), a line-oriented
pipeline language (`config/`) and an engine (`server/`) with routing,
`respond`/`redirect`/`files`/`proxy`/`stream` handlers, `header`/`require`/
`ratelimit`/`cors` middleware, Prometheus metrics, access logs, hot reload
on SIGHUP and graceful drain on SIGINT/SIGTERM. Standard library only. See
[`ARCHITECTURE.md`](../ARCHITECTURE.md).

Out of scope (README "Limits"): TLS, HTTP/2/3, WebSocket, Range,
compression, directory listings, shared rate-limit store, write timeouts
and connection caps.

## Platforms and prerequisites

- Go 1.24+. Verified locally on macOS arm64 only.
- CI covers ubuntu/macOS/Windows unit tests plus an ubuntu job that serves,
  curls, benchmarks, reloads and drains; it has not yet run on this branch.
- The Dockerfile was not built (no Docker on the audit host).

## Clean-clone verification

Fresh clone of the branch into an empty scratch directory, README "Build,
test, run" followed literally:

| Command | Exit | Result |
|---|---|---|
| `go build -o spindrift ./cmd/spindrift` | 0 | |
| `go test ./...` | 0 | 39 passed, 0 failed, 0 skipped (top-level tests and subtests) |
| `go test -race ./...` | 0 | config, http1, server ok |
| `./spindrift check examples/spindrift.conf` | 0 | 14 rules listed |
| `./spindrift serve examples/spindrift.conf` | — | `spindrift listening on [::]:8080 (14 rules)` |
| `curl -s 'localhost:8080/users/7?q=hi'` | 0 | `{"id":"7","q":"hi","from":"::1"}` |
| `./spindrift bench http://localhost:8080/health -c 64 -n 50000` | 0 | 0 errors (56.5 k req/s with `log json` to a file) |
| `kill -HUP <pid>` then `curl localhost:8080/health` | 0 | `reloaded` logged, `ok` served |
| `kill -INT <pid>` | 0 | `draining` logged, process exited 0 |

## Commands executed (working clone)

| Command | Exit | Outcome |
|---|---|---|
| `go build ./...`, `go vet ./...`, `gofmt -l .` | 0 | clean |
| `go test -race -count=1 -v ./...` (baseline, before fixes) | 0 | 23 passed, 0 failed, 0 skipped |
| `go test -race -count=1 -v ./...` (after) | 0 | 39 passed, 0 failed, 0 skipped |
| `go test -race -count=5 ./...` | 0 | stable |
| `go run honnef.co/go/tools/cmd/staticcheck@latest ./...` (v0.8.2) | 0 | no findings |
| `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` (v1.9.0) | 0 | No vulnerabilities found |
| `go run github.com/zricethezav/gitleaks/v8@latest git --redact .` | 0 | no leaks in any commit |
| CI "real-execution" job replayed locally (serve, 9 curl checks, bench 20k, SIGHUP reload, SIGTERM drain with a stream in flight) | 0 | all steps passed |
| Benchmarks (README table), `log off`, 3 runs per route | 0 | `/health` 79.7–88.2 k req/s; `/users/7?q=hi` 73.6–78.6 k req/s; 0 errors |

Each new regression test was run against the pre-fix code and failed
there (the smuggled `SMUGGLED` response, the leaked symlink target, 51
metric series, header injection lines, accepted malformed framing).

## CI status

- Default branch (`main`): last run `success`.
- Audit branch: pending until pushed.

## Bugs found and fixed

| # | Severity | Bug | Fix | Regression test |
|---|---|---|---|---|
| 1 | High | **Request smuggling**: a chunked body whose framing broke, or that exceeded `max_body`, while being drained left the connection open; the bytes after the failure were parsed and served as a new request. | Close the connection when draining the body returns any error. | `server.TestMalformedChunkedBodyClosesConnection` |
| 2 | High | Chunk-size parsing accepted `+3`, ` 3`, bare-LF lines and unbounded line length; trailer sections were unbounded (memory), accepted bare LF and obs-fold, and silently skipped invalid names. | Strict hex chunk-size, CRLF-only bounded lines, bounded trailer section with header rules. | `http1.TestSmugglingVectorsAreRejected` |
| 3 | Medium | TE+CL request did not close the connection (RFC 9112 §6.1 MUST); TE on HTTP/1.0 accepted; duplicate `Host` accepted; control bytes accepted in the target and field values. | Close / reject as specified. | `http1.TestHeaderLevelSmugglingVectors`, `http1.TestTEPlusCLClosesConnection`, `server.TestTEPlusCLRequestClosesAfterResponse`, `server.TestProtocolRejectionsOnTheWire` |
| 4 | Medium | Trailers were merged into the request header map, so a trailer could pose as a header (RFC 9110 §6.5.1). | `Request.Trailer`. The existing chunked test's expectation was changed accordingly (justified in the test). | `http1.TestTrailersAreNotMergedIntoHeaders`, `http1.TestChunkedBodyWithTrailersAndExtensions` |
| 5 | High | **Response splitting**: templated `header`/`redirect` values carry decoded query data (`%0D%0A`) and were written verbatim; `SECURITY.md` claimed otherwise. | Writer replaces CR/LF/NUL in values, drops non-token names. | `http1.TestResponseHeaderInjectionIsNeutralised`, `server.TestTemplatedHeadersCannotSplitResponses` |
| 6 | High | `files`: a symlinked `index.html` inside a directory served a file outside the root (README claimed symlink safety; no test existed). | Resolve and re-check containment of `index.html`. | `server.TestFilesRefuseSymlinksOutOfRoot` |
| 7 | Medium | Metrics used the raw client method as a label: unbounded series (memory) from random methods. | Non-standard methods → `OTHER`. | `server.TestMetricsMethodLabelIsBounded` |
| 8 | Medium | `proxy` forwarded the raw request-target although routing and middleware evaluated the cleaned path; `Connection`-listed headers were forwarded. | Forward the routed path (re-escaped, trailing slash kept); strip listed headers both ways. | `server.TestProxyForwardsRoutedPathAndStripsConnectionListedHeaders` |
| 9 | Medium | README's token gate `require header.X-Token == env.API_TOKEN` passes every request without the header when `API_TOKEN` is unset (`"" == ""`). | Documented form guarded with `exists`; `serve`/`check` warn on empty env vars read by `require`. | `server.TestDocumentedTokenGateFailsClosedWhenEnvUnset`, `config.TestRequireEnvListsGateVariables` |
| 10 | Low | Default `listen` (when omitted) bound every interface. | `127.0.0.1:8080`. | `config.TestDefaultListenIsLoopback` |
| 11 | Low | `Expect: 100-continue` honoured on HTTP/1.0; leading CRLF before a request-line rejected; absolute-form without a path left Host unset. | Per RFC 9110 §10.1.1 / RFC 9112 §2.2. | `http1.TestLeadingEmptyLinesAndExpectOn10` |
| 12 | Docs | Benchmark table and hero output came from a machine the audit could not reproduce; "idle/read/header timeouts" (there is no separate header timeout); "30 protocol tests". | Re-measured here with command and hardware; wording corrected; limits added. | — |

## Security review summary

- Parser and framing: see bugs 1–4 and 11. Slowloris on headers is bounded
  by `idle_timeout` (`server.TestSlowlorisHeaderTimeout`).
- Path traversal: five encoded forms plus three symlink forms → 404.
- SSRF: upstreams come only from the config file.
- Injection: response splitting fixed (bug 5); JSON access logs are
  encoded; text logs print the target, which can no longer contain
  control bytes.
- Resource exhaustion: header/body/target budgets, bounded chunk lines and
  trailers, bounded metrics labels, rate-limiter map capped at 100k keys.
  Not addressed: write timeout, global connection cap (documented).
- Secrets: gitleaks over all history found nothing.
- Workflow permissions: `contents: read`. Action references are tags (not
  SHAs); the CI file was not changed in this audit.

## Known limitations and unresolved risks

- No write timeout / connection cap (slow-read clients hold goroutines).
- No separate header timeout shorter than `idle_timeout`.
- `require` string comparison is not constant-time (documented).
- Trailers are not forwarded by `proxy`.
- Windows, Linux and Docker were not exercised locally.

## Acceptance checklist

| Item | Expected behaviour | Command / test | Result | Actual | Reference |
|---|---|---|---|---|---|
| Strict request parsing | Malformed input → 400/431/413/414/505 and close | `http1.TestMalformedInputs`, `server.TestRawProtocolBehaviours`, `server.TestProtocolRejectionsOnTheWire` | pass | | `http1/http1.go` |
| Smuggling resistance | TE+CL closes; broken/oversized chunked body never yields a second request | `server.TestMalformedChunkedBodyClosesConnection`, `server.TestTEPlusCLRequestClosesAfterResponse`, `http1.TestSmugglingVectorsAreRejected` | pass | no `SMUGGLED` response | `server/server.go` `serveConn` |
| Oversized headers | 431, endless line bounded | `http1.TestHeaderTooLarge` | pass | | |
| Slowloris | 408 and close at `idle_timeout` | `server.TestSlowlorisHeaderTimeout` | pass | | |
| Pipelining and keep-alive | Ordered responses, connection reuse under load | `server.TestRawProtocolBehaviours`, `server.TestLoadManyConcurrentKeepAliveClients` | pass | | |
| Static files | Correct type/index/304; traversal and symlinks → 404 | `server.TestStaticFilesAndTraversal`, `server.TestFilesRefuseSymlinksOutOfRoot` | pass | | |
| Proxy | Streams both ways, strips hop-by-hop, forwards routed path | `server.TestProxyStreamsBothWays`, `server.TestProxyForwardsRoutedPathAndStripsConnectionListedHeaders` | pass | | |
| No response splitting | Templated headers cannot add lines | `server.TestTemplatedHeadersCannotSplitResponses` | pass | | |
| Hot reload / drain | New routes after SIGHUP; in-flight stream finishes on SIGTERM | `server.TestReload…`, `server.TestGracefulShutdown…`, CI job replay | pass | 5/5 stream ticks after SIGTERM | `cmd/spindrift` |
| Fresh clone | README steps work | clean-clone table | pass | | `README.md` |
| Static analysis / deps | vet, staticcheck, govulncheck clean | above | pass | | |

## Classification proposal

**Partially Verified** — every documented feature is tested and passes on
macOS, and the protocol defects found were fixed with regression tests,
but the Linux/Windows CI matrix and the Docker image have not run on this
branch, and the server has no write timeout or connection cap, which the
README now states as limits. Proposed for "Production-Ready for Its
Documented Scope" once CI is green and the maintainer accepts those limits.
