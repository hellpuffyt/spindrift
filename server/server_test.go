package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellpuffyt/spindrift/config"
)

// start boots a server on a random port and returns its base URL.
func start(t *testing.T, src string) (*Server, string) {
	t.Helper()
	cfg, err := config.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, io.Discard)
	s.Getenv = func(k string) string {
		if k == "API_TOKEN" {
			return "s3cret"
		}
		return ""
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s, "http://" + ln.Addr().String()
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

const base = `
log off
middleware * /*          -> header "X-Served-By" "spindrift"
middleware * /api/*      -> require header.X-Token == env.API_TOKEN else 401
middleware * /limited/*  -> ratelimit 5/s burst 3
route GET /              -> respond 200 html "<h1>hi</h1>"
route GET,HEAD /health   -> respond 200 text "ok"
route GET /users/:id     -> respond 200 json "{\"id\":\"{{param.id}}\",\"q\":\"{{query.q}}\",\"m\":\"{{method}}\"}"
route GET /api/secret    -> respond 200 text "the goods"
route GET /limited/x     -> respond 200 text "x"
route GET /old           -> redirect 302 "/health"
route GET /events        -> stream every 10ms "tick {{param.n}}" count 3
route POST /echo-len     -> respond 200 text "len {{header.Content-Length}}"
`

func TestRoutingRespondParamsAndHeaders(t *testing.T) {
	_, u := start(t, base)
	resp, body := get(t, u+"/users/42?q=hello")
	if resp.StatusCode != 200 || body != `{"id":"42","q":"hello","m":"GET"}` || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp.Header.Get("X-Served-By") != "spindrift" {
		t.Fatal("middleware header missing")
	}
	resp, body = get(t, u+"/")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || body != "<h1>hi</h1>" {
		t.Fatalf("root: %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, u+"/nope")
	if resp.StatusCode != 404 || body != "Not Found\n" {
		t.Fatalf("404: %d %q", resp.StatusCode, body)
	}
	resp, _ = get(t, u+"/users")
	if resp.StatusCode != 404 {
		t.Fatal("missing param segment must not match")
	}
	// Method mismatch → 404 (no route), not a crash.
	r, _ := http.Post(u+"/health", "text/plain", nil)
	if r.StatusCode != 404 {
		t.Fatalf("POST /health: %d", r.StatusCode)
	}
	r, _ = http.Head(u + "/health")
	if r.StatusCode != 200 || r.ContentLength != 2 {
		t.Fatalf("HEAD: %d %d", r.StatusCode, r.ContentLength)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ = client.Get(u + "/old")
	if r.StatusCode != 302 || r.Header.Get("Location") != "/health" {
		t.Fatalf("redirect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestRequireAndRateLimit(t *testing.T) {
	_, u := start(t, base)
	resp, _ := get(t, u+"/api/secret")
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", u+"/api/secret", nil)
	req.Header.Set("X-Token", "s3cret")
	r, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(b) != "the goods" {
		t.Fatalf("with token: %d %q", r.StatusCode, b)
	}
	codes := []int{}
	for i := 0; i < 5; i++ {
		resp, _ := get(t, u+"/limited/x")
		codes = append(codes, resp.StatusCode)
	}
	if fmt.Sprint(codes) != "[200 200 200 429 429]" {
		t.Fatalf("burst 3 then 429: %v", codes)
	}
	time.Sleep(250 * time.Millisecond) // 5/s refills ~1 token
	if resp, _ := get(t, u+"/limited/x"); resp.StatusCode != 200 {
		t.Fatalf("token should have refilled, got %d", resp.StatusCode)
	}
}

func TestStaticFilesAndTraversal(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>index</p>"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(dir), "outside.txt"), []byte("secret"), 0o644)
	_, u := start(t, "log off\nroute * /static/* -> files \""+strings.ReplaceAll(dir, `\`, `\\`)+"\"\n")
	resp, body := get(t, u+"/static/sub/a.txt")
	if resp.StatusCode != 200 || body != "alpha" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.ContentLength != 5 {
		t.Fatalf("file: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if _, body = get(t, u+"/static/"); body != "<p>index</p>" {
		t.Fatalf("index: %q", body)
	}
	for _, p := range []string{"/static/../outside.txt", "/static/sub/../../outside.txt", "/static/%2e%2e/outside.txt", "/static/..%2foutside.txt", "/static/sub/%2e%2e%2f%2e%2e%2foutside.txt"} {
		// Use a raw connection so the client library can't normalise the path for us.
		raw := rawRequest(t, u, "GET "+p+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		if strings.Contains(raw, "secret") || !strings.HasPrefix(raw, "HTTP/1.1 404") {
			t.Fatalf("traversal %s leaked: %q", p, raw)
		}
	}
	r, _ := http.Post(u+"/static/sub/a.txt", "text/plain", nil)
	if r.StatusCode != 405 {
		t.Fatalf("POST to files: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", u+"/static/sub/a.txt", nil)
	req.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 304 {
		t.Fatalf("304: %d", r.StatusCode)
	}
}

func rawRequest(t *testing.T, base, raw string) string {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Write([]byte(raw))
	b, _ := io.ReadAll(conn)
	return string(b)
}

func TestProxyStreamsBothWays(t *testing.T) {
	var gotXFF, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Up", "1")
		w.WriteHeader(201)
		fmt.Fprintf(w, "upstream saw %s %s", r.Method, r.URL.RequestURI())
	}))
	defer up.Close()
	_, u := start(t, "log off\nroute * /api/* -> proxy \""+up.URL+"\"\n")
	r, err := http.Post(u+"/api/things?x=1", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 201 || string(b) != "upstream saw POST /api/things?x=1" || r.Header.Get("X-Up") != "1" || r.Header.Get("Via") != "1.1 spindrift" {
		t.Fatalf("proxy: %d %q %v", r.StatusCode, b, r.Header)
	}
	if gotXFF != "127.0.0.1" || gotBody != "payload" {
		t.Fatalf("upstream request: xff=%q body=%q", gotXFF, gotBody)
	}
	up.Close()
	if r, _ := get(t, u+"/api/down"); r.StatusCode != 502 {
		t.Fatalf("dead upstream: %d", r.StatusCode)
	}
}

func TestStreamIsChunkedSSE(t *testing.T) {
	_, u := start(t, base)
	resp, err := http.Get(u + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" || len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("headers: %v %v", resp.Header, resp.TransferEncoding)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Count(string(b), "data: tick") != 3 {
		t.Fatalf("events: %q", b)
	}
	// Second request on the same keep-alive connection still works after a chunked stream.
	if r, body := get(t, u+"/health"); r.StatusCode != 200 || body != "ok" {
		t.Fatal("connection unusable after stream")
	}
}

func TestRawProtocolBehaviours(t *testing.T) {
	_, u := start(t, base)
	// Pipelining: two requests in one write, two responses in order.
	raw := rawRequest(t, u, "GET /health HTTP/1.1\r\nHost: x\r\n\r\nGET /users/9 HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if strings.Count(raw, "HTTP/1.1 200 OK") != 2 || !strings.Contains(raw, "ok") || !strings.Contains(raw, `"id":"9"`) {
		t.Fatalf("pipelining: %q", raw)
	}
	i, j := strings.Index(raw, "ok"), strings.Index(raw, `"id":"9"`)
	if i > j {
		t.Fatal("pipelined responses out of order")
	}
	// HTTP/1.0 without keep-alive closes; body is fixed-length.
	raw = rawRequest(t, u, "GET /health HTTP/1.0\r\n\r\n")
	if !strings.HasPrefix(raw, "HTTP/1.1 200 OK") || !strings.Contains(raw, "Connection: close") || !strings.HasSuffix(raw, "ok") {
		t.Fatalf("http/1.0: %q", raw)
	}
	// Expect: 100-continue gets an interim response before the final one.
	raw = rawRequest(t, u, "POST /echo-len HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 4\r\nConnection: close\r\n\r\nabcd")
	if !strings.HasPrefix(raw, "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK") || !strings.HasSuffix(raw, "len 4") {
		t.Fatalf("100-continue: %q", raw)
	}
	// Chunked request body is drained so the next pipelined request is served.
	raw = rawRequest(t, u, "POST /echo-len HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\nGET /health HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if strings.Count(raw, "HTTP/1.1 200 OK") != 2 {
		t.Fatalf("after chunked body: %q", raw)
	}
	// Malformed → 400, oversized header → 431, bad version → 505.
	for raw, want := range map[string]string{
		"GET / HTTP/1.1\nHost: x\n\n": "HTTP/1.1 400",
		"GET / HTTP/1.1\r\nHost: x\r\nX: " + strings.Repeat("a", 70000) + "\r\n\r\n": "HTTP/1.1 431",
		"GET / HTTP/3.0\r\nHost: x\r\n\r\n":                                          "HTTP/1.1 505",
		"GET / HTTP/1.1\r\nHost: x\r\nContent-Length: 999999999\r\n\r\n":             "HTTP/1.1 413",
		"GET /%zz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n":                  "HTTP/1.1 400",
	} {
		if got := rawRequest(t, u, raw); !strings.HasPrefix(got, want) {
			t.Errorf("want %s, got %q", want, got[:min(len(got), 60)])
		}
	}
	// Unknown built-in path and metrics.
	if got := rawRequest(t, u, "GET /_spindrift/metrics HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); !strings.Contains(got, "spindrift_requests_total{method=\"GET\",route=\"/health\",status=\"200\"}") {
		t.Fatalf("metrics: %q", got)
	}
}

func TestReloadSwapsRoutesWithoutDroppingConnections(t *testing.T) {
	s, u := start(t, base)
	// Hold a streaming connection open across the reload.
	resp, err := http.Get(u + "/events")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Parse("log off\nroute GET /health -> respond 200 text \"v2\"\n")
	s.Reload(cfg)
	if _, body := get(t, u+"/health"); body != "v2" {
		t.Fatalf("after reload: %q", body)
	}
	if r, _ := get(t, u+"/users/1"); r.StatusCode != 404 {
		t.Fatal("old route survived reload")
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Count(string(b), "data:") != 3 {
		t.Fatalf("in-flight stream was cut by reload: %q", b)
	}
}

func TestGracefulShutdownFinishesInFlight(t *testing.T) {
	cfg, _ := config.Parse("log off\nroute GET /slow -> stream every 50ms \"x\" count 4\nroute GET /fast -> respond 200 text \"f\"\n")
	s := New(cfg, io.Discard)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go s.Serve(ln)
	u := "http://" + ln.Addr().String()
	resp, err := http.Get(u + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done <- s.Shutdown(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := http.Get(u + "/fast"); err == nil {
		t.Fatal("listener should be closed during shutdown")
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Count(string(b), "data: x") != 4 {
		t.Fatalf("in-flight response truncated: %q", b)
	}
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestLoadManyConcurrentKeepAliveClients(t *testing.T) {
	s, u := start(t, base)
	clients, per := 32, 200
	if testing.Short() {
		clients, per = 8, 50
	}
	var wg sync.WaitGroup
	var failures atomic.Int64
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			cl := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
			for i := 0; i < per; i++ {
				r, err := cl.Get(fmt.Sprintf("%s/users/%d?q=%d", u, c, i))
				if err != nil {
					failures.Add(1)
					continue
				}
				b, _ := io.ReadAll(r.Body)
				r.Body.Close()
				if r.StatusCode != 200 || !bytes.Contains(b, []byte(fmt.Sprintf(`"id":"%d"`, c))) {
					failures.Add(1)
				}
			}
		}(c)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d failed requests", failures.Load())
	}
	snap := s.Metrics.Snapshot()
	if snap["GET|/users/:id|200"] != int64(clients*per) {
		t.Fatalf("metrics count %d, want %d", snap["GET|/users/:id|200"], clients*per)
	}
	if s.Metrics.ConnsTotal.Load() > int64(clients*8) {
		t.Fatalf("keep-alive not working: %d connections for %d clients", s.Metrics.ConnsTotal.Load(), clients)
	}
}

func TestSlowlorisHeaderTimeout(t *testing.T) {
	cfg, _ := config.Parse("log off\nidle_timeout 200ms\nroute GET / -> respond 200 text \"x\"\n")
	s := New(cfg, io.Discard)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go s.Serve(ln)
	defer s.Shutdown(context.Background())
	conn, _ := net.Dial("tcp", ln.Addr().String())
	defer conn.Close()
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n"))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(bufio.NewReader(conn))
	if !strings.HasPrefix(string(b), "HTTP/1.1 408") {
		t.Fatalf("slow client should get 408 and be closed, got %q", b)
	}
}
