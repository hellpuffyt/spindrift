// Package server is the Spindrift engine: connection handling over the
// http1 package, the router, the built-in handlers and middleware, metrics,
// structured logging, hot reload and graceful shutdown.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hellpuffyt/spindrift/config"
	"github.com/hellpuffyt/spindrift/http1"
)

// Server serves one listener. Config can be swapped at any time with
// Reload; in-flight requests finish on the config they started with.
type Server struct {
	cfg      atomic.Pointer[compiled]
	logw     io.Writer
	Metrics  *Metrics
	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    map[net.Conn]*connState
	closing  atomic.Bool
	ln       net.Listener
	upstream *http.Client
	Getenv   func(string) string
}

// connState tracks whether a connection is between requests (idle) so
// Shutdown can close it without cutting a response short.
type connState struct{ idle atomic.Bool }

type compiled struct {
	cfg    *config.Config
	rules  []*rule
	limits http1.Limits
}

type rule struct {
	config.Rule
	segs    []string // pattern segments
	wild    bool     // trailing /*
	limiter *limiter
}

// New builds a server from a config. Logs go to logw (nil = stdout).
func New(cfg *config.Config, logw io.Writer) *Server {
	if logw == nil {
		logw = os.Stdout
	}
	s := &Server{logw: logw, Metrics: newMetrics(), conns: map[net.Conn]*connState{}, Getenv: os.Getenv}
	s.upstream = &http.Client{
		Transport:     &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.Reload(cfg)
	return s
}

// Reload atomically swaps the pipeline. Rate limiters are keyed by rule
// line so a reload keeps existing buckets for unchanged lines.
func (s *Server) Reload(cfg *config.Config) {
	old := s.cfg.Load()
	c := &compiled{cfg: cfg, limits: http1.Limits{MaxHeaderBytes: cfg.MaxHeader, MaxBodyBytes: cfg.MaxBody}}
	for _, r := range cfg.Rules {
		cr := &rule{Rule: r}
		p := strings.TrimSuffix(r.Pattern, "/*")
		cr.wild = strings.HasSuffix(r.Pattern, "/*") || r.Pattern == "*"
		if r.Pattern != "*" {
			cr.segs = strings.Split(strings.TrimPrefix(p, "/"), "/")
			if p == "/" || p == "" {
				cr.segs = nil
			}
		}
		if r.Action.Name == "ratelimit" {
			if old != nil {
				for _, o := range old.rules {
					if o.Line == r.Line && o.limiter != nil && o.Action.Rate == r.Action.Rate && o.Action.Burst == r.Action.Burst {
						cr.limiter = o.limiter
					}
				}
			}
			if cr.limiter == nil {
				cr.limiter = newLimiter(r.Action.Rate, r.Action.Burst)
			}
		}
		c.rules = append(c.rules, cr)
	}
	s.cfg.Store(c)
}

// Config returns the active configuration.
func (s *Server) Config() *config.Config { return s.cfg.Load().cfg }

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.closing.Load() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		st := &connState{}
		s.mu.Lock()
		s.conns[conn] = st
		s.mu.Unlock()
		s.wg.Add(1)
		s.Metrics.ConnsActive.Add(1)
		s.Metrics.ConnsTotal.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.Metrics.ConnsActive.Add(-1)
			s.serveConn(conn, st)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
		}()
	}
}

// Shutdown stops accepting, closes idle keep-alive connections, lets
// in-flight requests finish until ctx is done, then closes whatever is left.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closing.Store(true)
	s.mu.Lock()
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		s.closeIdle()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			s.mu.Lock()
			for c := range s.conns {
				c.Close()
			}
			s.mu.Unlock()
			<-done
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (s *Server) closeIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, st := range s.conns {
		if st.idle.Load() {
			c.Close()
		}
	}
}

// ----------------------------------------------------------- connection --

func (s *Server) serveConn(conn net.Conn, st *connState) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 16<<10)
	bw := bufio.NewWriterSize(conn, 16<<10)
	for {
		c := s.cfg.Load()
		if s.closing.Load() {
			return
		}
		st.idle.Store(true)
		conn.SetReadDeadline(time.Now().Add(c.cfg.IdleTimeout))
		req, err := http1.ReadRequest(br, c.limits)
		st.idle.Store(false)
		if err != nil {
			if !errors.Is(err, io.EOF) && !s.closing.Load() {
				s.writeError(bw, err)
			}
			return
		}
		conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
		req.RemoteAddr = conn.RemoteAddr().String()
		if s.closing.Load() {
			req.Close = true
		}
		rw := http1.NewResponseWriter(bw, req)
		if req.Expect100 {
			if err := rw.WriteContinue(); err != nil {
				return
			}
		}
		start := time.Now()
		routeName := s.dispatch(c, req, rw)
		if err := rw.Finish(); err != nil {
			return
		}
		s.Metrics.observe(req.Method, routeName, rw.Status(), time.Since(start))
		s.logRequest(c.cfg, req, rw, routeName, time.Since(start))
		// Drain what the handler didn't read so the next request parses.
		if n, _ := io.CopyN(io.Discard, req.Body, 1<<20); n == 1<<20 {
			return
		}
		if req.Close || rw.CloseAfter() {
			return
		}
	}
}

func (s *Server) writeError(bw *bufio.Writer, err error) {
	status := 400
	switch {
	case errors.Is(err, http1.ErrHeaderTooLarge):
		status = 431
	case errors.Is(err, http1.ErrBodyTooLarge):
		status = 413
	case errors.Is(err, http1.ErrUnsupportedVersion):
		status = 505
	case errors.Is(err, http1.ErrURITooLong):
		status = 414
	case errors.Is(err, http1.ErrMalformed):
		status = 400
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			status = 408
		} else {
			return // connection-level error; nothing to say
		}
	}
	rw := http1.NewResponseWriter(bw, nil)
	rw.Respond(status, "text/plain; charset=utf-8", []byte(http1.StatusText(status)+"\n"))
	s.Metrics.observe("-", "-", status, 0)
}

// --------------------------------------------------------------- router --

type match struct {
	params map[string]string
	rest   string
}

func (r *rule) match(method, p string) (match, bool) {
	if r.Methods != nil {
		ok := false
		for _, m := range r.Methods {
			if m == method {
				ok = true
			}
		}
		if !ok {
			return match{}, false
		}
	}
	if r.Pattern == "*" {
		return match{rest: strings.TrimPrefix(p, "/")}, true
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if p == "/" {
		segs = nil
	}
	m := match{}
	for i, ps := range r.segs {
		if i >= len(segs) {
			return match{}, false
		}
		if strings.HasPrefix(ps, ":") {
			if m.params == nil {
				m.params = map[string]string{}
			}
			m.params[ps[1:]] = segs[i]
		} else if ps != segs[i] {
			return match{}, false
		}
	}
	if len(segs) > len(r.segs) {
		if !r.wild {
			return match{}, false
		}
		m.rest = strings.Join(segs[len(r.segs):], "/")
	} else if r.wild {
		m.rest = ""
	}
	return m, true
}

func (s *Server) env(req *http1.Request, params map[string]string) *config.Env {
	var q url.Values
	return &config.Env{
		Method: req.Method, Path: req.Path, RawQuery: req.RawQuery, Remote: hostOf(req.RemoteAddr),
		Params: params,
		Header: req.Header.Get,
		Query: func(k string) string {
			if q == nil {
				q, _ = url.ParseQuery(req.RawQuery)
			}
			return q.Get(k)
		},
		Getenv: s.Getenv,
		Now:    time.Now,
	}
}

func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// dispatch runs middleware then the first matching route. Returns the
// route pattern for metrics/logs.
func (s *Server) dispatch(c *compiled, req *http1.Request, rw *http1.ResponseWriter) string {
	p, err := url.PathUnescape(req.Path)
	if err != nil {
		rw.Respond(400, "text/plain", []byte("bad path encoding\n"))
		return "-"
	}
	p = path.Clean("/" + p)
	if strings.HasPrefix(p, "/_spindrift/") {
		return s.builtin(p, req, rw)
	}
	for _, r := range c.rules {
		m, ok := r.match(req.Method, p)
		if !ok {
			continue
		}
		e := s.env(req, m.params)
		if r.Middleware {
			if !s.middleware(r, e, req, rw) {
				return r.Pattern
			}
			continue
		}
		s.route(r, m, e, req, rw)
		return r.Pattern
	}
	rw.Respond(404, "text/plain; charset=utf-8", []byte("Not Found\n"))
	return "-"
}

func (s *Server) builtin(p string, req *http1.Request, rw *http1.ResponseWriter) string {
	switch p {
	case "/_spindrift/health":
		rw.Respond(200, "application/json", []byte(`{"status":"ok"}`+"\n"))
	case "/_spindrift/metrics":
		rw.Respond(200, "text/plain; version=0.0.4", []byte(s.Metrics.Render()))
	default:
		rw.Respond(404, "text/plain", []byte("Not Found\n"))
	}
	return p
}

// middleware returns false when the chain was short-circuited.
func (s *Server) middleware(r *rule, e *config.Env, req *http1.Request, rw *http1.ResponseWriter) bool {
	a := r.Action
	switch a.Name {
	case "header":
		rw.Header().Set(a.Args[0], a.Tpl.Render(e))
	case "require":
		if !a.Cond.Eval(e) {
			rw.Respond(a.Status, "text/plain; charset=utf-8", []byte(http1.StatusText(a.Status)+"\n"))
			return false
		}
	case "ratelimit":
		if !r.limiter.allow(e.Remote, time.Now()) {
			rw.Header().Set("Retry-After", "1")
			rw.Respond(429, "text/plain; charset=utf-8", []byte("Too Many Requests\n"))
			return false
		}
	case "cors":
		origin := a.Args[0]
		if origin == "*" || origin == req.Header.Get("Origin") {
			rw.Header().Set("Access-Control-Allow-Origin", origin)
			rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			rw.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			if req.Method == "OPTIONS" {
				rw.Header().Set("Content-Length", "0")
				rw.WriteHeader(204)
				return false
			}
		}
	}
	return true
}

func (s *Server) route(r *rule, m match, e *config.Env, req *http1.Request, rw *http1.ResponseWriter) {
	a := r.Action
	switch a.Name {
	case "respond":
		ct := map[string]string{"text": "text/plain; charset=utf-8", "json": "application/json", "html": "text/html; charset=utf-8"}[a.Kind]
		rw.Respond(a.Status, ct, []byte(a.Tpl.Render(e)))
	case "redirect":
		rw.Header().Set("Location", a.Tpl.Render(e))
		rw.Respond(a.Status, "text/plain; charset=utf-8", []byte(http1.StatusText(a.Status)+"\n"))
	case "files":
		s.files(a.Args[0], m.rest, req, rw)
	case "proxy":
		s.proxy(a.Args[0], req, rw)
	case "stream":
		s.stream(a, e, rw)
	}
}

// ------------------------------------------------------------- handlers --

func (s *Server) files(root, rest string, req *http1.Request, rw *http1.ResponseWriter) {
	if req.Method != "GET" && req.Method != "HEAD" {
		rw.Header().Set("Allow", "GET, HEAD")
		rw.Respond(405, "text/plain; charset=utf-8", []byte("Method Not Allowed\n"))
		return
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		rw.Respond(500, "text/plain", []byte("bad root\n"))
		return
	}
	// rest was already path.Clean'ed via the request path; re-clean the join and
	// verify containment both lexically and after symlink resolution.
	full := filepath.Join(absRoot, filepath.FromSlash(path.Clean("/"+rest)))
	if full != absRoot && !strings.HasPrefix(full, absRoot+string(filepath.Separator)) {
		rw.Respond(404, "text/plain; charset=utf-8", []byte("Not Found\n"))
		return
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		rw.Respond(404, "text/plain; charset=utf-8", []byte("Not Found\n"))
		return
	}
	realRoot, _ := filepath.EvalSymlinks(absRoot)
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
		rw.Respond(404, "text/plain; charset=utf-8", []byte("Not Found\n"))
		return
	}
	st, err := os.Stat(real)
	if err == nil && st.IsDir() {
		real = filepath.Join(real, "index.html")
		st, err = os.Stat(real)
	}
	if err != nil || !st.Mode().IsRegular() {
		rw.Respond(404, "text/plain; charset=utf-8", []byte("Not Found\n"))
		return
	}
	f, err := os.Open(real)
	if err != nil {
		rw.Respond(403, "text/plain; charset=utf-8", []byte("Forbidden\n"))
		return
	}
	defer f.Close()
	ct := mime.TypeByExtension(filepath.Ext(real))
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := rw.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	h.Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	if ims := req.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !st.ModTime().Truncate(time.Second).After(t) {
			h.Del("Content-Length")
			h.Del("Content-Type")
			rw.WriteHeader(304)
			return
		}
	}
	rw.WriteHeader(200)
	if req.Method != "HEAD" {
		io.Copy(rw, f)
	}
}

var hopByHop = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (s *Server) proxy(upstream string, req *http1.Request, rw *http1.ResponseWriter) {
	target := strings.TrimSuffix(upstream, "/") + req.Path
	if req.RawQuery != "" {
		target += "?" + req.RawQuery
	}
	var body io.Reader = req.Body
	if req.ContentLength == 0 {
		body = nil
	}
	out, err := http.NewRequest(req.Method, target, body)
	if err != nil {
		rw.Respond(502, "text/plain; charset=utf-8", []byte("Bad Gateway\n"))
		return
	}
	if req.ContentLength > 0 {
		out.ContentLength = req.ContentLength
	}
	for k, vs := range req.Header {
		if isHop(k) || k == "Host" {
			continue
		}
		out.Header[k] = append([]string(nil), vs...)
	}
	out.Header.Set("X-Forwarded-For", hostOf(req.RemoteAddr))
	out.Header.Set("X-Forwarded-Host", req.Header.Get("Host"))
	out.Header.Set("X-Forwarded-Proto", "http")
	resp, err := s.upstream.Do(out)
	if err != nil {
		rw.Respond(502, "text/plain; charset=utf-8", []byte("Bad Gateway\n"))
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if isHop(k) {
			continue
		}
		for _, v := range vs {
			rw.Header().Add(k, v)
		}
	}
	rw.Header().Set("Via", "1.1 spindrift")
	if resp.ContentLength >= 0 {
		rw.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	rw.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := rw.Write(buf[:n]); werr != nil {
				return
			}
			rw.Flush()
		}
		if err != nil {
			return
		}
	}
}

func isHop(k string) bool {
	for _, h := range hopByHop {
		if h == k {
			return true
		}
	}
	return false
}

func (s *Server) stream(a config.Action, e *config.Env, rw *http1.ResponseWriter) {
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(200)
	if err := rw.Flush(); err != nil {
		return
	}
	for i := 0; a.Count == 0 || i < a.Count; i++ {
		if i > 0 {
			time.Sleep(a.Every)
		}
		msg := "data: " + strings.ReplaceAll(a.Tpl.Render(e), "\n", "\ndata: ") + "\n\n"
		if _, err := rw.Write([]byte(msg)); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return // client went away
		}
	}
}

// ------------------------------------------------------------- limiter --

type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		if len(l.buckets) > 100_000 { // ponytail: crude cap; LRU if this bites
			for k := range l.buckets {
				delete(l.buckets, k)
				break
			}
		}
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ------------------------------------------------------------- metrics --

// Metrics is a small Prometheus-text-format registry.
type Metrics struct {
	ConnsActive atomic.Int64
	ConnsTotal  atomic.Int64
	mu          sync.Mutex
	requests    map[string]int64   // "method|route|status"
	latency     map[string][]int64 // route -> bucket counts
	latencySum  map[string]float64
	latencyN    map[string]int64
}

var buckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

func newMetrics() *Metrics {
	return &Metrics{requests: map[string]int64{}, latency: map[string][]int64{}, latencySum: map[string]float64{}, latencyN: map[string]int64{}}
}

func (m *Metrics) observe(method, route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[method+"|"+route+"|"+strconv.Itoa(status)]++
	if _, ok := m.latency[route]; !ok {
		m.latency[route] = make([]int64, len(buckets)+1)
	}
	sec := d.Seconds()
	i := 0
	for i < len(buckets) && sec > buckets[i] {
		i++
	}
	m.latency[route][i]++
	m.latencySum[route] += sec
	m.latencyN[route]++
}

// Snapshot returns request counts keyed "method|route|status".
func (m *Metrics) Snapshot() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(m.requests))
	for k, v := range m.requests {
		out[k] = v
	}
	return out
}

func (m *Metrics) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "# TYPE spindrift_connections_active gauge\nspindrift_connections_active %d\n", m.ConnsActive.Load())
	fmt.Fprintf(&b, "# TYPE spindrift_connections_total counter\nspindrift_connections_total %d\n", m.ConnsTotal.Load())
	b.WriteString("# TYPE spindrift_requests_total counter\n")
	keys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := strings.SplitN(k, "|", 3)
		fmt.Fprintf(&b, "spindrift_requests_total{method=%q,route=%q,status=%q} %d\n", p[0], p[1], p[2], m.requests[k])
	}
	b.WriteString("# TYPE spindrift_request_duration_seconds histogram\n")
	routes := make([]string, 0, len(m.latency))
	for r := range m.latency {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		var cum int64
		for i, c := range m.latency[r] {
			cum += c
			le := "+Inf"
			if i < len(buckets) {
				le = strconv.FormatFloat(buckets[i], 'g', -1, 64)
			}
			fmt.Fprintf(&b, "spindrift_request_duration_seconds_bucket{route=%q,le=%q} %d\n", r, le, cum)
		}
		fmt.Fprintf(&b, "spindrift_request_duration_seconds_sum{route=%q} %g\n", r, m.latencySum[r])
		fmt.Fprintf(&b, "spindrift_request_duration_seconds_count{route=%q} %d\n", r, m.latencyN[r])
	}
	return b.String()
}

// ------------------------------------------------------------- logging --

func (s *Server) logRequest(cfg *config.Config, req *http1.Request, rw *http1.ResponseWriter, route string, d time.Duration) {
	switch cfg.Log {
	case "off":
		return
	case "text":
		fmt.Fprintf(s.logw, "%s %s %s %d %dB %.1fms %s %s\n", time.Now().UTC().Format(time.RFC3339), req.Method, req.Target, rw.Status(), rw.Written(), float64(d.Microseconds())/1000, hostOf(req.RemoteAddr), route)
	default:
		rec := map[string]any{
			"ts": time.Now().UTC().Format(time.RFC3339Nano), "method": req.Method, "target": req.Target,
			"status": rw.Status(), "bytes": rw.Written(), "duration_ms": float64(d.Microseconds()) / 1000,
			"remote": hostOf(req.RemoteAddr), "route": route, "proto": req.Proto(), "ua": req.Header.Get("User-Agent"),
		}
		b, _ := json.Marshal(rec)
		s.logw.Write(append(b, '\n'))
	}
}
