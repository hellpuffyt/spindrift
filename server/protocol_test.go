package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellpuffyt/spindrift/config"
)

const smuggleCfg = `
log off
route POST /echo-len  -> respond 200 text "len {{header.Content-Length}}"
route GET /smuggled   -> respond 200 text "SMUGGLED"
route GET /go         -> redirect 302 "{{query.next}}"
middleware * /*       -> header "X-Echo" "{{query.h}}"
route GET /h          -> respond 200 text "h"
`

// A body whose chunked framing breaks mid-way must end the connection:
// carrying on would parse attacker-chosen body bytes as the next request.
func TestMalformedChunkedBodyClosesConnection(t *testing.T) {
	_, u := start(t, smuggleCfg)
	raw := rawRequest(t, u, "POST /echo-len HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n"+
		"3\r\nabc\r\nZZ\r\nGET /smuggled HTTP/1.1\r\nHost: x\r\n\r\n")
	if strings.Contains(raw, "SMUGGLED") {
		t.Fatalf("request smuggled through a broken chunked body: %q", raw)
	}
	// Oversized chunked body (limit hit while draining) also closes.
	_, u = start(t, "log off\nmax_body 4\n"+smuggleCfg)
	raw = rawRequest(t, u, "POST /echo-len HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n"+
		"8\r\n12345678\r\n0\r\n\r\nGET /smuggled HTTP/1.1\r\nHost: x\r\n\r\n")
	if strings.Contains(raw, "SMUGGLED") {
		t.Fatalf("request served after an over-limit chunked body: %q", raw)
	}
}

// RFC 9112 §6.1: TE+CL is processed by TE and then the connection closes,
// so a front end that framed by CL cannot desync us.
func TestTEPlusCLRequestClosesAfterResponse(t *testing.T) {
	_, u := start(t, smuggleCfg)
	raw := rawRequest(t, u, "POST /echo-len HTTP/1.1\r\nHost: x\r\nContent-Length: 40\r\nTransfer-Encoding: chunked\r\n\r\n"+
		"0\r\n\r\nGET /smuggled HTTP/1.1\r\nHost: x\r\n\r\n")
	if strings.Count(raw, "HTTP/1.1 200") != 1 || strings.Contains(raw, "SMUGGLED") || !strings.Contains(raw, "Connection: close") {
		t.Fatalf("TE+CL: %q", raw)
	}
}

func TestProtocolRejectionsOnTheWire(t *testing.T) {
	_, u := start(t, smuggleCfg)
	for name, req := range map[string]string{
		"duplicate Host":       "GET /h HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n",
		"HTTP/1.0 chunked":     "POST /echo-len HTTP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
		"obs-fold":             "GET /h HTTP/1.1\r\nHost: x\r\nX: a\r\n  b\r\n\r\n",
		"duplicate CL differs": "POST /echo-len HTTP/1.1\r\nHost: x\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\nab",
		"control in target":    "GET /h\x1b[2J HTTP/1.1\r\nHost: x\r\n\r\n",
	} {
		if got := rawRequest(t, u, req); !strings.HasPrefix(got, "HTTP/1.1 400") || !strings.Contains(got, "Connection: close") {
			t.Errorf("%s: want 400 + close, got %q", name, got[:min(len(got), 80)])
		}
	}
	// HTTP/1.0 never gets an interim 100 Continue.
	got := rawRequest(t, u, "POST /echo-len HTTP/1.0\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\nab")
	if strings.Contains(got, "100 Continue") || !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Fatalf("HTTP/1.0 Expect: %q", got)
	}
}

// Templated header values carry request data; they must not split the
// response or add header lines.
func TestTemplatedHeadersCannotSplitResponses(t *testing.T) {
	_, u := start(t, smuggleCfg)
	for _, target := range []string{
		"/go?next=%2Fok%0D%0ASet-Cookie:%20pwned=1",
		"/h?h=v%0D%0ASet-Cookie:%20pwned=1%0D%0A%0D%0A<script>",
	} {
		raw := rawRequest(t, u, "GET "+target+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		head, _, _ := strings.Cut(raw, "\r\n\r\n")
		for _, line := range strings.Split(head, "\r\n") {
			if strings.HasPrefix(strings.ToLower(line), "set-cookie") {
				t.Fatalf("%s: injected header line: %q", target, raw)
			}
		}
	}
}

func TestFilesRefuseSymlinksOutOfRoot(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.html")
	os.WriteFile(secret, []byte("TOP SECRET"), 0o644)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	if err := os.Symlink(secret, filepath.Join(root, "sub", "index.html")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	_, u := start(t, "log off\nroute * /static/* -> files \""+strings.ReplaceAll(root, `\`, `\\`)+"\"\n")
	for _, p := range []string{"/static/link.html", "/static/sub/", "/static/sub/index.html", "/static/dirlink/secret.html", "/static/dirlink/"} {
		resp, body := get(t, u+p)
		if resp.StatusCode != 404 || strings.Contains(body, "TOP SECRET") {
			t.Errorf("%s escaped the root: %d %q", p, resp.StatusCode, body)
		}
	}
}

// Request methods are client-chosen tokens; using them raw as a metrics
// label let a client grow the registry without bound.
func TestMetricsMethodLabelIsBounded(t *testing.T) {
	s, u := start(t, smuggleCfg)
	for i := 0; i < 50; i++ {
		rawRequest(t, u, "M"+strings.Repeat("X", i)+" /h HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	}
	rawRequest(t, u, "GET /h HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	snap := s.Metrics.Snapshot()
	if len(snap) > 3 {
		t.Fatalf("metrics series grew with client-chosen methods: %d series %v", len(snap), snap)
	}
	if snap["OTHER|-|404"] != 50 || snap["GET|/h|200"] != 1 {
		t.Fatalf("unexpected series: %v", snap)
	}
}

// The proxy must forward the path it routed and authorised (cleaned), and
// strip headers the client listed in Connection (RFC 9110 §7.6.1).
func TestProxyForwardsRoutedPathAndStripsConnectionListedHeaders(t *testing.T) {
	var gotURI, gotInternal, gotKeep string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		gotInternal = r.Header.Get("X-Internal")
		gotKeep = r.Header.Get("X-Keep")
		io.WriteString(w, "up")
	}))
	defer up.Close()
	_, u := start(t, "log off\nmiddleware * /api/admin/* -> require header.X-Admin == \"yes\" else 403\nroute * /api/* -> proxy \""+up.URL+"\"\n")
	raw := rawRequest(t, u, "GET /api/public/../x/./y?q=1 HTTP/1.1\r\nHost: x\r\nConnection: close, X-Internal\r\nX-Internal: secret\r\nX-Keep: 1\r\n\r\n")
	if !strings.HasPrefix(raw, "HTTP/1.1 200") {
		t.Fatalf("proxy: %q", raw)
	}
	if gotURI != "/api/x/y?q=1" {
		t.Fatalf("upstream saw %q, want the routed path /api/x/y?q=1", gotURI)
	}
	if gotInternal != "" || gotKeep != "1" {
		t.Fatalf("connection-listed header forwarded: X-Internal=%q X-Keep=%q", gotInternal, gotKeep)
	}
	rawRequest(t, u, "GET /api/dir/ HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if gotURI != "/api/dir/" {
		t.Fatalf("trailing slash lost: upstream saw %q", gotURI)
	}
	rawRequest(t, u, "GET /api/a%20b HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if gotURI != "/api/a%20b" {
		t.Fatalf("escaping lost: upstream saw %q", gotURI)
	}
}

// With the token variable unset, the README's gate must still refuse a
// request that sends no token. The bare `==` form documented before the
// audit let it through ("" == "").
func TestDocumentedTokenGateFailsClosedWhenEnvUnset(t *testing.T) {
	cfg, err := config.Parse("log off\nmiddleware * /api/* -> require header.X-Token exists && header.X-Token == env.API_TOKEN else 401\nroute GET /api/x -> respond 200 text \"in\"\n")
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, io.Discard)
	s.Getenv = func(string) string { return "" }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	defer s.Shutdown(context.Background())
	u := "http://" + ln.Addr().String()
	if raw := rawRequest(t, u, "GET /api/x HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); !strings.HasPrefix(raw, "HTTP/1.1 401") {
		t.Fatalf("gate open with unset env and no header: %q", raw)
	}
	if raw := rawRequest(t, u, "GET /api/x HTTP/1.1\r\nHost: x\r\nX-Token: \r\nConnection: close\r\n\r\n"); !strings.HasPrefix(raw, "HTTP/1.1 401") {
		t.Fatalf("gate open with unset env and empty header: %q", raw)
	}
}
