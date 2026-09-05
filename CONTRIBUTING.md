# Contributing

## Ground rules

- **Standard library only.** `go.mod` has no requirements and that is a
  feature; CI fails if one appears.
- **No `net/http` on the server path.** It is used as an upstream *client*
  in `proxy` and as a test client; the parser and writer in `http1` are the
  server.
- Every parser change gets a case in `http1_test.go` (a valid one and the
  invalid neighbour). Every language change gets a parse test and an error
  test with a line number. Every handler/middleware change gets an
  integration test in `server_test.go`.
- `gofmt -l .` empty, `go vet ./...` clean, `go test -race ./...` green.

## Workflow

```
git clone https://github.com/hellpuffyt/spindrift
cd spindrift
go test -race ./...
go build -o spindrift ./cmd/spindrift && ./spindrift serve examples/spindrift.conf
```

## Where things live

| Want to… | Look in |
|---|---|
| Change how requests are parsed or responses framed | `http1/http1.go` |
| Add a directive, action, operator or template field | `config/config.go` (`Parse`, `parseAction`, `parseCond`, `parseOperand`), then `docs/CONFIG.md` |
| Add a handler or middleware | `server/server.go` (`route`, `middleware`), plus the action in `config` |
| Change connection handling, reload, drain | `server/server.go` (`serveConn`, `Reload`, `Shutdown`) |
| CLI | `cmd/spindrift/main.go` |

## Reporting bugs

A config file plus `curl -v` (or a raw request for protocol issues) is the
ideal report. Security issues: see `SECURITY.md`.
