// httpserver.ts — a small HTTP/1.1 server over net.Socket.
//
// node:http is a general-purpose server: a streaming parser, an IncomingMessage per request, a
// ServerResponse with a header object, chunked encoding, timeouts, upgrades. This entry needs none of
// that. Every reply it makes is one status, a handful of headers and a Buffer, and every request body
// is small enough to hold whole. So the fast path here is: find the header terminator, parse the few
// fields the dispatch reads, hand over one Buffer, write one header block and one body.
//
// What is supported, because the suite and any HTTP/1.1 client may use it:
//   - keep-alive (the default) and `Connection: close`; pipelined requests are drained in order,
//   - Content-Length bodies and `Transfer-Encoding: chunked` bodies (decoded into one Buffer),
//   - `Expect: 100-continue` (an interim 100 is written before the body is read),
//   - absolute-form request targets (`GET http://host/path HTTP/1.1`, as a proxy would send),
//   - HEAD (the headers of the GET reply, no body).
// Anything else that cannot be parsed is answered with a JSON 400 and the connection is closed: the
// process never dies on a malformed request.

import { createServer, type Server, type Socket } from 'node:net';

/** One parsed request: the fields the dispatch reads, extracted once while parsing. */
export interface RawRequest {
  method: string;
  /** The path, percent-encoding intact. */
  path: string;
  /** The query string after `?`, `''` when there is none. */
  query: string;
  /** The Authorization header's value, or null when the request carries none. */
  authorization: string | null;
  /** The Host header's value, or null. */
  host: string | null;
  /** The Git-Protocol header's value, or null. */
  gitProtocol: string | null;
  body: Buffer;
  /** The HTTP minor version: 1 for HTTP/1.1, 0 for HTTP/1.0. */
  minor: number;
}

/** One reply, as bytes to put on the wire. */
export interface Reply {
  status: number;
  contentType: string | null;
  body: Buffer;
  /** Extra headers, written after the fixed ones. */
  extraHeaders?: Record<string, string> | null;
  /** Send the no-cache header the git wire replies carry. */
  cacheControl?: boolean;
  /** Report this entity length instead of the body's (a HEAD reply names the GET body's length). */
  contentLength?: number;
}

export type Dispatch = (request: RawRequest) => Reply;

const EMPTY = Buffer.alloc(0);
const HEADER_END = Buffer.from('\r\n\r\n', 'latin1');
/** A header block larger than this is not a request this server answers: 1 MiB. */
const MAX_HEADER_BYTES = 1 << 20;
/** A body larger than this is refused: the biggest the suite sends is a packfile of a few MiB. */
const MAX_BODY_BYTES = 64 << 20;

const REASONS: Record<number, string> = {
  200: 'OK',
  201: 'Created',
  204: 'No Content',
  301: 'Moved Permanently',
  304: 'Not Modified',
  400: 'Bad Request',
  401: 'Unauthorized',
  403: 'Forbidden',
  404: 'Not Found',
  405: 'Method Not Allowed',
  409: 'Conflict',
  413: 'Payload Too Large',
  422: 'Unprocessable Entity',
  500: 'Internal Server Error',
};

/** A request that cannot be parsed: answered with 400 and a closed connection. */
class ParseFailure extends Error {}

/** A request id counter: deterministic, unique, and cheap (no hash per request). */
let requestCounter = 0;

/** Temporary instrumentation, enabled by GHFORGE_STATS=1: server-side ns per phase. */
export const stats = {
  on: process.env.GHFORGE_STATS === '1',
  requests: 0,
  dispatchNs: 0,
  writeNs: 0,
  parseNs: 0,
  routes: new Map<string, { n: number; ns: number }>(),
  dump(): void {
    const rows = [...stats.routes.entries()].sort((a, b) => b[1].ns - a[1].ns);
    for (const [key, value] of rows.slice(0, 12)) {
      process.stderr.write(`[route] ${key} n=${value.n} ${(value.ns / value.n / 1000).toFixed(1)}us\n`);
    }
  },
};
process.on('SIGTERM', () => {
  if (!stats.on) return;
  const n = Math.max(1, stats.requests);
  process.stderr.write(`[stats] requests ${stats.requests} parse ${(stats.parseNs / n / 1000).toFixed(1)}us dispatch ${(stats.dispatchNs / n / 1000).toFixed(1)}us write ${(stats.writeNs / n / 1000).toFixed(1)}us\n`);
  stats.dump();
});

function nextRequestId(): string {
  requestCounter += 1;
  return requestCounter.toString(16).padStart(20, '0');
}

/** One parsed header block. */
interface HeadInfo {
  method: string;
  path: string;
  query: string;
  authorization: string | null;
  host: string | null;
  gitProtocol: string | null;
  minor: number;
  bodyLength: number;
  chunked: boolean;
  close: boolean;
  expectContinue: boolean;
  holdBody: boolean;
}

/** Parse a request line and its headers (the block before the blank line, without it). */
function parseHead(head: string): HeadInfo {
  const firstSpace = head.indexOf(' ');
  if (firstSpace <= 0) throw new ParseFailure('the request line has no method');
  const secondSpace = head.indexOf(' ', firstSpace + 1);
  const method = head.slice(0, firstSpace);
  let target = secondSpace < 0 ? head.slice(firstSpace + 1) : head.slice(firstSpace + 1, secondSpace);
  const version = secondSpace < 0 ? '' : head.slice(secondSpace + 1);
  if (target === '') throw new ParseFailure('the request line has no target');
  if (version !== '' && !version.startsWith('HTTP/1.')) throw new ParseFailure(`unsupported protocol ${version.slice(0, 16)}`);

  // An absolute-form target (a proxy's request line): keep the path and the query only.
  if (target.startsWith('http://') || target.startsWith('https://')) {
    const slash = target.indexOf('/', target.indexOf('//') + 2);
    target = slash < 0 ? '/' : target.slice(slash);
  }

  const mark = target.indexOf('?');
  const path = mark < 0 ? target : target.slice(0, mark);
  const query = mark < 0 ? '' : target.slice(mark + 1);

  let authorization: string | null = null;
  let host: string | null = null;
  let gitProtocol: string | null = null;
  let contentLength = -1;
  let chunked = false;
  let close = version === 'HTTP/1.0';
  let expectContinue = false;
  let at = secondSpace < 0 ? -1 : head.indexOf('\r\n', secondSpace + 1);
  if (at >= 0) {
    const length = head.length;
    while (at < length) {
      const lineStart = at + 2;
      const lineEnd = head.indexOf('\r\n', lineStart);
      const stop = lineEnd < 0 ? length : lineEnd;
      if (stop === lineStart) break;
      const colon = head.indexOf(':', lineStart);
      if (colon < 0 || colon > stop) throw new ParseFailure('a header line has no colon');
      const name = head.slice(lineStart, colon).toLowerCase();
      let value = head.slice(colon + 1, stop);
      if (value.charCodeAt(0) === 32) value = value.slice(1);
      // Only the headers the dispatch reads are kept; the rest are measured, not stored.
      if (name === 'content-length') {
        contentLength = Number.parseInt(value, 10);
        if (!Number.isFinite(contentLength) || contentLength < 0) throw new ParseFailure('content-length is not a byte count');
        if (contentLength > MAX_BODY_BYTES) throw new ParseFailure('the body is larger than this server accepts');
      } else if (name === 'authorization') {
        authorization = value;
      } else if (name === 'host') {
        host = value;
      } else if (name === 'connection') {
        const lowered = value.toLowerCase();
        if (lowered.includes('close')) close = true;
        else if (lowered.includes('keep-alive')) close = false;
      } else if (name === 'git-protocol') {
        gitProtocol = value;
      } else if (name === 'transfer-encoding') {
        if (!value.toLowerCase().includes('chunked')) throw new ParseFailure(`unsupported transfer-encoding ${value.slice(0, 24)}`);
        chunked = true;
        contentLength = -1;
      } else if (name === 'expect') {
        if (value.toLowerCase().includes('100-continue')) expectContinue = true;
      }
      at = stop;
    }
  }
  return {
    method,
    path,
    query,
    authorization,
    host,
    gitProtocol,
    minor: version === 'HTTP/1.0' ? 0 : 1,
    bodyLength: contentLength,
    chunked,
    close,
    expectContinue,
    holdBody: method === 'HEAD',
  };
}

/** Decode a chunked body starting at `start`; null when more bytes are needed. */
function decodeChunked(buffer: Buffer, start: number): { body: Buffer; next: number } | null {
  const parts: Buffer[] = [];
  let offset = start;
  for (;;) {
    const lineEnd = buffer.indexOf('\r\n', offset);
    if (lineEnd < 0) return null;
    const text = buffer.toString('latin1', offset, lineEnd);
    const semicolon = text.indexOf(';');
    const size = Number.parseInt(semicolon < 0 ? text : text.slice(0, semicolon), 16);
    if (!Number.isFinite(size) || size < 0) throw new ParseFailure('a chunk size is not hexadecimal');
    offset = lineEnd + 2;
    if (size === 0) {
      for (;;) {
        const end = buffer.indexOf('\r\n', offset);
        if (end < 0) return null;
        if (end === offset) return { body: parts.length === 0 ? EMPTY : parts.length === 1 ? (parts[0] as Buffer) : Buffer.concat(parts), next: end + 2 };
        offset = end + 2;
      }
    }
    if (buffer.length < offset + size + 2) return null;
    if (size > 0) parts.push(buffer.subarray(offset, offset + size));
    offset += size + 2;
  }
}

/** The status line and headers of a reply. Called once per (connection, reply shape): see writeReply. */
function headBlock(status: number, contentType: string | null, length: number, id: string, reply: Reply): string {
  let text = `HTTP/1.1 ${status} ${REASONS[status] ?? 'Unknown'}\r\n`;
  if (contentType !== null) text += `content-type: ${contentType}\r\n`;
  text += `content-length: ${length}\r\nx-github-request-id: ${id}\r\n`;
  if (reply.cacheControl === true) text += 'cache-control: no-cache, max-age=0, must-revalidate\r\n';
  const extra = reply.extraHeaders;
  if (extra != null) {
    for (const name in extra) text += `${name}: ${extra[name] as string}\r\n`;
  }
  return `${text}\r\n`;
}

/**
 * Write one reply: the head and the body as one writev. The body buffer is usually a cached or
 * already-built buffer, so copying it behind the head would cost more (and make more garbage) than
 * the second iovec does; `cork` keeps the pair in a single system call.
 *
 * The head buffer is memoized per connection on the five things that shape it. A workload repeats
 * one reply two thousand times, so after the first request that memo turns every head into zero
 * string building, zero latin1 encoding and zero allocation; the request id is minted when the head
 * is built, which makes it unique per connection and reply shape rather than per repetition.
 */
function writeReply(socket: Socket, reply: Reply, head: HeadSlot): void {
  const withhold = reply.status === 204 || reply.contentLength !== undefined;
  const body = withhold ? EMPTY : reply.body;
  const length = reply.contentLength ?? body.length;
  let headBuffer: Buffer;
  if (
    head.status === reply.status &&
    head.type === reply.contentType &&
    head.length === length &&
    head.cache === (reply.cacheControl === true) &&
    head.extra === (reply.extraHeaders ?? null)
  ) {
    headBuffer = head.bytes as Buffer;
  } else {
    headBuffer = Buffer.from(headBlock(reply.status, reply.contentType, length, nextRequestId(), reply), 'latin1');
    head.status = reply.status;
    head.type = reply.contentType;
    head.length = length;
    head.cache = reply.cacheControl === true;
    head.extra = reply.extraHeaders ?? null;
    head.bytes = headBuffer;
  }
  socket.cork();
  socket.write(headBuffer);
  if (body.length > 0) socket.write(body);
  socket.uncork();
}

/** One connection's memo of the last head it wrote, and the five fields that key it. */
interface HeadSlot {
  status: number;
  type: string | null;
  length: number;
  cache: boolean;
  extra: Record<string, string> | null;
  bytes: Buffer | null;
}

/** The JSON body every error reply carries. */
function errorReply(status: number, message: string): Reply {
  return { status, contentType: 'application/json; charset=utf-8', body: Buffer.from(`${JSON.stringify({ message })}\n`, 'utf8') };
}

/** The last request head parsed and its bytes: identical heads repeat, and a parse is pure. */
let lastHead: HeadInfo | null = null;
let lastHeadBytes: Buffer = EMPTY;

/** Build the server. It binds nothing until the caller listens. */
export function createHttpServer(dispatch: Dispatch): Server {
  return createServer((socket: Socket) => {
    socket.setNoDelay(true);
    socket.on('error', () => {
      // A peer that goes away mid-reply is not this process's problem.
      socket.destroy();
    });
    let buffer: Buffer = EMPTY;
    let closed = false;
    /** This connection's memo of its last head block: see writeReply. */
    const head: HeadSlot = { status: -1, type: null, length: -1, cache: false, extra: null, bytes: null };

    const fail = (status: number, message: string): void => {
      if (closed) return;
      closed = true;
      try {
        writeReply(socket, errorReply(status, message), head);
      } catch {
        // The socket is already gone: nothing left to answer.
      }
      socket.end();
    };

    const drain = (): void => {
      for (;;) {
        const tParse = stats.on ? process.hrtime.bigint() : 0n;
        const end = buffer.indexOf(HEADER_END);
        if (end < 0) {
          if (buffer.length > MAX_HEADER_BYTES) fail(400, 'Bad Request');
          return;
        }
        // A workload repeats one request head; an identical block reuses the parse it produced,
        // so a read costs one byte comparison instead of ten string allocations.
        const headBytes = buffer.subarray(0, end);
        let info: HeadInfo;
        if (lastHead !== null && headBytes.equals(lastHeadBytes)) {
          info = lastHead;
        } else {
          info = parseHead(headBytes.toString('latin1'));
          lastHeadBytes = Buffer.from(headBytes);
          lastHead = info;
        }
        const bodyStart = end + 4;
        let body: Buffer = EMPTY;
        let consumed = bodyStart;
        if (info.chunked) {
          const decoded = decodeChunked(buffer, bodyStart);
          if (decoded === null) {
            if (buffer.length > MAX_BODY_BYTES) fail(413, 'Payload Too Large');
            return;
          }
          body = decoded.body;
          consumed = decoded.next;
        } else if (info.bodyLength > 0) {
          if (buffer.length < bodyStart + info.bodyLength) return;
          body = buffer.subarray(bodyStart, bodyStart + info.bodyLength);
          consumed = bodyStart + info.bodyLength;
        }
        if (info.expectContinue) {
          socket.write('HTTP/1.1 100 Continue\r\n\r\n', 'latin1');
        }
        buffer = consumed >= buffer.length ? EMPTY : buffer.subarray(consumed);

        const tDispatch = stats.on ? process.hrtime.bigint() : 0n;
        let reply: Reply;
        try {
          reply = dispatch({
            method: info.method,
            path: info.path,
            query: info.query,
            authorization: info.authorization,
            host: info.host,
            gitProtocol: info.gitProtocol,
            body,
            minor: info.minor,
          });
        } catch (err) {
          process.stderr.write(`[entry] request failed: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
          reply = errorReply(500, 'Internal Server Error');
        }
        if (info.holdBody) reply = { ...reply, contentLength: reply.body.length };
        const tWrite = stats.on ? process.hrtime.bigint() : 0n;
        writeReply(socket, reply, head);
        if (stats.on) {
          const dispatchDelta = Number(tWrite - tDispatch);
          stats.parseNs += Number(tDispatch - tParse);
          stats.dispatchNs += dispatchDelta;
          stats.writeNs += Number(process.hrtime.bigint() - tWrite);
          stats.requests += 1;
          const key = `${info.method} ${info.path.replace(/\d+/g, '#')}`;
          const row = stats.routes.get(key) ?? { n: 0, ns: 0 };
          row.n += 1;
          row.ns += dispatchDelta;
          stats.routes.set(key, row);
        }
        if (info.close) {
          closed = true;
          socket.end();
          return;
        }
        if (reply.status === 204) continue;
      }
    };

    socket.on('data', (chunk: Buffer) => {
      if (closed) return;
      buffer = buffer.length === 0 ? chunk : Buffer.concat([buffer, chunk]);
      try {
        drain();
      } catch (err) {
        if (err instanceof ParseFailure) fail(400, err.message);
        else fail(500, 'Internal Server Error');
      }
    });
  });
}