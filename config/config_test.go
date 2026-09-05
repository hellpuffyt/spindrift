package config

import (
	"strings"
	"testing"
	"time"
)

const sample = `
listen :9090
log text
max_body 2MB
read_timeout 5s

middleware * /*        -> header "Server" "Spindrift/{{env.VERSION}}"   # comment
middleware * /api/*    -> require header.X-Token == env.API_TOKEN && method != "TRACE" else 401
middleware * /api/*    -> ratelimit 50/s burst 100
route GET,HEAD /health -> respond 200 text "ok"
route GET /users/:id   -> respond 200 json "{\"id\":\"{{param.id}}\",\"q\":\"{{query.q}}\"}"
route GET /events      -> stream every 500ms "tick {{now}}" count 3
route * /static/*      -> files "./public"
route * /api/*         -> proxy "http://127.0.0.1:9000"
route GET /old         -> redirect 301 "/new?from={{path}}"
`

func TestParseSample(t *testing.T) {
	cfg, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9090" || cfg.Log != "text" || cfg.MaxBody != 2<<20 || cfg.ReadTimeout != 5*time.Second {
		t.Fatalf("globals: %+v", cfg)
	}
	if len(cfg.Rules) != 9 {
		t.Fatalf("rules: %d", len(cfg.Rules))
	}
	r := cfg.Rules[3]
	if r.Middleware || r.Methods[0] != "GET" || r.Methods[1] != "HEAD" || r.Pattern != "/health" || r.Action.Status != 200 || r.Action.Kind != "text" {
		t.Fatalf("health rule: %+v", r)
	}
	if cfg.Rules[2].Action.Rate != 50 || cfg.Rules[2].Action.Burst != 100 {
		t.Fatalf("ratelimit: %+v", cfg.Rules[2].Action)
	}
	if cfg.Rules[5].Action.Every != 500*time.Millisecond || cfg.Rules[5].Action.Count != 3 {
		t.Fatalf("stream: %+v", cfg.Rules[5].Action)
	}
	if cfg.Rules[0].Line != 7 {
		t.Fatalf("line numbers: %d", cfg.Rules[0].Line)
	}
}

func env() *Env {
	return &Env{
		Method: "GET", Path: "/users/7", RawQuery: "q=hi", Remote: "10.0.0.1",
		Params: map[string]string{"id": "7"},
		Header: func(k string) string {
			if k == "X-Token" {
				return "secret"
			}
			return ""
		},
		Query: func(k string) string {
			if k == "q" {
				return "hi"
			}
			return ""
		},
		Getenv: func(k string) string {
			if k == "API_TOKEN" {
				return "secret"
			}
			return "1.0"
		},
		Now: func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) },
	}
}

func TestTemplatesAndConditions(t *testing.T) {
	cfg, _ := Parse(sample)
	e := env()
	if got := cfg.Rules[4].Action.Tpl.Render(e); got != `{"id":"7","q":"hi"}` {
		t.Fatalf("template: %s", got)
	}
	if got := cfg.Rules[0].Action.Tpl.Render(e); got != "Spindrift/1.0" {
		t.Fatalf("env template: %s", got)
	}
	if got := cfg.Rules[5].Action.Tpl.Render(e); got != "tick 2026-09-06T12:00:00Z" {
		t.Fatalf("now: %s", got)
	}
	if !cfg.Rules[1].Action.Cond.Eval(e) {
		t.Fatal("require should pass with matching token")
	}
	e.Header = func(string) string { return "wrong" }
	if cfg.Rules[1].Action.Cond.Eval(e) {
		t.Fatal("require should fail with wrong token")
	}
	e = env()
	c, err := parseCond(strings.Fields(`path startswith "/users" || remote == "x"`))
	if err != nil || !c.Eval(e) {
		t.Fatalf("or: %v", err)
	}
	c, _ = parseCond(strings.Fields(`path matches "^/users/[0-9]+$" && query.q exists`))
	if !c.Eval(e) {
		t.Fatal("matches && exists")
	}
	c, _ = parseCond(strings.Fields(`header.Nope exists`))
	if c.Eval(e) {
		t.Fatal("missing header does not exist")
	}
}

func TestParseErrorsCarryLineNumbers(t *testing.T) {
	cases := []struct{ src, want string }{
		{"bogus 1", "unknown directive"},
		{"route GET /x", "expected `route"},
		{"route GET x -> respond 200 \"a\"", "must start with /"},
		{"route GET /a/*/b -> respond 200 \"a\"", "trailing /*"},
		{"route GET /a -> respond 999 \"a\"", "status code"},
		{"route GET /a -> respond 200 xml \"a\"", "text, json or html"},
		{"route GET /a -> header \"a\" \"b\"", "middleware action"},
		{"middleware * /a -> respond 200 \"x\"", "route action"},
		{"middleware * /a -> require header.X == else 401", "needs a right-hand side"},
		{"middleware * /a -> require header.X == \"y\" else 200", "4xx/5xx"},
		{"middleware * /a -> ratelimit fast", "ratelimit syntax"},
		{"route GET /a -> proxy \"ftp://x\"", "http(s) URL"},
		{"route GET /a -> respond 200 \"unterminated", "unterminated string"},
		{"route GET /a -> respond 200 \"{{nope}}\"", "unknown operand"},
		{"route GET /a -> stream every 0s \"x\"", "positive duration"},
		{"middleware * /a -> require path matches \"(\" else 400", "bad regexp"},
		{"max_body lots", "bad size"},
	}
	for _, c := range cases {
		_, err := Parse("listen :1\n" + c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "line 2:") {
			t.Errorf("%q: want line-2 error containing %q, got %v", c.src, c.want, err)
		}
	}
}

func TestTokenizerEscapes(t *testing.T) {
	toks, err := tokenize(`respond 200 "a \"quoted\" \\ back\nline" x`)
	if err != nil || len(toks) != 4 || toks[2] != "\"a \"quoted\" \\ back\nline\"" {
		t.Fatalf("%v %q", err, toks)
	}
	if stripComment(`route "#notacomment" # real`) != `route "#notacomment" ` {
		t.Fatal("comment stripping inside quotes")
	}
}
