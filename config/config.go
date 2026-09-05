// Package config parses Spindrift's pipeline language.
//
//	listen :8080
//	log json                      # json | text | off
//	max_body 8MB
//
//	middleware * /*         -> header "Server" "Spindrift"
//	middleware * /api/*     -> require header.X-Token == env.API_TOKEN else 401
//	middleware * /api/*     -> ratelimit 50/s burst 100
//	route GET /health       -> respond 200 text "ok"
//	route GET /users/:id    -> respond 200 json "{\"id\":\"{{param.id}}\"}"
//	route GET /events       -> stream every 500ms "tick {{now}}" count 10
//	route * /static/*       -> files "./public"
//	route * /api/*          -> proxy "http://127.0.0.1:9000"
//	route GET /old          -> redirect 301 "/new"
//
// Rules apply in order: every matching middleware runs before the first
// matching route. Conditions and templates use a tiny expression language
// (see Operand, Cond, Template).
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is a parsed file.
type Config struct {
	Listen      string
	Log         string // json | text | off
	MaxBody     int64
	MaxHeader   int
	ReadTimeout time.Duration
	IdleTimeout time.Duration
	Rules       []Rule
}

// Rule is one `route` or `middleware` line.
type Rule struct {
	Line       int
	Middleware bool
	Methods    []string // nil = any
	Pattern    string
	Action     Action
}

// Action is the right-hand side of a rule.
type Action struct {
	Name   string   // respond | redirect | files | proxy | stream | header | require | ratelimit | cors
	Status int      // respond/redirect/require-else status
	Args   []string // positional string arguments
	Tpl    *Template
	Cond   *Cond
	Every  time.Duration
	Count  int
	Rate   float64 // ratelimit tokens per second
	Burst  int
	Kind   string // respond: text | json | html
}

// Env is what expressions can see about a request.
type Env struct {
	Method, Path, RawQuery, Remote string
	Params                         map[string]string
	Header                         func(string) string
	Query                          func(string) string
	Getenv                         func(string) string
	Now                            func() time.Time
}

// Operand is a value source: a literal or a field of Env.
type Operand struct {
	Lit   string
	Field string // method | path | query | remote | header | query.k | param.k | env.k | now
	Key   string
}

func (o Operand) Eval(e *Env) string {
	switch o.Field {
	case "":
		return o.Lit
	case "method":
		return e.Method
	case "path":
		return e.Path
	case "rawquery":
		return e.RawQuery
	case "remote":
		return e.Remote
	case "now":
		return e.Now().UTC().Format(time.RFC3339)
	case "unix":
		return strconv.FormatInt(e.Now().Unix(), 10)
	case "header":
		return e.Header(o.Key)
	case "query":
		return e.Query(o.Key)
	case "param":
		return e.Params[o.Key]
	case "env":
		return e.Getenv(o.Key)
	}
	return ""
}

func parseOperand(tok string) (Operand, error) {
	if strings.HasPrefix(tok, "\"") {
		return Operand{Lit: tok[1 : len(tok)-1]}, nil
	}
	switch tok {
	case "method", "path", "rawquery", "remote", "now", "unix":
		return Operand{Field: tok}, nil
	}
	for _, p := range []string{"header.", "query.", "param.", "env."} {
		if strings.HasPrefix(tok, p) && len(tok) > len(p) {
			return Operand{Field: p[:len(p)-1], Key: tok[len(p):]}, nil
		}
	}
	return Operand{}, fmt.Errorf("unknown operand %q (want a \"string\", method, path, remote, now, header.X, query.k, param.k or env.K)", tok)
}

// Cond is `a OP b` terms joined by && / || (left to right, no parentheses).
type Cond struct {
	Terms []Term
	Joins []string // between terms: "&&" or "||"
}

type Term struct {
	Left  Operand
	Op    string // == != contains startswith endswith matches exists
	Right Operand
	re    *regexp.Regexp
}

func (t *Term) Eval(e *Env) bool {
	l := t.Left.Eval(e)
	switch t.Op {
	case "exists":
		return l != ""
	case "==":
		return l == t.Right.Eval(e)
	case "!=":
		return l != t.Right.Eval(e)
	case "contains":
		return strings.Contains(l, t.Right.Eval(e))
	case "startswith":
		return strings.HasPrefix(l, t.Right.Eval(e))
	case "endswith":
		return strings.HasSuffix(l, t.Right.Eval(e))
	case "matches":
		return t.re.MatchString(l)
	}
	return false
}

func (c *Cond) Eval(e *Env) bool {
	if c == nil {
		return true
	}
	result := c.Terms[0].Eval(e)
	for i, join := range c.Joins {
		next := c.Terms[i+1].Eval(e)
		if join == "&&" {
			result = result && next
		} else {
			result = result || next
		}
	}
	return result
}

func parseCond(toks []string) (*Cond, error) {
	c := &Cond{}
	for len(toks) > 0 {
		if len(toks) < 2 {
			return nil, fmt.Errorf("incomplete condition")
		}
		left, err := parseOperand(toks[0])
		if err != nil {
			return nil, err
		}
		term := Term{Left: left, Op: toks[1]}
		n := 3
		switch toks[1] {
		case "exists":
			n = 2
		case "==", "!=", "contains", "startswith", "endswith", "matches":
			if len(toks) < 3 {
				return nil, fmt.Errorf("operator %s needs a right-hand side", toks[1])
			}
			term.Right, err = parseOperand(toks[2])
			if err != nil {
				return nil, err
			}
			if toks[1] == "matches" {
				if term.Right.Field != "" {
					return nil, fmt.Errorf("matches needs a literal pattern")
				}
				term.re, err = regexp.Compile(term.Right.Lit)
				if err != nil {
					return nil, fmt.Errorf("bad regexp: %v", err)
				}
			}
		default:
			return nil, fmt.Errorf("unknown operator %q", toks[1])
		}
		c.Terms = append(c.Terms, term)
		toks = toks[n:]
		if len(toks) == 0 {
			break
		}
		if toks[0] != "&&" && toks[0] != "||" {
			return nil, fmt.Errorf("expected && or ||, got %q", toks[0])
		}
		c.Joins = append(c.Joins, toks[0])
		toks = toks[1:]
	}
	if len(c.Terms) == 0 {
		return nil, fmt.Errorf("empty condition")
	}
	return c, nil
}

// Template is text with `{{operand}}` holes.
type Template struct {
	parts []string
	ops   []Operand // ops[i] follows parts[i]
}

func ParseTemplate(s string) (*Template, error) {
	t := &Template{}
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			t.parts = append(t.parts, s)
			return t, nil
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unterminated {{ in template")
		}
		op, err := parseOperand(strings.TrimSpace(s[i+2 : i+j]))
		if err != nil {
			return nil, err
		}
		t.parts = append(t.parts, s[:i])
		t.ops = append(t.ops, op)
		s = s[i+j+2:]
	}
}

func (t *Template) Render(e *Env) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	for i, p := range t.parts {
		b.WriteString(p)
		if i < len(t.ops) {
			b.WriteString(t.ops[i].Eval(e))
		}
	}
	return b.String()
}

// Static reports whether rendering needs no request (no holes).
func (t *Template) Static() bool { return t == nil || len(t.ops) == 0 }

// ----------------------------------------------------------------- parse --

type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// Parse parses a config document.
func Parse(src string) (*Config, error) {
	cfg := &Config{Listen: ":8080", Log: "json", MaxBody: 8 << 20, MaxHeader: 64 << 10, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	for i, raw := range strings.Split(src, "\n") {
		ln := i + 1
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		toks, err := tokenize(line)
		if err != nil {
			return nil, &ParseError{ln, err.Error()}
		}
		fail := func(f string, a ...any) (*Config, error) { return nil, &ParseError{ln, fmt.Sprintf(f, a...)} }
		switch toks[0] {
		case "listen":
			if len(toks) != 2 {
				return fail("listen needs an address")
			}
			cfg.Listen = unquote(toks[1])
		case "log":
			if len(toks) != 2 || (toks[1] != "json" && toks[1] != "text" && toks[1] != "off") {
				return fail("log needs json, text or off")
			}
			cfg.Log = toks[1]
		case "max_body", "max_header":
			if len(toks) != 2 {
				return fail("%s needs a size", toks[0])
			}
			n, err := parseSize(toks[1])
			if err != nil {
				return fail("%v", err)
			}
			if toks[0] == "max_body" {
				cfg.MaxBody = n
			} else {
				cfg.MaxHeader = int(n)
			}
		case "read_timeout", "idle_timeout":
			if len(toks) != 2 {
				return fail("%s needs a duration", toks[0])
			}
			d, err := time.ParseDuration(toks[1])
			if err != nil {
				return fail("%v", err)
			}
			if toks[0] == "read_timeout" {
				cfg.ReadTimeout = d
			} else {
				cfg.IdleTimeout = d
			}
		case "route", "middleware":
			arrow := indexOf(toks, "->")
			if arrow < 0 || arrow != 3 {
				return fail("expected `%s METHODS PATTERN -> action`", toks[0])
			}
			rule := Rule{Line: ln, Middleware: toks[0] == "middleware", Pattern: toks[2]}
			if toks[1] != "*" {
				rule.Methods = strings.Split(strings.ToUpper(toks[1]), ",")
			}
			if !strings.HasPrefix(rule.Pattern, "/") && rule.Pattern != "*" {
				return fail("pattern must start with /")
			}
			if strings.Contains(rule.Pattern, "*") && !strings.HasSuffix(rule.Pattern, "/*") && rule.Pattern != "*" {
				return fail("wildcard * is only allowed as a trailing /* segment")
			}
			act, err := parseAction(toks[arrow+1:], rule.Middleware)
			if err != nil {
				return fail("%v", err)
			}
			rule.Action = act
			cfg.Rules = append(cfg.Rules, rule)
		default:
			return fail("unknown directive %q", toks[0])
		}
	}
	return cfg, nil
}

// Load reads and parses a file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(b))
}

func parseAction(toks []string, middleware bool) (Action, error) {
	if len(toks) == 0 {
		return Action{}, fmt.Errorf("missing action after ->")
	}
	a := Action{Name: toks[0]}
	args := toks[1:]
	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("%s needs %d argument(s)", a.Name, n)
		}
		return nil
	}
	var err error
	switch a.Name {
	case "respond":
		if err = need(2); err != nil {
			return a, err
		}
		if a.Status, err = strconv.Atoi(args[0]); err != nil || a.Status < 100 || a.Status > 599 {
			return a, fmt.Errorf("respond needs a status code")
		}
		a.Kind = "text"
		bodyTok := args[1]
		if len(args) >= 3 {
			a.Kind, bodyTok = args[1], args[2]
			if a.Kind != "text" && a.Kind != "json" && a.Kind != "html" {
				return a, fmt.Errorf("respond kind must be text, json or html")
			}
		}
		if !strings.HasPrefix(bodyTok, "\"") {
			return a, fmt.Errorf("respond body must be a quoted string")
		}
		if a.Tpl, err = ParseTemplate(unquote(bodyTok)); err != nil {
			return a, err
		}
	case "redirect":
		if err = need(2); err != nil {
			return a, err
		}
		if a.Status, err = strconv.Atoi(args[0]); err != nil || a.Status/100 != 3 {
			return a, fmt.Errorf("redirect needs a 3xx status")
		}
		if a.Tpl, err = ParseTemplate(unquote(args[1])); err != nil {
			return a, err
		}
	case "files", "proxy":
		if err = need(1); err != nil {
			return a, err
		}
		a.Args = []string{unquote(args[0])}
		if a.Name == "proxy" && !strings.HasPrefix(a.Args[0], "http://") && !strings.HasPrefix(a.Args[0], "https://") {
			return a, fmt.Errorf("proxy upstream must be an http(s) URL")
		}
	case "stream":
		// stream every DUR "template" [count N]
		if len(args) < 3 || args[0] != "every" {
			return a, fmt.Errorf("stream syntax: stream every DURATION \"template\" [count N]")
		}
		if a.Every, err = time.ParseDuration(args[1]); err != nil || a.Every <= 0 {
			return a, fmt.Errorf("stream needs a positive duration")
		}
		if a.Tpl, err = ParseTemplate(unquote(args[2])); err != nil {
			return a, err
		}
		if len(args) >= 5 && args[3] == "count" {
			if a.Count, err = strconv.Atoi(args[4]); err != nil {
				return a, fmt.Errorf("stream count must be an integer")
			}
		}
	case "header":
		if err = need(2); err != nil {
			return a, err
		}
		a.Args = []string{unquote(args[0])}
		if a.Tpl, err = ParseTemplate(unquote(args[1])); err != nil {
			return a, err
		}
	case "require":
		// require COND else STATUS
		e := indexOf(args, "else")
		if e < 1 || e != len(args)-2 {
			return a, fmt.Errorf("require syntax: require CONDITION else STATUS")
		}
		if a.Cond, err = parseCond(args[:e]); err != nil {
			return a, err
		}
		if a.Status, err = strconv.Atoi(args[e+1]); err != nil || a.Status < 400 || a.Status > 599 {
			return a, fmt.Errorf("require else status must be 4xx/5xx")
		}
	case "ratelimit":
		// ratelimit N/s [burst M]
		if err = need(1); err != nil {
			return a, err
		}
		num, unit, ok := strings.Cut(args[0], "/")
		if !ok {
			return a, fmt.Errorf("ratelimit syntax: ratelimit N/s|N/m [burst M]")
		}
		n, err := strconv.ParseFloat(num, 64)
		if err != nil || n <= 0 {
			return a, fmt.Errorf("ratelimit needs a positive rate")
		}
		switch unit {
		case "s":
			a.Rate = n
		case "m":
			a.Rate = n / 60
		default:
			return a, fmt.Errorf("ratelimit unit must be s or m")
		}
		a.Burst = int(n)
		if a.Burst < 1 {
			a.Burst = 1
		}
		if len(args) >= 3 && args[1] == "burst" {
			if a.Burst, err = strconv.Atoi(args[2]); err != nil || a.Burst < 1 {
				return a, fmt.Errorf("burst must be a positive integer")
			}
		}
	case "cors":
		if err = need(1); err != nil {
			return a, err
		}
		a.Args = []string{unquote(args[0])}
	default:
		return a, fmt.Errorf("unknown action %q", a.Name)
	}
	isMw := a.Name == "header" || a.Name == "require" || a.Name == "ratelimit" || a.Name == "cors"
	if middleware && !isMw {
		return a, fmt.Errorf("%s is a route action, not a middleware", a.Name)
	}
	if !middleware && isMw {
		return a, fmt.Errorf("%s is a middleware action, not a route", a.Name)
	}
	return a, nil
}

func stripComment(s string) string {
	inQ := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && inQ:
			i++
		case s[i] == '"':
			inQ = !inQ
		case s[i] == '#' && !inQ:
			return s[:i]
		}
	}
	return s
}

// tokenize splits on whitespace, keeping quoted strings (with \" and \\
// escapes) as single tokens that retain their surrounding quotes.
func tokenize(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	inQ := false
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQ && c == '\\' && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				cur.WriteByte('\n')
			case 't':
				cur.WriteByte('\t')
			default:
				cur.WriteByte(s[i])
			}
		case c == '"':
			cur.WriteByte('"')
			inQ = !inQ
			if !inQ {
				flush()
			}
		case !inQ && (c == ' ' || c == '\t'):
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	if inQ {
		return nil, fmt.Errorf("unterminated string")
	}
	flush()
	return toks, nil
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func indexOf(toks []string, s string) int {
	for i, t := range toks {
		if t == s {
			return i
		}
	}
	return -1
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	u := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(u, "GB"):
		mult, u = 1<<30, u[:len(u)-2]
	case strings.HasSuffix(u, "MB"):
		mult, u = 1<<20, u[:len(u)-2]
	case strings.HasSuffix(u, "KB"):
		mult, u = 1<<10, u[:len(u)-2]
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}
