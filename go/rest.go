package main

// rest.go — the `/api/v3` REST surface (the GitHub v3 subset the suite drives).
//
// Covered here: the API root and meta, the token's user, repository create/read/patch/delete/list,
// the contents API (which is also how a repository gets its first commit), git refs, branches,
// commits, labels, issues with comments, and pull requests with their files and merges. Every
// reply is JSON with an X-GitHub-Request-Id, an absent token is 401, and an unknown route is a
// JSON 404.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

const apiPrefix = "/api/v3"

// apiError is a request-level failure the router turns into a JSON reply.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func badJSON() *apiError {
	return &apiError{status: 400, message: "Problems parsing JSON"}
}

// apiCall is one REST call, already split into what a handler needs.
type apiCall struct {
	method methodCode
	segs   [][]byte
	query  []byte
	body   []byte
	login  string
	authed bool
	host   []byte
}

// origin is the origin the request arrived on, e.g. `http://127.0.0.1:41234`.
func (c *apiCall) origin() string {
	if len(c.host) == 0 {
		return "http://127.0.0.1"
	}
	return "http://" + string(c.host)
}

func segEq(seg []byte, want string) bool {
	if len(seg) != len(want) {
		return false
	}
	for i := 0; i < len(seg); i++ {
		if seg[i] != want[i] {
			return false
		}
	}
	return true
}

func segAt(segs [][]byte, index int) []byte {
	if index < len(segs) {
		return segs[index]
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// replies
// ---------------------------------------------------------------------------------------------

func notFoundReply() response {
	return messageJSON(404, "Not Found")
}

func unauthorizedReply() response {
	return messageJSON(401, "Requires authentication")
}

func unprocessableReply(message string) response {
	return messageJSON(422, message)
}

func unprocessableErrors(message string, resources []string) response {
	body := make([]byte, 0, 128+48*len(resources))
	body = append(body, `{"message":`...)
	body = appendJSONString(body, message)
	body = append(body, `,"errors":[`...)
	for i, resource := range resources {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, `{"resource":`...)
		body = appendJSONString(body, resource)
		body = append(body, `,"code":"custom","field":"name"}`...)
	}
	body = append(body, `]}`...)
	body = append(body, '\n')
	return jsonReply(422, body)
}

// ---------------------------------------------------------------------------------------------
// users and repositories
// ---------------------------------------------------------------------------------------------

// appendUserJSON appends GitHub's user object for a login.
func appendUserJSON(dst []byte, login string) []byte {
	view := userViewFor(login)
	dst = append(dst, `{"login":`...)
	dst = appendJSONString(dst, view.login)
	dst = append(dst, `,"id":`...)
	dst = appendJSONInt(dst, int64(view.id))
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, "U_"+strconv.Itoa(view.id))
	dst = append(dst, `,"avatar_url":`...)
	dst = appendJSONString(dst, "https://avatars.example.invalid/u/"+strconv.Itoa(view.id))
	dst = append(dst, `,"gravatar_id":"","url":`...)
	dst = appendJSONString(dst, "https://api.example.invalid/users/"+view.login)
	dst = append(dst, `,"html_url":`...)
	dst = appendJSONString(dst, "https://example.invalid/"+view.login)
	dst = append(dst, `,"type":"User","site_admin":false}`...)
	return dst
}

// appendRepoJSON appends GitHub's repository object, cut to the fields the suite reads.
func appendRepoJSON(dst []byte, repo *Repo) []byte {
	dst = append(dst, `{"id":`...)
	dst = appendJSONInt(dst, int64(repo.id))
	dst = append(dst, `,"name":`...)
	dst = appendJSONString(dst, repo.name)
	dst = append(dst, `,"full_name":`...)
	dst = appendJSONString(dst, repo.owner+"/"+repo.name)
	dst = append(dst, `,"private":`...)
	if repo.isPrivate {
		dst = append(dst, "true"...)
	} else {
		dst = append(dst, "false"...)
	}
	dst = append(dst, `,"description":`...)
	dst = appendJSONString(dst, repo.description)
	dst = append(dst, `,"default_branch":`...)
	dst = appendJSONString(dst, defaultBranch)
	dst = append(dst, '}')
	return dst
}

// appendViewerJSON appends the token's own user object.
func appendViewerJSON(dst []byte, login string) []byte {
	view := userViewFor(login)
	dst = append(dst, `{"login":`...)
	dst = appendJSONString(dst, view.login)
	dst = append(dst, `,"id":`...)
	dst = appendJSONInt(dst, int64(view.id))
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, "U_"+strconv.Itoa(view.id))
	dst = append(dst, `,"name":`...)
	dst = appendJSONString(dst, view.name)
	dst = append(dst, `,"email":`...)
	dst = appendJSONString(dst, view.email)
	dst = append(dst, `,"type":"User","site_admin":false,"public_repos":0,"followers":0,"following":0,"created_at":`...)
	dst = appendJSONString(dst, stateEpochISO)
	dst = append(dst, `,"updated_at":`...)
	dst = appendJSONString(dst, stateEpochISO)
	dst = append(dst, '}')
	return dst
}

// ---------------------------------------------------------------------------------------------
// the git plumbing the contents API needs
// ---------------------------------------------------------------------------------------------

// entryAt is the tree entry at segments under treeOid, or nil.
func entryAt(repo *Repo, treeOid oid, segments [][]byte) *treeEntry {
	current := treeOid
	for index := 0; index < len(segments); index++ {
		object := repo.objects[current]
		if object == nil || object.typ != typeTree {
			return nil
		}
		entries, err := parseTree(object.data)
		if err != nil {
			return nil
		}
		var found *treeEntry
		for i := range entries {
			if entries[i].name == string(segments[index]) {
				entry := entries[i]
				found = &entry
				break
			}
		}
		if found == nil {
			return nil
		}
		if index == len(segments)-1 {
			return found
		}
		current = found.oid
	}
	return nil
}

// blobAt is the blob at segments, or nil when the path is not a file.
func blobAt(repo *Repo, treeOid oid, segments [][]byte) *GitObject {
	entry := entryAt(repo, treeOid, segments)
	if entry == nil || entry.mode == "40000" {
		return nil
	}
	object := repo.objects[entry.oid]
	if object == nil || object.typ != typeBlob {
		return nil
	}
	return object
}

// blobOidAt is the id of the blob at segments, or false.
func blobOidAt(repo *Repo, treeOid oid, segments [][]byte) (oid, bool) {
	entry := entryAt(repo, treeOid, segments)
	if entry == nil || entry.mode == "40000" {
		return oid{}, false
	}
	object := repo.objects[entry.oid]
	if object == nil || object.typ != typeBlob {
		return oid{}, false
	}
	return entry.oid, true
}

// writePath writes a blob at segments, creating the trees on the way up; the answer is the new
// root tree id.
func writePath(repo *Repo, rootTree oid, hasRoot bool, segments [][]byte, blobOid oid) oid {
	name := string(segments[0])
	var entries []treeEntry
	if hasRoot {
		if object := repo.objects[rootTree]; object != nil && object.typ == typeTree {
			existing, err := parseTree(object.data)
			if err == nil {
				entries = make([]treeEntry, 0, len(existing)+1)
				for _, entry := range existing {
					if entry.name != name {
						entries = append(entries, entry)
					}
				}
			}
		}
	}
	if len(segments) == 1 {
		entries = append(entries, treeEntry{mode: "100644", name: name, oid: blobOid})
	} else {
		var child oid
		hasChild := false
		if hasRoot {
			if existing := entryAt(repo, rootTree, segments[:1]); existing != nil && existing.mode == "40000" {
				child = existing.oid
				hasChild = true
			}
		}
		child = writePath(repo, child, hasChild, segments[1:], blobOid)
		entries = append(entries, treeEntry{mode: "40000", name: name, oid: child})
	}
	return repo.storeObject(typeTree, buildTree(entries))
}

// removePath removes segments; ok is false when nothing was there, and the tree id is zero when
// nothing is left at all.
func removePath(repo *Repo, rootTree oid, segments [][]byte) (oid, bool) {
	name := string(segments[0])
	object := repo.objects[rootTree]
	if object == nil || object.typ != typeTree {
		return oid{}, false
	}
	entries, err := parseTree(object.data)
	if err != nil {
		return oid{}, false
	}
	var entry *treeEntry
	for i := range entries {
		if entries[i].name == name {
			candidate := entries[i]
			entry = &candidate
			break
		}
	}
	if entry == nil {
		return oid{}, false
	}
	if len(segments) == 1 {
		kept := make([]treeEntry, 0, len(entries))
		for _, candidate := range entries {
			if candidate.name != name {
				kept = append(kept, candidate)
			}
		}
		if len(kept) == 0 {
			return oid{}, true
		}
		return repo.storeObject(typeTree, buildTree(kept)), true
	}
	if entry.mode != "40000" {
		return oid{}, false
	}
	child, ok := removePath(repo, entry.oid, segments[1:])
	if !ok {
		return oid{}, false
	}
	kept := make([]treeEntry, 0, len(entries))
	for _, candidate := range entries {
		if candidate.name != name {
			kept = append(kept, candidate)
		}
	}
	if child != (oid{}) {
		kept = append(kept, treeEntry{mode: "40000", name: name, oid: child})
	}
	if len(kept) == 0 {
		return oid{}, true
	}
	return repo.storeObject(typeTree, buildTree(kept)), true
}

// makeCommit builds and stores a commit; the answer is its content hash.
func makeCommit(repo *Repo, treeOid oid, parents []oid, message string, login string) oid {
	view := userViewFor(login)
	identity := view.name + " <" + view.email + "> 1735689600 +0000"
	body := message
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	out := make([]byte, 0, 128+len(body)+48*len(parents))
	out = append(out, "tree "...)
	out = treeOid.appendHex(out)
	for _, parent := range parents {
		out = append(out, "\nparent "...)
		out = parent.appendHex(out)
	}
	out = append(out, "\nauthor "...)
	out = append(out, identity...)
	out = append(out, "\ncommitter "...)
	out = append(out, identity...)
	out = append(out, "\n\n"...)
	out = append(out, body...)
	return repo.storeObject(typeCommit, out)
}

// gitRefURL is the API url of one ref of a repository.
func gitRefURL(call *apiCall, repo *Repo, ref string) string {
	trimmed := strings.TrimPrefix(ref, "refs/")
	return call.origin() + "/api/v3/repos/" + repo.owner + "/" + repo.name + "/git/refs/" + trimmed
}

// resolveRef resolves a ref name, branch name, tag name or raw object id.
func resolveRef(repo *Repo, ref string) (oid, bool) {
	if direct, ok := repo.refs[ref]; ok {
		return direct, true
	}
	if direct, ok := repo.refs["refs/heads/"+ref]; ok {
		return direct, true
	}
	if direct, ok := repo.refs["refs/tags/"+ref]; ok {
		return direct, true
	}
	if id, ok := oidFromHex([]byte(ref)); ok {
		if _, exists := repo.objects[id]; exists {
			return id, true
		}
	}
	return oid{}, false
}

// resolveCommit resolves a ref-ish name to the commit it names, following annotated tags.
func resolveCommit(repo *Repo, refish string) (oid, bool) {
	current, ok := resolveRef(repo, refish)
	if !ok {
		return oid{}, false
	}
	for hop := 0; hop < 16; hop++ {
		object := repo.objects[current]
		if object == nil {
			return oid{}, false
		}
		if object.typ == typeCommit {
			return current, true
		}
		if object.typ != typeTag {
			return oid{}, false
		}
		parsed, err := parseTag(object.data)
		if err != nil {
			return oid{}, false
		}
		current = parsed.object
	}
	return oid{}, false
}

// treeOf is the tree of the commit a refish names.
func treeOf(repo *Repo, refish string) (oid, bool) {
	commitOid, ok := resolveCommit(repo, refish)
	if !ok {
		return oid{}, false
	}
	object := repo.objects[commitOid]
	if object == nil {
		return oid{}, false
	}
	view, err := parseCommit(object.data)
	if err != nil {
		return oid{}, false
	}
	return view.tree, true
}

const base64Wrap = 60

func base64Wrapped(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	var out strings.Builder
	out.Grow(len(encoded) + len(encoded)/base64Wrap + 2)
	for offset := 0; offset < len(encoded); offset += base64Wrap {
		end := offset + base64Wrap
		if end > len(encoded) {
			end = len(encoded)
		}
		out.WriteString(encoded[offset:end])
		out.WriteByte('\n')
	}
	return out.String()
}

// decodeBase64Lenient decodes the way node's Buffer.from(x, 'base64') does: it skips anything
// outside the alphabet and tolerates missing padding.
func decodeBase64Lenient(text string) []byte {
	cleaned := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '=' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' {
			cleaned = append(cleaned, c)
		}
	}
	// Drop padding, then re-add what the decoder needs.
	for len(cleaned) > 0 && cleaned[len(cleaned)-1] == '=' {
		cleaned = cleaned[:len(cleaned)-1]
	}
	switch len(cleaned) % 4 {
	case 2:
		cleaned = append(cleaned, '=', '=')
	case 3:
		cleaned = append(cleaned, '=')
	}
	out, err := base64.StdEncoding.DecodeString(string(cleaned))
	if err != nil {
		return nil
	}
	return out
}

func appendFileJSON(dst []byte, call *apiCall, repo *Repo, path string, id oid, data []byte) []byte {
	name := path
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		name = path[slash+1:]
	}
	origin := call.origin()
	base := origin + "/api/v3/repos/" + repo.owner + "/" + repo.name + "/contents"
	dst = append(dst, `{"type":"file","encoding":"base64","size":`...)
	dst = appendJSONInt(dst, int64(len(data)))
	dst = append(dst, `,"name":`...)
	dst = appendJSONString(dst, name)
	dst = append(dst, `,"path":`...)
	dst = appendJSONString(dst, path)
	dst = append(dst, `,"content":`...)
	dst = appendJSONString(dst, base64Wrapped(data))
	dst = append(dst, `,"sha":`...)
	dst = appendJSONString(dst, id.hex())
	dst = append(dst, `,"url":`...)
	dst = appendJSONString(dst, base+"/"+path+"?ref="+defaultBranch)
	dst = append(dst, `,"git_url":`...)
	dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/git/blobs/"+id.hex())
	dst = append(dst, `,"html_url":`...)
	dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/blob/"+defaultBranch+"/"+path)
	dst = append(dst, `,"download_url":`...)
	dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/raw/"+defaultBranch+"/"+path)
	dst = append(dst, `,"_links":{"self":`...)
	dst = appendJSONString(dst, base+"/"+path)
	dst = append(dst, `,"git":`...)
	dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/git/blobs/"+id.hex())
	dst = append(dst, `,"html":`...)
	dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/blob/"+defaultBranch+"/"+path)
	dst = append(dst, `}}`...)
	return dst
}

func appendDirJSON(dst []byte, call *apiCall, repo *Repo, path string, entry treeEntry) []byte {
	childPath := entry.name
	if path != "" {
		childPath = path + "/" + entry.name
	}
	origin := call.origin()
	base := origin + "/api/v3/repos/" + repo.owner + "/" + repo.name + "/contents"
	isDir := entry.mode == "40000"
	size := 0
	if !isDir {
		if object := repo.objects[entry.oid]; object != nil {
			size = len(object.data)
		}
	}
	dst = append(dst, `{"type":`...)
	if isDir {
		dst = append(dst, `"dir"`...)
	} else {
		dst = append(dst, `"file"`...)
	}
	dst = append(dst, `,"size":`...)
	dst = appendJSONInt(dst, int64(size))
	dst = append(dst, `,"name":`...)
	dst = appendJSONString(dst, entry.name)
	dst = append(dst, `,"path":`...)
	dst = appendJSONString(dst, childPath)
	dst = append(dst, `,"sha":`...)
	dst = appendJSONString(dst, entry.oid.hex())
	dst = append(dst, `,"url":`...)
	dst = appendJSONString(dst, base+"/"+childPath)
	dst = append(dst, `,"git_url":`...)
	kind := "blobs"
	htmlKind := "blob"
	if isDir {
		kind = "trees"
		htmlKind = "tree"
	}
	dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/git/"+kind+"/"+entry.oid.hex())
	dst = append(dst, `,"html_url":`...)
	dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/"+htmlKind+"/"+defaultBranch+"/"+childPath)
	dst = append(dst, `,"download_url":`...)
	if isDir {
		dst = append(dst, "null"...)
	} else {
		dst = appendJSONString(dst, origin+"/"+repo.owner+"/"+repo.name+"/raw/"+defaultBranch+"/"+childPath)
	}
	dst = append(dst, '}')
	return dst
}

// ---------------------------------------------------------------------------------------------
// issues, labels, pulls
// ---------------------------------------------------------------------------------------------

const labelColour = "ededed"

func appendLabelJSON(dst []byte, name string) []byte {
	dst = append(dst, `{"name":`...)
	dst = appendJSONString(dst, name)
	dst = append(dst, `,"color":"`+labelColour+`"}`...)
	return dst
}

// appendIssueJSON appends an issue as the REST surface answers it: number, title, body, state and
// labels, plus the id the MCP parity check compares.
func appendIssueJSON(dst []byte, issue *Issue) []byte {
	dst = append(dst, `{"id":`...)
	dst = appendJSONInt(dst, int64(issue.id))
	dst = append(dst, `,"number":`...)
	dst = appendJSONInt(dst, int64(issue.number))
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, issue.title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, issue.body)
	dst = append(dst, `,"state":`...)
	dst = appendJSONString(dst, issue.state)
	dst = append(dst, `,"labels":[`...)
	for i, name := range issue.labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendLabelJSON(dst, name)
	}
	dst = append(dst, `]}`...)
	return dst
}

// cachedHead builds a reply head with a request-id slot at headIDOff.
func cachedHead(status int, contentLength int) (head []byte, headIDOff int) {
	head = make([]byte, 0, 192)
	head = appendStatus(head, status)
	head = append(head, "content-type: application/json; charset=utf-8\r\ncontent-length: "...)
	head = appendInt(head, contentLength)
	head = append(head, "\r\nx-github-request-id: "...)
	headIDOff = len(head)
	head = append(head, "00000000000000000000\r\n\r\n"...)
	return head, headIDOff
}

// buildIssueReply prebuilds one issue's reply: the same bytes answer every read of it.
func buildIssueReply(issue *Issue) *cachedReply {
	body := make([]byte, 0, 256)
	body = appendIssueJSON(body, issue)
	body = append(body, '\n')
	head, offset := cachedHead(200, len(body))
	return &cachedReply{head: head, headIDOff: offset, body: body}
}

// issueReplies is the repository's prebuilt issue replies, rebuilt when the store has changed.
func (f *Forge) issueReplies(repo *Repo) *issueTable {
	if table := repo.issueCache.Load(); table != nil && table.rev == f.rev {
		return table
	}
	highest := 0
	for number := range repo.issues {
		if number > highest {
			highest = number
		}
	}
	rows := make([]*cachedReply, highest+1)
	for number, issue := range repo.issues {
		rows[number] = buildIssueReply(issue)
	}
	table := &issueTable{rev: f.rev, rows: rows}
	repo.issueCache.Store(table)
	return table
}

// ---------------------------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------------------------

// handleAPI answers one `/api/v3` call.
func (s *Server) handleAPI(c *apiCall) (response, *apiError) {
	forge := s.forge
	head := segAt(c.segs, 0)
	second := segAt(c.segs, 1)
	third := segAt(c.segs, 2)
	fourth := segAt(c.segs, 3)
	var rest [][]byte
	if len(c.segs) > 4 {
		rest = c.segs[4:]
	}

	if len(c.segs) == 0 {
		if c.method != methodGet {
			return notFoundReply(), nil
		}
		origin := c.origin()
		body := make([]byte, 0, 160)
		body = append(body, `{"current_user_url":`...)
		body = appendJSONString(body, origin+"/api/v3/user")
		body = append(body, `,"rate_limit_url":`...)
		body = appendJSONString(body, origin+"/api/v3/rate_limit")
		body = append(body, `}`...)
		body = append(body, '\n')
		return response{status: 200, ctype: jsonMediaType, body: body, extra: []header{{name: "x-oauth-scopes", value: "repo, read:org"}}}, nil
	}
	if segEq(head, "meta") && len(c.segs) == 1 {
		if c.method != methodGet {
			return notFoundReply(), nil
		}
		return jsonReply(200, []byte(`{"installed_version":"ghforge-1.0.0","verifiable_password_authentication":true}`+"\n")), nil
	}
	if segEq(head, "rate_limit") {
		body := `{"resources":{},"rate":{"limit":5000,"remaining":4999,"reset":1735689600}}` + "\n"
		return jsonReply(200, []byte(body)), nil
	}
	if !c.authed {
		return unauthorizedReply(), nil
	}

	if segEq(head, "user") {
		if len(c.segs) == 1 {
			if c.method != methodGet {
				return notFoundReply(), nil
			}
			body := appendViewerJSON(make([]byte, 0, 320), c.login)
			body = append(body, '\n')
			return jsonReply(200, body), nil
		}
		if segEq(second, "repos") {
			if len(c.segs) != 2 {
				return notFoundReply(), nil
			}
			if c.method == methodPost {
				return s.createRepo(c)
			}
			if c.method == methodGet {
				return s.listRepos(c)
			}
			return notFoundReply(), nil
		}
		if segEq(second, "orgs") && len(c.segs) == 2 {
			return jsonReply(200, []byte("[]\n")), nil
		}
		return notFoundReply(), nil
	}

	if !segEq(head, "repos") || second == nil || third == nil {
		return notFoundReply(), nil
	}
	repo := forge.get(second, third)
	if repo == nil || !strings.EqualFold(repo.owner, c.login) {
		return notFoundReply(), nil
	}

	if fourth == nil {
		switch c.method {
		case methodGet:
			body := appendRepoJSON(make([]byte, 0, 256), repo)
			body = append(body, '\n')
			return jsonReply(200, body), nil
		case methodPatch:
			parsed, err := parseJSONObject(c.body)
			if err != nil {
				return response{}, err
			}
			if description, ok := parsed["description"].(string); ok {
				repo.description = description
			}
			if isPrivate, ok := parsed["private"].(bool); ok {
				repo.isPrivate = isPrivate
			}
			body := appendRepoJSON(make([]byte, 0, 256), repo)
			body = append(body, '\n')
			return jsonReply(200, body), nil
		case methodDelete:
			forge.remove(repo.owner, repo.name)
			return response{status: 204, body: emptyBytes}, nil
		}
		return notFoundReply(), nil
	}

	if segEq(fourth, "contents") {
		return s.handleContents(c, repo, rest)
	}

	if segEq(fourth, "git") && segAt(rest, 0) != nil && segEq(rest[0], "refs") {
		if len(rest) == 1 {
			if c.method == methodPost {
				parsed, err := parseJSONObject(c.body)
				if err != nil {
					return response{}, err
				}
				ref, ok := parsed["ref"].(string)
				sha, shaOk := parsed["sha"].(string)
				if !ok || !shaOk {
					return unprocessableReply("Validation Failed"), nil
				}
				id, valid := oidFromHex([]byte(sha))
				if !valid {
					return unprocessableReply("Validation Failed"), nil
				}
				if _, exists := repo.refs[ref]; exists {
					return unprocessableReply("Reference already exists"), nil
				}
				if _, exists := repo.objects[id]; !exists {
					return unprocessableReply("Object does not exist"), nil
				}
				repo.refs[ref] = id
				repo.recomputeRefDigest()
				body := appendRefJSON(make([]byte, 0, 256), c, repo, ref, id)
				body = append(body, '\n')
				return jsonReply(201, body), nil
			}
			if c.method == methodGet {
				prefix := "refs/"
				if value, ok := queryGet(c.query, "ref"); ok {
					prefix = value
				}
				names := make([]string, 0, len(repo.refs))
				for name := range repo.refs {
					if strings.HasPrefix(name, prefix) {
						names = append(names, name)
					}
				}
				sort.Strings(names)
				body := make([]byte, 0, 128*len(names)+2)
				body = append(body, '[')
				for i, name := range names {
					if i > 0 {
						body = append(body, ',')
					}
					body = appendRefJSON(body, c, repo, name, repo.refs[name])
				}
				body = append(body, ']')
				body = append(body, '\n')
				return jsonReply(200, body), nil
			}
			return notFoundReply(), nil
		}
		if c.method == methodGet {
			ref := joinSegments(rest)
			id, ok := repo.refs[ref]
			if !ok {
				return notFoundReply(), nil
			}
			body := appendRefJSON(make([]byte, 0, 256), c, repo, ref, id)
			body = append(body, '\n')
			return jsonReply(200, body), nil
		}
		return notFoundReply(), nil
	}

	if segEq(fourth, "branches") {
		if len(rest) == 1 {
			name := string(rest[0])
			id, ok := repo.refs["refs/heads/"+name]
			if !ok {
				return notFoundReply(), nil
			}
			body := make([]byte, 0, 256)
			body = append(body, `{"name":`...)
			body = appendJSONString(body, name)
			body = append(body, `,"commit":{"sha":`...)
			body = appendJSONString(body, id.hex())
			body = append(body, `,"url":`...)
			body = appendJSONString(body, c.origin()+"/api/v3/repos/"+repo.owner+"/"+repo.name+"/commits/"+id.hex())
			body = append(body, `},"protected":false}`...)
			body = append(body, '\n')
			return jsonReply(200, body), nil
		}
		if len(rest) == 0 {
			names := make([]string, 0, len(repo.refs))
			for name := range repo.refs {
				if strings.HasPrefix(name, "refs/heads/") {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			body := make([]byte, 0, 96*len(names)+2)
			body = append(body, '[')
			for i, name := range names {
				if i > 0 {
					body = append(body, ',')
				}
				body = append(body, `{"name":`...)
				body = appendJSONString(body, strings.TrimPrefix(name, "refs/heads/"))
				body = append(body, `,"commit":{"sha":`...)
				body = appendJSONString(body, repo.refs[name].hex())
				body = append(body, `,"url":""},"protected":false}`...)
			}
			body = append(body, ']')
			body = append(body, '\n')
			return jsonReply(200, body), nil
		}
	}

	if segEq(fourth, "commits") && len(rest) == 0 {
		refish := defaultBranch
		if value, ok := queryGet(c.query, "sha"); ok {
			refish = value
		}
		tip, ok := resolveCommit(repo, refish)
		if !ok {
			return jsonReply(200, []byte("[]\n")), nil
		}
		max := numberParam(c, "per_page", 100)
		body := make([]byte, 0, 256)
		body = append(body, '[')
		written := 0
		seen := make(map[oid]struct{}, 32)
		queue := make([]oid, 0, 32)
		queue = append(queue, tip)
		for len(queue) > 0 && written < max {
			id := queue[0]
			queue = queue[1:]
			if _, done := seen[id]; done {
				continue
			}
			seen[id] = struct{}{}
			object := repo.objects[id]
			if object == nil || object.typ != typeCommit {
				continue
			}
			commit, err := parseCommit(object.data)
			if err != nil {
				continue
			}
			if written > 0 {
				body = append(body, ',')
			}
			written++
			message := commit.message
			if strings.HasSuffix(message, "\n") {
				message = message[:len(message)-1]
			}
			body = append(body, `{"sha":`...)
			body = appendJSONString(body, id.hex())
			body = append(body, `,"node_id":`...)
			body = appendJSONString(body, "C_"+id.hex())
			body = append(body, `,"commit":{"message":`...)
			body = appendJSONString(body, message)
			body = append(body, `,"comment_count":0,"tree":{"sha":`...)
			body = appendJSONString(body, commit.tree.hex())
			body = append(body, `,"url":""}},"url":`...)
			body = appendJSONString(body, c.origin()+"/api/v3/repos/"+repo.owner+"/"+repo.name+"/commits/"+id.hex())
			body = append(body, `,"html_url":`...)
			body = appendJSONString(body, c.origin()+"/"+repo.owner+"/"+repo.name+"/commit/"+id.hex())
			body = append(body, `,"parents":[`...)
			for j, parent := range commit.parents {
				if j > 0 {
					body = append(body, ',')
				}
				body = append(body, `{"sha":`...)
				body = appendJSONString(body, parent.hex())
				body = append(body, `,"url":""}`...)
			}
			body = append(body, `]}`...)
			queue = append(queue, commit.parents...)
		}
		body = append(body, ']')
		body = append(body, '\n')
		return jsonReply(200, body), nil
	}

	if segEq(fourth, "labels") {
		if c.method == methodPost && len(rest) == 0 {
			parsed, err := parseJSONObject(c.body)
			if err != nil {
				return response{}, err
			}
			name, ok := parsed["name"].(string)
			if !ok || name == "" {
				return unprocessableReply("Validation Failed"), nil
			}
			repo.labels[name] = struct{}{}
			body := appendLabelJSON(make([]byte, 0, 80), name)
			body = append(body, '\n')
			return jsonReply(201, body), nil
		}
		if c.method == methodGet && len(rest) == 0 {
			names := make([]string, 0, len(repo.labels))
			for name := range repo.labels {
				names = append(names, name)
			}
			sort.Strings(names)
			body := make([]byte, 0, 48*len(names)+2)
			body = append(body, '[')
			for i, name := range names {
				if i > 0 {
					body = append(body, ',')
				}
				body = appendLabelJSON(body, name)
			}
			body = append(body, ']')
			body = append(body, '\n')
			return jsonReply(200, body), nil
		}
		return notFoundReply(), nil
	}

	if segEq(fourth, "pulls") {
		return s.handlePulls(c, repo, rest)
	}

	if segEq(fourth, "issues") {
		return s.handleIssues(c, repo, rest)
	}

	return notFoundReply(), nil
}

func appendRefJSON(dst []byte, call *apiCall, repo *Repo, ref string, id oid) []byte {
	dst = append(dst, `{"ref":`...)
	dst = appendJSONString(dst, ref)
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, "MDM6UmVm"+ref)
	dst = append(dst, `,"url":`...)
	dst = appendJSONString(dst, gitRefURL(call, repo, ref))
	dst = append(dst, `,"object":{"sha":`...)
	dst = appendJSONString(dst, id.hex())
	dst = append(dst, `,"type":"commit","url":""}}`...)
	return dst
}

func joinSegments(segments [][]byte) string {
	var builder strings.Builder
	for i, segment := range segments {
		if i > 0 {
			builder.WriteByte('/')
		}
		builder.Write(segment)
	}
	return builder.String()
}

// ---------------------------------------------------------------------------------------------
// repositories
// ---------------------------------------------------------------------------------------------

func (s *Server) createRepo(c *apiCall) (response, *apiError) {
	parsed, err := parseJSONObject(c.body)
	if err != nil {
		return response{}, err
	}
	name, ok := parsed["name"].(string)
	if !ok || trimSpace(name) == "" {
		return unprocessableErrors("Repository creation failed.", []string{"Repository"}), nil
	}
	description, _ := parsed["description"].(string)
	isPrivate, _ := parsed["private"].(bool)
	created := s.forge.create(c.login, name, description, isPrivate)
	if created == nil {
		return unprocessableErrors("Repository creation failed. Name already exists on this account", []string{"Repository"}), nil
	}
	body := appendRepoJSON(make([]byte, 0, 256), created)
	body = append(body, '\n')
	return jsonReply(201, body), nil
}

func (s *Server) listRepos(c *apiCall) (response, *apiError) {
	all := s.forge.listByOwner(c.login)
	perPage := numberParam(c, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	page := numberParam(c, "page", 1)
	start := (page - 1) * perPage
	if start > len(all) {
		start = len(all)
	}
	end := page * perPage
	if end > len(all) {
		end = len(all)
	}
	slice := all[start:end]

	lastPage := 1
	if len(all) > 0 {
		lastPage = (len(all) + perPage - 1) / perPage
	}
	path := c.origin() + "/api/v3/user/repos"
	target := func(p int) string {
		return path + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(p)
	}
	var links []string
	if page < lastPage {
		links = append(links, "<"+target(page+1)+`>; rel="next"`, "<"+target(lastPage)+`>; rel="last"`)
	}
	if page > 1 {
		links = append(links, "<"+target(page-1)+`>; rel="prev"`, "<"+target(1)+`>; rel="first"`)
	}
	body := make([]byte, 0, 256*len(slice)+2)
	body = append(body, '[')
	for i, repo := range slice {
		if i > 0 {
			body = append(body, ',')
		}
		body = appendRepoJSON(body, repo)
	}
	body = append(body, ']')
	body = append(body, '\n')
	reply := jsonReply(200, body)
	if len(links) > 0 {
		reply.extra = []header{{name: "link", value: strings.Join(links, ", ")}}
	}
	return reply, nil
}

// ---------------------------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------------------------

func (s *Server) handleContents(c *apiCall, repo *Repo, segments [][]byte) (response, *apiError) {
	path := joinSegments(segments)
	branch := defaultBranch
	if value, ok := queryGet(c.query, "ref"); ok {
		branch = value
	}
	tree, hasTree := treeOf(repo, branch)

	switch c.method {
	case methodGet:
		if !hasTree {
			return notFoundReply(), nil
		}
		if path == "" {
			object := repo.objects[tree]
			var entries []treeEntry
			if object != nil {
				entries, _ = parseTree(object.data)
			}
			return jsonReply(200, appendEntries(c, repo, "", entries)), nil
		}
		entry := entryAt(repo, tree, segments)
		if entry == nil {
			return notFoundReply(), nil
		}
		if entry.mode == "40000" {
			object := repo.objects[entry.oid]
			var entries []treeEntry
			if object != nil {
				entries, _ = parseTree(object.data)
			}
			return jsonReply(200, appendEntries(c, repo, path, entries)), nil
		}
		object := repo.objects[entry.oid]
		if object == nil {
			return notFoundReply(), nil
		}
		body := appendFileJSON(make([]byte, 0, 512+len(object.data)), c, repo, path, entry.oid, object.data)
		body = append(body, '\n')
		return jsonReply(200, body), nil
	case methodPut:
		parsed, err := parseJSONObject(c.body)
		if err != nil {
			return response{}, err
		}
		message, hasMessage := parsed["message"].(string)
		content, hasContent := parsed["content"].(string)
		if !hasMessage {
			return unprocessableReply("Validation Failed"), nil
		}
		if !hasContent {
			return unprocessableReply("Validation Failed"), nil
		}
		data := decodeBase64Lenient(content)
		targetBranch := defaultBranch
		if value, ok := parsed["branch"].(string); ok {
			targetBranch = value
		}
		refName := "refs/heads/" + targetBranch
		parent, hasParent := repo.refs[refName]
		var parentTree oid
		hasParentTree := false
		if hasParent {
			if object := repo.objects[parent]; object != nil {
				if view, err := parseCommit(object.data); err == nil {
					parentTree = view.tree
					hasParentTree = true
				}
			}
		}
		expected, hasExpected := parsed["sha"].(string)
		if hasExpected && hasParentTree {
			existing, ok := blobOidAt(repo, parentTree, segments)
			if !ok || existing.hex() != expected {
				return messageJSON(409, "sha does not match"), nil
			}
		}
		blobOid := repo.storeObject(typeBlob, data)
		newTree := writePath(repo, parentTree, hasParentTree, segments, blobOid)
		var parents []oid
		if hasParent {
			parents = append(parents, parent)
		}
		commitOid := makeCommit(repo, newTree, parents, message, c.login)
		repo.refs[refName] = commitOid
		repo.recomputeRefDigest()

		body := make([]byte, 0, 1024+len(data))
		body = append(body, `{"content":`...)
		body = appendFileJSON(body, c, repo, path, blobOid, data)
		body = append(body, `,"commit":{"sha":`...)
		body = appendJSONString(body, commitOid.hex())
		body = append(body, `,"message":`...)
		body = appendJSONString(body, message)
		body = append(body, `,"tree":{"sha":`...)
		body = appendJSONString(body, newTree.hex())
		body = append(body, `,"url":""},"parents":[`...)
		if hasParent {
			body = append(body, `{"sha":`...)
			body = appendJSONString(body, parent.hex())
			body = append(body, '}')
		}
		body = append(body, `],"url":"","html_url":""}}`...)
		body = append(body, '\n')
		return jsonReply(201, body), nil
	case methodDelete:
		parsed, err := parseJSONObject(c.body)
		if err != nil {
			return response{}, err
		}
		message, hasMessage := parsed["message"].(string)
		if !hasMessage {
			message = "Delete " + path
		}
		refName := "refs/heads/" + defaultBranch
		parent, hasParent := repo.refs[refName]
		if !hasParent || !hasTree {
			return notFoundReply(), nil
		}
		object := repo.objects[parent]
		if object == nil {
			return notFoundReply(), nil
		}
		view, commitErr := parseCommit(object.data)
		if commitErr != nil {
			return notFoundReply(), nil
		}
		parentTree := view.tree
		existing, ok := blobOidAt(repo, parentTree, segments)
		if !ok {
			return notFoundReply(), nil
		}
		if expected, hasExpected := parsed["sha"].(string); hasExpected && existing.hex() != expected {
			return messageJSON(409, "sha does not match"), nil
		}
		pruned, removed := removePath(repo, parentTree, segments)
		if !removed {
			return notFoundReply(), nil
		}
		newTree := pruned
		if pruned == (oid{}) {
			newTree = repo.storeObject(typeTree, emptyBytes)
		}
		commitOid := makeCommit(repo, newTree, []oid{parent}, message, c.login)
		repo.refs[refName] = commitOid
		repo.recomputeRefDigest()
		body := make([]byte, 0, 512)
		body = append(body, `{"content":null,"commit":{"sha":`...)
		body = appendJSONString(body, commitOid.hex())
		body = append(body, `,"message":`...)
		body = appendJSONString(body, message)
		body = append(body, `,"tree":{"sha":`...)
		body = appendJSONString(body, newTree.hex())
		body = append(body, `,"url":""},"parents":[{"sha":`...)
		body = appendJSONString(body, parent.hex())
		body = append(body, `}]}}`...)
		body = append(body, '\n')
		return jsonReply(200, body), nil
	}
	return notFoundReply(), nil
}

func appendEntries(c *apiCall, repo *Repo, path string, entries []treeEntry) []byte {
	body := make([]byte, 0, 256*len(entries)+2)
	body = append(body, '[')
	for i := range entries {
		if i > 0 {
			body = append(body, ',')
		}
		body = appendDirJSON(body, c, repo, path, entries[i])
	}
	body = append(body, ']')
	body = append(body, '\n')
	return body
}

// ---------------------------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------------------------

func (s *Server) handleIssues(c *apiCall, repo *Repo, rest [][]byte) (response, *apiError) {
	if len(rest) == 0 {
		switch c.method {
		case methodPost:
			return s.createIssue(c, repo)
		case methodGet:
			return s.listIssues(c, repo)
		}
		return notFoundReply(), nil
	}
	number, err := strconv.Atoi(string(rest[0]))
	if err != nil {
		return notFoundReply(), nil
	}
	issue := repo.issues[number]
	if issue == nil {
		return notFoundReply(), nil
	}
	if len(rest) == 1 {
		switch c.method {
		case methodGet:
			table := s.forge.issueReplies(repo)
			if number < len(table.rows) && table.rows[number] != nil {
				return response{cached: table.rows[number]}, nil
			}
			return notFoundReply(), nil
		case methodPatch:
			parsed, parseErr := parseJSONObject(c.body)
			if parseErr != nil {
				return response{}, parseErr
			}
			if state, ok := parsed["state"].(string); ok {
				if state == "open" || state == "closed" {
					issue.state = state
				}
			}
			if title, ok := parsed["title"].(string); ok {
				issue.title = title
			}
			if body, ok := parsed["body"].(string); ok {
				issue.body = body
			}
			s.forge.noteChange()
			reply := make([]byte, 0, 256)
			reply = appendIssueJSON(reply, issue)
			reply = append(reply, '\n')
			return jsonReply(200, reply), nil
		}
		return notFoundReply(), nil
	}
	if segEq(rest[1], "comments") && len(rest) == 2 {
		if c.method == methodGet {
			body := make([]byte, 0, 256*len(issue.comments)+2)
			body = append(body, '[')
			for i, comment := range issue.comments {
				if i > 0 {
					body = append(body, ',')
				}
				body = appendCommentJSON(body, comment.id, comment.body, comment.user)
			}
			body = append(body, ']')
			body = append(body, '\n')
			return jsonReply(200, body), nil
		}
		if c.method == methodPost {
			parsed, parseErr := parseJSONObject(c.body)
			if parseErr != nil {
				return response{}, parseErr
			}
			text, _ := parsed["body"].(string)
			comment := IssueComment{id: repo.nextCommentID, body: text, user: c.login}
			repo.nextCommentID++
			issue.comments = append(issue.comments, comment)
			body := appendCommentJSON(make([]byte, 0, 256), comment.id, comment.body, comment.user)
			body = append(body, '\n')
			return jsonReply(201, body), nil
		}
		return notFoundReply(), nil
	}
	return notFoundReply(), nil
}

func appendCommentJSON(dst []byte, id int, body string, user string) []byte {
	dst = append(dst, `{"id":`...)
	dst = appendJSONInt(dst, int64(id))
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, "IC_"+strconv.Itoa(id))
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, body)
	dst = append(dst, `,"user":`...)
	dst = appendUserJSON(dst, user)
	dst = append(dst, `,"created_at":`...)
	dst = appendJSONString(dst, stateEpochISO)
	dst = append(dst, `,"updated_at":`...)
	dst = appendJSONString(dst, stateEpochISO)
	dst = append(dst, '}')
	return dst
}

func (s *Server) createIssue(c *apiCall, repo *Repo) (response, *apiError) {
	parsed, err := parseJSONObject(c.body)
	if err != nil {
		return response{}, err
	}
	title, ok := parsed["title"].(string)
	if !ok || title == "" {
		return unprocessableReply("Validation Failed"), nil
	}
	var wanted []string
	if raw, ok := parsed["labels"].([]any); ok {
		for _, item := range raw {
			switch value := item.(type) {
			case string:
				wanted = append(wanted, value)
			case map[string]any:
				if name, ok := value["name"].(string); ok {
					wanted = append(wanted, name)
				} else {
					wanted = append(wanted, "")
				}
			}
		}
	}
	var missing []string
	for _, label := range wanted {
		if _, exists := repo.labels[label]; !exists {
			missing = append(missing, label)
		}
	}
	if len(missing) > 0 {
		resources := make([]string, len(missing))
		for i := range missing {
			resources[i] = "Issue"
		}
		return unprocessableErrors("Validation Failed", resources), nil
	}
	number := repo.nextIssue
	repo.nextIssue++
	bodyText, _ := parsed["body"].(string)
	issue := &Issue{
		number: number,
		id:     s.forge.issueID(),
		title:  title,
		body:   bodyText,
		state:  "open",
		labels: wanted,
		user:   c.login,
	}
	repo.issues[number] = issue
	// The 201 body is byte-for-byte what a later GET answers, so it is built once here.
	reply := buildIssueReply(issue)
	head, offset := cachedHead(201, len(reply.body))
	return response{cached: &cachedReply{head: head, headIDOff: offset, body: reply.body}}, nil
}

func (s *Server) listIssues(c *apiCall, repo *Repo) (response, *apiError) {
	state := "open"
	if value, ok := queryGet(c.query, "state"); ok {
		state = value
	}
	sortBy := "created"
	if value, ok := queryGet(c.query, "sort"); ok {
		sortBy = value
	}
	direction := "desc"
	if value, ok := queryGet(c.query, "direction"); ok {
		direction = value
	}
	list := make([]*Issue, 0, len(repo.issues))
	for _, issue := range repo.issues {
		if state != "all" && issue.state != state {
			continue
		}
		list = append(list, issue)
	}
	if sortBy == "created" || sortBy == "updated" {
		ascending := direction == "asc"
		sort.SliceStable(list, func(i, j int) bool {
			if ascending {
				return list[i].number < list[j].number
			}
			return list[i].number > list[j].number
		})
	}
	perPage := numberParam(c, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	page := numberParam(c, "page", 1)
	start := (page - 1) * perPage
	if start > len(list) {
		start = len(list)
	}
	end := page * perPage
	if end > len(list) {
		end = len(list)
	}
	slice := list[start:end]
	body := make([]byte, 0, 256*len(slice)+2)
	body = append(body, '[')
	for i, issue := range slice {
		if i > 0 {
			body = append(body, ',')
		}
		body = appendIssueJSON(body, issue)
	}
	body = append(body, ']')
	body = append(body, '\n')
	return jsonReply(200, body), nil
}

// ---------------------------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------------------------

// flattenTree is every blob under treeOid, keyed by its full path.
func flattenTree(repo *Repo, treeOid oid, prefix string, out map[string]oid) {
	object := repo.objects[treeOid]
	if object == nil || object.typ != typeTree {
		return
	}
	entries, err := parseTree(object.data)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := entry.name
		if prefix != "" {
			path = prefix + "/" + entry.name
		}
		if entry.mode == "40000" {
			flattenTree(repo, entry.oid, path, out)
		} else if entry.mode != "160000" {
			out[path] = entry.oid
		}
	}
}

type treeDiff struct {
	added    []string
	removed  []string
	modified []string
}

// diffTrees is the blobs that differ between two trees, by path.
func diffTrees(repo *Repo, fromTree oid, hasFrom bool, toTree oid, hasTo bool) treeDiff {
	before := make(map[string]oid, 16)
	if hasFrom {
		flattenTree(repo, fromTree, "", before)
	}
	after := make(map[string]oid, 16)
	if hasTo {
		flattenTree(repo, toTree, "", after)
	}
	var diff treeDiff
	for path, id := range after {
		was, existed := before[path]
		if !existed {
			diff.added = append(diff.added, path)
		} else if was != id {
			diff.modified = append(diff.modified, path)
		}
	}
	for path := range before {
		if _, exists := after[path]; !exists {
			diff.removed = append(diff.removed, path)
		}
	}
	sort.Strings(diff.added)
	sort.Strings(diff.modified)
	sort.Strings(diff.removed)
	return diff
}

// commitTree is the tree a commit names.
func commitTree(repo *Repo, commitOid oid, ok bool) (oid, bool) {
	if !ok {
		return oid{}, false
	}
	object := repo.objects[commitOid]
	if object == nil {
		return oid{}, false
	}
	view, err := parseCommit(object.data)
	if err != nil {
		return oid{}, false
	}
	return view.tree, true
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	count := 0
	start := 0
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			if i > start {
				count++
			}
			start = i + 1
		}
	}
	if count < 1 {
		return 1
	}
	return count
}

// appendPullFiles appends the per-file view of a pull request.
func appendPullFiles(dst []byte, repo *Repo, pull *Pull) []byte {
	baseTree, hasBase := commitTree(repo, pull.baseSha, true)
	headTree, hasHead := commitTree(repo, pull.headSha, true)
	diff := diffTrees(repo, baseTree, hasBase, headTree, hasHead)
	head := make(map[string]oid, 16)
	if hasHead {
		flattenTree(repo, headTree, "", head)
	}
	base := make(map[string]oid, 16)
	if hasBase {
		flattenTree(repo, baseTree, "", base)
	}
	sizeOf := func(id oid, ok bool) int {
		if !ok {
			return 0
		}
		object := repo.objects[id]
		if object == nil {
			return 0
		}
		return lineCount(object.data)
	}
	dst = append(dst, '[')
	first := true
	push := func(path, status string, additions, deletions int) {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = append(dst, `{"filename":`...)
		dst = appendJSONString(dst, path)
		dst = append(dst, `,"status":`...)
		dst = appendJSONString(dst, status)
		dst = append(dst, `,"additions":`...)
		dst = appendJSONInt(dst, int64(additions))
		dst = append(dst, `,"deletions":`...)
		dst = appendJSONInt(dst, int64(deletions))
		dst = append(dst, `,"changes":`...)
		dst = appendJSONInt(dst, int64(additions+deletions))
		dst = append(dst, `,"raw_url":"","blob_url":"","contents_url":""}`...)
	}
	for _, path := range diff.added {
		id, ok := head[path]
		additions := sizeOf(id, ok)
		push(path, "added", additions, 0)
	}
	for _, path := range diff.modified {
		id, ok := head[path]
		additions := sizeOf(id, ok)
		baseID, baseOk := base[path]
		deletions := sizeOf(baseID, baseOk)
		push(path, "modified", additions, deletions)
	}
	for _, path := range diff.removed {
		baseID, baseOk := base[path]
		deletions := sizeOf(baseID, baseOk)
		push(path, "removed", 0, deletions)
	}
	return append(dst, ']')
}

func appendPullJSON(dst []byte, pull *Pull) []byte {
	dst = append(dst, `{"id":`...)
	dst = appendJSONInt(dst, int64(pull.id))
	dst = append(dst, `,"number":`...)
	dst = appendJSONInt(dst, int64(pull.number))
	dst = append(dst, `,"state":`...)
	dst = appendJSONString(dst, pull.state)
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, pull.title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, pull.body)
	dst = append(dst, `,"merged":`...)
	if pull.merged {
		dst = append(dst, "true"...)
	} else {
		dst = append(dst, "false"...)
	}
	dst = append(dst, `,"merged_at":`...)
	if pull.mergedAt == "" {
		dst = append(dst, "null"...)
	} else {
		dst = appendJSONString(dst, pull.mergedAt)
	}
	dst = append(dst, `,"merge_commit_sha":`...)
	if pull.mergeCommitSha == (oid{}) {
		dst = append(dst, "null"...)
	} else {
		dst = appendJSONString(dst, pull.mergeCommitSha.hex())
	}
	dst = append(dst, `,"head":{"ref":`...)
	dst = appendJSONString(dst, pull.headRef)
	dst = append(dst, `,"sha":`...)
	dst = appendJSONString(dst, pull.headSha.hex())
	dst = append(dst, `},"base":{"ref":`...)
	dst = appendJSONString(dst, pull.baseRef)
	dst = append(dst, `,"sha":`...)
	dst = appendJSONString(dst, pull.baseSha.hex())
	dst = append(dst, `}}`...)
	return dst
}

func (s *Server) handlePulls(c *apiCall, repo *Repo, rest [][]byte) (response, *apiError) {
	if len(rest) == 0 {
		switch c.method {
		case methodPost:
			return s.createPull(c, repo)
		case methodGet:
			return s.listPulls(c, repo)
		}
		return notFoundReply(), nil
	}
	number, err := strconv.Atoi(string(rest[0]))
	if err != nil {
		return notFoundReply(), nil
	}
	pull := repo.pulls[number]
	if pull == nil {
		return notFoundReply(), nil
	}
	if len(rest) == 1 {
		if c.method != methodGet {
			return notFoundReply(), nil
		}
		body := appendPullJSON(make([]byte, 0, 384), pull)
		body = append(body, '\n')
		return jsonReply(200, body), nil
	}
	if segEq(rest[1], "files") && len(rest) == 2 {
		if c.method != methodGet {
			return notFoundReply(), nil
		}
		body := appendPullFiles(make([]byte, 0, 256), repo, pull)
		body = append(body, '\n')
		return jsonReply(200, body), nil
	}
	if segEq(rest[1], "merge") && len(rest) == 2 {
		if c.method != methodPut && c.method != methodPost {
			return notFoundReply(), nil
		}
		mergeSha, ok := s.mergePullInto(repo, pull, c.login)
		if !ok {
			return messageJSON(405, "Pull Request is not mergeable"), nil
		}
		body := make([]byte, 0, 192)
		body = append(body, `{"sha":`...)
		body = appendJSONString(body, mergeSha.hex())
		body = append(body, `,"merged":true,"message":"Pull Request successfully merged"}`...)
		body = append(body, '\n')
		return jsonReply(200, body), nil
	}
	return notFoundReply(), nil
}

func (s *Server) createPull(c *apiCall, repo *Repo) (response, *apiError) {
	parsed, err := parseJSONObject(c.body)
	if err != nil {
		return response{}, err
	}
	headRef, hasHead := parsed["head"].(string)
	baseRef := defaultBranch
	if value, ok := parsed["base"].(string); ok {
		baseRef = value
	}
	title, hasTitle := parsed["title"].(string)
	if !hasHead || !hasTitle {
		return unprocessableReply("Validation Failed"), nil
	}
	headSha, headOk := repo.refs["refs/heads/"+headRef]
	if !headOk {
		headSha, headOk = repo.refs[headRef]
	}
	baseSha, baseOk := repo.refs["refs/heads/"+baseRef]
	if !baseOk {
		baseSha, baseOk = repo.refs[baseRef]
	}
	if !headOk || !baseOk {
		return unprocessableReply("Validation Failed"), nil
	}
	for _, existing := range repo.pulls {
		if existing.state == "open" && existing.headRef == headRef && existing.baseRef == baseRef {
			return unprocessableErrors("A pull request already exists for this head and base", []string{"PullRequest"}), nil
		}
	}
	number := repo.nextPull
	repo.nextPull++
	bodyText, _ := parsed["body"].(string)
	pull := &Pull{
		number:  number,
		id:      s.forge.pullID(),
		title:   title,
		body:    bodyText,
		state:   "open",
		headRef: headRef,
		headSha: headSha,
		baseRef: baseRef,
		baseSha: baseSha,
	}
	repo.pulls[number] = pull
	body := appendPullJSON(make([]byte, 0, 384), pull)
	body = append(body, '\n')
	return jsonReply(201, body), nil
}

func (s *Server) listPulls(c *apiCall, repo *Repo) (response, *apiError) {
	state := "open"
	if value, ok := queryGet(c.query, "state"); ok {
		state = value
	}
	perPage := numberParam(c, "per_page", 30)
	if perPage > 100 {
		perPage = 100
	}
	page := numberParam(c, "page", 1)
	list := make([]*Pull, 0, len(repo.pulls))
	for _, pull := range repo.pulls {
		if state != "all" && pull.state != state {
			continue
		}
		list = append(list, pull)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].number > list[j].number })
	start := (page - 1) * perPage
	if start > len(list) {
		start = len(list)
	}
	end := page * perPage
	if end > len(list) {
		end = len(list)
	}
	slice := list[start:end]
	body := make([]byte, 0, 384*len(slice)+2)
	body = append(body, '[')
	for i, pull := range slice {
		if i > 0 {
			body = append(body, ',')
		}
		body = appendPullJSON(body, pull)
	}
	body = append(body, ']')
	body = append(body, '\n')
	return jsonReply(200, body), nil
}

// mergeTrees replays the head's changes onto the base tree; paths changed on both sides conflict.
func mergeTrees(repo *Repo, mergeBaseTree oid, hasBase bool, headTree oid, hasHead bool, baseTree oid, hasTarget bool) (oid, []string) {
	headDiff := diffTrees(repo, mergeBaseTree, hasBase, headTree, hasHead)
	baseDiff := diffTrees(repo, mergeBaseTree, hasBase, baseTree, hasTarget)
	touched := make(map[string]struct{}, len(baseDiff.added)+len(baseDiff.modified)+len(baseDiff.removed))
	for _, path := range baseDiff.added {
		touched[path] = struct{}{}
	}
	for _, path := range baseDiff.modified {
		touched[path] = struct{}{}
	}
	for _, path := range baseDiff.removed {
		touched[path] = struct{}{}
	}
	var conflicts []string
	for _, group := range [][]string{headDiff.added, headDiff.modified, headDiff.removed} {
		for _, path := range group {
			if _, hit := touched[path]; hit {
				conflicts = append(conflicts, path)
			}
		}
	}
	sort.Strings(conflicts)

	root := baseTree
	hasRoot := hasTarget
	head := make(map[string]oid, 16)
	if hasHead {
		flattenTree(repo, headTree, "", head)
	}
	paths := append(append([]string{}, headDiff.added...), headDiff.modified...)
	sort.Strings(paths)
	for _, path := range paths {
		if id, ok := head[path]; ok {
			root = writePath(repo, root, hasRoot, splitPath(path), id)
			hasRoot = true
		}
	}
	for _, path := range headDiff.removed {
		if !hasRoot {
			continue
		}
		pruned, ok := removePath(repo, root, splitPath(path))
		if !ok || pruned == (oid{}) {
			hasRoot = false
			root = oid{}
		} else {
			root = pruned
		}
	}
	if !hasRoot {
		root = repo.storeObject(typeTree, emptyBytes)
	}
	return root, conflicts
}

func splitPath(path string) [][]byte {
	parts := make([][]byte, 0, 4)
	start := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || path[i] == '/' {
			if i > start {
				parts = append(parts, []byte(path[start:i]))
			}
			start = i + 1
		}
	}
	return parts
}

// mergePullInto squash-merges one pull request: replay the head's changes onto the base branch's
// tip and move the branch. ok is false when the pull request cannot be merged.
func (s *Server) mergePullInto(repo *Repo, pull *Pull, login string) (oid, bool) {
	if pull.merged {
		return oid{}, false
	}
	baseTip, ok := repo.refs["refs/heads/"+pull.baseRef]
	if !ok {
		baseTip, ok = repo.refs[pull.baseRef]
	}
	if !ok {
		return oid{}, false
	}
	mergeBaseTree, hasBase := commitTree(repo, pull.baseSha, true)
	headTree, hasHead := commitTree(repo, pull.headSha, true)
	baseTree, hasTarget := commitTree(repo, baseTip, true)
	if !hasTarget {
		return oid{}, false
	}
	tree, conflicts := mergeTrees(repo, mergeBaseTree, hasBase, headTree, hasHead, baseTree, hasTarget)
	if len(conflicts) > 0 {
		return oid{}, false
	}
	message := "Merge pull request #" + strconv.Itoa(pull.number) + " from " + repo.owner + "/" + pull.headRef + "\n\n" + pull.title
	mergeSha := makeCommit(repo, tree, []oid{baseTip}, message, login)
	repo.refs["refs/heads/"+pull.baseRef] = mergeSha
	repo.recomputeRefDigest()
	pull.merged = true
	pull.state = "closed"
	pull.mergeCommitSha = mergeSha
	pull.mergedAt = stateEpochISO
	return mergeSha, true
}

// ---------------------------------------------------------------------------------------------
// bodies and query params
// ---------------------------------------------------------------------------------------------

// parseJSONObject parses a request body into a JSON object; a malformed body is a 400.
func parseJSONObject(body []byte) (map[string]any, *apiError) {
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, badJSON()
	}
	if parsed == nil {
		return nil, badJSON()
	}
	return parsed, nil
}

// numberParam reads a positive integer query parameter, or the fallback.
func numberParam(c *apiCall, name string, fallback int) int {
	raw, ok := queryGet(c.query, name)
	if !ok {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// queryGet reads one query parameter, decoded the way URLSearchParams reads it.
func queryGet(query []byte, name string) (string, bool) {
	at := 0
	for at <= len(query) {
		end := len(query)
		if index := bytes.IndexByte(query[at:], '&'); index >= 0 {
			end = at + index
		}
		pair := query[at:end]
		equals := bytes.IndexByte(pair, '=')
		key := pair
		value := []byte{}
		if equals >= 0 {
			key = pair[:equals]
			value = pair[equals+1:]
		}
		if decodedEqual(key, name) {
			if equals < 0 {
				return "", true
			}
			return decodeComponent(value), true
		}
		if end == len(query) {
			break
		}
		at = end + 1
	}
	return "", false
}

// decodedEqual compares a percent-decoded query key with name.
func decodedEqual(key []byte, name string) bool {
	if bytes.IndexByte(key, '%') < 0 && bytes.IndexByte(key, '+') < 0 {
		return segEq(key, name)
	}
	return decodeComponent(key) == name
}

// decodeComponent percent-decodes a query component, turning '+' into a space.
func decodeComponent(text []byte) string {
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '+':
			out = append(out, ' ')
		case '%':
			if i+2 < len(text) {
				hi := hexVal(text[i+1])
				lo := hexVal(text[i+2])
				if hi >= 0 && lo >= 0 {
					out = append(out, byte(hi<<4|lo))
					i += 2
					continue
				}
			}
			out = append(out, '%')
		default:
			out = append(out, text[i])
		}
	}
	return string(out)
}
