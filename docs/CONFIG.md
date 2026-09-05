# The pipeline language

One directive per line. `#` starts a comment (outside quotes). Strings are
double-quoted with `\"`, `\\`, `\n`, `\t` escapes. Sizes accept `KB`, `MB`,
`GB`; durations use Go syntax (`500ms`, `5s`, `2m`).

## Globals

| Directive | Default | Meaning |
|---|---|---|
| `listen ADDR` | `:8080` | TCP address |
| `log json\|text\|off` | `json` | access log format on stdout |
| `max_header SIZE` | `64KB` | request line + headers budget → 431 |
| `max_body SIZE` | `8MB` | body limit (declared or decoded) → 413 |
| `idle_timeout DUR` | `60s` | waiting for the next request on a keep-alive connection |
| `read_timeout DUR` | `30s` | reading a request body → 408 |

## Rules

```
route      METHODS PATTERN -> ACTION
middleware METHODS PATTERN -> ACTION
```

- `METHODS`: `*` or a comma list (`GET,HEAD`). Case-insensitive.
- `PATTERN`: `/`-rooted; segments are literals or `:name` captures; a
  trailing `/*` captures the remainder; `*` alone matches everything.
- Rules are evaluated **in file order**. Every matching middleware runs;
  the first matching route answers. No route → 404.

## Route actions

| Action | Example |
|---|---|
| `respond STATUS [text\|json\|html] "BODY"` | `respond 200 json "{\"id\":\"{{param.id}}\"}"` — fixed-length body, `Content-Type` by kind |
| `redirect 3xx "URL"` | `redirect 301 "/new?from={{path}}"` |
| `files "DIR"` | serves `rest` under `DIR`; `index.html` for directories; GET/HEAD only |
| `proxy "http://host:port"` | forwards path + query; streams both ways; adds `X-Forwarded-For/Host/Proto`, `Via` |
| `stream every DUR "TEMPLATE" [count N]` | `text/event-stream`, one `data:` frame per tick; `count` omitted = until disconnect |

## Middleware actions

| Action | Example |
|---|---|
| `header "Name" "VALUE"` | sets a response header (templated) |
| `require CONDITION else STATUS` | short-circuits with `STATUS` (4xx/5xx) when the condition is false |
| `ratelimit N/s\|N/m [burst M]` | token bucket per client IP; 429 + `Retry-After: 1` |
| `cors "ORIGIN"` | `*` or an exact origin; answers `OPTIONS` preflight with 204 |

## Operands

Usable in conditions and inside `{{ }}` in templates:

| Operand | Value |
|---|---|
| `"literal"` | the string |
| `method`, `path`, `rawquery`, `remote` | request method, cleaned path, raw query string, client IP |
| `header.Name` | request header (case-insensitive) |
| `query.key` | first value of a query parameter |
| `param.name` | route capture (`:name`) |
| `env.NAME` | process environment variable |
| `now` | RFC 3339 UTC timestamp |
| `unix` | seconds since the epoch |

## Conditions

`OPERAND OP [OPERAND]`, joined by `&&` / `||`, evaluated left to right
without precedence or parentheses.

| Op | True when |
|---|---|
| `==`, `!=` | string equality |
| `contains`, `startswith`, `endswith` | substring tests |
| `matches "RE"` | Go/RE2 regular expression (literal only) |
| `exists` | the left operand is non-empty |

## Built-in endpoints

- `GET /_spindrift/health` → `{"status":"ok"}`
- `GET /_spindrift/metrics` → Prometheus text format

## Signals

- `SIGHUP` — re-read the file; on parse error the old pipeline stays.
- `SIGINT`, `SIGTERM` — stop accepting, close idle connections, finish
  in-flight responses (10 s), exit.
