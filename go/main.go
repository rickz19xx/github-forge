package main

// main.go — the entry's lifecycle, owned by the server itself.
//
// The contract with the env is four steps and one guarantee:
//
//  1. MAKEFUN_ENV_URL is the env's origin. No env, nothing to serve: exit 1 straight away.
//  2. Listen on port 0 and let the kernel choose. The env owns 8080; this process never takes it.
//  3. POST /register {"port": N} says where the entry is listening. The env keeps the first
//     register it accepts; a 409 means someone else got there first, which is not a failure.
//  4. Poll GET /done until it answers {"done": true}; only then close the listener and exit 0, so
//     the env's export (what the scorer reads) is written after every request has been answered.
//
// Retries are exponential and capped: a register or a poll that fails is waited out, never
// treated as fatal, because the env may still be coming up when this process starts.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	// envRequestTimeout bounds one register or done request before it is abandoned and retried.
	envRequestTimeout = 30 * time.Second
	// retryFirst is the first wait between retries, and retryCeiling the last.
	retryFirst   = 200 * time.Millisecond
	retryCeiling = 2 * time.Second
)

func main() {
	// The forge's heap is small and steady (the store, the reply caches, per-connection
	// buffers): a higher GC target trades a few MiB of resident memory for fewer GC cycles
	// on the request path. The pod's limit is 1 GiB; this stays far below it.
	debug.SetGCPercent(300)

	origin := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if origin == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is unset: there is no env to register with")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] the server could not bind an ephemeral TCP port: %v\n", err)
		os.Exit(1)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "[entry] the server did not bind an ephemeral TCP port")
		os.Exit(1)
	}

	srv := newServer()
	go srv.ListenAndServe(listener)
	fmt.Printf("[entry] listening on port %d\n", address.Port)

	if err := announcePort(origin, address.Port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	if err := awaitTrialEnd(origin); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	listener.Close()
	fmt.Println("[entry] done")
	os.Exit(0)
}

// backoff is the wait after attempt failures: 200 ms, 400 ms, ... capped at two seconds.
func backoff(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	wait := retryFirst << uint(attempt)
	if wait > retryCeiling {
		wait = retryCeiling
	}
	return wait
}

// announcePort posts our port to the env until it answers 200 (accepted) or 409 (another entry
// won the race).
func announcePort(origin string, port int) error {
	payload := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	for attempt := 0; ; attempt++ {
		status, body, err := envCall(origin, "POST", "/register", payload)
		if err == nil {
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", status, body)
			}
		}
		time.Sleep(backoff(attempt))
	}
}

// awaitTrialEnd polls GET /done until the env reports the trial finished.
func awaitTrialEnd(origin string) error {
	for attempt := 0; ; attempt++ {
		status, body, err := envCall(origin, "GET", "/done", nil)
		if err == nil && status == 200 {
			var answer struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(body, &answer) == nil && answer.Done {
				return nil
			}
		}
		time.Sleep(backoff(attempt))
	}
}

// envCall is one small HTTP/1.1 request to the env, answered whole.
func envCall(origin, method, path string, payload []byte) (int, []byte, error) {
	hostPart := origin
	if index := strings.Index(hostPart, "://"); index >= 0 {
		hostPart = hostPart[index+3:]
	}
	prefix := ""
	if index := strings.IndexByte(hostPart, '/'); index >= 0 {
		prefix = hostPart[index:]
		hostPart = hostPart[:index]
	}
	conn, err := net.DialTimeout("tcp", hostPart, 5*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(envRequestTimeout))

	request := make([]byte, 0, 256+len(payload))
	request = append(request, method...)
	request = append(request, ' ')
	request = append(request, prefix...)
	request = append(request, path...)
	request = append(request, " HTTP/1.1\r\nHost: "...)
	request = append(request, hostPart...)
	request = append(request, "\r\nconnection: close\r\n"...)
	if payload != nil {
		request = append(request, "content-type: application/json\r\ncontent-length: "...)
		request = appendInt(request, len(payload))
		request = append(request, "\r\n"...)
	}
	request = append(request, "\r\n"...)
	request = append(request, payload...)
	if _, err := conn.Write(request); err != nil {
		return 0, nil, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return 0, nil, err
	}
	return parseEnvResponse(raw)
}

// parseEnvResponse splits one HTTP response into its status and body.
func parseEnvResponse(raw []byte) (int, []byte, error) {
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return 0, nil, fmt.Errorf("the env's answer had no header block")
	}
	head := raw[:headerEnd]
	body := raw[headerEnd+4:]
	lineEnd := bytes.IndexByte(head, '\r')
	if lineEnd < 0 {
		lineEnd = len(head)
	}
	fields := bytes.SplitN(head[:lineEnd], []byte(" "), 3)
	if len(fields) < 2 {
		return 0, nil, fmt.Errorf("the env's answer had no status line")
	}
	status, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0, nil, fmt.Errorf("the env's answer had no status code")
	}
	chunked := false
	length := -1
	for _, line := range bytes.Split(head[lineEnd+2:], []byte("\r\n")) {
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(string(bytes.TrimSpace(line[:colon])))
		value := strings.TrimSpace(string(line[colon+1:]))
		switch name {
		case "transfer-encoding":
			chunked = strings.Contains(strings.ToLower(value), "chunked")
		case "content-length":
			if parsed, err := strconv.Atoi(value); err == nil {
				length = parsed
			}
		}
	}
	if chunked {
		return status, decodeChunkedBody(body), nil
	}
	if length >= 0 && length <= len(body) {
		return status, body[:length], nil
	}
	return status, body, nil
}

// decodeChunkedBody decodes a chunked body from a whole buffer.
func decodeChunkedBody(body []byte) []byte {
	out := make([]byte, 0, len(body))
	for {
		lineEnd := bytes.Index(body, []byte("\r\n"))
		if lineEnd < 0 {
			return out
		}
		size := 0
		for _, c := range body[:lineEnd] {
			value := hexVal(c)
			if value < 0 {
				if c == ';' {
					break
				}
				return out
			}
			size = size<<4 | value
		}
		body = body[lineEnd+2:]
		if size == 0 {
			return out
		}
		if size > len(body) {
			return out
		}
		out = append(out, body[:size]...)
		body = body[size:]
		if len(body) >= 2 {
			body = body[2:]
		}
	}
}
