package http1

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// Request-smuggling and framing edge cases (RFC 9112 §2.2, §3, §5, §6, §7).

func TestSmugglingVectorsAreRejected(t *testing.T) {
	cases := map[string]string{
		"chunk size with sign":          "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n+3\r\nabc\r\n0\r\n\r\n",
		"chunk size with leading space": "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n 3\r\nabc\r\n0\r\n\r\n",
		"chunk size with 0x prefix":     "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n0x3\r\nabc\r\n0\r\n\r\n",
		"chunk size line bare LF":       "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n3\nabc\r\n0\r\n\r\n",
		"last-chunk bare LF":            "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\n\r\n",
		"trailer bare LF":               "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nX: y\n\r\n",
		"empty chunk size":              "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n\r\nabc\r\n0\r\n\r\n",
		"chunk size overflow":           "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\nfffffffffffffffff\r\nabc\r\n0\r\n\r\n",
		"chunk size line too long":      "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n3;" + strings.Repeat("e", 100_000) + "\r\nabc\r\n0\r\n\r\n",
		"trailer section too large":     "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n" + strings.Repeat("X-T: "+strings.Repeat("v", 1000)+"\r\n", 200) + "\r\n",
		"obs-fold in trailer":           "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nX: a\r\n b\r\n\r\n",
	}
	for name, raw := range cases {
		br := bufio.NewReader(strings.NewReader(raw))
		req, err := ReadRequest(br, Limits{MaxHeaderBytes: 8 << 10})
		if err != nil {
			t.Fatalf("%s: headers should parse, got %v", name, err)
		}
		if _, err := io.ReadAll(req.Body); err == nil {
			t.Errorf("%s: body accepted", name)
		}
	}
}

func TestHeaderLevelSmugglingVectors(t *testing.T) {
	cases := map[string]error{
		"duplicate Host":                  ErrMalformed,
		"HTTP/1.0 with Transfer-Encoding": ErrMalformed,
		"CTL in request target":           ErrMalformed,
		"DEL in request target":           ErrMalformed,
		"CTL in field value":              ErrMalformed,
		"TE chunked twice in one field":   ErrMalformed,
		"comma list Content-Length":       ErrMalformed,
		"Content-Length with 0x":          ErrMalformed,
		"empty Content-Length":            ErrMalformed,
		"whitespace before colon":         ErrMalformed,
	}
	raw := map[string]string{
		"duplicate Host":                  "GET / HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n",
		"HTTP/1.0 with Transfer-Encoding": "POST / HTTP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
		"CTL in request target":           "GET /a\x01b HTTP/1.1\r\nHost: h\r\n\r\n",
		"DEL in request target":           "GET /a\x7fb HTTP/1.1\r\nHost: h\r\n\r\n",
		"CTL in field value":              "GET / HTTP/1.1\r\nHost: h\r\nX: a\x01b\r\n\r\n",
		"TE chunked twice in one field":   "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked, chunked\r\n\r\n0\r\n\r\n",
		"comma list Content-Length":       "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 3, 3\r\n\r\nabc",
		"Content-Length with 0x":          "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 0x3\r\n\r\nabc",
		"empty Content-Length":            "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: \r\n\r\n",
		"whitespace before colon":         "GET / HTTP/1.1\r\nHost: h\r\nContent-Length : 3\r\n\r\nabc",
	}
	for name, want := range cases {
		_, _, err := parse(t, raw[name], Limits{})
		if !errors.Is(err, want) {
			t.Errorf("%s: want %v, got %v", name, want, err)
		}
	}
}

// RFC 9112 §6.1: a server that processes a request with both
// Transfer-Encoding and Content-Length MUST close the connection after
// responding.
func TestTEPlusCLClosesConnection(t *testing.T) {
	req, _, err := parse(t, "POST / HTTP/1.1\r\nHost: h\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !req.Close {
		t.Fatal("TE+CL request must close the connection after the response")
	}
	if req.Header.Has("Content-Length") || req.ContentLength != -1 {
		t.Fatalf("Transfer-Encoding must win: %v %d", req.Header, req.ContentLength)
	}
}

// RFC 9110 §6.5.1: trailers are kept apart from the header section.
func TestTrailersAreNotMergedIntoHeaders(t *testing.T) {
	req, _, err := parse(t, "POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nAuthorization: forged\r\nX-Sum: 1\r\n\r\n", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(req.Body); err != nil {
		t.Fatal(err)
	}
	if req.Header.Has("Authorization") || req.Header.Has("X-Sum") {
		t.Fatalf("trailer leaked into header section: %v", req.Header)
	}
	if req.Trailer.Get("X-Sum") != "1" {
		t.Fatalf("trailer lost: %v", req.Trailer)
	}
}

func TestLeadingEmptyLinesAndExpectOn10(t *testing.T) {
	// RFC 9112 §2.2: ignore at least one empty line before the request-line.
	req, _, err := parse(t, "\r\nGET /x HTTP/1.1\r\nHost: h\r\n\r\n", Limits{})
	if err != nil || req.Path != "/x" {
		t.Fatalf("leading CRLF: %v %+v", err, req)
	}
	// RFC 9110 §10.1.1: a 100-continue expectation in HTTP/1.0 is ignored.
	req, _, err = parse(t, "POST / HTTP/1.0\r\nExpect: 100-continue\r\nContent-Length: 1\r\n\r\nx", Limits{})
	if err != nil || req.Expect100 {
		t.Fatalf("HTTP/1.0 Expect must be ignored: %v %+v", err, req)
	}
	// Absolute-form without a path still yields the authority as Host.
	req, _, err = parse(t, "GET http://example.org HTTP/1.1\r\n\r\n", Limits{})
	if err != nil || req.Path != "/" || req.Header.Get("Host") != "example.org" {
		t.Fatalf("absolute-form without path: %v %+v", err, req)
	}
}

// Header values written by handlers (templates can include request data)
// must not be able to inject header lines or split the response.
func TestResponseHeaderInjectionIsNeutralised(t *testing.T) {
	var out strings.Builder
	bw := bufio.NewWriter(&out)
	req, _, _ := parse(t, "GET / HTTP/1.1\r\nHost: h\r\n\r\n", Limits{})
	rw := NewResponseWriter(bw, req)
	rw.Header().Set("Location", "/x\r\nSet-Cookie: pwned=1\r\n\r\n<html>")
	rw.Header().Set("Bad\r\nName", "v")
	rw.Respond(302, "text/plain", []byte("moved"))
	got := out.String()
	head, _, _ := strings.Cut(got, "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n")[1:] {
		if strings.HasPrefix(line, "Set-Cookie") || strings.HasPrefix(line, "Name") || strings.Contains(line, "\n") || strings.Contains(line, "\r") {
			t.Fatalf("header injection: %q", got)
		}
	}
	if !strings.Contains(head, "Location: /x  Set-Cookie: pwned=1    <html>") {
		t.Fatalf("Location should keep its text with CR/LF replaced by spaces: %q", head)
	}
}
