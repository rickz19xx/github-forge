package main

// scenario.go — the trial's fixed seed, reimplemented.
//
// The suite builds every name it uses from one constant seed (a xorshift64* stream seeded by
// FNV-1a over a label). The login a bearer token owns is users[0].login of that scenario, so
// this derives the same login from the same seed rather than being told it. No clock, no
// random source: two runs see the same names.

const (
	scenarioSeed  uint64 = 0x6a09e667f3bcc909
	scenarioUsers        = 12
)

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// fnv1a64 is FNV-1a, 64-bit.
func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

// mix64 is splitmix64's finalizer.
func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// rng is xorshift64*, one named stream.
type rng struct{ state uint64 }

func newRng(seed uint64) *rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &rng{state: seed}
}

func rngFromLabel(label string) *rng {
	return newRng(mix64(scenarioSeed ^ fnv1a64(label)))
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	limit := uint64(1)<<32 - (uint64(1)<<32)%uint64(maxExclusive)
	for {
		value := uint64(r.nextU32())
		if value < limit {
			return int(value % uint64(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

// scenarioUser is one generated account.
type scenarioUser struct {
	id    int
	login string
	name  string
	email string
}

// scenarioUserList builds count unique users in the scenario's own order.
func scenarioUserList(count int) []scenarioUser {
	stream := rngFromLabel("users")
	used := make(map[string]bool, count)
	out := make([]scenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := stream.pick(firstNames)
		last := stream.pick(lastNames)
		base := first + last
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		out = append(out, scenarioUser{
			id:    1000 + i,
			login: login,
			name:  first + " " + last,
			email: login + "@example.invalid",
		})
	}
	return out
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}

// users is the scenario's user list, built once.
var users = scenarioUserList(scenarioUsers)

// viewerLogin is the login the trial's bearer token owns (scenario.viewer).
var viewerLogin = users[0].login

// userViewFor answers the account a login names: a scenario user when it is one, otherwise an
// id derived from the login itself (FNV-1a, 32-bit), so a foreign token's account is stable.
func userViewFor(login string) scenarioUser {
	for i := range users {
		if users[i].login == login {
			return users[i]
		}
	}
	hash := uint32(2166136261)
	for i := 0; i < len(login); i++ {
		hash ^= uint32(login[i])
		hash *= 16777619
	}
	id := 100000 + int(hash%900000)
	return scenarioUser{id: id, login: login, name: login, email: login + "@example.invalid"}
}
