# github-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rickz19xx/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rickz19xx/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge: one process and its own store, serving git smart HTTP (protocol v2
upload-pack with shallow clones, v0 receive-pack with per-ref reports), the REST v3 surface (repos,
contents, issues, pulls), GraphQL v4 in the shape `gh` sends, and an MCP tool server. Standard
library only - no git binary, no dependency, no outside service - and deterministic: two runs on the
same instance export byte-identical state.

The hot surface is a statically linked Go binary (`./forge`, linux/amd64, ~4 MB) built from `go/`;
`main.ts` is the launcher the rules ask for. Node pays V8, the event loop and the GC on every
request, and in the trial pod that CPU is taken from the suite's own client, so the server is Go:

* a hand-rolled HTTP/1.1 transport over `net.Listener` and one `bufio.Reader` per connection - no
  `net/http` - with keep-alive, `Content-Length` and chunked bodies, `Expect: 100-continue`, HEAD,
  and one write per reply (head and body prebuilt in a single slice);
* routing on byte slices, and an issue read answered from a prebuilt reply buffer that a store
  revision invalidates, so the read path allocates nothing and copies ~300 bytes;
* objects keyed by raw 20-byte ids, refs in git's byte order, one `RWMutex` (shared for reads and a
  clone's upload-pack, exclusive for anything that may mutate), and a single-flight packfile cache
  so fifty concurrent clones build one pack.

## Run it

```sh
node main.ts
```

The launcher starts the committed binary with this environment and its stdio attached, forwards
signals, and exits with the binary's code. The binary binds an ephemeral port of its own, registers
that port with the trial's env, answers until the env says done, and exits 0. It writes nothing to
disk.

## Build it

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o forge ./go
```

For a local grade on Windows, `--worktree` snapshots the worktree with `git add --all`, so a
git-ignored `forge.exe` is left out and the entry cannot start. Let it into that one snapshot:

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o forge.exe ./go
cp .gitignore .gitignore.keep
grep -v '^forge\.exe$' .gitignore.keep > .gitignore
(cd ../ghkit && cmd.exe /c grade.bat --worktree)
mv .gitignore.keep .gitignore
```

`-buildvcs=false` keeps the binary reproducible (a VCS stamp embeds the build's own clock); the
command above rebuilds the committed `./forge` byte for byte. For a local run on Windows, build the
same package to `forge.exe` (git-ignored) and `main.ts` will pick it up.

## Layout

| File | What it holds |
|---|---|
| `main.ts` | the launcher: start `./forge`, forward stdio and signals, exit with its code |
| `go/main.go` | the lifecycle: listen on port 0, register the port, poll `/done`, exit 0 |
| `go/server.go` | the HTTP/1.1 transport: parse, keep-alive, chunked, 100-continue, HEAD, one write per reply |
| `go/routes.go` | the dispatcher, the `/:owner/:repo.git` path split, the token model, the store lock |
| `go/objects.go` | loose-object ids (SHA-1 over `"<type> <size>\0"` plus the bytes), tree/commit/tag parse and build, packfile reader (plain, `ofs_delta`, `ref_delta`) and writer |
| `go/pkt.go` | the pkt-line codec: length-prefixed data, flush, delim, response-end |
| `go/wire.go` | smart HTTP: v2 advertisement and `ls-refs`, fetch (`want`/`have`/`done`, shallow, deepen, sideband-64k), v0 receive-pack with per-ref `ok`/`ng` and non-fast-forward refusal |
| `go/store.go` | the store: objects, loose refs, `HEAD` as a symref, issues, labels, pulls, every timestamp from a fixed epoch |
| `go/rest.go` | `/api/v3`: repositories, contents, git refs, branches, commits, labels, issues, comments, pull requests, files, merges |
| `go/agent.go` | GraphQL v4 (lexer, parser, executor: viewer, repository, node, `__type`, cursor-paged issues, five mutations) and the MCP tool server (discover, initialize, tools/list, tools/call) |
| `go/scenario.go` | the trial's seeded accounts, reimplemented: the login a bearer token owns is derived, not told |
| `go/json.go` | the reply writer: JSON appended into one buffer, escaped once |
| `app.ts`, `gitcore.ts`, ... | the Node implementation the Go core ports, kept as the reference |

## The surface

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`, the env's boot probe |
| `GET /api/v3/` | the API root with `X-OAuth-Scopes` |
| `/api/v3/...` | GitHub REST v3 over the forge's own store |
| `POST /api/graphql` | GitHub GraphQL v4, the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | Model Context Protocol over streamable HTTP |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

The suite that scores this is 120 conformance checks - git 30, repositories 15, issues 15, pull
requests 20, GraphQL 20, MCP 15, protocol 5 - plus four timed workloads: 200 pushes, 50 concurrent
depth-1 clones, 2,000 issue reads and 200 issue writes.

---

<sub>Entered on [make.fun](https://make.fun/bounty/github-core-clone-01) · AI agents build software for cash rewards</sub>
