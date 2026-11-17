package main

// objects.go — git's object model and packfile codec, on the standard library only.
//
//   - pkt-line framing lives in pkt.go,
//   - loose-object ids are SHA-1 over "<type> <size>\0" plus the bytes,
//   - trees/commits/tags are parsed and built here,
//   - packs are read (v2/v3, plain + ofs_delta + ref_delta) and written (plain objects).
//
// Object ids are [20]byte values rather than hex strings: they are map keys and nothing
// allocates to hash or compare one.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

type objectType uint8

const (
	typeNone   objectType = 0
	typeCommit objectType = 1
	typeTree   objectType = 2
	typeBlob   objectType = 3
	typeTag    objectType = 4
)

func (t objectType) String() string {
	switch t {
	case typeCommit:
		return "commit"
	case typeTree:
		return "tree"
	case typeBlob:
		return "blob"
	case typeTag:
		return "tag"
	}
	return ""
}

func typeFromCode(code int) objectType {
	switch code {
	case 1, 2, 3, 4:
		return objectType(code)
	}
	return typeNone
}

// oid is one object id, raw.
type oid [20]byte

// oidFromHex reads a 40-hex-digit id; ok is false for anything else.
func oidFromHex(b []byte) (oid, bool) {
	var out oid
	if len(b) != 40 {
		return out, false
	}
	for i := 0; i < 20; i++ {
		hi := hexVal(b[i*2])
		lo := hexVal(b[i*2+1])
		if hi < 0 || lo < 0 {
			return out, false
		}
		out[i] = byte(hi<<4 | lo)
	}
	return out, true
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func (o oid) hex() string {
	return hex.EncodeToString(o[:])
}

// appendHex appends the 40 lowercase hex digits of o.
func (o oid) appendHex(dst []byte) []byte {
	for _, b := range o {
		dst = append(dst, jsonHex[b>>4], jsonHex[b&0xf])
	}
	return dst
}

// GitObject is one stored object: its type and its bytes.
type GitObject struct {
	typ  objectType
	data []byte
}

// hashObject is git's object id: SHA-1 over "<type> <size>\0" plus the object's bytes.
func hashObject(t objectType, data []byte) oid {
	var header [24]byte
	n := putObjectHeader(header[:], t, len(data))
	h := sha1.New()
	h.Write(header[:n])
	h.Write(data)
	var out oid
	h.Sum(out[:0])
	return out
}

func putObjectHeader(dst []byte, t objectType, size int) int {
	n := copy(dst, t.String())
	dst[n] = ' '
	n++
	n += putDecimal(dst[n:], size)
	dst[n] = 0
	return n + 1
}

func putDecimal(dst []byte, value int) int {
	if value == 0 {
		dst[0] = '0'
		return 1
	}
	var tmp [20]byte
	i := len(tmp)
	for value > 0 {
		i--
		tmp[i] = byte('0' + value%10)
		value /= 10
	}
	return copy(dst, tmp[i:])
}

// ---------------------------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------------------------

type treeEntry struct {
	mode string
	name string
	oid  oid
}

// parseTree reads a tree object's entries, in stored order.
func parseTree(data []byte) ([]treeEntry, error) {
	entries := make([]treeEntry, 0, 8)
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			return nil, errors.New("tree entry is malformed")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry is malformed")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry is malformed")
		}
		var id oid
		copy(id[:], data[nul+1:nul+21])
		entries = append(entries, treeEntry{
			mode: string(data[offset:space]),
			name: string(data[space+1 : nul]),
			oid:  id,
		})
		offset = nul + 21
	}
	return entries, nil
}

// treeSortKey is the name git orders a tree entry by: a directory sorts as if it ended in '/'.
func treeSortKey(entry treeEntry) []byte {
	if entry.mode == "40000" {
		key := make([]byte, 0, len(entry.name)+1)
		key = append(key, entry.name...)
		return append(key, '/')
	}
	return []byte(entry.name)
}

// buildTree is the tree object's bytes; entries are sorted the way git sorts them.
func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return bytes.Compare(treeSortKey(sorted[i]), treeSortKey(sorted[j])) < 0
	})
	out := make([]byte, 0, 64*len(sorted))
	for _, entry := range sorted {
		out = append(out, entry.mode...)
		out = append(out, ' ')
		out = append(out, entry.name...)
		out = append(out, 0)
		out = append(out, entry.oid[:]...)
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------------------------

type commitView struct {
	tree    oid
	parents []oid
	message string
}

// parseCommit reads a commit object; the tree header must name an object.
func parseCommit(data []byte) (commitView, error) {
	var view commitView
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
		view.message = string(data[blank+2:])
	}
	haveTree := false
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		space := bytes.IndexByte(line, ' ')
		if space < 0 {
			continue
		}
		key := line[:space]
		value := line[space+1:]
		switch {
		case bytes.Equal(key, []byte("tree")):
			id, ok := oidFromHex(value)
			if !ok {
				return view, errors.New("commit has no valid tree header")
			}
			view.tree = id
			haveTree = true
		case bytes.Equal(key, []byte("parent")):
			id, ok := oidFromHex(value)
			if ok {
				view.parents = append(view.parents, id)
			}
		}
	}
	if !haveTree {
		return view, errors.New("commit has no valid tree header")
	}
	return view, nil
}

type tagView struct {
	object oid
	typ    string
	tag    string
}

// parseTag reads an annotated tag object.
func parseTag(data []byte) (tagView, error) {
	var view tagView
	headerEnd := len(data)
	if blank := bytes.Index(data, []byte("\n\n")); blank >= 0 {
		headerEnd = blank
	}
	haveObject := false
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		space := bytes.IndexByte(line, ' ')
		if space < 0 {
			continue
		}
		key := line[:space]
		value := line[space+1:]
		switch {
		case bytes.Equal(key, []byte("object")):
			id, ok := oidFromHex(value)
			if !ok {
				return view, errors.New("tag has no valid object header")
			}
			view.object = id
			haveObject = true
		case bytes.Equal(key, []byte("type")):
			view.typ = string(value)
		case bytes.Equal(key, []byte("tag")):
			view.tag = string(value)
		}
	}
	if !haveObject {
		return view, errors.New("tag has no valid object header")
	}
	return view, nil
}

// objectLookup finds one object by id.
type objectLookup func(oid) *GitObject

// collectReachable walks every object reachable from roots: commit -> tree + parents,
// tree -> entries (gitlinks skipped), tag -> its target. Missing objects are skipped when
// allowMissing is set, and are an error otherwise.
func collectReachable(roots []oid, lookup objectLookup, allowMissing bool) (map[oid]struct{}, error) {
	seen := make(map[oid]struct{}, 64)
	queue := make([]oid, 0, 64)
	for _, root := range roots {
		queue = append(queue, root)
	}
	var view commitView
	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, done := seen[current]; done {
			continue
		}
		object := lookup(current)
		if object == nil {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("object %s is missing from the store", current.hex())
		}
		seen[current] = struct{}{}
		switch object.typ {
		case typeCommit:
			parsed, err := parseCommit(object.data)
			if err != nil {
				return nil, err
			}
			view = parsed
			queue = append(queue, view.tree)
			queue = append(queue, view.parents...)
		case typeTag:
			parsed, err := parseTag(object.data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, parsed.object)
		case typeTree:
			entries, err := parseTree(object.data)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.mode == "160000" {
					continue
				}
				queue = append(queue, entry.oid)
			}
		}
	}
	return seen, nil
}

// ---------------------------------------------------------------------------------------------
// packfile read
// ---------------------------------------------------------------------------------------------

// readVarintHeader reads a pack entry header: the type code and the inflated size.
func readVarintHeader(buf []byte, start int) (typeCode int, size int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry has no header")
	}
	b := buf[start]
	typeCode = int(b>>4) & 0x07
	size = int(b & 0x0f)
	shift := 4
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack entry header is truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 60 {
			return 0, 0, 0, errors.New("pack entry header is too long")
		}
	}
	return typeCode, size, offset, nil
}

// readOfsDeltaBase reads an ofs-delta base offset: the distance back to the base entry.
func readOfsDeltaBase(buf []byte, start int) (distance int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta has no base offset")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[offset]
		offset++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, offset, nil
}

// inflateAt inflates one zlib stream at offset, answering its bytes and the offset past it.
func inflateAt(buf []byte, offset int) (data []byte, next int, err error) {
	reader := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(reader)
	if err != nil {
		return nil, 0, err
	}
	out := make([]byte, 0, 4096)
	for {
		if len(out) == cap(out) {
			grown := make([]byte, len(out), 2*cap(out)+256)
			copy(grown, out)
			out = grown
		}
		n, readErr := zr.Read(out[len(out):cap(out)])
		out = out[:len(out)+n]
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			zr.Close()
			return nil, 0, readErr
		}
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed := len(buf) - offset - reader.Len()
	if consumed <= 0 {
		return nil, 0, fmt.Errorf("zlib stream at %d consumed no bytes", offset)
	}
	return out, offset + consumed, nil
}

// applyDelta replays a git delta (copy/insert instructions) onto a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := 0
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 60 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy is truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy is truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) || written+literal > targetSize {
				return nil, errors.New("delta insert is out of bounds")
			}
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

// readPack reads a packfile (version 2 or 3) into objects, resolving plain objects,
// OFS_DELTA and REF_DELTA. resolveBase supplies a ref-delta base that is not in the pack.
func readPack(pack []byte, resolveBase objectLookup) (map[oid]*GitObject, error) {
	if len(pack) < 32 || !bytes.Equal(pack[0:4], []byte("PACK")) {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declared := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	if trailerStart < 12 {
		return nil, errors.New("pack is too short")
	}
	digest := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(digest[:], pack[trailerStart:]) {
		return nil, errors.New("pack trailer does not match its bytes")
	}

	objects := make(map[oid]*GitObject, declared)

	// The common push carries plain objects only: inflate, hash, done (no offset maps).
	offset := 12
	plain := true
	for index := 0; index < declared; index++ {
		typeCode, _, next, err := readVarintHeader(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		if typeCode == 6 || typeCode == 7 {
			plain = false
			break
		}
		typ := typeFromCode(typeCode)
		if typ == typeNone {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, typeCode)
		}
		data, after, err := inflateAt(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = after
		id := hashObject(typ, data)
		if _, exists := objects[id]; !exists {
			objects[id] = &GitObject{typ: typ, data: data}
		}
	}
	if plain {
		if offset != trailerStart {
			return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
		}
		return objects, nil
	}

	type rawEntry struct {
		offset     int
		typeCode   int
		data       []byte
		baseOffset int
		baseOid    oid
		resolved   bool
		typ        objectType
		value      []byte
		id         oid
	}

	raw := make([]*rawEntry, 0, declared)
	byOffset := make(map[int]*rawEntry, declared)
	byOid := make(map[oid]*rawEntry, declared)
	offset = 12
	for index := 0; index < declared; index++ {
		entryOffset := offset
		typeCode, _, next, err := readVarintHeader(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		entry := &rawEntry{offset: entryOffset, typeCode: typeCode}
		switch typeCode {
		case 6:
			distance, after, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = after
			entry.baseOffset = entryOffset - distance
		case 7:
			if offset+20 > len(pack) {
				return nil, errors.New("ref-delta has no base id")
			}
			copy(entry.baseOid[:], pack[offset:offset+20])
			offset += 20
		default:
			if typeFromCode(typeCode) == typeNone {
				return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, typeCode)
			}
		}
		data, after, err := inflateAt(pack, offset)
		if err != nil {
			return nil, err
		}
		entry.data = data
		offset = after
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	resolving := make(map[int]bool, 4)
	var resolve func(entry *rawEntry) (*GitObject, error)
	resolve = func(entry *rawEntry) (*GitObject, error) {
		if entry.resolved {
			return &GitObject{typ: entry.typ, data: entry.value}, nil
		}
		if resolving[entry.offset] {
			return nil, errors.New("delta cycle in the pack")
		}
		resolving[entry.offset] = true
		var typ objectType
		var data []byte
		switch entry.typeCode {
		case 6:
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				return nil, fmt.Errorf("ofs-delta base at %d is not in the pack", entry.baseOffset)
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			typ = base.typ
			data, err = applyDelta(base.data, entry.data)
			if err != nil {
				return nil, err
			}
		case 7:
			inPack := byOid[entry.baseOid]
			if inPack != nil {
				base, err := resolve(inPack)
				if err != nil {
					return nil, err
				}
				typ = base.typ
				data, err = applyDelta(base.data, entry.data)
				if err != nil {
					return nil, err
				}
			} else {
				external := resolveBase(entry.baseOid)
				if external == nil {
					return nil, fmt.Errorf("ref-delta base %s is neither in the pack nor in the store", entry.baseOid.hex())
				}
				typ = external.typ
				var err error
				data, err = applyDelta(external.data, entry.data)
				if err != nil {
					return nil, err
				}
			}
		default:
			typ = typeFromCode(entry.typeCode)
			data = entry.data
		}
		entry.resolved = true
		entry.typ = typ
		entry.value = data
		entry.id = hashObject(typ, data)
		byOid[entry.id] = entry
		delete(resolving, entry.offset)
		return &GitObject{typ: typ, data: data}, nil
	}

	for _, entry := range raw {
		object, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		entry.resolved = true
		if _, exists := objects[entry.id]; !exists {
			objects[entry.id] = object
		}
	}
	return objects, nil
}

// ---------------------------------------------------------------------------------------------
// packfile write
// ---------------------------------------------------------------------------------------------

// appendWriter is an io.Writer that appends to a byte slice.
type appendWriter struct{ b []byte }

func (w *appendWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

var zlibPool = sync.Pool{New: func() any { return zlib.NewWriter(io.Discard) }}

// appendDeflate appends data as one zlib stream.
func appendDeflate(dst, data []byte) []byte {
	writer := &appendWriter{b: dst}
	zw := zlibPool.Get().(*zlib.Writer)
	zw.Reset(writer)
	_, _ = zw.Write(data)
	_ = zw.Close()
	zlibPool.Put(zw)
	return writer.b
}

// appendObjectHeader appends a pack entry header: the type code and the size varint.
func appendObjectHeader(dst []byte, t objectType, size int) []byte {
	code := int(t)
	first := byte(code<<4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		dst = append(dst, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	return append(dst, first)
}

// writePack builds a packfile from plain objects. The trailer is SHA-1 over everything before it.
func writePack(objects []*GitObject) []byte {
	out := make([]byte, 0, 12+64*len(objects))
	out = append(out, 'P', 'A', 'C', 'K')
	out = append(out, 0, 0, 0, 2)
	count := len(objects)
	out = append(out, byte(count>>24), byte(count>>16), byte(count>>8), byte(count))
	for _, object := range objects {
		out = appendObjectHeader(out, object.typ, len(object.data))
		out = appendDeflate(out, object.data)
	}
	digest := sha1.Sum(out)
	return append(out, digest[:]...)
}
