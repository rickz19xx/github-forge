# github-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rickz19xx/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rickz19xx/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in one Node 22 process and its own store: git smart HTTP (protocol v2
upload-pack with shallow clones, v0 receive-pack with per-ref reports), the REST v3 surface (repos,
contents, issues, pulls), GraphQL v4 in the shape `gh` sends, and an MCP tool server over the same
forge. Standard library only - no git binary, no dependency, no outside service - and deterministic:
two runs on the same instance export byte-identical state.

## Run it

```sh
node main.ts
```

The forge binds an ephemeral port of its own, registers that port with the trial's env, answers until
the env says done, and exits 0.

## Layout

| File | What it holds |
|---|---|
| `main.ts` | the lifecycle: listen on port 0, register the port, poll `/done` |
| `app.ts` | routing, `X-GitHub-Request-Id`, JSON 404s, the ephemeral port |
| `gitcore.ts` | pkt-line codec, blob/tree/commit/tag parse and build, SHA-1 ids, packfile reader (plain, `ofs_delta`, `ref_delta`) and writer |
| `gitwire.ts` | smart HTTP: v2 advertisement and `ls-refs`, fetch (`want`/`have`/`done`, shallow, deepen, sideband-64k), v0 receive-pack with per-ref `ok`/`ng` and non-fast-forward refusal |
| `gitstore.ts` | the store: objects, loose refs, `HEAD` as a symref, issues, labels, pulls, every timestamp from a fixed epoch |
| `restapi.ts` | `/api/v3`: repositories, contents, git refs, branches, commits, labels, issues, comments, pull requests, files, merges |
| `graphql.ts` | a lexer, parser and executor for the v4 wire: viewer, repository, node, `__type`, cursor-paged issues, five mutations |
| `mcp.ts` | JSON-RPC 2.0 tool server: discover, initialize, tools/list, tools/call |

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
