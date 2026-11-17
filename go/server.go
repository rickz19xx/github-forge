package main

// server.go — a small HTTP/1.1 server over net.Listener and one bufio.Reader per connection.
//
// net/http is a general-purpose server: a streaming parser, a Request per request, a ResponseWriter
// with a header map, chunked encoding, upgrades. This entry needs none of that. Every reply is one
// status, a handful of headers and a byte slice, and every request body is small enough to hold
// whole. So the fast path is: find the header terminator, parse the fields the router reads, hand
// over one slice, write one header block and one body in a single write.
//
// Supported, because the suite and any HTTP/1.1 client may use it:
//   - keep-alive (the default) and `Connection: close`; pipelined requests are answered in order,
//   - Content-Length bodies and `Transfer-Encoding: chunked` bodies (decoded into one slice),
//   - `Expect: 100-continue` (an interim 100 is written before the body is read),
//   - absolute-form request targets (`GET http://host/path HTTP/1.1`, as a proxy would send),
//   - HEAD (the headers of the GET reply, no body).
// Anything else that cannot be parsed is answered with a JSON 400 and the connection is closed:
// the process never dies on a malformed request.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
)

const (
	// headLimit is the largest header block this server answers (bufio's buffer, less the body).
	headLimit = 60 << 10
	// bodyLimit is the largest body it accepts: the biggest the suite sends is a pack of a few MiB.
	bodyLimit = 64 << 20
	// readBufferSize is one connection's reader: a request head plus some body reads.
	readBufferSize = 64 << 10
)

var (
	crlfcrlf   = []byte("\r\n\r\n")
	crlf       = []byte("\r\n")
	emptyBytes = []byte{}
)

// methodCode is the method the router switches on.
type methodCode uint8

const (
	methodOther methodCode = iota
	methodGet
	methodHead
	methodPost
	methodPut
	methodPatch
	methodDelete
)

func classifyMethod(method []byte) methodCode {
	switch len(method) {
	case 3:
		if bytes.Equal(method, []byte("GET")) {
			return methodGet
		}
		if bytes.Equal(method, []byte("PUT")) {
			return methodPut
		}
	case 4:
		if bytes.Equal(method, []byte("HEAD")) {
			return methodHead
		}
		if bytes.Equal(method, []byte("POST")) {
			return methodPost
		}
	case 5:
		if bytes.Equal(method, []byte("PATCH")) {
			return methodPatch
		}
	case 6:
		if bytes.Equal(method, []byte("DELETE")) {
			return methodDelete
		}
	}
	return methodOther
}

// request is one parsed request: only the fields any handler reads.
type request struct {
	method    methodCode
	methodRaw []byte
	path      []byte
	query     []byte
	auth      []byte
	hasAuth   bool
	host      []byte
	protocol  []byte
	body      []byte
	minor     int
	close     bool
	chunked   bool
	expect    bool
	length    int
}

// header is one extra reply header.
type header struct {
	name  string
	value string
}

// response is one reply, as bytes to put on the wire.
type response struct {
	status       int
	ctype        string
	body         []byte
	extra        []header
	cacheControl bool
	// entityLength, when positive, is the length the head reports instead of the body's (a HEAD
	// reply names the GET body's length).
	entityLength int
	// cached is a prebuilt reply: its head already carries the fixed headers and a slot for the
	// request id, so answering is one copy and one hex fill.
	cached *cachedReply
}

// Server owns the store and answers requests.
type Server struct {
	forge   *Forge
	token   []byte
	idCount uint64
	stats    bool
	builds   atomic.Uint64
	hits     atomic.Uint64
	accepts  atomic.Uint64
	requests atomic.Uint64
}

func newServer() *Server {
	return &Server{
		forge: newForge(),
		token: []byte(trimSpace(os.Getenv("MAKEFUN_SHOP_TOKEN"))),
		stats: os.Getenv("GHFORGE_STATS") == "1",
	}
}

func trimSpace(value string) string {
	start := 0
	end := len(value)
	for start < end && (value[start] == ' ' || value[start] == '\t' || value[start] == '\n' || value[start] == '\r') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\t' || value[end-1] == '\n' || value[end-1] == '\r') {
		end--
	}
	return value[start:end]
}

// ListenAndServe accepts connections forever.
func (s *Server) ListenAndServe(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

// conn is one client connection and its reusable buffers.
type conn struct {
	net.Conn
	reader  *bufio.Reader
	headBuf []byte
	bodyBuf []byte
	scratch []byte
	request request
}

func (s *Server) serveConn(raw net.Conn) {
	s.accepts.Add(1)
	if tcp, ok := raw.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	c := &conn{
		Conn:    raw,
		reader:  bufio.NewReaderSize(raw, readBufferSize),
		headBuf: make([]byte, 0, 2048),
		bodyBuf: make([]byte, 0, 4096),
		scratch: make([]byte, 0, 8192),
	}
	defer func() {
		// A peer that goes away mid-reply is not this process's problem.
		_ = recover()
		raw.Close()
	}()
	for {
		if !c.serveOne(s) {
			return
		}
	}
}

// serveOne reads, answers and reports whether the connection stays open.
func (c *conn) serveOne(s *Server) bool {
	req := &c.request
	*req = request{}
	if !c.readHead(req) {
		return false
	}
	if req.expect {
		if _, err := c.Conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return false
		}
	}
	if !c.readBody(req) {
		return false
	}

	var reply response
	failed := false
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				writeStderr("[entry] request failed: ", recovered)
				reply = jsonReply(500, []byte(`{"message":"Internal Server Error"}`))
				failed = true
			}
		}()
		reply = s.route(req)
	}()
	if failed {
		req.close = true
	}

	id := atomic.AddUint64(&s.idCount, 1)
	requests := s.requests.Add(1)
	if s.stats && requests%200 == 0 {
		fmt.Fprintf(os.Stderr, "[stat] requests=%d accepts=%d builds=%d hits=%d\n", requests, s.accepts.Load(), s.builds.Load(), s.hits.Load())
	}
	if err := c.writeResponse(&reply, req.method == methodHead, id); err != nil {
		return false
	}
	if req.close {
		return false
	}
	return true
}

func writeStderr(parts ...any) {
	for _, part := range parts {
		_, _ = os.Stderr.WriteString(toString(part))
	}
	_, _ = os.Stderr.WriteString("\n")
}

func toString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if err, ok := value.(error); ok {
		return err.Error()
	}
	return "error"
}

// expectContinue reports whether the request asked for an interim 100.
func (r *request) expectContinue() bool {
	return r.expect
}

// readHead fills r from the next request head; false closes the connection.
func (c *conn) readHead(r *request) bool {
	buffered := c.reader.Buffered()
	for {
		buf, _ := c.reader.Peek(buffered)
		if end := bytes.Index(buf, crlfcrlf); end >= 0 {
			c.headBuf = append(c.headBuf[:0], buf[:end]...)
			_, _ = c.reader.Discard(end + 4)
			if err := parseHead(c.headBuf, r); err != nil {
				c.fail(400, err.Error())
				return false
			}
			return true
		}
		if buffered >= headLimit {
			c.fail(400, "the request head is larger than this server accepts")
			return false
		}
		if _, err := c.reader.Peek(buffered + 1); err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				c.fail(400, "the request head is larger than this server accepts")
			}
			return false
		}
		buffered = c.reader.Buffered()
	}
}

// readBody reads the body a request announced, into one slice.
func (c *conn) readBody(r *request) bool {
	if r.chunked {
		body, ok := c.readChunked()
		if !ok {
			return false
		}
		r.body = body
		return true
	}
	if r.length <= 0 {
		r.body = emptyBytes
		return true
	}
	if cap(c.bodyBuf) < r.length {
		c.bodyBuf = make([]byte, r.length)
	}
	c.bodyBuf = c.bodyBuf[:r.length]
	if _, err := io.ReadFull(c.reader, c.bodyBuf); err != nil {
		return false
	}
	r.body = c.bodyBuf
	return true
}

// readChunked decodes a chunked body into the connection's body buffer.
func (c *conn) readChunked() ([]byte, bool) {
	body := c.bodyBuf[:0]
	line := make([]byte, 0, 64)
	for {
		raw, err := c.reader.ReadSlice('\n')
		if err != nil {
			return nil, false
		}
		line = append(line[:0], raw...)
		line = bytes.TrimSuffix(line, crlf)
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size := 0
		for _, ch := range line {
			value := hexVal(ch)
			if value < 0 {
				c.fail(400, "a chunk size is not hexadecimal")
				return nil, false
			}
			size = size<<4 | value
			if size > bodyLimit {
				c.fail(413, "the body is larger than this server accepts")
				return nil, false
			}
		}
		if size == 0 {
			for {
				trailer, err := c.reader.ReadSlice('\n')
				if err != nil {
					return nil, false
				}
				if len(bytes.TrimRight(trailer, "\r\n")) == 0 {
					c.bodyBuf = body
					return body, true
				}
			}
		}
		if len(body)+size > bodyLimit {
			c.fail(413, "the body is larger than this server accepts")
			return nil, false
		}
		start := len(body)
		if cap(body) < start+size+2 {
			grown := make([]byte, start+size+2, 2*(start+size+2)+256)
			copy(grown, body)
			body = grown
		}
		body = body[:start+size]
		if _, err := io.ReadFull(c.reader, body[start:]); err != nil {
			return nil, false
		}
		var terminator [2]byte
		if _, err := io.ReadFull(c.reader, terminator[:]); err != nil {
			return nil, false
		}
	}
}

// fail answers one parse failure and gives up on the connection.
func (c *conn) fail(status int, message string) {
	reply := jsonReply(status, []byte(`{"message":`+quoteJSON(message)+`}`))
	_ = c.writeResponse(&reply, false, 1)
}

// quoteJSON renders s as a JSON string.
func quoteJSON(s string) string {
	return string(appendJSONString(nil, s))
}

// writeResponse puts one reply on the wire in one write.
func (c *conn) writeResponse(reply *response, isHead bool, id uint64) error {
	out := c.scratch[:0]
	hide := isHead || reply.status == 204
	if reply.cached != nil {
		out = append(out, reply.cached.head...)
		putHex20(out[reply.cached.headIDOff:], id)
		if !hide {
			out = append(out, reply.cached.body...)
		}
	} else {
		out = appendStatus(out, reply.status)
		if reply.ctype != "" {
			out = append(out, "content-type: "...)
			out = append(out, reply.ctype...)
			out = append(out, '\r', '\n')
		}
		length := len(reply.body)
		if reply.entityLength > 0 {
			length = reply.entityLength
		}
		out = append(out, "content-length: "...)
		out = appendInt(out, length)
		out = append(out, "\r\nx-github-request-id: "...)
		var idBytes [20]byte
		putHex20(idBytes[:], id)
		out = append(out, idBytes[:]...)
		out = append(out, '\r', '\n')
		if reply.cacheControl {
			out = append(out, "cache-control: no-cache, max-age=0, must-revalidate\r\n"...)
		}
		for _, extra := range reply.extra {
			out = append(out, extra.name...)
			out = append(out, ": "...)
			out = append(out, extra.value...)
			out = append(out, '\r', '\n')
		}
		out = append(out, '\r', '\n')
		if !hide && len(reply.body) > 0 {
			out = append(out, reply.body...)
		}
	}
	_, err := c.Conn.Write(out)
	c.scratch = out[:0]
	return err
}

func appendStatus(dst []byte, status int) []byte {
	dst = append(dst, "HTTP/1.1 "...)
	dst = appendInt(dst, status)
	dst = append(dst, ' ')
	dst = append(dst, reasonFor(status)...)
	return append(dst, '\r', '\n')
}

func appendInt(dst []byte, value int) []byte {
	var buf [20]byte
	i := len(buf)
	if value == 0 {
		return append(dst, '0')
	}
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return append(dst, buf[i:]...)
}

// putHex20 writes v as 20 lowercase hex digits, left padded with zeros.
func putHex20(dst []byte, v uint64) {
	_ = dst[19]
	for i := 19; i >= 0; i-- {
		dst[i] = jsonHex[v&0xf]
		v >>= 4
	}
}

func reasonFor(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 301:
		return "Moved Permanently"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 413:
		return "Payload Too Large"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	}
	return "Unknown"
}

// parseHead parses a request line and its headers (the block before the blank line).
func parseHead(head []byte, r *request) error {
	lineEnd := bytes.Index(head, crlf)
	if lineEnd < 0 {
		lineEnd = len(head)
	}
	line := head[:lineEnd]
	firstSpace := bytes.IndexByte(line, ' ')
	if firstSpace <= 0 {
		return errors.New("the request line has no method")
	}
	secondSpace := -1
	if rest := line[firstSpace+1:]; len(rest) > 0 {
		if index := bytes.IndexByte(rest, ' '); index >= 0 {
			secondSpace = firstSpace + 1 + index
		}
	}
	r.methodRaw = line[:firstSpace]
	r.method = classifyMethod(r.methodRaw)
	target := line[firstSpace+1:]
	version := emptyBytes
	if secondSpace >= 0 {
		target = line[firstSpace+1 : secondSpace]
		version = line[secondSpace+1:]
	}
	if len(target) == 0 {
		return errors.New("the request line has no target")
	}
	if len(version) > 0 && !bytes.HasPrefix(version, []byte("HTTP/1.")) {
		return errors.New("unsupported protocol")
	}
	if bytes.HasPrefix(target, []byte("http://")) || bytes.HasPrefix(target, []byte("https://")) {
		scheme := bytes.Index(target, []byte("//"))
		if slash := bytes.IndexByte(target[scheme+2:], '/'); slash >= 0 {
			target = target[scheme+2+slash:]
		} else {
			target = []byte("/")
		}
	}
	if mark := bytes.IndexByte(target, '?'); mark >= 0 {
		r.path = target[:mark]
		r.query = target[mark+1:]
	} else {
		r.path = target
		r.query = emptyBytes
	}
	r.minor = 1
	if bytes.Equal(version, []byte("HTTP/1.0")) {
		r.minor = 0
		r.close = true
	}
	r.length = -1
	r.chunked = false

	at := lineEnd + 2
	for at < len(head) {
		end := bytes.Index(head[at:], crlf)
		stop := len(head)
		next := len(head)
		if end >= 0 {
			stop = at + end
			next = stop + 2
		}
		headerLine := head[at:stop]
		at = next
		if len(headerLine) == 0 {
			continue
		}
		colon := bytes.IndexByte(headerLine, ':')
		if colon < 0 {
			return errors.New("a header line has no colon")
		}
		name := headerLine[:colon]
		value := headerLine[colon+1:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch {
		case headerIs(name, "content-length"):
			length := 0
			for _, ch := range value {
				if ch < '0' || ch > '9' {
					return errors.New("content-length is not a byte count")
				}
				length = length*10 + int(ch-'0')
				if length > bodyLimit {
					return errors.New("the body is larger than this server accepts")
				}
			}
			r.length = length
		case headerIs(name, "authorization"):
			r.auth = value
			r.hasAuth = true
		case headerIs(name, "host"):
			r.host = value
		case headerIs(name, "connection"):
			if containsFold(value, "close") {
				r.close = true
			} else if containsFold(value, "keep-alive") {
				r.close = false
			}
		case headerIs(name, "git-protocol"):
			r.protocol = value
		case headerIs(name, "transfer-encoding"):
			if !containsFold(value, "chunked") {
				return errors.New("unsupported transfer-encoding")
			}
			r.chunked = true
			r.length = -1
		case headerIs(name, "expect"):
			if containsFold(value, "100-continue") {
				r.expect = true
			}
		}
	}
	return nil
}

// headerIs compares a header name with a lowercase literal, case-insensitively.
func headerIs(name []byte, want string) bool {
	if len(name) != len(want) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

// containsFold reports whether value contains want, ASCII case-insensitively.
func containsFold(value []byte, want string) bool {
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(value); i++ {
		match := true
		for j := 0; j < len(want); j++ {
			c := value[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
