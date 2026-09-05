// Package http1 is Spindrift's own HTTP/1.1 implementation: a request parser
// with bounded headers and bodies (Content-Length and chunked), and a
// response writer that does fixed-length, chunked, and close-delimited
// bodies, HEAD, keep-alive negotiation and 100-continue.
//
// It does not depend on net/http. Everything here is strict where the RFCs
// allow strictness (RFC 9112): CRLF line endings, no bare LF, no leading
// whitespace on the request line, no obs-fold, single Content-Length,
// Transfer-Encoding wins over Content-Length.
package http1

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Errors map to the status the server should answer with.
var (
	ErrMalformed          = errors.New("malformed request")          // 400
	ErrHeaderTooLarge     = errors.New("request header too large")   // 431
	ErrBodyTooLarge       = errors.New("request body too large")     // 413
	ErrUnsupportedVersion = errors.New("HTTP version not supported") // 505
	ErrLengthRequired     = errors.New("length required")            // 411
	ErrURITooLong         = errors.New("request target too long")    // 414
)

// Limits bound what the parser will accept.
type Limits struct {
	MaxHeaderBytes int   // request line + headers, default 64 KiB
	MaxBodyBytes   int64 // default 8 MiB; 0 means unlimited
	MaxTargetBytes int   // default 8 KiB
}

func (l Limits) withDefaults() Limits {
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = 64 << 10
	}
	if l.MaxBodyBytes == 0 {
		l.MaxBodyBytes = 8 << 20
	}
	if l.MaxTargetBytes <= 0 {
		l.MaxTargetBytes = 8 << 10
	}
	return l
}

// Header is a canonical-key header map (Content-Type, not content-type).
type Header map[string][]string

func (h Header) Get(k string) string {
	v := h[textproto.CanonicalMIMEHeaderKey(k)]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
func (h Header) Set(k, v string)   { h[textproto.CanonicalMIMEHeaderKey(k)] = []string{v} }
func (h Header) Add(k, v string)   { k = textproto.CanonicalMIMEHeaderKey(k); h[k] = append(h[k], v) }
func (h Header) Del(k string)      { delete(h, textproto.CanonicalMIMEHeaderKey(k)) }
func (h Header) Has(k string) bool { _, ok := h[textproto.CanonicalMIMEHeaderKey(k)]; return ok }

// Request is one parsed request. Body is always non-nil and reads exactly
// the declared body (empty for none); the caller must drain it before
// reading the next request on the connection.
type Request struct {
	Method   string
	Target   string // raw request-target as sent
	Path     string // origin-form path (before '?'), unescaped by the router, not here
	RawQuery string
	Major    int
	Minor    int
	Header   Header
	Body     io.Reader
	// ContentLength is -1 for chunked bodies.
	ContentLength int64
	// Close reports whether the connection must close after the response
	// (HTTP/1.0 without keep-alive, or Connection: close).
	Close bool
	// Expect100 is set when the client sent `Expect: 100-continue`.
	Expect100  bool
	RemoteAddr string
}

func (r *Request) Proto() string { return fmt.Sprintf("HTTP/%d.%d", r.Major, r.Minor) }

// ReadRequest parses one request from br. Returns io.EOF when the
// connection closed cleanly between requests.
func ReadRequest(br *bufio.Reader, lim Limits) (*Request, error) {
	lim = lim.withDefaults()
	budget := lim.MaxHeaderBytes

	line, err := readLine(br, &budget)
	if err != nil {
		if errors.Is(err, io.EOF) && budget == lim.MaxHeaderBytes {
			return nil, io.EOF
		}
		return nil, err
	}
	if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
		return nil, ErrMalformed
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	req := &Request{Method: parts[0], Target: parts[1], Header: Header{}}
	if !validToken(req.Method) {
		return nil, ErrMalformed
	}
	if len(req.Target) > lim.MaxTargetBytes {
		return nil, ErrURITooLong
	}
	if req.Target == "" || strings.ContainsAny(req.Target, " \t\r\n") {
		return nil, ErrMalformed
	}
	switch parts[2] {
	case "HTTP/1.1":
		req.Major, req.Minor = 1, 1
	case "HTTP/1.0":
		req.Major, req.Minor = 1, 0
	default:
		if strings.HasPrefix(parts[2], "HTTP/") {
			return nil, ErrUnsupportedVersion
		}
		return nil, ErrMalformed
	}
	if req.Method == "CONNECT" {
		return nil, ErrMalformed // not supported; keeps the router simple
	}
	if strings.HasPrefix(req.Target, "/") {
		req.Path, req.RawQuery, _ = strings.Cut(req.Target, "?")
	} else if req.Target == "*" && req.Method == "OPTIONS" {
		req.Path = "*"
	} else if i := strings.Index(req.Target, "://"); i > 0 {
		// absolute-form: scheme://host/path?query
		rest := req.Target[i+3:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			req.Path = "/"
		} else {
			req.Path, req.RawQuery, _ = strings.Cut(rest[slash:], "?")
			host := rest[:slash]
			req.Header.Set("Host", host)
		}
	} else {
		return nil, ErrMalformed
	}

	for {
		line, err := readLine(br, &budget)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, ErrMalformed
			}
			return nil, err
		}
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, ErrMalformed // obs-fold is rejected (RFC 9112 §5.2)
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !validToken(name) {
			return nil, ErrMalformed
		}
		value = strings.Trim(value, " \t")
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, ErrMalformed
		}
		req.Header.Add(name, value)
	}

	if req.Major == 1 && req.Minor == 1 && !req.Header.Has("Host") {
		return nil, ErrMalformed
	}

	// Connection semantics.
	conn := strings.ToLower(req.Header.Get("Connection"))
	if req.Minor == 0 {
		req.Close = !strings.Contains(conn, "keep-alive")
	} else {
		req.Close = strings.Contains(conn, "close")
	}
	req.Expect100 = strings.EqualFold(req.Header.Get("Expect"), "100-continue")

	// Body framing (RFC 9112 §6).
	te := req.Header["Transfer-Encoding"]
	cls := req.Header["Content-Length"]
	switch {
	case len(te) > 0:
		if len(te) != 1 || !strings.EqualFold(strings.TrimSpace(te[0]), "chunked") {
			return nil, ErrMalformed // only a lone "chunked" is accepted
		}
		if len(cls) > 0 {
			req.Header.Del("Content-Length") // TE wins; the request is suspicious
		}
		req.ContentLength = -1
		req.Body = &chunkedReader{br: br, max: lim.MaxBodyBytes, trailers: req.Header}
	case len(cls) > 0:
		n, err := parseContentLength(cls)
		if err != nil {
			return nil, ErrMalformed
		}
		if lim.MaxBodyBytes > 0 && n > lim.MaxBodyBytes {
			return nil, ErrBodyTooLarge
		}
		req.ContentLength = n
		req.Body = &lengthReader{r: br, n: n}
	default:
		req.ContentLength = 0
		req.Body = bytes.NewReader(nil)
	}
	return req, nil
}

// readLine reads one CRLF-terminated line, charging its bytes to *budget.
func readLine(br *bufio.Reader, budget *int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		*budget -= len(chunk)
		if *budget < 0 {
			return "", ErrHeaderTooLarge
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return "", io.EOF
		}
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", ErrMalformed // bare LF
	}
	return string(line[:len(line)-2]), nil
}

func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 127 || strings.IndexByte("()<>@,;:\\\"/[]?={}", c) >= 0 {
			return false
		}
	}
	return true
}

func parseContentLength(vals []string) (int64, error) {
	first := strings.TrimSpace(vals[0])
	for _, v := range vals[1:] {
		if strings.TrimSpace(v) != first {
			return 0, ErrMalformed // conflicting lengths
		}
	}
	if first == "" || first[0] == '+' || first[0] == '-' {
		return 0, ErrMalformed
	}
	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil || n < 0 {
		return 0, ErrMalformed
	}
	return n, nil
}

type lengthReader struct {
	r io.Reader
	n int64
}

func (l *lengthReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if err == io.EOF && l.n > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// chunkedReader decodes chunked transfer coding, enforcing the body limit
// on decoded bytes and collecting trailers into the request header.
type chunkedReader struct {
	br       *bufio.Reader
	max      int64
	total    int64
	remain   int64
	done     bool
	err      error
	trailers Header
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.done {
		return 0, io.EOF
	}
	if c.remain == 0 {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return 0, c.fail(io.ErrUnexpectedEOF)
		}
		line = strings.TrimRight(line, "\r\n")
		if ext := strings.IndexByte(line, ';'); ext >= 0 {
			line = line[:ext] // chunk extensions are ignored
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 {
			return 0, c.fail(ErrMalformed)
		}
		if size == 0 {
			// trailers until an empty line
			for {
				t, err := c.br.ReadString('\n')
				if err != nil {
					return 0, c.fail(io.ErrUnexpectedEOF)
				}
				t = strings.TrimRight(t, "\r\n")
				if t == "" {
					break
				}
				if name, value, ok := strings.Cut(t, ":"); ok && validToken(name) {
					c.trailers.Add(name, strings.TrimSpace(value))
				}
			}
			c.done = true
			return 0, io.EOF
		}
		if c.max > 0 && c.total+size > c.max {
			return 0, c.fail(ErrBodyTooLarge)
		}
		c.remain = size
	}
	if int64(len(p)) > c.remain {
		p = p[:c.remain]
	}
	n, err := c.br.Read(p)
	c.remain -= int64(n)
	c.total += int64(n)
	if err != nil {
		return n, c.fail(io.ErrUnexpectedEOF)
	}
	if c.remain == 0 {
		crlf := make([]byte, 2)
		if _, err := io.ReadFull(c.br, crlf); err != nil || crlf[0] != '\r' || crlf[1] != '\n' {
			return n, c.fail(ErrMalformed)
		}
	}
	return n, nil
}

func (c *chunkedReader) fail(err error) error { c.err = err; return err }

// ---------------------------------------------------------------- writer --

// ResponseWriter writes one response. Call WriteHeader (or let the first
// Write do it), Write any number of times, then Finish.
type ResponseWriter struct {
	w           *bufio.Writer
	req         *Request
	status      int
	header      Header
	wroteHeader bool
	chunked     bool
	head        bool
	closeAfter  bool
	written     int64
	finished    bool
}

func NewResponseWriter(w *bufio.Writer, req *Request) *ResponseWriter {
	return &ResponseWriter{w: w, req: req, header: Header{}, head: req != nil && req.Method == "HEAD", closeAfter: req == nil || req.Close}
}

func (rw *ResponseWriter) Header() Header   { return rw.header }
func (rw *ResponseWriter) Status() int      { return rw.status }
func (rw *ResponseWriter) Written() int64   { return rw.written }
func (rw *ResponseWriter) HeaderSent() bool { return rw.wroteHeader }

// CloseAfter reports whether the connection must be closed once Finish
// returns: the request asked for it, or the body was close-delimited.
func (rw *ResponseWriter) CloseAfter() bool { return rw.closeAfter }

// WriteContinue sends an interim `100 Continue`.
func (rw *ResponseWriter) WriteContinue() error {
	if rw.wroteHeader {
		return nil
	}
	_, err := rw.w.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
	if err == nil {
		err = rw.w.Flush()
	}
	return err
}

func (rw *ResponseWriter) WriteHeader(status int) {
	if rw.wroteHeader {
		return
	}
	rw.wroteHeader = true
	rw.status = status
	h := rw.header
	if !h.Has("Date") {
		h.Set("Date", nowHTTP())
	}
	noBody := status/100 == 1 || status == 204 || status == 304 || rw.head
	if !noBody && !h.Has("Content-Length") && !h.Has("Transfer-Encoding") {
		if rw.req != nil && rw.req.Minor >= 1 {
			rw.chunked = true
			h.Set("Transfer-Encoding", "chunked")
		} else {
			rw.closeAfter = true // HTTP/1.0: close delimits the body
		}
	}
	if rw.head && !h.Has("Content-Length") && !h.Has("Transfer-Encoding") {
		// A HEAD response should say how long the body would be; leave it
		// to the handler if it knows, otherwise omit.
	}
	if rw.closeAfter {
		h.Set("Connection", "close")
	} else if rw.req != nil && rw.req.Minor == 0 {
		h.Set("Connection", "keep-alive")
	}
	fmt.Fprintf(rw.w, "HTTP/1.1 %d %s\r\n", status, StatusText(status))
	for k, vs := range h {
		for _, v := range vs {
			rw.w.WriteString(k)
			rw.w.WriteString(": ")
			rw.w.WriteString(v)
			rw.w.WriteString("\r\n")
		}
	}
	rw.w.WriteString("\r\n")
}

func (rw *ResponseWriter) Write(p []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(200)
	}
	if rw.head || len(p) == 0 {
		return len(p), nil
	}
	rw.written += int64(len(p))
	if rw.chunked {
		if _, err := fmt.Fprintf(rw.w, "%x\r\n", len(p)); err != nil {
			return 0, err
		}
		if _, err := rw.w.Write(p); err != nil {
			return 0, err
		}
		_, err := rw.w.WriteString("\r\n")
		return len(p), err
	}
	return rw.w.Write(p)
}

// Flush pushes buffered bytes to the connection (streaming responses).
func (rw *ResponseWriter) Flush() error {
	if !rw.wroteHeader {
		rw.WriteHeader(200)
	}
	return rw.w.Flush()
}

// Finish terminates the body (last chunk for chunked) and flushes.
func (rw *ResponseWriter) Finish() error {
	if rw.finished {
		return nil
	}
	rw.finished = true
	if !rw.wroteHeader {
		rw.header.Set("Content-Length", "0")
		rw.WriteHeader(200)
	}
	if rw.chunked && !rw.head {
		rw.w.WriteString("0\r\n\r\n")
	}
	return rw.w.Flush()
}

// Respond writes a complete fixed-length response in one call.
func (rw *ResponseWriter) Respond(status int, contentType string, body []byte) error {
	if contentType != "" {
		rw.header.Set("Content-Type", contentType)
	}
	rw.header.Set("Content-Length", strconv.Itoa(len(body)))
	rw.WriteHeader(status)
	if _, err := rw.Write(body); err != nil {
		return err
	}
	return rw.Finish()
}

// StatusText returns the reason phrase for common status codes.
func StatusText(code int) string {
	if s, ok := statusText[code]; ok {
		return s
	}
	switch code / 100 {
	case 1:
		return "Informational"
	case 2:
		return "Success"
	case 3:
		return "Redirection"
	case 4:
		return "Client Error"
	default:
		return "Server Error"
	}
}

var statusText = map[int]string{
	100: "Continue", 101: "Switching Protocols",
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content", 206: "Partial Content",
	301: "Moved Permanently", 302: "Found", 303: "See Other", 304: "Not Modified", 307: "Temporary Redirect", 308: "Permanent Redirect",
	400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed",
	408: "Request Timeout", 409: "Conflict", 411: "Length Required", 413: "Content Too Large", 414: "URI Too Long",
	415: "Unsupported Media Type", 417: "Expectation Failed", 422: "Unprocessable Content", 429: "Too Many Requests", 431: "Request Header Fields Too Large",
	500: "Internal Server Error", 501: "Not Implemented", 502: "Bad Gateway", 503: "Service Unavailable", 504: "Gateway Timeout", 505: "HTTP Version Not Supported",
}

func nowHTTP() string { return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") }
