# Roadmap

Each item is one reviewable PR.

## 0.2 — production edges

- [ ] TLS listener (`listen :443 tls "cert.pem" "key.pem"`) via `crypto/tls`.
- [ ] Global connection cap with 503 + `Retry-After` when exceeded.
- [ ] `Range` requests and `ETag` for `files`.
- [ ] `gzip` middleware for text types (stdlib `compress/gzip`).
- [ ] `spindrift bench` JSON output for CI regression tracking.

## 0.3 — language

- [ ] `rewrite` action (change the path before continuing the walk).
- [ ] `set` middleware to define request-scoped variables for later templates.
- [ ] Response-side conditions (`respond … if status == 404`) for error pages.
- [ ] `include "other.conf"`.

## 0.4 — protocols

- [ ] WebSocket upgrade passthrough for `proxy`.
- [ ] HTTP/2 over TLS (h2) using the stdlib `golang.org/x/net/http2` *only*
  for the frame layer, keeping the pipeline unchanged — or a native frame
  decoder if the dependency rule holds.

## Someday

- Zero-downtime binary upgrade by listener fd inheritance.
- Lua or Starlark handlers behind a build tag for people who need code.

## Non-goals

- Becoming a framework. Handlers stay declarative; write a service behind
  `proxy` when you need code.
- HTTP/3.
