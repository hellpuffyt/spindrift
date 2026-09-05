# Architecture

```
            ┌──────────────┐   Parse    ┌──────────────┐  Reload (atomic swap)
 file.conf ─► config.Parse ├───────────►│ *config.Config├──────────┐
            └──────────────┘            └──────────────┘          ▼
                                                        ┌────────────────────┐
  TCP accept ──► goroutine per conn ──► http1.ReadRequest│ compiled pipeline  │
                    │                        │          │  rules[] (in order)│
                    │  ResponseWriter ◄──────┘          │  limiters (by line)│
                    │        ▲                          └─────────┬──────────┘
                    │        │ dispatch: middleware… then first route
                    │        └── respond | redirect | files | proxy | stream
                    ▼
      metrics.observe + JSON/text access log ──► drain body ──► next request or close
```

## Packages

- `http1` — protocol only. No goroutines, no sockets: `ReadRequest(*bufio.Reader, Limits)`
  and `ResponseWriter` over a `*bufio.Writer`. Testable with strings.
- `config` — the pipeline language: tokenizer (quoted strings with escapes,
  `#` comments), directives, actions, conditions, templates. Pure functions;
  `Env` is the only thing an expression can see.
- `server` — everything with side effects: listener, connections, router,
  handlers, middleware, rate limiter, metrics, logging, reload, drain.
- `cmd/spindrift` — `serve`, `check`, `bench`.

## Connection lifecycle

1. `Serve` accepts and spawns `serveConn` with a `connState` (an `idle` flag).
2. Loop: load the current pipeline pointer; if draining, return. Mark idle,
   set the **idle timeout** deadline, `ReadRequest`. Unmark idle. A parse
   error writes a one-line error response with the mapped status (400 / 413
   / 414 / 431 / 505, or 408 on deadline) and closes.
3. Set the **read timeout** deadline for the body. Send `100 Continue` if
   asked. `dispatch` runs the pipeline. `Finish` terminates the body
   (last chunk) and flushes.
4. Record metrics and the access log. Drain up to 1 MiB of unread body so the
   next pipelined request parses; a larger leftover closes the connection.
5. Close if the request or the response framing demands it; else loop.

There is one goroutine per connection and no per-request goroutine. Buffers
are 16 KiB each way.

## Router

Rules are evaluated in file order. Pattern matching is segment-wise:
literal, `:param` (captures one segment), and a trailing `/*` (captures the
rest into `rest`). `*` alone matches everything. Method lists are exact
(`GET,HEAD`) or `*`.

`dispatch` unescapes and `path.Clean`s the request path once (so `/a/../b`
and `%2e%2e` are normalised before any rule sees them — traversal is
defeated here and again in `files`), then walks the rules: each matching
**middleware** runs and may short-circuit (require/ratelimit/cors-preflight);
the first matching **route** handles the request and ends the walk. No
match → 404.

`/_spindrift/health` and `/_spindrift/metrics` are handled before the rules.

## Reload and drain

The compiled pipeline lives in an `atomic.Pointer`. `Reload` builds a new
one — reusing rate-limit buckets from old rules with the same line number,
rate and burst — and stores it. Connections pick it up on their next
request; a request in flight keeps the pointer it loaded. Nothing is locked,
nothing is dropped.

`Shutdown` sets `closing`, closes the listener, and every 10 ms closes
connections whose `idle` flag is set (waiting between requests) until the
wait group drains or the context expires; then it closes the rest. A
connection that finishes a request while draining returns instead of
waiting for the next one.

## Handlers

- **respond** renders the template with the request `Env`, sets
  `Content-Type` by kind, fixed `Content-Length`.
- **files** resolves `rest` under the root, checks containment lexically,
  then again after `EvalSymlinks`, serves `index.html` for directories, sets
  type by extension, honours `If-Modified-Since`, 405 for non-GET/HEAD.
- **proxy** builds an outgoing `net/http` request (the stdlib client is used
  *upstream* only), strips hop-by-hop headers, adds `X-Forwarded-*`, streams
  the response body back chunk by chunk with `Flush` after each read. 502 on
  connect failure.
- **stream** writes `text/event-stream` frames every `Every` until `Count`
  or until a write fails (client gone).
- **ratelimit** is a token bucket per client IP with a crude 100k-key cap
  (`ponytail:` LRU if a real deployment needs it).

## Metrics

`spindrift_requests_total{method,route,status}`,
`spindrift_request_duration_seconds` histogram per route (11 buckets from
1 ms to 5 s), `spindrift_connections_active`, `spindrift_connections_total`.
Rendered on demand in Prometheus text format.

## Deliberate simplifications

| Simplification | Upgrade path |
|---|---|
| One goroutine per connection, blocking reads | Fine to ~10k conns; an epoll-style loop is the next step, not needed yet |
| Whole pipeline re-compiled on reload | Pipelines are small; diffing is not worth the code |
| `files` reads whole file through `io.Copy` | `sendfile` via `net.TCPConn.ReadFrom` when the writer is unbuffered |
| Access log is a synchronous write | Buffered/async logger if logging becomes the bottleneck |
