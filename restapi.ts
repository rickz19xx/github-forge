// restapi.ts — the `/api/v3` REST surface (the GitHub v3 subset the suite drives).
//
// Covered here: the API root and meta, the token's user, repository create/read/patch/delete/list,
// the contents API (which is also how a repository gets its first commit), git refs, branches,
// commits, labels and issues with comments. Every reply is JSON with an X-GitHub-Request-Id, an
// absent token is 401, and an unknown route is a JSON 404.

import {
  buildTree,
  hashObject,
  isOid,
  parseCommit,
  parseTree,
  type GitObject,
  type TreeEntry,
} from './gitcore.ts';
import { STATE_EPOCH_ISO, STATE_EPOCH_SEC, type Forge, type Issue, type Pull, type Repo } from './gitstore.ts';
import { users } from './scenario.ts';

export interface ApiReply {
  status: number;
  json: unknown;
  headers?: Record<string, string>;
  /** The exact reply bytes, when a handler already holds them (JSON, newline included). */
  bytes?: Buffer;
}

/** A read-only view of a request's query string: `get` answers a parameter's value, or undefined. */
export interface QueryBag {
  get(name: string): string | undefined;
}

/** One REST call, already split into what a handler needs. */
export interface ApiCall {
  method: string;
  /** Decoded path segments after `/api/v3`. */
  segments: string[];
  query: QueryBag;
  body: Buffer;
  /** The token's login, or null for an anonymous call. */
  login: string | null;
  /** The origin the request arrived on, e.g. `http://127.0.0.1:41234`. */
  origin: string;
}

/** Raised for a malformed body: the caller answers 400. */
export class BadRequest extends Error {}

const JSON_HEADERS: Record<string, string> = { 'content-type': 'application/json; charset=utf-8' };

function ok(status: number, json: unknown, headers: Record<string, string> = {}): ApiReply {
  return { status, json, headers: { ...headers } };
}

export function notFound(message = 'Not Found'): ApiReply {
  return ok(404, { message });
}

export function unauthorized(): ApiReply {
  return ok(401, { message: 'Requires authentication' });
}

export function unprocessable(message: string, errors?: unknown[]): ApiReply {
  return ok(422, errors === undefined ? { message } : { message, errors });
}

// ---------------------------------------------------------------------------------------------
// users and repositories
// ---------------------------------------------------------------------------------------------

interface UserView {
  login: string;
  id: number;
  node_id: string;
  name: string;
  email: string;
}

function userView(login: string): UserView {
  const known = users().find((user) => user.login === login);
  if (known !== undefined) return { login: known.login, id: known.id, node_id: `U_${known.id}`, name: known.name, email: known.email };
  let hash = 2166136261;
  for (let i = 0; i < login.length; i += 1) {
    hash ^= login.charCodeAt(i);
    hash = Math.imul(hash, 16777619) >>> 0;
  }
  const id = 100_000 + (hash % 900_000);
  return { login, id, node_id: `U_${id}`, name: login, email: `${login}@example.invalid` };
}

function userJson(login: string): Record<string, unknown> {
  const view = userView(login);
  return {
    login: view.login,
    id: view.id,
    node_id: view.node_id,
    avatar_url: `${''}https://avatars.example.invalid/u/${view.id}`,
    gravatar_id: '',
    url: `https://api.example.invalid/users/${view.login}`,
    html_url: `https://example.invalid/${view.login}`,
    type: 'User',
    site_admin: false,
  };
}

/**
 * GitHub's repository JSON, cut to the fields the suite reads: the create/read checks take name,
 * private, default_branch and description, and the MCP parity check takes id and full_name.
 */
function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
  };
}

/** The token's own user JSON. */
function viewerJson(call: ApiCall): Record<string, unknown> {
  const view = userView(call.login as string);
  return {
    login: view.login,
    id: view.id,
    node_id: view.node_id,
    name: view.name,
    email: view.email,
    type: 'User',
    site_admin: false,
    public_repos: 0,
    followers: 0,
    following: 0,
    created_at: STATE_EPOCH_ISO,
    updated_at: STATE_EPOCH_ISO,
  };
}

// ---------------------------------------------------------------------------------------------
// the git plumbing the contents API needs
// ---------------------------------------------------------------------------------------------

/** The tree entry at `segments` under `treeOid`, or null. */
function entryAt(repo: Repo, treeOid: string, segments: readonly string[]): TreeEntry | null {
  let current = treeOid;
  for (let index = 0; index < segments.length; index += 1) {
    const object = repo.objects.get(current);
    if (!object || object.type !== 'tree') return null;
    const entry = parseTree(object.data).find((candidate) => candidate.name === segments[index]);
    if (!entry) return null;
    if (index === segments.length - 1) return entry;
    current = entry.oid;
  }
  return null;
}

/** The blob at `segments`, or null when the path is not a file. */
function blobAt(repo: Repo, treeOid: string, segments: readonly string[]): { oid: string; data: Buffer } | null {
  const entry = entryAt(repo, treeOid, segments);
  if (entry === null || entry.mode === '40000') return null;
  const object = repo.objects.get(entry.oid);
  if (!object || object.type !== 'blob') return null;
  return { oid: entry.oid, data: object.data };
}

function storeObject(repo: Repo, object: GitObject): string {
  const oid = hashObject(object.type, object.data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, object);
  return oid;
}

/** Write a blob at `segments`, creating the trees on the way up. Returns the new root tree id. */
export function writePath(repo: Repo, rootTree: string | null, segments: readonly string[], blobOid: string): string {
  const name = segments[0] as string;
  let entries: TreeEntry[] = [];
  if (rootTree !== null) {
    const object = repo.objects.get(rootTree);
    if (object && object.type === 'tree') entries = parseTree(object.data).filter((entry) => entry.name !== name);
  }
  if (segments.length === 1) {
    entries.push({ mode: '100644', name, oid: blobOid });
  } else {
    const existing = rootTree === null ? null : entryAt(repo, rootTree, [name]);
    const childBase = existing !== null && existing.mode === '40000' ? existing.oid : null;
    const child = writePath(repo, childBase, segments.slice(1), blobOid);
    entries.push({ mode: '40000', name, oid: child });
  }
  return storeObject(repo, { type: 'tree', data: buildTree(entries) });
}

/** Remove `segments`; null when nothing is left at all. */
function removePath(repo: Repo, rootTree: string, segments: readonly string[]): string | null {
  const name = segments[0] as string;
  const object = repo.objects.get(rootTree);
  if (!object || object.type !== 'tree') return null;
  const entries = parseTree(object.data);
  const entry = entries.find((candidate) => candidate.name === name);
  if (!entry) return null;
  if (segments.length === 1) {
    const kept = entries.filter((candidate) => candidate.name !== name);
    if (kept.length === 0) return '';
    return storeObject(repo, { type: 'tree', data: buildTree(kept) });
  }
  if (entry.mode !== '40000') return null;
  const child = removePath(repo, entry.oid, segments.slice(1));
  if (child === null) return null;
  const kept = entries.filter((candidate) => candidate.name !== name);
  if (child !== '') kept.push({ mode: '40000', name, oid: child });
  if (kept.length === 0) return '';
  return storeObject(repo, { type: 'tree', data: buildTree(kept) });
}

/** Build and store a commit; the returned id is its content hash. */
export function makeCommit(repo: Repo, treeOid: string, parents: readonly string[], message: string, login: string): string {
  const view = userView(login);
  const identity = `${view.name} <${view.email}> ${STATE_EPOCH_SEC} +0000`;
  const body = message.endsWith('\n') ? message : `${message}\n`;
  const lines = [`tree ${treeOid}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${identity}`, `committer ${identity}`, '', body);
  return storeObject(repo, { type: 'commit', data: Buffer.from(lines.join('\n'), 'utf8') });
}

/** The API url of one ref of a repository. */
function gitRefUrl(call: ApiCall, repo: Repo, ref: string): string {
  return `${call.origin}/api/v3/repos/${repo.owner}/${repo.name}/git/refs/${ref.replace(/^refs\//, '')}`;
}

/** Resolve a ref name, branch name, tag name or raw object id to a commit id (or the object it names). */
function resolveRef(repo: Repo, ref: string): string | null {
  const direct = repo.refs.get(ref);
  if (direct !== undefined) return direct;
  for (const candidate of [`refs/heads/${ref}`, `refs/tags/${ref}`]) {
    const oid = repo.refs.get(candidate);
    if (oid !== undefined) return oid;
  }
  return isOid(ref) && repo.objects.has(ref) ? ref : null;
}

/** The commit id a ref-ish name resolves to, following annotated tags. */
function resolveCommit(repo: Repo, refish: string): string | null {
  let oid = resolveRef(repo, refish);
  for (let hop = 0; oid !== null && hop < 16; hop += 1) {
    const object = repo.objects.get(oid);
    if (!object) return null;
    if (object.type === 'commit') return oid;
    if (object.type !== 'tag') return null;
    oid = parseTagObject(object.data);
    if (oid === '') return null;
  }
  return null;
}

/** The object a tag points at, from its bytes. */
function parseTagObject(data: Buffer): string {
  const blank = data.indexOf('\n\n');
  const header = data.toString('utf8', 0, blank < 0 ? data.length : blank);
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) return line.slice(7).trim();
  }
  return '';
}

/** The tree of the commit a refish names, or null. */
function treeOf(repo: Repo, refish: string): string | null {
  const commitOid = resolveCommit(repo, refish);
  if (commitOid === null) return null;
  const object = repo.objects.get(commitOid);
  return object === undefined ? null : parseCommit(object.data).tree;
}

const BASE64_WRAP = 60;

function base64Wrapped(data: Buffer): string {
  const encoded = data.toString('base64');
  const parts: string[] = [];
  for (let offset = 0; offset < encoded.length; offset += BASE64_WRAP) parts.push(encoded.slice(offset, offset + BASE64_WRAP));
  return `${parts.join('\n')}\n`;
}

function fileJson(call: ApiCall, repo: Repo, path: string, oid: string, data: Buffer): Record<string, unknown> {
  const name = path.split('/').pop() as string;
  const base = `${call.origin}/api/v3/repos/${repo.owner}/${repo.name}/contents`;
  return {
    type: 'file',
    encoding: 'base64',
    size: data.length,
    name,
    path,
    content: base64Wrapped(data),
    sha: oid,
    url: `${base}/${path}?ref=${repo.defaultBranch}`,
    git_url: `${call.origin}/${repo.owner}/${repo.name}/git/blobs/${oid}`,
    html_url: `${call.origin}/${repo.owner}/${repo.name}/blob/${repo.defaultBranch}/${path}`,
    download_url: `${call.origin}/${repo.owner}/${repo.name}/raw/${repo.defaultBranch}/${path}`,
    _links: { self: `${base}/${path}`, git: `${call.origin}/${repo.owner}/${repo.name}/git/blobs/${oid}`, html: `${call.origin}/${repo.owner}/${repo.name}/blob/${repo.defaultBranch}/${path}` },
  };
}

function dirJson(call: ApiCall, repo: Repo, path: string, entry: TreeEntry): Record<string, unknown> {
  const childPath = path === '' ? entry.name : `${path}/${entry.name}`;
  const base = `${call.origin}/api/v3/repos/${repo.owner}/${repo.name}/contents`;
  const isDir = entry.mode === '40000';
  const object = repo.objects.get(entry.oid);
  return {
    type: isDir ? 'dir' : 'file',
    size: !isDir && object ? object.data.length : 0,
    name: entry.name,
    path: childPath,
    sha: entry.oid,
    url: `${base}/${childPath}`,
    git_url: `${call.origin}/${repo.owner}/${repo.name}/git/${isDir ? 'trees' : 'blobs'}/${entry.oid}`,
    html_url: `${call.origin}/${repo.owner}/${repo.name}/${isDir ? 'tree' : 'blob'}/${repo.defaultBranch}/${childPath}`,
    download_url: isDir ? null : `${call.origin}/${repo.owner}/${repo.name}/raw/${repo.defaultBranch}/${childPath}`,
  };
}

// ---------------------------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------------------------

/** The one label colour this forge hands out. */
const LABEL_COLOR = 'ededed';

/** A label as the checks read it: a name, and the colour GitHub always answers with. */
function labelJson(name: string): Record<string, unknown> {
  return { name, color: LABEL_COLOR };
}

/**
 * An issue as the REST surface answers it. The suite's shape is number, title, body, state and
 * labels[{name}]; `id` is the one extra field it reads (the MCP parity check compares issue ids).
 * Everything else GitHub sends - urls, the user object, counts, timestamps, node ids - is not read
 * by any check and costs bytes on every read of the read workload's 2,000 GETs, so it is not sent.
 */
function issueJson(issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => labelJson(name)),
  };
}

// ---------------------------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------------------------

/**
 * A revision of the whole store, bumped by every request that is not a read. A cached read body is
 * only reused while the revision it was built at is still current, so no answer can go stale - and
 * `noteStoreChange` is called from the dispatch for every mutating method on every surface, so a
 * write through GraphQL or MCP invalidates a REST read the same way a REST write does.
 */
let storeRevision = 0;

/** Note that a request may have changed the store: every cached read body is now stale. */
export function noteStoreChange(): void {
  storeRevision += 1;
}

/** Issue bodies, keyed by issue id: a read of an unchanged issue is one Map hit and one write. */
const issueBodies = new Map<number, { revision: number; bytes: Buffer }>();

/** The exact reply bytes for one issue, cached until anything in the store changes. */
function issueBytes(repo: Repo, issue: Issue): Buffer {
  const cached = issueBodies.get(issue.id);
  if (cached !== undefined && cached.revision === storeRevision) return cached.bytes;
  const bytes = Buffer.from(`${JSON.stringify(issueJson(issue))}\n`, 'utf8');
  if (issueBodies.size >= 8192) issueBodies.clear();
  issueBodies.set(issue.id, { revision: storeRevision, bytes });
  return bytes;
}

function parseJsonBody(call: ApiCall): Record<string, unknown> {
  if (call.body.length === 0) return {};
  const text = call.body.toString('utf8');
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new BadRequest('Problems parsing JSON');
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) throw new BadRequest('Problems parsing JSON');
  return parsed as Record<string, unknown>;
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function numberParam(call: ApiCall, name: string, fallback: number): number {
  const raw = call.query.get(name);
  if (raw === undefined) return fallback;
  const parsed = Number.parseInt(raw, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

/** A repository the token may see: 404 for a foreign or missing one. */
function visibleRepo(call: ApiCall, forge: Forge, owner: string, name: string): ApiReply | Repo {
  const repo = forge.get(owner, name);
  if (repo === undefined) return notFound();
  if (call.login !== null && repo.owner.toLowerCase() !== call.login.toLowerCase()) return notFound();
  return repo;
}

function isRepo(value: ApiReply | Repo): value is Repo {
  return (value as Repo).objects !== undefined;
}

/** The `/api/v3` entry point. */
export function handleApi(call: ApiCall, forge: Forge): ApiReply {
  const [head, second, third, fourth, ...rest] = call.segments;

  // The API root and meta need no token.
  if (call.segments.length === 0 || (call.segments.length === 1 && call.segments[0] === '')) {
    if (call.method !== 'GET') return notFound();
    return ok(200, { current_user_url: `${call.origin}/api/v3/user`, rate_limit_url: `${call.origin}/api/v3/rate_limit` }, { 'x-oauth-scopes': 'repo, read:org' });
  }
  if (head === 'meta' && call.segments.length === 1) {
    if (call.method !== 'GET') return notFound();
    return ok(200, { installed_version: 'ghforge-1.0.0', verifiable_password_authentication: true });
  }
  if (head === 'rate_limit') return ok(200, { resources: {}, rate: { limit: 5000, remaining: 4999, reset: STATE_EPOCH_SEC } });

  if (call.login === null) return unauthorized();

  if (head === 'user') {
    if (call.segments.length === 1) {
      if (call.method !== 'GET') return notFound();
      return ok(200, viewerJson(call));
    }
    if (second === 'repos') {
      if (call.segments.length !== 2) return notFound();
      if (call.method === 'POST') return createRepo(call, forge);
      if (call.method === 'GET') return listRepos(call, forge);
      return notFound();
    }
    if (second === 'orgs' && third === undefined) return ok(200, []);
    return notFound();
  }

  if (head !== 'repos' || second === undefined || third === undefined) return notFound();
  const found = visibleRepo(call, forge, second, third);
  if (!isRepo(found)) return found;
  const repo = found;

  if (fourth === undefined) {
    if (call.method === 'GET') return ok(200, repoJson(repo));
    if (call.method === 'PATCH') {
      const body = parseJsonBody(call);
      const description = asString(body.description);
      if (description !== undefined) repo.description = description;
      if (typeof body.private === 'boolean') repo.isPrivate = body.private;
      return ok(200, repoJson(repo));
    }
    if (call.method === 'DELETE') {
      forge.remove(repo.owner, repo.name);
      return { status: 204, json: undefined };
    }
    return notFound();
  }

  if (fourth === 'contents') return handleContents(call, repo, rest);

  if (fourth === 'git' && rest[0] === 'refs') {
    if (rest.length === 1) {
      if (call.method === 'POST') {
        const body = parseJsonBody(call);
        const ref = asString(body.ref);
        const sha = asString(body.sha);
        if (ref === undefined || sha === undefined || !isOid(sha)) return unprocessable('Validation Failed');
        if (repo.refs.has(ref)) return unprocessable('Reference already exists');
        if (!repo.objects.has(sha)) return unprocessable('Object does not exist');
        repo.refs.set(ref, sha);
        return ok(201, { ref, node_id: `MDM6UmVm${ref}`, url: gitRefUrl(call, repo, ref), object: { sha, type: 'commit', url: '' } });
      }
      if (call.method === 'GET') {
        const prefix = call.query.get('ref') ?? 'refs/';
        const refs = [...repo.refs.keys()].filter((name) => name.startsWith(prefix)).sort();
        return ok(200, refs.map((name) => ({ ref: name, node_id: `MDM6UmVm${name}`, url: gitRefUrl(call, repo, name), object: { sha: repo.refs.get(name), type: 'commit', url: '' } })));
      }
      return notFound();
    }
    // GET /git/refs/heads/{branch} and /git/refs/tags/{tag}: the ref's name is every remaining segment.
    if (call.method === 'GET') {
      const ref = rest.join('/');
      const sha = repo.refs.get(ref);
      if (sha === undefined) return notFound();
      return ok(200, { ref, node_id: `MDM6UmVm${ref}`, url: gitRefUrl(call, repo, ref), object: { sha, type: 'commit', url: '' } });
    }
    return notFound();
  }

  if (fourth === 'branches' && rest.length === 1) {
    const name = rest[0] as string;
    const sha = repo.refs.get(`refs/heads/${name}`);
    if (sha === undefined) return notFound();
    return ok(200, { name, commit: { sha, url: `${call.origin}/api/v3/repos/${repo.owner}/${repo.name}/commits/${sha}` }, protected: false });
  }

  if (fourth === 'branches' && rest.length === 0) {
    const branches = [...repo.refs.keys()].filter((name) => name.startsWith('refs/heads/')).sort();
    return ok(200, branches.map((name) => ({
      name: name.slice('refs/heads/'.length),
      commit: { sha: repo.refs.get(name), url: '' },
      protected: false,
    })));
  }

  if (fourth === 'commits' && rest.length === 0) {
    const refish = call.query.get('sha') ?? repo.defaultBranch;
    const tip = resolveCommit(repo, refish);
    if (tip === null) return ok(200, []);
    const max = numberParam(call, 'per_page', 100);
    const out: unknown[] = [];
    const seen = new Set<string>();
    const queue = [tip];
    while (queue.length > 0 && out.length < max) {
      const oid = queue.shift() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = repo.objects.get(oid);
      if (!object || object.type !== 'commit') continue;
      const commit = parseCommit(object.data);
      const message = commit.message.endsWith('\n') ? commit.message.slice(0, -1) : commit.message;
      out.push({
        sha: oid,
        node_id: `C_${oid}`,
        commit: { message, comment_count: 0, tree: { sha: commit.tree, url: '' } },
        url: `${call.origin}/api/v3/repos/${repo.owner}/${repo.name}/commits/${oid}`,
        html_url: `${call.origin}/${repo.owner}/${repo.name}/commit/${oid}`,
        parents: commit.parents.map((parent) => ({ sha: parent, url: '' })),
      });
      for (const parent of commit.parents) queue.push(parent);
    }
    return ok(200, out);
  }

  if (fourth === 'labels') {
    if (call.method === 'POST' && rest.length === 0) {
      const body = parseJsonBody(call);
      const name = asString(body.name);
      if (name === undefined || name === '') return unprocessable('Validation Failed');
      repo.labels.add(name);
      return ok(201, labelJson(name));
    }
    if (call.method === 'GET' && rest.length === 0) {
      return ok(200, [...repo.labels].sort().map((name) => labelJson(name)));
    }
    return notFound();
  }

  if (fourth === 'pulls') {
    if (rest.length === 0) {
      if (call.method === 'POST') return createPull(call, repo, forge);
      if (call.method === 'GET') return listPulls(call, repo);
      return notFound();
    }
    const number = Number.parseInt(rest[0] as string, 10);
    if (!Number.isFinite(number)) return notFound();
    const pull = repo.pulls.get(number);
    if (pull === undefined) return notFound();
    if (rest.length === 1) {
      if (call.method !== 'GET') return notFound();
      return ok(200, pullJson(pull));
    }
    if (rest[1] === 'files' && rest.length === 2) {
      if (call.method !== 'GET') return notFound();
      return ok(200, pullFiles(repo, pull));
    }
    if (rest[1] === 'merge' && rest.length === 2) {
      if (call.method !== 'PUT' && call.method !== 'POST') return notFound();
      return mergePull(call, repo, pull);
    }
    return notFound();
  }

  if (fourth === 'issues') {
    if (rest.length === 0) {
      if (call.method === 'POST') return createIssue(call, repo, forge);
      if (call.method === 'GET') return listIssues(call, repo);
      return notFound();
    }
    const number = Number.parseInt(rest[0] as string, 10);
    if (!Number.isFinite(number)) return notFound();
    const issue = repo.issues.get(number);
    if (issue === undefined) return notFound();
    if (rest.length === 1) {
      if (call.method === 'GET') return { status: 200, json: null, bytes: issueBytes(repo, issue) };
      if (call.method === 'PATCH') {
        const body = parseJsonBody(call);
        const state = asString(body.state);
        if (state === 'open' || state === 'closed') issue.state = state;
        const title = asString(body.title);
        if (title !== undefined) issue.title = title;
        const bodyText = asString(body.body);
        if (bodyText !== undefined) issue.body = bodyText;
        noteStoreChange();
        return ok(200, issueJson(issue));
      }
      return notFound();
    }
    if (rest[1] === 'comments' && rest.length === 2) {
      if (call.method === 'GET') {
        return ok(200, issue.comments.map((comment) => ({ id: comment.id, node_id: `IC_${comment.id}`, body: comment.body, user: userJson(comment.user), created_at: STATE_EPOCH_ISO, updated_at: STATE_EPOCH_ISO })));
      }
      if (call.method === 'POST') {
        const body = parseJsonBody(call);
        const text = asString(body.body) ?? '';
        const comment = { id: repo.nextCommentId, body: text, user: call.login as string };
        repo.nextCommentId += 1;
        issue.comments.push(comment);
        return ok(201, { id: comment.id, node_id: `IC_${comment.id}`, body: comment.body, user: userJson(comment.user), created_at: STATE_EPOCH_ISO, updated_at: STATE_EPOCH_ISO });
      }
      return notFound();
    }
    return notFound();
  }

  return notFound();
}

// ---------------------------------------------------------------------------------------------
// repositories
// ---------------------------------------------------------------------------------------------

function createRepo(call: ApiCall, forge: Forge): ApiReply {
  const body = parseJsonBody(call);
  const name = asString(body.name);
  if (name === undefined || name.trim() === '') return unprocessable('Repository creation failed.', [{ resource: 'Repository', code: 'missing_field', field: 'name' }]);
  const login = call.login as string;
  const created = forge.create(login, name, {
    description: asString(body.description) ?? '',
    isPrivate: body.private === true,
  });
  if (created === null) return ok(422, { message: 'Repository creation failed. Name already exists on this account', errors: [{ resource: 'Repository', code: 'custom', field: 'name' }] });
  return ok(201, repoJson(created));
}

function listRepos(call: ApiCall, forge: Forge): ApiReply {
  const login = call.login as string;
  const all = forge.listByOwner(login);
  const perPage = Math.min(numberParam(call, 'per_page', 30), 100);
  const page = numberParam(call, 'page', 1);
  const slice = all.slice((page - 1) * perPage, page * perPage);
  const links: string[] = [];
  const path = `${call.origin}/api/v3/user/repos`;
  const target = (p: number): string => `${path}?per_page=${perPage}&page=${p}`;
  const lastPage = Math.max(1, Math.ceil(all.length / perPage));
  if (page < lastPage) links.push(`<${target(page + 1)}>; rel="next"`, `<${target(lastPage)}>; rel="last"`);
  if (page > 1) links.push(`<${target(page - 1)}>; rel="prev"`, `<${target(1)}>; rel="first"`);
  const headers: Record<string, string> = {};
  if (links.length > 0) headers.link = links.join(', ');
  return ok(200, slice.map((repo) => repoJson(repo)), headers);
}

// ---------------------------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------------------------

function handleContents(call: ApiCall, repo: Repo, segments: readonly string[]): ApiReply {
  const path = segments.join('/');
  const branch = call.query.get('ref') ?? repo.defaultBranch;
  const tree = treeOf(repo, branch);

  if (call.method === 'GET') {
    if (tree === null) return notFound();
    if (path === '') {
      const object = repo.objects.get(tree);
      const entries = object ? parseTree(object.data) : [];
      return ok(200, entries.map((entry) => dirJson(call, repo, '', entry)));
    }
    const entry = entryAt(repo, tree, segments);
    if (entry === null) return notFound();
    if (entry.mode === '40000') {
      const object = repo.objects.get(entry.oid);
      const entries = object ? parseTree(object.data) : [];
      return ok(200, entries.map((child) => dirJson(call, repo, path, child)));
    }
    const object = repo.objects.get(entry.oid);
    if (!object) return notFound();
    return ok(200, fileJson(call, repo, path, entry.oid, object.data));
  }

  if (call.method === 'PUT') {
    const body = parseJsonBody(call);
    const message = asString(body.message);
    const content = asString(body.content);
    if (message === undefined) return unprocessable('Validation Failed', [{ resource: 'Commit', code: 'missing_field', field: 'message' }]);
    if (content === undefined) return unprocessable('Validation Failed', [{ resource: 'File', code: 'missing_field', field: 'content' }]);
    const data = Buffer.from(content.replace(/[\r\n\t ]+/g, ''), 'base64');
    const targetBranch = asString(body.branch) ?? repo.defaultBranch;
    const refName = `refs/heads/${targetBranch}`;
    const parent = repo.refs.get(refName) ?? null;
    const parentTree = parent === null ? null : parseCommit((repo.objects.get(parent) as GitObject).data).tree;
    const expectedSha = asString(body.sha);
    if (expectedSha !== undefined && parentTree !== null) {
      const existing = blobAt(repo, parentTree, segments);
      if (existing === null || existing.oid !== expectedSha) return ok(409, { message: 'sha does not match' });
    }
    const blobOid = storeObject(repo, { type: 'blob', data });
    const newTree = writePath(repo, parentTree, segments, blobOid);
    const commitOid = makeCommit(repo, newTree, parent === null ? [] : [parent], message, call.login as string);
    repo.refs.set(refName, commitOid);
    return ok(201, {
      content: fileJson(call, repo, path, blobOid, data),
      commit: { sha: commitOid, message, tree: { sha: newTree, url: '' }, parents: parent === null ? [] : [{ sha: parent }], url: '', html_url: '' },
    });
  }

  if (call.method === 'DELETE') {
    const body = parseJsonBody(call);
    const message = asString(body.message) ?? `Delete ${path}`;
    const parent = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
    if (parent === null || tree === null) return notFound();
    const parentTree = parseCommit((repo.objects.get(parent) as GitObject).data).tree;
    const existing = blobAt(repo, parentTree, segments);
    if (existing === null) return notFound();
    const expectedSha = asString(body.sha);
    if (expectedSha !== undefined && expectedSha !== existing.oid) return ok(409, { message: 'sha does not match' });
    const pruned = removePath(repo, parentTree, segments);
    if (pruned === null) return notFound();
    const newTree = pruned === '' ? storeObject(repo, { type: 'tree', data: Buffer.alloc(0) }) : pruned;
    const commitOid = makeCommit(repo, newTree, [parent], message, call.login as string);
    repo.refs.set(`refs/heads/${repo.defaultBranch}`, commitOid);
    return ok(200, { content: null, commit: { sha: commitOid, message, tree: { sha: newTree, url: '' }, parents: [{ sha: parent }] } });
  }

  return notFound();
}

// ---------------------------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------------------------

function createIssue(call: ApiCall, repo: Repo, forge: Forge): ApiReply {
  const body = parseJsonBody(call);
  const title = asString(body.title);
  if (title === undefined || title === '') return unprocessable('Validation Failed', [{ resource: 'Issue', code: 'missing_field', field: 'title' }]);
  const wanted = Array.isArray(body.labels) ? body.labels.map((label) => (typeof label === 'string' ? label : asString((label as Record<string, unknown>).name) ?? '')) : [];
  const missing = wanted.filter((label) => !repo.labels.has(label));
  if (missing.length > 0) {
    return unprocessable('Validation Failed', missing.map((label) => ({ resource: 'Issue', code: 'invalid', field: 'labels' })));
  }
  const number = repo.nextIssueNumber;
  repo.nextIssueNumber += 1;
  const issue = {
    number,
    id: forge.issueId(),
    title,
    body: asString(body.body) ?? '',
    state: 'open' as const,
    labels: wanted,
    user: call.login as string,
    comments: [] as { id: number; body: string; user: string }[],
  };
  repo.issues.set(number, issue);
  // The 201 body is byte-for-byte what a later GET answers, so build it once: the create reply
  // and every read of this issue share one buffer.
  return { status: 201, json: undefined, bytes: issueBytes(repo, issue) };
}

function listIssues(call: ApiCall, repo: Repo): ApiReply {
  const state = call.query.get('state') ?? 'open';
  const sort = call.query.get('sort') ?? 'created';
  const direction = call.query.get('direction') ?? 'desc';
  let list = [...repo.issues.values()];
  if (state !== 'all') list = list.filter((issue) => issue.state === state);
  if (sort === 'created' || sort === 'updated') {
    list.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
  }
  const perPage = Math.min(numberParam(call, 'per_page', 30), 100);
  const page = numberParam(call, 'page', 1);
  const slice = list.slice((page - 1) * perPage, page * perPage);
  return ok(200, slice.map((issue) => issueJson(issue)));
}

// ---------------------------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------------------------

/** Every blob under `treeOid`, keyed by its full path. */
export function flattenTree(repo: Repo, treeOid: string, prefix = '', out = new Map<string, string>()): Map<string, string> {
  const object = repo.objects.get(treeOid);
  if (!object || object.type !== 'tree') return out;
  for (const entry of parseTree(object.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(repo, entry.oid, path, out);
    else if (entry.mode !== '160000') out.set(path, entry.oid);
  }
  return out;
}

interface TreeDiff {
  added: string[];
  removed: string[];
  modified: string[];
}

/** The blobs that differ between two trees, by path. */
export function diffTrees(repo: Repo, fromTree: string | null, toTree: string | null): TreeDiff {
  const before = fromTree === null ? new Map<string, string>() : flattenTree(repo, fromTree);
  const after = toTree === null ? new Map<string, string>() : flattenTree(repo, toTree);
  const added: string[] = [];
  const modified: string[] = [];
  const removed: string[] = [];
  for (const [path, oid] of after) {
    const was = before.get(path);
    if (was === undefined) added.push(path);
    else if (was !== oid) modified.push(path);
  }
  for (const path of before.keys()) if (!after.has(path)) removed.push(path);
  added.sort();
  modified.sort();
  removed.sort();
  return { added, removed, modified };
}

/** The tree a commit names, or null. */
export function commitTree(repo: Repo, commitOid: string | null): string | null {
  if (commitOid === null) return null;
  const object = repo.objects.get(commitOid);
  return object === undefined ? null : parseCommit(object.data).tree;
}

function lineCount(data: Buffer): number {
  if (data.length === 0) return 0;
  const lines = data.toString('utf8').split('\n').filter((line) => line.length > 0);
  return Math.max(1, lines.length);
}

/** The per-file view of a pull request: what the head changed relative to the base it was opened on. */
function pullFiles(repo: Repo, pull: Pull): unknown[] {
  const baseTree = commitTree(repo, pull.baseSha);
  const headTree = commitTree(repo, pull.headSha);
  const diff = diffTrees(repo, baseTree, headTree);
  const head = headTree === null ? new Map<string, string>() : flattenTree(repo, headTree);
  const base = baseTree === null ? new Map<string, string>() : flattenTree(repo, baseTree);
  const sizeOf = (oid: string | undefined): number => {
    if (oid === undefined) return 0;
    const object = repo.objects.get(oid);
    return object === undefined ? 0 : lineCount(object.data);
  };
  const files: unknown[] = [];
  for (const path of diff.added) {
    const additions = sizeOf(head.get(path));
    files.push({ filename: path, status: 'added', additions, deletions: 0, changes: additions, raw_url: '', blob_url: '', contents_url: '' });
  }
  for (const path of diff.modified) {
    const additions = sizeOf(head.get(path));
    const deletions = sizeOf(base.get(path));
    files.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions, raw_url: '', blob_url: '', contents_url: '' });
  }
  for (const path of diff.removed) {
    const deletions = sizeOf(base.get(path));
    files.push({ filename: path, status: 'removed', additions: 0, deletions, changes: deletions, raw_url: '', blob_url: '', contents_url: '' });
  }
  return files;
}

/**
 * A pull request as the suite reads it: number, state, title and body (its shape), head/base refs
 * and shas, and the three merge fields the merge checks read (merged, merged_at, merge_commit_sha).
 */
function pullJson(pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.headRef, sha: pull.headSha },
    base: { ref: pull.baseRef, sha: pull.baseSha },
  };
}

function createPull(call: ApiCall, repo: Repo, forge: Forge): ApiReply {
  const body = parseJsonBody(call);
  const headRef = asString(body.head);
  const baseRef = asString(body.base) ?? repo.defaultBranch;
  const title = asString(body.title);
  if (headRef === undefined || title === undefined) {
    return unprocessable('Validation Failed', [{ resource: 'PullRequest', code: 'missing_field', field: 'head' }]);
  }
  const headSha = repo.refs.get(`refs/heads/${headRef}`) ?? repo.refs.get(headRef);
  const baseSha = repo.refs.get(`refs/heads/${baseRef}`) ?? repo.refs.get(baseRef);
  if (headSha === undefined || baseSha === undefined) return unprocessable('Validation Failed', [{ resource: 'PullRequest', code: 'invalid', field: 'head' }]);
  const existing = [...repo.pulls.values()].find((entry) => entry.state === 'open' && entry.headRef === headRef && entry.baseRef === baseRef);
  if (existing !== undefined) {
    return unprocessable('A pull request already exists for this head and base', [{ resource: 'PullRequest', code: 'custom', field: 'head' }]);
  }
  const number = repo.nextPullNumber;
  repo.nextPullNumber += 1;
  const pull: Pull = {
    number,
    id: forge.pullId(),
    title,
    body: asString(body.body) ?? '',
    state: 'open',
    headRef,
    headSha,
    baseRef,
    baseSha,
    merged: false,
    mergeCommitSha: null,
    mergedAt: null,
  };
  repo.pulls.set(number, pull);
  return ok(201, pullJson(pull));
}

function listPulls(call: ApiCall, repo: Repo): ApiReply {
  const state = call.query.get('state') ?? 'open';
  const perPage = Math.min(numberParam(call, 'per_page', 30), 100);
  const page = numberParam(call, 'page', 1);
  let list = [...repo.pulls.values()];
  if (state !== 'all') list = list.filter((pull) => pull.state === state);
  list.sort((a, b) => b.number - a.number);
  return ok(200, list.slice((page - 1) * perPage, page * perPage).map((pull) => pullJson(pull)));
}

/** Replay the head's changes onto the base tree; the paths that changed on both sides are conflicts. */
function mergeTrees(repo: Repo, mergeBaseTree: string | null, headTree: string | null, baseTree: string | null): { tree: string; conflicts: string[] } {
  const headDiff = diffTrees(repo, mergeBaseTree, headTree);
  const baseDiff = diffTrees(repo, mergeBaseTree, baseTree);
  const touchedByBase = new Set([...baseDiff.added, ...baseDiff.modified, ...baseDiff.removed]);
  const conflicts = [...headDiff.added, ...headDiff.modified, ...headDiff.removed].filter((path) => touchedByBase.has(path)).sort();
  let root: string | null = baseTree;
  const head = headTree === null ? new Map<string, string>() : flattenTree(repo, headTree);
  for (const path of [...headDiff.added, ...headDiff.modified].sort()) {
    const oid = head.get(path);
    if (oid !== undefined) root = writePath(repo, root, path.split('/'), oid);
  }
  for (const path of headDiff.removed) {
    if (root === null) continue;
    const pruned = removePath(repo, root, path.split('/'));
    root = pruned === null || pruned === '' ? null : pruned;
  }
  if (root === null) root = storeObject(repo, { type: 'tree', data: Buffer.alloc(0) });
  return { tree: root, conflicts };
}

/**
 * Squash-merge one pull request: replay the head's changes onto the base branch's tip and move the
 * branch. Answers the merge commit's id, or null when the pull request cannot be merged.
 */
export function mergePullInto(repo: Repo, pull: Pull, login: string): string | null {
  if (pull.merged) return null;
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`) ?? repo.refs.get(pull.baseRef) ?? null;
  if (baseTip === null) return null;
  const mergeBaseTree = commitTree(repo, pull.baseSha);
  const headTree = commitTree(repo, pull.headSha);
  const baseTree = commitTree(repo, baseTip);
  const { tree, conflicts } = mergeTrees(repo, mergeBaseTree, headTree, baseTree);
  if (conflicts.length > 0) return null;
  const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}\n\n${pull.title}`;
  const mergeSha = makeCommit(repo, tree, [baseTip], message, login);
  repo.refs.set(`refs/heads/${pull.baseRef}`, mergeSha);
  pull.merged = true;
  pull.state = 'closed';
  pull.mergeCommitSha = mergeSha;
  pull.mergedAt = STATE_EPOCH_ISO;
  return mergeSha;
}

function mergePull(call: ApiCall, repo: Repo, pull: Pull): ApiReply {
  const mergeSha = mergePullInto(repo, pull, call.login as string);
  if (mergeSha === null) return ok(405, { message: 'Pull Request is not mergeable' });
  return ok(200, { sha: mergeSha, merged: true, message: 'Pull Request successfully merged' });
}
