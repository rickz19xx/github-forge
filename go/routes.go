package main

// routes.go — the dispatcher: git smart HTTP under `/:owner/:repo.git`, the `/api/v3` REST subset,
// and a JSON 404 (never HTML) for anything else. Every reply carries an X-GitHub-Request-Id.
//
// One lock guards the whole store. A read (GET/HEAD, and a clone's upload-pack) takes it shared;
// everything that may mutate takes it exclusive, so fifty concurrent clones parse their packs and
// serve their answers at the same time while a push still sees one consistent store.

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
)

// route answers one parsed request.
func (s *Server) route(req *request) response {
	forge := s.forge
	readOnly := req.method == methodGet || req.method == methodHead || isUploadPackPath(req.path)
	if readOnly {
		forge.mu.RLock()
		defer forge.mu.RUnlock()
	} else {
		forge.mu.Lock()
		defer forge.mu.Unlock()
	}
	// A method that is not a read may change the store on any surface, so one note here
	// invalidates the cached read bodies before anything below can answer one.
	if req.method != methodGet && req.method != methodHead {
		forge.noteChange()
	}

	path := req.path

	// The env's boot check.
	if req.method == methodGet && bytes.Equal(path, []byte("/healthz")) {
		return jsonReply(200, []byte("{\"ok\":true}\n"))
	}

	// GitHub's GraphQL v4 API and the MCP endpoint: the agent surfaces (phase B).
	if bytes.Equal(path, []byte("/api/graphql")) {
		return s.handleGraphQL(req)
	}
	if bytes.Equal(path, []byte("/mcp")) || bytes.Equal(path, []byte("/mcp/")) {
		return s.handleMCP(req)
	}

	// The GitHub API surface.
	if bytes.Equal(path, []byte(apiPrefix)) || bytes.HasPrefix(path, []byte(apiPrefix+"/")) {
		var segmentBuf [24][]byte
		count := splitSegments(path[len(apiPrefix):], segmentBuf[:])
		login, authed := s.loginOf(req.auth, req.hasAuth)
		call := apiCall{
			method: req.method,
			segs:   segmentBuf[:count],
			query:  req.query,
			body:   req.body,
			login:  login,
			authed: authed,
			host:   req.host,
		}
		reply, err := s.handleAPI(&call)
		if err != nil {
			return messageJSON(err.status, err.message)
		}
		return reply
	}

	// git smart HTTP.
	if owner, name, rest, ok := parseGitPath(path); ok {
		repo := forge.get(owner, name)
		if bytes.Equal(rest, []byte("/info/refs")) && req.method == methodGet {
			service, _ := queryGet(req.query, "service")
			return s.handleInfoRefs(repo, service, req.protocol)
		}
		if bytes.Equal(rest, []byte("/git-upload-pack")) && req.method == methodPost {
			if repo == nil {
				return messageJSON(404, "Not Found")
			}
			reply, err := s.handleUploadPack(repo, req.body)
			if err != nil {
				return messageJSON(400, err.Error())
			}
			return reply
		}
		if bytes.Equal(rest, []byte("/git-receive-pack")) && req.method == methodPost {
			if repo == nil {
				return messageJSON(404, "Not Found")
			}
			reply, err := s.handleReceivePack(repo, req.body)
			if err != nil {
				return messageJSON(400, err.Error())
			}
			return reply
		}
		return messageJSON(404, "Not Found")
	}

	// Everything else, every method: GitHub's 404 body.
	return messageJSON(404, "Not Found")
}

// isUploadPackPath reports whether a request is a clone's upload-pack POST: read-only work that
// must not serialise behind the store lock.
func isUploadPackPath(path []byte) bool {
	return bytes.HasSuffix(path, []byte("/git-upload-pack"))
}

// parseGitPath splits `/:owner/:name.git(/rest)?` into its parts.
func parseGitPath(path []byte) (owner, name, rest []byte, ok bool) {
	if len(path) < 2 || path[0] != '/' {
		return nil, nil, nil, false
	}
	firstSlash := bytes.IndexByte(path[1:], '/')
	if firstSlash < 0 {
		return nil, nil, nil, false
	}
	firstSlash++
	if firstSlash <= 1 {
		return nil, nil, nil, false
	}
	owner = path[1:firstSlash]
	secondStart := firstSlash + 1
	secondEnd := len(path)
	if index := bytes.IndexByte(path[secondStart:], '/'); index >= 0 {
		secondEnd = secondStart + index
	}
	second := path[secondStart:secondEnd]
	dot := bytes.Index(second, []byte(".git"))
	if dot <= 0 {
		return nil, nil, nil, false
	}
	return owner, second[:dot], path[secondStart+dot+4:], true
}

// splitSegments splits a path into decoded segments, dropping empty ones.
func splitSegments(rest []byte, dst [][]byte) int {
	count := 0
	at := 0
	for at <= len(rest) && count < len(dst) {
		end := len(rest)
		if index := bytes.IndexByte(rest[at:], '/'); index >= 0 {
			end = at + index
		}
		if end > at {
			segment := rest[at:end]
			if bytes.IndexByte(segment, '%') >= 0 {
				segment = []byte(decodeComponent(segment))
			}
			dst[count] = segment
			count++
		}
		if end == len(rest) {
			break
		}
		at = end + 1
	}
	return count
}

// loginOf answers the login a request's Authorization header names.
func (s *Server) loginOf(auth []byte, has bool) (string, bool) {
	if !has {
		return "", false
	}
	value := auth
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	if len(value) >= 6 && (equalFoldPrefix(value, "token ") || equalFoldPrefix(value, "bearer ")) {
		value = value[6:]
		for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
			value = value[1:]
		}
	} else {
		value = bytes.TrimSpace(value)
		if len(value) == 0 {
			return "", false
		}
	}
	if len(s.token) == 0 || bytes.Equal(value, s.token) {
		return viewerLogin, true
	}
	hashed := sha1.Sum(append([]byte("ghforge-token/"), value...))
	return "ghost-" + hex.EncodeToString(hashed[:])[:12], true
}

// equalFoldPrefix reports whether value starts with want, ASCII case-insensitively.
func equalFoldPrefix(value []byte, want string) bool {
	if len(value) < len(want) {
		return false
	}
	for i := 0; i < len(want); i++ {
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}
