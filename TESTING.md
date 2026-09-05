# Testing

`go test ./...` runs in a few seconds; `go test -race ./...` is part of CI.

| Suite | File | What it proves |
|---|---|---|
| Protocol | `http1/http1_test.go` | Request-line and header parsing with canonical keys; pipelined requests on one reader; chunked bodies with extensions and trailers, then the next request; 14 malformed inputs each mapped to the right error; header budget including an endless line; keep-alive rules for 1.0/1.1, `Connection: close`, `Expect`, absolute-form targets; body limits and truncation for both framings; response writer: fixed, chunked, `HEAD`, HTTP/1.0 close-delimited, empty finish. |
| Pipeline language | `config/config_test.go` | Full sample parse incl. globals, method lists, rate/burst, stream every/count, line numbers; template rendering and every condition operator incl. `&&`/`||`; 17 error cases each with the right message on the right line; tokenizer escapes and comments inside quotes. |
| Engine (integration) | `server/server_test.go` | Real TCP on a random port with `net/http` clients **and** raw sockets. Routing, params, headers, 404/405/HEAD/redirect; `require` and token-bucket `ratelimit` (burst then 429 then refill); static files with content type, index, `If-Modified-Since`, 405 for POST, and five traversal forms via raw requests; reverse proxy both directions incl. `X-Forwarded-For`, `Via`, 502 on dead upstream; SSE stream is chunked and the connection is reusable afterwards; pipelining order, HTTP/1.0, `100 Continue`, chunked request drain, 400/431/505/413/400 status mapping, metrics output; reload swaps routes while a stream stays alive; graceful shutdown closes the listener yet finishes an in-flight 4-event stream; 32 clients × 200 keep-alive requests with zero failures and connection reuse asserted via metrics; slowloris → 408. |

## Real execution in CI

The workflow builds the binary, runs `check` on the example config, starts
`serve`, hits it with `curl` (JSON route, 401 gate, static file, traversal
404, metrics), runs `bench` for 20k requests and asserts zero errors, sends
`SIGHUP` after editing the config and checks the new route answers, then
`SIGTERM` and checks a clean exit.

## Benchmarks

`spindrift bench URL -c N -n N` prints throughput and p50/p90/p99/max. It is
a keep-alive generator on `net/http`; run it on a separate machine for
numbers you would quote.

## Writing a test

- Protocol behaviour → a string in `http1_test.go` through `parse()`.
- Language behaviour → `config_test.go`; error tests assert the message
  substring and the line.
- Anything observable on the wire → `server_test.go`; use `start(t, cfg)`
  for a server on a random port and `rawRequest` when the Go client would
  normalise what you're testing (paths, framing).
