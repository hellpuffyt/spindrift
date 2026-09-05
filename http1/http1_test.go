package http1

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func parse(t *testing.T, raw string, lim Limits) (*Request, *bufio.Reader, error) {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(raw))
	req, err := ReadRequest(br, lim)
	return req, br, err
}

func body(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestSimpleGet(t *testing.T) {
	req, _, err := parse(t, "GET /a/b?x=1&y=2 HTTP/1.1\r\nHost: example.com\r\nuser-agent: t\r\n\r\n", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "GET" || req.Path != "/a/b" || req.RawQuery != "x=1&y=2" || req.Proto() != "HTTP/1.1" {
		t.Fatalf("bad parse: %+v", req)
	}
	if req.Header.Get("User-Agent") != "t" || req.Header.Get("host") != "example.com" {
		t.Fatalf("headers not canonical: %v", req.Header)
	}
	if req.Close || req.ContentLength != 0 || body(t, req.Body) != "" {
		t.Fatalf("keep-alive/body wrong: %+v", req)
	}
}

func TestPipelinedRequestsOnOneReader(t *testing.T) {
	raw := "POST /one HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\nhelloGET /two HTTP/1.1\r\nHost: h\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(raw))
	r1, err := ReadRequest(br, Limits{})
	if err != nil || body(t, r1.Body) != "hello" {
		t.Fatalf("first: %v %+v", err, r1)
	}
	r2, err := ReadRequest(br, Limits{})
	if err != nil || r2.Path != "/two" {
		t.Fatalf("second: %v %+v", err, r2)
	}
	if _, err := ReadRequest(br, Limits{}); !errors.Is(err, io.EOF) {
		t.Fatalf("expected clean EOF, got %v", err)
	}
}

func TestChunkedBodyWithTrailersAndExtensions(t *testing.T) {
	raw := "POST /up HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"4;ext=1\r\nWiki\r\n5\r\npedia\r\nE\r\n in\r\n\r\nchunks.\r\n0\r\nX-Checksum: abc\r\n\r\n" +
		"GET /next HTTP/1.1\r\nHost: h\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(raw))
	req, err := ReadRequest(br, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != -1 {
		t.Fatalf("chunked should report -1, got %d", req.ContentLength)
	}
	if got := body(t, req.Body); got != "Wikipedia in\r\n\r\nchunks." {
		t.Fatalf("body %q", got)
	}
	if req.Header.Get("X-Checksum") != "abc" {
		t.Fatalf("trailer not merged: %v", req.Header)
	}
	next, err := ReadRequest(br, Limits{})
	if err != nil || next.Path != "/next" {
		t.Fatalf("request after chunked body: %v %+v", err, next)
	}
}

func TestMalformedInputs(t *testing.T) {
	cases := map[string]error{
		"GET / HTTP/1.1\nHost: h\n\n":                                                 ErrMalformed, // bare LF
		" GET / HTTP/1.1\r\nHost: h\r\n\r\n":                                          ErrMalformed, // leading space
		"GET / HTTP/1.1\r\n\r\n":                                                      ErrMalformed, // no Host on 1.1
		"GET /  HTTP/1.1\r\nHost: h\r\n\r\n":                                          ErrMalformed, // double space
		"GET / HTTP/2.0\r\nHost: h\r\n\r\n":                                           ErrUnsupportedVersion,
		"GET / HTTP/1.1\r\nHost: h\r\nBad Header: x\r\n\r\n":                          ErrMalformed, // space in field name
		"GET / HTTP/1.1\r\nHost: h\r\n folded: x\r\n\r\n":                             ErrMalformed, // obs-fold
		"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: -1\r\n\r\n":                     ErrMalformed,
		"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\n": ErrMalformed,
		"GET / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: gzip, chunked\r\n\r\n":       ErrMalformed,
		"CONNECT h:443 HTTP/1.1\r\nHost: h\r\n\r\n":                                   ErrMalformed,
		"G(T / HTTP/1.1\r\nHost: h\r\n\r\n":                                           ErrMalformed,
		"GET / HTTP/1.1\r\nHost: h":                                                   ErrMalformed, // truncated headers
		"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: 100000000\r\n\r\n":              ErrBodyTooLarge,
	}
	for raw, want := range cases {
		_, _, err := parse(t, raw, Limits{MaxBodyBytes: 1000})
		if !errors.Is(err, want) {
			t.Errorf("%q: want %v, got %v", raw, want, err)
		}
	}
}

func TestHeaderTooLarge(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: h\r\nX: " + strings.Repeat("a", 5000) + "\r\n\r\n"
	_, _, err := parse(t, raw, Limits{MaxHeaderBytes: 1024})
	if !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("want 431, got %v", err)
	}
	// A single enormous line without newline must not be buffered forever.
	_, _, err = parse(t, strings.Repeat("a", 100000), Limits{MaxHeaderBytes: 1024})
	if !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("want 431 for endless line, got %v", err)
	}
}

func TestConnectionSemantics(t *testing.T) {
	r, _, _ := parse(t, "GET / HTTP/1.0\r\n\r\n", Limits{})
	if !r.Close {
		t.Fatal("HTTP/1.0 defaults to close")
	}
	r, _, _ = parse(t, "GET / HTTP/1.0\r\nConnection: keep-alive\r\n\r\n", Limits{})
	if r.Close {
		t.Fatal("HTTP/1.0 keep-alive honoured")
	}
	r, _, _ = parse(t, "GET / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n", Limits{})
	if !r.Close {
		t.Fatal("Connection: close honoured")
	}
	r, _, _ = parse(t, "PUT / HTTP/1.1\r\nHost: h\r\nExpect: 100-continue\r\nContent-Length: 1\r\n\r\nx", Limits{})
	if !r.Expect100 {
		t.Fatal("Expect: 100-continue not detected")
	}
	r, _, _ = parse(t, "GET http://example.org/p?q=1 HTTP/1.1\r\n\r\n", Limits{})
	if r == nil || r.Path != "/p" || r.RawQuery != "q=1" || r.Header.Get("Host") != "example.org" {
		t.Fatalf("absolute-form: %+v", r)
	}
}

func TestChunkedBodyLimitAndTruncation(t *testing.T) {
	raw := "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n10\r\n0123456789abcdef\r\n10\r\n0123456789abcdef\r\n0\r\n\r\n"
	req, _, err := parse(t, raw, Limits{MaxBodyBytes: 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(req.Body); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("want body too large, got %v", err)
	}
	req, _, _ = parse(t, "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nabc", Limits{})
	if _, err := io.ReadAll(req.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want unexpected EOF, got %v", err)
	}
	req, _, _ = parse(t, "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 10\r\n\r\nabc", Limits{})
	if _, err := io.ReadAll(req.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short content-length body: %v", err)
	}
}

func TestResponseWriterFixedChunkedHead(t *testing.T) {
	req, _, _ := parse(t, "GET / HTTP/1.1\r\nHost: h\r\n\r\n", Limits{})
	var buf bytes.Buffer
	rw := NewResponseWriter(bufio.NewWriter(&buf), req)
	rw.Header().Set("Date", "D")
	if err := rw.Respond(201, "text/plain", []byte("hey")); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "HTTP/1.1 201 Created\r\n") || !strings.Contains(got, "Content-Length: 3\r\n") || !strings.HasSuffix(got, "\r\n\r\nhey") {
		t.Fatalf("fixed: %q", got)
	}

	buf.Reset()
	rw = NewResponseWriter(bufio.NewWriter(&buf), req)
	rw.Header().Set("Date", "D")
	rw.Write([]byte("ab"))
	rw.Write([]byte("cde"))
	rw.Finish()
	got = buf.String()
	if !strings.Contains(got, "Transfer-Encoding: chunked\r\n") || !strings.HasSuffix(got, "\r\n\r\n2\r\nab\r\n3\r\ncde\r\n0\r\n\r\n") {
		t.Fatalf("chunked: %q", got)
	}

	head, _, _ := parse(t, "HEAD / HTTP/1.1\r\nHost: h\r\n\r\n", Limits{})
	buf.Reset()
	rw = NewResponseWriter(bufio.NewWriter(&buf), head)
	rw.Header().Set("Date", "D")
	rw.Respond(200, "text/plain", []byte("hidden"))
	got = buf.String()
	if !strings.Contains(got, "Content-Length: 6\r\n") || strings.Contains(got, "hidden") {
		t.Fatalf("HEAD must send length and no body: %q", got)
	}

	old, _, _ := parse(t, "GET / HTTP/1.0\r\n\r\n", Limits{})
	buf.Reset()
	rw = NewResponseWriter(bufio.NewWriter(&buf), old)
	rw.Header().Set("Date", "D")
	rw.Write([]byte("x"))
	rw.Finish()
	got = buf.String()
	if strings.Contains(got, "chunked") || !strings.Contains(got, "Connection: close\r\n") || !rw.CloseAfter() {
		t.Fatalf("HTTP/1.0 unknown length must be close-delimited: %q", got)
	}

	buf.Reset()
	rw = NewResponseWriter(bufio.NewWriter(&buf), req)
	rw.Finish()
	if !strings.Contains(buf.String(), "HTTP/1.1 200 OK\r\n") || !strings.Contains(buf.String(), "Content-Length: 0\r\n") {
		t.Fatalf("empty finish: %q", buf.String())
	}
}

func TestStatusText(t *testing.T) {
	if StatusText(404) != "Not Found" || StatusText(299) != "Success" || StatusText(599) != "Server Error" {
		t.Fatal("status text")
	}
}
