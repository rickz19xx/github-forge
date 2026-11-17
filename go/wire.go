package main

// wire.go — git's smart HTTP protocol as the conformance client drives it.
//
//	GET  /:owner/:repo.git/info/refs?service=...  the ref/capability advertisement (v2 for
//	                                              upload-pack when Git-Protocol: version=2, v0 for
//	                                              receive-pack and for clients that ask for v0),
//	POST /:owner/:repo.git/git-upload-pack        ls-refs and fetch (want/have/done, shallow,
//	                                              deepen), answered with a sideband packfile,
//	POST /:owner/:repo.git/git-receive-pack       a command list plus a pack, answered per ref
//	                                              with unpack/ok/ng in side-band-64k frames.
//
// Every byte here is git's; nothing shells out to git.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
)

const (
	agentName      = "ghforge/1.0"
	uploadService  = "git-upload-pack"
	receiveService = "git-receive-pack"
	jsonMediaType  = "application/json; charset=utf-8"
	sideBandChunk  = 65000
)

const uploadCapsV0 = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow no-progress " +
	"symref=HEAD:refs/heads/" + defaultBranch + " object-format=sha1 agent=" + agentName

const receiveCapsV0 = "report-status report-status-v2 delete-refs side-band-64k quiet atomic ofs-delta " +
	"object-format=sha1 agent=" + agentName + " symref=HEAD:refs/heads/" + defaultBranch

// advertisementV2 is the upload-pack capability advertisement: ref-independent, built once.
var advertisementV2 = func() []byte {
	var out []byte
	out = appendPktLineString(out, "# service="+uploadService+"\n")
	out = append(out, pktFlush...)
	out = appendPktLineString(out, "version 2\n")
	out = appendPktLineString(out, "agent="+agentName+"\n")
	out = appendPktLineString(out, "ls-refs=unborn\n")
	out = appendPktLineString(out, "fetch=shallow wait-for-done\n")
	out = appendPktLineString(out, "object-format=sha1\n")
	out = append(out, pktFlush...)
	return out
}()

// wireError is a protocol-level problem: the caller answers 400 and the process keeps running.
type wireError struct{ message string }

func (e *wireError) Error() string { return e.message }

func wireErrorf(message string) error { return &wireError{message: message} }

func smartType(service, kind string) string {
	return "application/x-" + service + "-" + kind
}

func jsonReply(status int, body []byte) response {
	if body == nil {
		body = emptyBytes
	}
	return response{status: status, ctype: jsonMediaType, body: body}
}

// messageJSON builds `{"message":"..."}` with a trailing newline.
func messageJSON(status int, message string) response {
	body := make([]byte, 0, len(message)+20)
	body = append(body, `{"message":`...)
	body = appendJSONString(body, message)
	body = append(body, '}', '\n')
	return jsonReply(status, body)
}

// ---------------------------------------------------------------------------------------------
// refs
// ---------------------------------------------------------------------------------------------

// peel answers the commit an annotated tag peels to, when the object is one.
func peel(repo *Repo, id oid) (oid, bool) {
	current := id
	followed := false
	for hop := 0; hop < 16; hop++ {
		object := repo.objects[current]
		if object == nil || object.typ != typeTag {
			break
		}
		parsed, err := parseTag(object.data)
		if err != nil {
			break
		}
		current = parsed.object
		followed = true
	}
	if followed {
		if _, ok := repo.objects[current]; ok {
			return current, true
		}
	}
	return oid{}, false
}

// ---------------------------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------------------------

// advertisementV0 is the v0 ref advertisement, refs and peeled tags included, with the
// capability line on the first entry.
func advertisementV0(repo *Repo, service string) []byte {
	caps := uploadCapsV0
	if service == receiveService {
		caps = receiveCapsV0
	}
	out := make([]byte, 0, 512+64*len(repo.refs))
	out = appendPktLineString(out, "# service="+service+"\n")
	out = append(out, pktFlush...)

	capsAttached := false
	line := make([]byte, 0, 256)
	flush := func() {
		if !capsAttached {
			capsAttached = true
			withCaps := append(line, 0)
			withCaps = append(withCaps, caps...)
			withCaps = append(withCaps, '\n')
			out = appendPktLine(out, withCaps)
		} else {
			out = appendPktLine(out, append(line, '\n'))
		}
		line = line[:0]
	}

	names := repo.refNames()
	head, born := repo.headOid()
	if len(names) == 0 && !born {
		line = append(line, "0000000000000000000000000000000000000000 capabilities^{}"...)
		flush()
	} else {
		if born {
			line = head.appendHex(line)
			line = append(line, " HEAD"...)
			flush()
		}
		for _, name := range names {
			line = repo.refs[name].appendHex(line)
			line = append(line, ' ')
			line = append(line, name...)
			flush()
			if peeled, ok := peel(repo, repo.refs[name]); ok {
				peeledLine := peeled.appendHex(line[:0])
				peeledLine = append(peeledLine, ' ')
				peeledLine = append(peeledLine, name...)
				peeledLine = append(peeledLine, "^{}\n"...)
				out = appendPktLine(out, peeledLine)
			}
		}
	}
	out = append(out, pktFlush...)
	return out
}

// handleInfoRefs answers GET /:owner/:repo.git/info/refs.
func (s *Server) handleInfoRefs(repo *Repo, service string, protocol []byte) response {
	if repo == nil {
		return messageJSON(404, "Not Found")
	}
	if service != uploadService && service != receiveService {
		return messageJSON(403, "Smart HTTP is only available for git-upload-pack and git-receive-pack")
	}
	if service == uploadService && bytes.Contains(protocol, []byte("version=2")) {
		return response{status: 200, ctype: smartType(service, "advertisement"), body: advertisementV2, cacheControl: true}
	}
	key := hashKey3(uint64(repo.id), bytesHash([]byte(service)), bytesHash([]byte(repo.refDigest)))
	if body := advertisementCache.get(key); body != nil {
		return response{status: 200, ctype: smartType(service, "advertisement"), body: body, cacheControl: true}
	}
	body := advertisementV0(repo, service)
	advertisementCache.put(key, body)
	return response{status: 200, ctype: smartType(service, "advertisement"), body: body, cacheControl: true}
}

var advertisementCache = newBodyCache(48)

// ---------------------------------------------------------------------------------------------
// parsing a command request
// ---------------------------------------------------------------------------------------------

type requestLine struct {
	command    string
	hasCommand bool
	args       [][]byte
}

// parseV2Request splits a v2 request into its command and its arguments.
func parseV2Request(body []byte) (requestLine, error) {
	var out requestLine
	offset := 0
	inCapabilities := true
	for {
		packet, ok, err := readPacketAt(body, offset)
		if err != nil {
			return out, wireErrorf("malformed request: " + err.Error())
		}
		if !ok {
			break
		}
		offset = packet.next
		if packet.kind == packetFlush {
			break
		}
		if packet.kind == packetDelim {
			inCapabilities = false
			continue
		}
		if packet.kind != packetData {
			return out, wireErrorf("unexpected control packet in a request")
		}
		line := packet.payload
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		key := line
		value := []byte(nil)
		if equals := bytes.IndexByte(line, '='); equals >= 0 {
			key = line[:equals]
			value = line[equals+1:]
		}
		if bytes.Equal(key, []byte("command")) {
			out.command = string(value)
			out.hasCommand = true
		} else if inCapabilities && (bytes.Equal(key, []byte("agent")) || bytes.Equal(key, []byte("object-format")) || bytes.Equal(key, []byte("session-id"))) {
			continue
		} else {
			out.args = append(out.args, line)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------------------------
// ls-refs
// ---------------------------------------------------------------------------------------------

func handleLsRefs(repo *Repo, args [][]byte) []byte {
	wantPeel := false
	wantSymrefs := false
	wantUnborn := false
	var prefixes []string
	for _, arg := range args {
		switch {
		case bytes.Equal(arg, []byte("peel")):
			wantPeel = true
		case bytes.Equal(arg, []byte("symrefs")):
			wantSymrefs = true
		case bytes.Equal(arg, []byte("unborn")):
			wantUnborn = true
		case bytes.HasPrefix(arg, []byte("ref-prefix ")):
			prefixes = append(prefixes, string(arg[len("ref-prefix "):]))
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
				return true
			}
		}
		return false
	}
	var out []byte
	line := make([]byte, 0, 256)
	target := repo.headTarget()
	if matches("HEAD") {
		if head, ok := repo.headOid(); ok {
			line = head.appendHex(line[:0])
			line = append(line, " HEAD"...)
			if wantSymrefs {
				line = append(line, " symref-target:"...)
				line = append(line, target...)
			}
			line = append(line, '\n')
			out = appendPktLine(out, line)
		} else if wantUnborn {
			line = append(line[:0], "unborn HEAD"...)
			if wantSymrefs {
				line = append(line, " symref-target:"...)
				line = append(line, target...)
			}
			line = append(line, '\n')
			out = appendPktLine(out, line)
		}
	}
	for _, name := range repo.refNames() {
		if !matches(name) {
			continue
		}
		id := repo.refs[name]
		line = id.appendHex(line[:0])
		line = append(line, ' ')
		line = append(line, name...)
		if wantPeel {
			if peeled, ok := peel(repo, id); ok {
				line = append(line, " peeled:"...)
				line = peeled.appendHex(line)
			}
		}
		line = append(line, '\n')
		out = appendPktLine(out, line)
	}
	return append(out, pktFlush...)
}

// ---------------------------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------------------------

// commitOf follows annotated tags from id to the commit it names.
func commitOf(lookup objectLookup, id oid) (oid, bool) {
	current := id
	for hop := 0; hop < 16; hop++ {
		object := lookup(current)
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

// depthCommits is the commits depth generations from wants (level 0 is the wants themselves).
func depthCommits(repo *Repo, wants []oid, depth int) map[oid]struct{} {
	lookup := repo.lookup()
	included := make(map[oid]struct{}, 16)
	frontier := make([]oid, 0, len(wants))
	for _, want := range wants {
		if commit, ok := commitOf(lookup, want); ok {
			frontier = append(frontier, commit)
		}
	}
	if depth < 1 {
		depth = 1
	}
	for level := 0; level < depth; level++ {
		next := make([]oid, 0, len(frontier))
		for _, id := range frontier {
			if _, done := included[id]; done {
				continue
			}
			included[id] = struct{}{}
			object := lookup(id)
			if object == nil || object.typ != typeCommit {
				continue
			}
			commit, err := parseCommit(object.data)
			if err != nil {
				continue
			}
			for _, parent := range commit.parents {
				if _, done := included[parent]; !done {
					next = append(next, parent)
				}
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	return included
}

// heldObjects is the closure of haves, walking no further than the shallow boundary the client
// named (a shallow commit's parents are not the client's).
func heldObjects(repo *Repo, haves []oid, boundary map[oid]struct{}) map[oid]struct{} {
	lookup := repo.lookup()
	seen := make(map[oid]struct{}, 64)
	queue := make([]oid, 0, len(haves)+16)
	queue = append(queue, haves...)
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, done := seen[id]; done {
			continue
		}
		object := lookup(id)
		if object == nil {
			continue
		}
		seen[id] = struct{}{}
		switch object.typ {
		case typeCommit:
			commit, err := parseCommit(object.data)
			if err != nil {
				continue
			}
			queue = append(queue, commit.tree)
			if _, shallow := boundary[id]; !shallow {
				queue = append(queue, commit.parents...)
			}
		case typeTag:
			parsed, err := parseTag(object.data)
			if err != nil {
				continue
			}
			queue = append(queue, parsed.object)
		case typeTree:
			entries, err := parseTree(object.data)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.mode != "160000" {
					queue = append(queue, entry.oid)
				}
			}
		}
	}
	return seen
}

// commitObjects is the objects of a set of commits: each commit, its tree and everything under it.
func commitObjects(repo *Repo, commits map[oid]struct{}) (map[oid]struct{}, error) {
	lookup := repo.lookup()
	trees := make([]oid, 0, len(commits))
	out := make(map[oid]struct{}, len(commits)*8)
	var view commitView
	for id := range commits {
		object := lookup(id)
		if object == nil || object.typ != typeCommit {
			continue
		}
		parsed, err := parseCommit(object.data)
		if err != nil {
			continue
		}
		view = parsed
		out[id] = struct{}{}
		trees = append(trees, view.tree)
	}
	reachable, err := collectReachable(trees, lookup, true)
	if err != nil {
		return nil, err
	}
	for id := range reachable {
		out[id] = struct{}{}
	}
	return out, nil
}

// handleFetch answers a fetch command: acknowledgments, optional shallow-info, then the pack.
func handleFetch(repo *Repo, args [][]byte) ([]byte, error) {
	lookup := repo.lookup()
	var wants []oid
	var haves []oid
	boundaries := make(map[oid]struct{}, 4)
	depth := 0
	for _, arg := range args {
		switch {
		case bytes.HasPrefix(arg, []byte("want ")):
			if id, ok := oidFromHex(bytes.TrimSpace(arg[5:])); ok {
				wants = append(wants, id)
			}
		case bytes.HasPrefix(arg, []byte("have ")):
			if id, ok := oidFromHex(bytes.TrimSpace(arg[5:])); ok {
				haves = append(haves, id)
			}
		case bytes.HasPrefix(arg, []byte("shallow ")):
			if id, ok := oidFromHex(bytes.TrimSpace(arg[8:])); ok {
				boundaries[id] = struct{}{}
			}
		case bytes.HasPrefix(arg, []byte("deepen ")):
			if parsed, err := strconv.Atoi(string(bytes.TrimSpace(arg[7:]))); err == nil && parsed > 0 {
				depth = parsed
			}
		}
	}

	var out []byte
	out = appendPktLineString(out, "acknowledgments\n")
	out = appendPktLineString(out, "NAK\n")
	out = append(out, pktFlush...)

	var included map[oid]struct{}
	if depth > 0 {
		commits := depthCommits(repo, wants, depth)
		fresh := make(map[oid]struct{}, len(commits))
		for id := range commits {
			object := lookup(id)
			if object == nil || object.typ != typeCommit {
				continue
			}
			commit, err := parseCommit(object.data)
			if err != nil {
				continue
			}
			for _, parent := range commit.parents {
				if _, inside := commits[parent]; !inside {
					fresh[id] = struct{}{}
					break
				}
			}
		}
		var shallowIDs []oid
		for id := range boundaries {
			if _, inside := commits[id]; inside {
				if _, isFresh := fresh[id]; !isFresh {
					shallowIDs = append(shallowIDs, id)
				}
			}
		}
		sortOids(shallowIDs)
		var freshIDs []oid
		for id := range fresh {
			if _, named := boundaries[id]; !named {
				freshIDs = append(freshIDs, id)
			}
		}
		sortOids(freshIDs)
		if len(shallowIDs) > 0 || len(freshIDs) > 0 {
			out = appendPktLineString(out, "shallow-info\n")
			line := make([]byte, 0, 60)
			for _, id := range shallowIDs {
				line = append(line[:0], "unshallow "...)
				line = id.appendHex(line)
				line = append(line, '\n')
				out = appendPktLine(out, line)
			}
			for _, id := range freshIDs {
				line = append(line[:0], "shallow "...)
				line = id.appendHex(line)
				line = append(line, '\n')
				out = appendPktLine(out, line)
			}
			out = append(out, pktFlush...)
		}
		collected, err := commitObjects(repo, commits)
		if err != nil {
			return nil, err
		}
		included = collected
	} else {
		collected, err := collectReachable(wants, lookup, true)
		if err != nil {
			return nil, err
		}
		included = collected
	}

	boundary := boundaries
	if depth == 0 {
		boundary = map[oid]struct{}{}
	}
	held := heldObjects(repo, haves, boundary)

	send := make([]oid, 0, len(included))
	for id := range included {
		if _, have := held[id]; have {
			continue
		}
		if lookup(id) == nil {
			continue
		}
		send = append(send, id)
	}
	sortOids(send)
	objects := make([]*GitObject, 0, len(send))
	for _, id := range send {
		objects = append(objects, lookup(id))
	}

	out = appendPktLineString(out, "packfile\n")
	pack := writePack(objects)
	frame := make([]byte, 0, sideBandChunk+8)
	for offset := 0; offset < len(pack); offset += sideBandChunk {
		end := offset + sideBandChunk
		if end > len(pack) {
			end = len(pack)
		}
		frame = frame[:0]
		frame = append(frame, 1)
		frame = append(frame, pack[offset:end]...)
		out = appendPktLine(out, frame)
	}
	if len(pack) == 0 {
		out = appendPktLine(out, []byte{1})
	}
	out = append(out, pktFlush...)
	return out, nil
}

var answerCache = newBodyCache(64)
var lsRefsCache = newBodyCache(48)

// handleUploadPack answers POST /:owner/:repo.git/git-upload-pack.
func (s *Server) handleUploadPack(repo *Repo, body []byte) (response, error) {
	request, err := parseV2Request(body)
	if err != nil {
		return response{}, err
	}
	if request.hasCommand && request.command == "fetch" {
		key := hashKey3(uint64(repo.id), bytesHash([]byte(repo.refDigest)), bytesHash(body))
		if cached := answerCache.get(key); cached != nil {
			s.hits.Add(1)
			return response{status: 200, ctype: smartType(uploadService, "result"), body: cached, cacheControl: true}, nil
		}
		answerCache.lockBuild()
		if cached := answerCache.get(key); cached != nil {
			answerCache.unlockBuild()
			s.hits.Add(1)
			return response{status: 200, ctype: smartType(uploadService, "result"), body: cached, cacheControl: true}, nil
		}
		s.builds.Add(1)
		answer, err := handleFetch(repo, request.args)
		if err != nil {
			answerCache.unlockBuild()
			return response{}, err
		}
		if s.stats {
			fmt.Fprintf(os.Stderr, "[stat] fetch built repo=%d answer=%d accepts=%d requests=%d hits=%d\n", repo.id, len(answer), s.accepts.Load(), s.requests.Load(), s.hits.Load())
		}
		// One entry is one packfile: keep a single one from eating the pod's memory.
		if len(answer) <= 1<<20 {
			answerCache.put(key, answer)
		}
		answerCache.unlockBuild()
		return response{status: 200, ctype: smartType(uploadService, "result"), body: answer, cacheControl: true}, nil
	}
	if request.hasCommand && request.command == "ls-refs" {
		key := hashKey3(uint64(repo.id), bytesHash([]byte(repo.refDigest)), argsHash(request.args))
		if cached := lsRefsCache.get(key); cached != nil {
			return response{status: 200, ctype: smartType(uploadService, "result"), body: cached, cacheControl: true}, nil
		}
		answer := handleLsRefs(repo, request.args)
		lsRefsCache.put(key, answer)
		return response{status: 200, ctype: smartType(uploadService, "result"), body: answer, cacheControl: true}, nil
	}
	name := request.command
	if !request.hasCommand {
		name = ""
	}
	return response{}, wireErrorf("unknown upload-pack command " + strconv.Quote(name))
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

type pushCommand struct {
	oldOid oid
	newOid oid
	ref    string
}

type refResult struct {
	ref    string
	ok     bool
	reason string
}

// isAncestor reports whether ancestor is descendant or one of its forebears.
func isAncestor(repo *Repo, ancestor, descendant oid) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[oid]struct{}, 64)
	queue := make([]oid, 0, 64)
	queue = append(queue, descendant)
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, done := seen[id]; done {
			continue
		}
		seen[id] = struct{}{}
		if id == ancestor {
			return true
		}
		object := repo.objects[id]
		if object == nil || object.typ != typeCommit {
			continue
		}
		commit, err := parseCommit(object.data)
		if err != nil {
			continue
		}
		queue = append(queue, commit.parents...)
	}
	return false
}

// parseCommandList splits the push command list from the pack that follows it.
func parseCommandList(body []byte) (commands []pushCommand, capabilities map[string]struct{}, packStart int, err error) {
	capabilities = make(map[string]struct{}, 8)
	offset := 0
	first := true
	packStart = len(body)
	for {
		packet, ok, readErr := readPacketAt(body, offset)
		if readErr != nil {
			return nil, nil, 0, wireErrorf("malformed command list: " + readErr.Error())
		}
		if !ok {
			return nil, nil, 0, wireErrorf("the push command list has no flush packet")
		}
		if packet.kind == packetFlush {
			packStart = packet.next
			break
		}
		if packet.kind != packetData {
			return nil, nil, 0, wireErrorf("unexpected control packet in a push")
		}
		line := packet.payload
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if first {
			first = false
			if nul := bytes.IndexByte(line, 0); nul >= 0 {
				for _, token := range bytes.Split(line[nul+1:], []byte(" ")) {
					if len(token) == 0 {
						continue
					}
					if equals := bytes.IndexByte(token, '='); equals >= 0 {
						token = token[:equals]
					}
					capabilities[string(token)] = struct{}{}
				}
				line = line[:nul]
			}
		}
		parts := bytes.Split(line, []byte(" "))
		if len(parts) != 3 {
			return nil, nil, 0, wireErrorf("bad push command")
		}
		oldOid, okOld := oidFromHex(parts[0])
		newOid, okNew := oidFromHex(parts[1])
		if !okOld || !okNew {
			return nil, nil, 0, wireErrorf("bad push command")
		}
		commands = append(commands, pushCommand{oldOid: oldOid, newOid: newOid, ref: string(parts[2])})
		offset = packet.next
	}
	return commands, capabilities, packStart, nil
}

// applyCommand applies one command, mutating the repository only when it is accepted.
func applyCommand(repo *Repo, command pushCommand) refResult {
	current, exists := repo.refs[command.ref]
	result := refResult{ref: command.ref}
	if command.newOid == (oid{}) {
		if !exists {
			result.reason = "no such ref"
			return result
		}
		if current != command.oldOid {
			result.reason = "stale info"
			return result
		}
		delete(repo.refs, command.ref)
		repo.recomputeRefDigest()
		result.ok = true
		return result
	}
	if command.oldOid == (oid{}) {
		if exists {
			result.reason = "already exists"
			return result
		}
		repo.refs[command.ref] = command.newOid
		repo.recomputeRefDigest()
		result.ok = true
		return result
	}
	if !exists || current != command.oldOid {
		result.reason = "stale info"
		return result
	}
	if !isAncestor(repo, command.oldOid, command.newOid) {
		result.reason = "non-fast-forward"
		return result
	}
	repo.refs[command.ref] = command.newOid
	repo.recomputeRefDigest()
	result.ok = true
	return result
}

// handleReceivePack answers POST /:owner/:repo.git/git-receive-pack.
func (s *Server) handleReceivePack(repo *Repo, body []byte) (response, error) {
	commands, capabilities, packStart, err := parseCommandList(body)
	if err != nil {
		return response{}, err
	}
	if len(commands) == 0 {
		return response{}, wireErrorf("a push needs at least one command")
	}
	if packStart < len(body) {
		packBytes := body[packStart:]
		if len(packBytes) > 0 {
			objects, err := readPack(packBytes, repo.lookup())
			if err != nil {
				return response{}, wireErrorf("the pack could not be read: " + err.Error())
			}
			repo.storePacked(objects)
		}
	}

	results := make([]refResult, len(commands))
	for i, command := range commands {
		results[i] = applyCommand(repo, command)
	}

	_, wantsReport := capabilities["report-status"]
	if !wantsReport {
		_, wantsReport = capabilities["report-status-v2"]
	}
	if !wantsReport {
		return response{status: 200, ctype: smartType(receiveService, "result"), body: emptyBytes, cacheControl: true}, nil
	}

	report := make([]byte, 0, 64+64*len(results))
	report = appendPktLineString(report, "unpack ok\n")
	for _, result := range results {
		if result.ok {
			report = appendPktLineString(report, "ok "+result.ref+"\n")
		} else {
			report = appendPktLineString(report, "ng "+result.ref+" "+result.reason+"\n")
		}
	}
	report = append(report, pktFlush...)

	if _, sideband := capabilities["side-band-64k"]; sideband {
		var framed []byte
		for offset := 0; offset < len(report); offset += sideBandChunk {
			end := offset + sideBandChunk
			if end > len(report) {
				end = len(report)
			}
			chunk := make([]byte, 0, end-offset+1)
			chunk = append(chunk, 1)
			chunk = append(chunk, report[offset:end]...)
			framed = appendPktLine(framed, chunk)
		}
		framed = append(framed, pktFlush...)
		return response{status: 200, ctype: smartType(receiveService, "result"), body: framed, cacheControl: true}, nil
	}
	return response{status: 200, ctype: smartType(receiveService, "result"), body: report, cacheControl: true}, nil
}

// ---------------------------------------------------------------------------------------------
// keys and caches
// ---------------------------------------------------------------------------------------------

// bytesHash is FNV-1a, 64-bit, over a byte slice.
func bytesHash(data []byte) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for _, c := range data {
		hash ^= uint64(c)
		hash *= 0x100000001b3
	}
	return hash
}

func hashKey3(a, b, c uint64) uint64 {
	hash := a*0x100000001b3 + 0x9e3779b97f4a7c15
	hash ^= b + 0x9e3779b97f4a7c15 + (hash << 6) + (hash >> 2)
	hash *= 0x100000001b3
	hash ^= c + 0x9e3779b97f4a7c15 + (hash << 6) + (hash >> 2)
	return hash * 0x100000001b3
}

func argsHash(args [][]byte) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for _, arg := range args {
		hash ^= bytesHash(arg)
		hash *= 0x100000001b3
		hash ^= uint64(len(arg))
	}
	return hash
}

// bodyCache holds prebuilt reply bodies, dropping the oldest entry when full.
type bodyCache struct {
	mu      sync.Mutex
	entries map[uint64][]byte
	order   []uint64
	limit   int
	build   sync.Mutex
}

func newBodyCache(limit int) *bodyCache {
	return &bodyCache{entries: make(map[uint64][]byte, limit), order: make([]uint64, 0, limit), limit: limit}
}

func (c *bodyCache) get(key uint64) []byte {
	c.mu.Lock()
	body := c.entries[key]
	c.mu.Unlock()
	return body
}

func (c *bodyCache) put(key uint64, body []byte) {
	c.mu.Lock()
	if _, exists := c.entries[key]; !exists {
		if len(c.order) >= c.limit {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = body
	c.mu.Unlock()
}

// lockBuild serialises cache misses: fifty concurrent clones build one packfile, not fifty.
func (c *bodyCache) lockBuild()   { c.build.Lock() }
func (c *bodyCache) unlockBuild() { c.build.Unlock() }

var errBodyTooLarge = errors.New("body is too large")
