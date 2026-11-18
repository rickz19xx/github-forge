package main

// store.go — the forge's state: one record per repository, holding its git objects (in memory,
// keyed by object id) and its loose refs, plus the resources the REST surface exposes.
//
// Determinism: object ids are content hashes, refs move only by explicit commands, and every
// id this file hands out is a pure function of creation order, never a clock. Map iteration is
// never allowed to decide an order: every listing sorts.

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// stateEpochMilli is the fixed instant every stored timestamp names (2025-01-01T00:00:00.000Z).
const stateEpochMilli = 1735689600000

// stateEpochISO is stateEpochMilli as an ISO string.
const stateEpochISO = "2025-01-01T00:00:00.000Z"

// defaultBranch is the branch every repository is born with.
const defaultBranch = "main"

// IssueComment is one comment on an issue.
type IssueComment struct {
	id   int
	body string
	user string
}

// Issue is one issue.
type Issue struct {
	number   int
	id       int
	title    string
	body     string
	state    string // "open" or "closed"
	labels   []string
	user     string
	comments []IssueComment
}

// Pull is one pull request: the two refs it names, frozen shas, and its merge state.
type Pull struct {
	number         int
	id             int
	title          string
	body           string
	state          string
	headRef        string
	headSha        oid
	baseRef        string
	baseSha        oid
	merged         bool
	mergeCommitSha oid
	mergedAt       string
}

// cachedReply is a reply whose head is fixed except for the request id: a repeated read is a
// copy and a hex fill, never a rebuild.
type cachedReply struct {
	head      []byte
	headIDOff int
	body      []byte
}

// issueTable is a repository's prebuilt issue replies, tagged with the store revision it was
// built at. Reads load the pointer; no lock is taken to answer one.
type issueTable struct {
	rev  int64
	rows []*cachedReply // index by issue number; entry 0 is nil
}

// Repo is one repository: its objects, its refs and the REST resources hanging off it.
type Repo struct {
	id            int
	owner         string
	name          string
	ownerLower    string
	nameLower     string
	description   string
	isPrivate     bool
	objects       map[oid]*GitObject
	refs          map[string]oid
	refDigest     string
	nextIssue     int
	nextCommentID int
	nextPull      int
	issues        map[int]*Issue
	pulls         map[int]*Pull
	labels        map[string]struct{}
	issueCache    atomic.Pointer[issueTable]
}

// headTarget is HEAD's symref target.
func (r *Repo) headTarget() string {
	return "refs/heads/" + defaultBranch
}

// headOid is the object HEAD resolves to; ok is false for an unborn branch.
func (r *Repo) headOid() (oid, bool) {
	id, ok := r.refs[r.headTarget()]
	return id, ok
}

// recomputeRefDigest rebuilds the ref digest the wire caches key on. It must be called (under
// the store's write lock) whenever a ref moves, so an unchanged repository answers from cache.
func (r *Repo) recomputeRefDigest() {
	names := r.refNames()
	buf := make([]byte, 0, 48*len(names)+16)
	for _, name := range names {
		buf = append(buf, name...)
		buf = append(buf, '=')
		buf = r.refs[name].appendHex(buf)
		buf = append(buf, '\n')
	}
	r.refDigest = string(buf)
}

// refNames is every loose ref name, in byte order.
func (r *Repo) refNames() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// object answers one stored object, or nil.
func (r *Repo) object(id oid) *GitObject {
	return r.objects[id]
}

// lookup is this repository's object lookup.
func (r *Repo) lookup() objectLookup {
	return func(id oid) *GitObject { return r.objects[id] }
}

// storeObject hashes and stores one object; the id is its content hash.
func (r *Repo) storeObject(t objectType, data []byte) oid {
	id := hashObject(t, data)
	if _, exists := r.objects[id]; !exists {
		r.objects[id] = &GitObject{typ: t, data: data}
	}
	return id
}

// storePacked stores objects a pack reader already hashed.
func (r *Repo) storePacked(objects map[oid]*GitObject) {
	for id, object := range objects {
		if _, exists := r.objects[id]; !exists {
			r.objects[id] = object
		}
	}
}

// Forge is the whole store: one process, one of these, one lock.
type Forge struct {
	mu       sync.RWMutex
	repos    map[uint64]*Repo
	overflow map[uint64][]*Repo
	order    []*Repo
	nextRepo int
	nextIss  int
	nextPull int
	// rev counts store changes. It is read without the write lock when a cached read decides
	// whether it is still fresh, so it is atomic: a plain int64 there would be a data race.
	rev atomic.Int64
}

func newForge() *Forge {
	return &Forge{
		repos:    make(map[uint64]*Repo, 64),
		overflow: make(map[uint64][]*Repo, 4),
		order:    make([]*Repo, 0, 64),
		nextRepo: 1,
		nextIss:  1,
		nextPull: 1,
	}
}

// repoKey is the hash a repository is filed under: FNV-1a over "owner/name", lowercased.
func repoKey(owner, name []byte) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for _, c := range owner {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		hash ^= uint64(c)
		hash *= 0x100000001b3
	}
	hash ^= uint64('/')
	hash *= 0x100000001b3
	for _, c := range name {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		hash ^= uint64(c)
		hash *= 0x100000001b3
	}
	return hash
}

func equalFoldAscii(value string, other []byte) bool {
	if len(value) != len(other) {
		return false
	}
	for i := 0; i < len(value); i++ {
		a := value[i]
		b := other[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// get answers the repository owner/name, or nil.
func (f *Forge) get(owner, name []byte) *Repo {
	key := repoKey(owner, name)
	if repo := f.repos[key]; repo != nil {
		if equalFoldAscii(repo.ownerLower, owner) && equalFoldAscii(repo.nameLower, name) {
			return repo
		}
	}
	for _, repo := range f.overflow[key] {
		if equalFoldAscii(repo.ownerLower, owner) && equalFoldAscii(repo.nameLower, name) {
			return repo
		}
	}
	return nil
}

// getStrings answers owner/name from strings.
func (f *Forge) getStrings(owner, name string) *Repo {
	return f.get([]byte(owner), []byte(name))
}

// findById answers the repository with numeric id id, or nil.
func (f *Forge) findById(id int) *Repo {
	for _, repo := range f.order {
		if repo.id == id {
			return repo
		}
	}
	return nil
}

// listByOwner is every repository owner owns, in creation order.
func (f *Forge) listByOwner(owner string) []*Repo {
	out := make([]*Repo, 0, 8)
	for _, repo := range f.order {
		if equalFoldAscii(repo.ownerLower, []byte(owner)) {
			out = append(out, repo)
		}
	}
	return out
}

// create makes owner/name; nil when it already exists.
func (f *Forge) create(owner, name string, description string, isPrivate bool) *Repo {
	if f.get([]byte(owner), []byte(name)) != nil {
		return nil
	}
	repo := &Repo{
		id:            f.nextRepo,
		owner:         owner,
		name:          name,
		ownerLower:    lowerAscii(owner),
		nameLower:     lowerAscii(name),
		description:   description,
		isPrivate:     isPrivate,
		objects:       make(map[oid]*GitObject, 64),
		refs:          make(map[string]oid, 8),
		nextIssue:     1,
		nextCommentID: 1,
		nextPull:      1,
		issues:        make(map[int]*Issue, 8),
		pulls:         make(map[int]*Pull, 4),
		labels:        make(map[string]struct{}, 8),
	}
	f.nextRepo++
	key := repoKey([]byte(owner), []byte(name))
	if existing := f.repos[key]; existing != nil {
		f.overflow[key] = append(f.overflow[key], repo)
	} else {
		f.repos[key] = repo
	}
	f.order = append(f.order, repo)
	return repo
}

// remove deletes owner/name; false when it was not there.
func (f *Forge) remove(owner, name string) bool {
	key := repoKey([]byte(owner), []byte(name))
	repo := f.get([]byte(owner), []byte(name))
	if repo == nil {
		return false
	}
	if f.repos[key] == repo {
		delete(f.repos, key)
		if chain := f.overflow[key]; len(chain) > 0 {
			f.repos[key] = chain[0]
			if len(chain) > 1 {
				f.overflow[key] = chain[1:]
			} else {
				delete(f.overflow, key)
			}
		}
	} else {
		chain := f.overflow[key]
		for i, candidate := range chain {
			if candidate == repo {
				f.overflow[key] = append(chain[:i], chain[i+1:]...)
				break
			}
		}
	}
	for i, candidate := range f.order {
		if candidate == repo {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	return true
}

// issueID is the next globally unique issue id.
func (f *Forge) issueID() int {
	id := f.nextIss
	f.nextIss++
	return id
}

// pullID is the next globally unique pull request id.
func (f *Forge) pullID() int {
	id := f.nextPull
	f.nextPull++
	return id
}

// noteChange marks the store as changed: every cached read body is now stale.
func (f *Forge) noteChange() {
	f.rev.Add(1)
}

func lowerAscii(value string) string {
	for i := 0; i < len(value); i++ {
		if value[i] >= 'A' && value[i] <= 'Z' {
			out := []byte(value)
			for j := i; j < len(out); j++ {
				if out[j] >= 'A' && out[j] <= 'Z' {
					out[j] += 'a' - 'A'
				}
			}
			return string(out)
		}
	}
	return value
}

// sortStrings orders names the way git does: byte order.
func sortStrings(names []string) {
	// Insertion sort is the right tool for the handful of refs a repository carries; it is also
	// allocation-free, unlike a comparator-based sort on strings.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}

// sortOids orders object ids by their bytes.
func sortOids(ids []oid) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && bytes.Compare(ids[j][:], ids[j-1][:]) < 0; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}
