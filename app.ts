// app.ts — the entry's HTTP surface: git smart HTTP under `/:owner/:repo.git`, the `/api/v3` REST
// subset, and a JSON 404 (never HTML) for anything else. Every reply carries an X-GitHub-Request-Id.
//
// The server holds all of its state in one Forge (objects, refs, issues) and binds nothing until the
// caller listens: main.ts owns the ephemeral port and the entry <-> env lifecycle.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { handleReceivePack, handleUploadPack, handleInfoRefs, WireError, type WireResponse } from './gitwire.ts';
import { Forge, type Repo } from './gitstore.ts';
import { BadRequest, handleApi, type ApiCall, type ApiReply } from './restapi.ts';
import { viewerLogin } from './scenario.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
/** The path every REST route hangs under. */
const API_PREFIX = '/api/v3';
/** The env's bearer token, as the grader hands it to the entry. */
const SHOP_TOKEN = (process.env.MAKEFUN_SHOP_TOKEN ?? '').trim();

/** A deterministic, non-empty request id. */
let requestCounter = 0;
function nextRequestId(): string {
  requestCounter += 1;
  return createHash('sha1').update(`ghforge/${requestCounter}`).digest('hex').slice(0, 20);
}

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

/** The bearer token a request carries, or null when it carries no Authorization header at all. */
function tokenOf(req: IncomingMessage): string | null {
  const raw = req.headers.authorization;
  if (typeof raw !== 'string') return null;
  const match = /^\s*(token|bearer)\s+(.*)$/i.exec(raw);
  if (match !== null) return match[2] ?? '';
  const bare = raw.trim();
  return bare === '' ? null : bare;
}

function sendText(res: ServerResponse, status: number, contentType: string, body: Buffer, id: string): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': id,
    'cache-control': 'no-cache, max-age=0, must-revalidate',
  });
  res.end(body);
}

function sendJson(res: ServerResponse, status: number, body: unknown, id: string, headers: Record<string, string> = {}): void {
  const buffer = Buffer.from(body === undefined ? '' : `${JSON.stringify(body)}\n`, 'utf8');
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': buffer.length,
    'x-github-request-id': id,
    ...headers,
  });
  res.end(buffer);
}

/** The whole body of a request, as bytes. */
async function readBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as string));
  return Buffer.concat(chunks);
}

/** The query string as a flat map (last value wins). */
function parseQuery(url: URL): Map<string, string> {
  const out = new Map<string, string>();
  for (const [key, value] of url.searchParams) out.set(key, value);
  return out;
}

function sendWire(res: ServerResponse, reply: WireResponse, id: string): void {
  sendText(res, reply.status, reply.contentType, reply.body, id);
}

function sendApi(res: ServerResponse, reply: ApiReply, id: string): void {
  if (reply.status === 204) {
    res.writeHead(204, { 'x-github-request-id': id });
    res.end();
    return;
  }
  sendJson(res, reply.status, reply.json, id, reply.headers ?? {});
}

/** A repository path split into its parts, or null when the path is not a `.git` route. */
function parseGitPath(pathname: string): { owner: string; name: string; rest: string } | null {
  const match = /^\/([^/]+)\/([^/]+?)\.git(\/.*)?$/.exec(pathname);
  if (match === null) return null;
  return { owner: match[1] as string, name: match[2] as string, rest: match[3] ?? '' };
}

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  const forge = new Forge();

  const handler = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const id = nextRequestId();
    let url: URL;
    try {
      url = new URL(req.url ?? '/', 'http://entry');
    } catch {
      sendJson(res, 400, { message: 'Bad Request' }, id);
      return;
    }
    const pathname = url.pathname;
    const method = req.method ?? 'GET';

    try {
      // The env's boot check: 200 {"ok":true}.
      if (pathname === '/healthz' && method === 'GET') {
        sendJson(res, 200, { ok: true }, id);
        return;
      }

      // The GitHub API surface.
      if (pathname === API_PREFIX || pathname.startsWith(`${API_PREFIX}/`)) {
        const rest = pathname.slice(API_PREFIX.length);
        const segments = rest
          .split('/')
          .filter((segment) => segment !== '')
          .map((segment) => {
            try {
              return decodeURIComponent(segment);
            } catch {
              return segment;
            }
          });
        const token = tokenOf(req);
        const call: ApiCall = {
          method,
          segments,
          query: parseQuery(url),
          body: await readBody(req),
          login: token === null ? null : loginForToken(token),
          origin: `http://${req.headers.host ?? '127.0.0.1'}`,
        };
        sendApi(res, handleApi(call, forge), id);
        return;
      }

      // git smart HTTP.
      const route = parseGitPath(pathname);
      if (route !== null) {
        const repo: Repo | undefined = forge.get(route.owner, route.name);
        if (route.rest === '/info/refs' && method === 'GET') {
          const protocolHeader = req.headers['git-protocol'];
          const protocol = Array.isArray(protocolHeader) ? protocolHeader.join(' ') : protocolHeader;
          sendWire(res, handleInfoRefs(repo, url.searchParams.get('service'), protocol), id);
          return;
        }
        if ((route.rest === '/git-upload-pack' || route.rest === '/git-receive-pack') && method === 'POST') {
          if (repo === undefined) {
            sendJson(res, 404, { message: 'Not Found' }, id);
            return;
          }
          const body = await readBody(req);
          const reply = route.rest === '/git-upload-pack' ? handleUploadPack(repo, body) : handleReceivePack(repo, body);
          sendWire(res, reply, id);
          return;
        }
        sendJson(res, 404, { message: 'Not Found' }, id);
        return;
      }

      // Everything else, every method: GitHub's 404 body.
      sendJson(res, 404, { message: 'Not Found' }, id);
    } catch (err) {
      if (err instanceof WireError || err instanceof BadRequest) {
        sendJson(res, 400, { message: err.message || 'Bad Request' }, id);
        return;
      }
      process.stderr.write(`[entry] request failed: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
      if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' }, id);
      else res.end();
    }
  };

  return createServer((req, res) => {
    void handler(req, res);
  });
}
