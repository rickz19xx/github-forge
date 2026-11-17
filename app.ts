// app.ts — the entry's HTTP surface: git smart HTTP under `/:owner/:repo.git`, the `/api/v3` REST
// subset, and a JSON 404 (never HTML) for anything else. Every reply carries an X-GitHub-Request-Id.
//
// The server holds all of its state in one Forge (objects, refs, issues) and binds nothing until the
// caller listens: main.ts owns the ephemeral port and the entry <-> env lifecycle.
//
// The transport is httpserver.ts (a hand-rolled HTTP/1.1 over net.Socket); every handler below is
// synchronous and answers one Reply, so a request costs one parse and one write.

import type { Server } from 'node:net';
import { createHash } from 'node:crypto';
import { handleReceivePack, handleUploadPack, handleInfoRefs, WireError, type WireResponse } from './gitwire.ts';
import { Forge, type Repo } from './gitstore.ts';
import { BadRequest, handleApi, type ApiCall, type ApiReply, type QueryBag } from './restapi.ts';
import { executeDocument, parseGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { viewerLogin } from './scenario.ts';
import { createHttpServer, type Dispatch, type Reply, type RawRequest } from './httpserver.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
/** The path every REST route hangs under. */
const API_PREFIX = '/api/v3';
/** The env's bearer token, as the grader hands it to the entry. */
const SHOP_TOKEN = (process.env.MAKEFUN_SHOP_TOKEN ?? '').trim();

/**
 * The login a bearer token owns. The trial's own token is the scenario's viewer; any other token is
 * a different account, so one login never sees another's repositories. With no token configured in
 * the environment, every presented token is taken to be the trial's.
 */
function loginForToken(token: string): string {
  if (SHOP_TOKEN === '') return viewerLogin();
  if (token === SHOP_TOKEN) return viewerLogin();
  return `ghost-${createHash('sha1').update(`ghforge-token/${token}`).digest('hex').slice(0, 12)}`;
}

/** The last Authorization value seen and the login it maps to: a workload repeats one token. */
let lastAuthorization: string | null = null;
let lastAuthorizationLogin: string | null = null;

/** The login a request's Authorization header names, or null when it carries none. */
function loginOf(raw: string | null): string | null {
  if (raw === null) return null;
  if (raw === lastAuthorization) return lastAuthorizationLogin;
  const match = /^\s*(token|bearer)\s+(.*)$/i.exec(raw);
  let login: string | null;
  if (match !== null) {
    login = loginForToken(match[2] ?? '');
  } else {
    const bare = raw.trim();
    login = bare === '' ? null : loginForToken(bare);
  }
  lastAuthorization = raw;
  lastAuthorizationLogin = login;
  return login;
}

/** One query component, decoded the way URLSearchParams reads it. */
function decodeComponent(text: string): string {
  if (text.indexOf('%') < 0 && text.indexOf('+') < 0) return text;
  try {
    return decodeURIComponent(text.replace(/\+/g, ' '));
  } catch {
    return text;
  }
}

/** The query string, decoded on demand: most requests carry none and never pay for one. */
class RawQuery implements QueryBag {
  private readonly raw: string;

  constructor(raw: string) {
    this.raw = raw;
  }

  get(name: string): string | undefined {
    const raw = this.raw;
    if (raw === '') return undefined;
    let at = 0;
    while (at <= raw.length) {
      let end = raw.indexOf('&', at);
      if (end < 0) end = raw.length;
      const pair = raw.slice(at, end);
      const equals = pair.indexOf('=');
      const key = equals < 0 ? pair : pair.slice(0, equals);
      if (decodeComponent(key) === name) return equals < 0 ? '' : decodeComponent(pair.slice(equals + 1));
      at = end + 1;
    }
    return undefined;
  }
}

function jsonReply(status: number, body: unknown, extraHeaders?: Record<string, string> | null): Reply {
  const buffer = Buffer.from(body === undefined ? '' : `${JSON.stringify(body)}\n`, 'utf8');
  return { status, contentType: JSON_TYPE, body: buffer, extraHeaders: extraHeaders ?? null };
}

function textReply(status: number, contentType: string, body: Buffer): Reply {
  return { status, contentType, body, cacheControl: true };
}

function sendWire(reply: WireResponse): Reply {
  return textReply(reply.status, reply.contentType, reply.body);
}

function sendApi(reply: ApiReply): Reply {
  if (reply.status === 204) return { status: 204, contentType: null, body: Buffer.alloc(0) };
  // A handler that already holds the exact bytes (a repeated read) skips serialize-into-string.
  if (reply.bytes !== undefined) return { status: reply.status, contentType: JSON_TYPE, body: reply.bytes, extraHeaders: reply.headers ?? null };
  return jsonReply(reply.status, reply.json, reply.headers ?? null);
}

/** A repository path split into its parts, or null when the path is not a `.git` route. */
const gitPaths = new Map<string, { owner: string; name: string; rest: string } | null>();

function parseGitPath(pathname: string): { owner: string; name: string; rest: string } | null {
  const cached = gitPaths.get(pathname);
  if (cached !== undefined) return cached;
  const match = /^\/([^/]+)\/([^/]+?)\.git(\/.*)?$/.exec(pathname);
  const route = match === null ? null : { owner: match[1] as string, name: match[2] as string, rest: match[3] ?? '' };
  if (gitPaths.size >= 2048) gitPaths.clear();
  gitPaths.set(pathname, route);
  return route;
}

/** The decoded path segments of a REST path (`/api/v3` already removed), empty segments dropped. */
function segmentsOf(rest: string): string[] {
  const out: string[] = [];
  let at = 0;
  while (at <= rest.length) {
    let end = rest.indexOf('/', at);
    if (end < 0) end = rest.length;
    if (end > at) {
      const segment = rest.slice(at, end);
      if (segment.indexOf('%') < 0) out.push(segment);
      else {
        try {
          out.push(decodeURIComponent(segment));
        } catch {
          out.push(segment);
        }
      }
    }
    at = end + 1;
  }
  return out;
}

/** The decoded REST path segments for a path, memoized: a workload repeats one path 2,000 times. */
const segmentsByPath = new Map<string, string[]>();

function segmentsFor(pathname: string): string[] {
  const cached = segmentsByPath.get(pathname);
  if (cached !== undefined) return cached;
  const built = segmentsOf(pathname.slice(API_PREFIX.length));
  if (segmentsByPath.size >= 2048) segmentsByPath.clear();
  segmentsByPath.set(pathname, built);
  return built;
}

/** The query bag for a query string, memoized the same way; `RawQuery` holds no state. */
let lastQueryRaw: string | null = null;
let lastQueryBag: RawQuery | null = null;

function queryFor(raw: string): RawQuery {
  if (raw === lastQueryRaw) return lastQueryBag as RawQuery;
  const bag = new RawQuery(raw);
  lastQueryRaw = raw;
  lastQueryBag = bag;
  return bag;
}

/** Build the dispatcher over a fresh Forge: one function from a parsed request to the reply. */
export function createDispatch(): Dispatch {
  const forge = new Forge();

  const dispatch = (req: RawRequest): Reply => {
    const pathname = req.path;
    const method = req.method;

    try {
      // The env's boot check: 200 {"ok":true}.
      if (pathname === '/healthz' && method === 'GET') return jsonReply(200, { ok: true });

      // GitHub's GraphQL v4 API: one document or an array of them in, {data, errors} out.
      if (pathname === '/api/graphql') {
        if (method !== 'POST') return jsonReply(404, { message: 'Not Found' });
        const login = loginOf(req.authorization);
        if (login === null) return jsonReply(401, { message: 'Requires authentication' });
        let parsed: unknown;
        try {
          parsed = JSON.parse(req.body.toString('utf8'));
        } catch {
          return jsonReply(400, { message: 'Problems parsing JSON' });
        }
        const origin = `http://${req.host ?? '127.0.0.1'}`;
        const operations = Array.isArray(parsed) ? parsed : [parsed];
        const answers = operations.map((operation) => {
          const entry = operation as { query?: unknown; variables?: unknown; operationName?: unknown };
          const document = typeof entry.query === 'string' ? entry.query : '';
          const variables = (entry.variables !== null && typeof entry.variables === 'object' ? entry.variables : {}) as Record<string, unknown>;
          try {
            return executeDocument(parseGraphQL(document), forge, login, origin, variables);
          } catch (err) {
            return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
          }
        });
        return jsonReply(200, Array.isArray(parsed) ? answers : (answers[0] ?? { data: null }));
      }

      // The Model Context Protocol endpoint: JSON-RPC over POST, one request per body.
      if (pathname === '/mcp' || pathname === '/mcp/') {
        if (method === 'DELETE') {
          // No session to forget: the modern transport is stateless.
          return { status: 204, contentType: null, body: Buffer.alloc(0) };
        }
        if (method !== 'POST') return jsonReply(405, { message: 'Method Not Allowed' });
        const login = loginOf(req.authorization);
        const answer = handleMcp(req.body.toString('utf8'), forge, login);
        if (answer.body === null) return { status: answer.status, contentType: null, body: Buffer.alloc(0) };
        return jsonReply(answer.status, answer.body);
      }

      // The GitHub API surface.
      if (pathname === API_PREFIX || pathname.startsWith(`${API_PREFIX}/`)) {
        const token = loginOf(req.authorization);
        const call: ApiCall = {
          method,
          segments: segmentsFor(pathname),
          query: queryFor(req.query),
          body: req.body,
          login: token,
          origin: `http://${req.host ?? '127.0.0.1'}`,
        };
        return sendApi(handleApi(call, forge));
      }

      // git smart HTTP.
      const route = parseGitPath(pathname);
      if (route !== null) {
        const repo: Repo | undefined = forge.get(route.owner, route.name);
        if (route.rest === '/info/refs' && method === 'GET') {
          const protocol = req.gitProtocol;
          return sendWire(handleInfoRefs(repo, queryFor(req.query).get('service') ?? null, protocol));
        }
        if ((route.rest === '/git-upload-pack' || route.rest === '/git-receive-pack') && method === 'POST') {
          if (repo === undefined) return jsonReply(404, { message: 'Not Found' });
          const reply = route.rest === '/git-upload-pack' ? handleUploadPack(repo, req.body) : handleReceivePack(repo, req.body);
          return sendWire(reply);
        }
        return jsonReply(404, { message: 'Not Found' });
      }

      // Everything else, every method: GitHub's 404 body.
      return jsonReply(404, { message: 'Not Found' });
    } catch (err) {
      if (err instanceof WireError || err instanceof BadRequest) {
        return jsonReply(400, { message: err.message || 'Bad Request' });
      }
      throw err;
    }
  };

  return dispatch;
}

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  return createHttpServer(createDispatch());
}