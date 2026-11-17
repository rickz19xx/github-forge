// gitwire.ts — git's smart HTTP protocol as the conformance client drives it.
//
//   GET  /:owner/:repo.git/info/refs?service=...   the ref/capability advertisement (v2 for
//                                                  upload-pack when Git-Protocol: version=2, v0 for
//                                                  receive-pack and for clients that ask for v0),
//   POST /:owner/:repo.git/git-upload-pack         ls-refs and fetch (want/have/done, shallow,
//                                                  deepen), answered with a sideband packfile,
//   POST /:owner/:repo.git/git-receive-pack        a command list plus a pack, answered per ref
//                                                  with unpack/ok/ng in side-band-64k frames.
//
// Every byte here is git's; nothing shells out to git.

import {
  PKT_FLUSH,
  ZERO_OID,
  collectReachable,
  hashObject,
  isOid,
  parseCommit,
  parseTag,
  parseTree,
  pktLine,
  readPacketAt,
  readPack,
  writePack,
  type GitObject,
  type ObjectLookup,
  type Packet,
} from './gitcore.ts';
import { DEFAULT_BRANCH, Forge, type Repo } from './gitstore.ts';

const AGENT = 'ghforge/1.0';
const UPLOAD_SERVICE = 'git-upload-pack';
const RECEIVE_SERVICE = 'git-receive-pack';
const JSON_TYPE = 'application/json; charset=utf-8';

/** The largest sideband payload we put in one pkt-line. */
const SIDEBAND_CHUNK = 65_000;

export interface WireResponse {
  readonly status: number;
  readonly contentType: string;
  readonly body: Buffer;
}

/** A protocol-level error: the caller answers 400 and the process keeps running. */
export class WireError extends Error {}

function json(status: number, message: string): WireResponse {
  return { status, contentType: JSON_TYPE, body: Buffer.from(`${JSON.stringify({ message })}\n`, 'utf8') };
}

/** The smart-HTTP content type for one service and direction. */
function smartType(service: string, kind: 'advertisement' | 'result'): string {
  return `application/x-${service}-${kind}`;
}

function pktLines(lines: readonly string[]): Buffer[] {
  return lines.map((line) => pktLine(line));
}

// ---------------------------------------------------------------------------------------------
// refs
// ---------------------------------------------------------------------------------------------

/** Every loose ref name, in git's own (byte) order. */
function refNames(repo: Repo): string[] {
  return [...repo.refs.keys()].sort();
}

/** HEAD's symref target. */
function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** The object HEAD resolves to, or null for an unborn branch. */
function headOid(repo: Repo): string | null {
  return repo.refs.get(headTarget(repo)) ?? null;
}

/** The commit an object peels to when it is an annotated tag, else null. */
function peel(repo: Repo, oid: string): string | null {
  let current = oid;
  let followed = false;
  for (let hop = 0; hop < 16; hop += 1) {
    const object = repo.objects.get(current);
    if (!object || object.type !== 'tag') break;
    current = parseTag(object.data).object;
    followed = true;
  }
  return followed && repo.objects.has(current) ? current : null;
}

// ---------------------------------------------------------------------------------------------
// info/refs
// ---------------------------------------------------------------------------------------------

const UPLOAD_CAPS_V0 = [
  'multi_ack_detailed',
  'side-band-64k',
  'thin-pack',
  'ofs-delta',
  'shallow',
  'no-progress',
  `symref=HEAD:refs/heads/${DEFAULT_BRANCH}`,
  'object-format=sha1',
  `agent=${AGENT}`,
].join(' ');

const RECEIVE_CAPS_V0 = [
  'report-status',
  'report-status-v2',
  'delete-refs',
  'side-band-64k',
  'quiet',
  'atomic',
  'ofs-delta',
  'object-format=sha1',
  `agent=${AGENT}`,
  `symref=HEAD:refs/heads/${DEFAULT_BRANCH}`,
].join(' ');

/** The v2 capability advertisement (upload-pack). */
function advertisementV2(): Buffer {
  const lines = [
    `# service=${UPLOAD_SERVICE}\n`,
  ];
  const parts: Buffer[] = [...pktLines(lines), PKT_FLUSH];
  parts.push(...pktLines([
    'version 2\n',
    `agent=${AGENT}\n`,
    'ls-refs=unborn\n',
    'fetch=shallow wait-for-done\n',
    'object-format=sha1\n',
  ]));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** The v0 ref advertisement, refs and peeled tags included, capability line on the first entry. */
function advertisementV0(repo: Repo, service: string): Buffer {
  const caps = service === RECEIVE_SERVICE ? RECEIVE_CAPS_V0 : UPLOAD_CAPS_V0;
  const parts: Buffer[] = [...pktLines([`# service=${service}\n`]), PKT_FLUSH];
  const lines: string[] = [];
  let capsAttached = false;
  const push = (line: string): void => {
    if (!capsAttached) {
      capsAttached = true;
      lines.push(`${line}\u0000${caps}\n`);
    } else {
      lines.push(`${line}\n`);
    }
  };
  const names = refNames(repo);
  const head = headOid(repo);
  if (names.length === 0 && head === null) {
    push(`${ZERO_OID} capabilities^{}`);
  } else {
    if (head !== null) push(`${head} HEAD`);
    for (const name of names) {
      const oid = repo.refs.get(name) as string;
      push(`${oid} ${name}`);
      const peeled = peel(repo, oid);
      if (peeled !== null) lines.push(`${peeled} ${name}^{}\n`);
    }
  }
  parts.push(...lines.map((line) => pktLine(line)));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** GET /:owner/:repo.git/info/refs. */
export function handleInfoRefs(repo: Repo | undefined, service: string | null, gitProtocol: string | undefined): WireResponse {
  if (repo === undefined) return json(404, 'Not Found');
  if (service !== UPLOAD_SERVICE && service !== RECEIVE_SERVICE) {
    return json(403, 'Smart HTTP is only available for git-upload-pack and git-receive-pack');
  }
  if (service === UPLOAD_SERVICE && /version=2/.test(gitProtocol ?? '')) {
    return { status: 200, contentType: smartType(service, 'advertisement'), body: advertisementV2() };
  }
  return { status: 200, contentType: smartType(service, 'advertisement'), body: advertisementV0(repo, service) };
}

// ---------------------------------------------------------------------------------------------
// parsing a command request
// ---------------------------------------------------------------------------------------------

interface RequestLine {
  command: string | null;
  capabilityArgs: string[];
  args: string[];
}

/** Split a v2 request into its command, its capability args and its command arguments. */
function parseV2Request(body: Buffer): RequestLine {
  const out: RequestLine = { command: null, capabilityArgs: [], args: [] };
  let offset = 0;
  let inCapabilities = true;
  for (;;) {
    let packet: Packet | null;
    try {
      packet = readPacketAt(body, offset);
    } catch (err) {
      throw new WireError(err instanceof Error ? err.message : 'malformed request');
    }
    if (packet === null) break;
    offset = packet.next;
    if (packet.kind === 'flush') break;
    if (packet.kind === 'delim') {
      inCapabilities = false;
      continue;
    }
    if (packet.kind !== 'data') throw new WireError('unexpected control packet in a request');
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    const equals = line.indexOf('=');
    const key = equals < 0 ? line : line.slice(0, equals);
    const value = equals < 0 ? '' : line.slice(equals + 1);
    if (key === 'command') {
      out.command = value;
    } else if (inCapabilities && (key === 'agent' || key === 'object-format' || key === 'session-id')) {
      out.capabilityArgs.push(line);
    } else {
      out.args.push(line);
    }
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// ls-refs
// ---------------------------------------------------------------------------------------------

function handleLsRefs(repo: Repo, args: readonly string[]): Buffer {
  let wantPeel = false;
  let wantSymrefs = false;
  let wantUnborn = false;
  const prefixes: string[] = [];
  for (const arg of args) {
    if (arg === 'peel') wantPeel = true;
    else if (arg === 'symrefs') wantSymrefs = true;
    else if (arg === 'unborn') wantUnborn = true;
    else if (arg.startsWith('ref-prefix ')) prefixes.push(arg.slice('ref-prefix '.length));
  }
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const lines: string[] = [];
  const target = headTarget(repo);
  if (matches('HEAD')) {
    const oid = headOid(repo);
    if (oid !== null) {
      lines.push(`${oid} HEAD${wantSymrefs ? ` symref-target:${target}` : ''}`);
    } else if (wantUnborn) {
      lines.push(`unborn HEAD${wantSymrefs ? ` symref-target:${target}` : ''}`);
    }
  }
  for (const name of refNames(repo)) {
    if (!matches(name)) continue;
    const oid = repo.refs.get(name) as string;
    let line = `${oid} ${name}`;
    if (wantPeel) {
      const peeled = peel(repo, oid);
      if (peeled !== null) line += ` peeled:${peeled}`;
    }
    lines.push(line);
  }
  return Buffer.concat([...lines.map((line) => pktLine(`${line}\n`)), PKT_FLUSH]);
}

// ---------------------------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------------------------

/** The commits `depth` generations from `wants` (level 0 is the wants themselves). */
function depthCommits(repo: Repo, wants: readonly string[], depth: number): Set<string> {
  const lookup = Forge.lookup(repo);
  const included = new Set<string>();
  let frontier: string[] = [];
  for (const want of wants) {
    const commit = commitOf(lookup, want);
    if (commit !== null) frontier.push(commit);
  }
  for (let level = 0; level < Math.max(1, depth); level += 1) {
    const next: string[] = [];
    for (const oid of frontier) {
      if (included.has(oid)) continue;
      included.add(oid);
      const object = lookup(oid);
      if (!object || object.type !== 'commit') continue;
      for (const parent of parseCommit(object.data).parents) if (!included.has(parent)) next.push(parent);
    }
    frontier = next;
    if (frontier.length === 0) break;
  }
  return included;
}

/** The commit `oid` names, following annotated tags. */
function commitOf(lookup: ObjectLookup, oid: string): string | null {
  let current = oid;
  for (let hop = 0; hop < 16; hop += 1) {
    const object = lookup(current);
    if (!object) return null;
    if (object.type === 'commit') return current;
    if (object.type !== 'tag') return null;
    current = parseTag(object.data).object;
  }
  return null;
}

/**
 * The objects the client already holds: the closure of `haves`, walking no further than the
 * shallow boundary the client named (a shallow commit's parents are not the client's).
 */
function heldObjects(repo: Repo, haves: readonly string[], boundary: ReadonlySet<string>): Set<string> {
  const lookup = Forge.lookup(repo);
  const seen = new Set<string>();
  const queue = [...haves];
  while (queue.length) {
    const oid = queue.pop() as string;
    if (seen.has(oid)) continue;
    const object = lookup(oid);
    if (!object) continue;
    seen.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      queue.push(commit.tree);
      if (!boundary.has(oid)) for (const parent of commit.parents) queue.push(parent);
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) if (entry.mode !== '160000') queue.push(entry.oid);
    }
  }
  return seen;
}

/** The objects of a set of commits: each commit, its tree and everything under it. */
function commitObjects(repo: Repo, commits: Iterable<string>): Set<string> {
  const lookup = Forge.lookup(repo);
  const trees: string[] = [];
  const out = new Set<string>();
  for (const oid of commits) {
    const object = lookup(oid);
    if (!object || object.type !== 'commit') continue;
    out.add(oid);
    trees.push(parseCommit(object.data).tree);
  }
  for (const oid of collectReachable(trees, lookup, true)) out.add(oid);
  return out;
}

function handleFetch(repo: Repo, args: readonly string[]): Buffer {
  const lookup = Forge.lookup(repo);
  const wants: string[] = [];
  const haves: string[] = [];
  const boundaries = new Set<string>();
  let depth: number | null = null;
  for (const arg of args) {
    if (arg.startsWith('want ')) {
      const oid = arg.slice(5).trim();
      if (isOid(oid)) wants.push(oid);
    } else if (arg.startsWith('have ')) {
      const oid = arg.slice(5).trim();
      if (isOid(oid)) haves.push(oid);
    } else if (arg.startsWith('shallow ')) {
      const oid = arg.slice(8).trim();
      if (isOid(oid)) boundaries.add(oid);
    } else if (arg.startsWith('deepen ')) {
      const parsed = Number.parseInt(arg.slice(7).trim(), 10);
      if (Number.isFinite(parsed) && parsed > 0) depth = parsed;
    }
  }

  const sections: Buffer[] = [];
  sections.push(...pktLines(['acknowledgments\n', 'NAK\n']), PKT_FLUSH);

  let included: Set<string>;
  if (depth !== null) {
    const commits = depthCommits(repo, wants, depth);
    const fresh = new Set<string>();
    for (const oid of commits) {
      const object = lookup(oid);
      if (!object || object.type !== 'commit') continue;
      if (parseCommit(object.data).parents.some((parent) => !commits.has(parent))) fresh.add(oid);
    }
    const shallowLines: string[] = [];
    for (const oid of [...boundaries].sort()) {
      if (commits.has(oid) && !fresh.has(oid)) shallowLines.push(`unshallow ${oid}\n`);
    }
    for (const oid of [...fresh].sort()) {
      if (!boundaries.has(oid)) shallowLines.push(`shallow ${oid}\n`);
    }
    if (shallowLines.length > 0) {
      sections.push(...pktLines(['shallow-info\n']), ...pktLines(shallowLines), PKT_FLUSH);
    }
    included = commitObjects(repo, commits);
  } else {
    included = collectReachable(wants, lookup, true);
  }

  const held = heldObjects(repo, haves, depth !== null ? boundaries : new Set());
  const send: GitObject[] = [];
  for (const oid of [...included].sort()) {
    if (held.has(oid)) continue;
    const object = lookup(oid);
    if (object) send.push(object);
  }

  sections.push(...pktLines(['packfile\n']));
  const pack = writePack(send);
  for (let offset = 0; offset < pack.length; offset += SIDEBAND_CHUNK) {
    const chunk = pack.subarray(offset, offset + SIDEBAND_CHUNK);
    sections.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  if (pack.length === 0) {
    // An empty pack is still a pack: band 1 carrying just the 20-byte trailer's header block.
    sections.push(pktLine(Buffer.concat([Buffer.from([1]), pack])));
  }
  sections.push(PKT_FLUSH);
  return Buffer.concat(sections);
}

/** POST /:owner/:repo.git/git-upload-pack. */
export function handleUploadPack(repo: Repo, body: Buffer): WireResponse {
  const request = parseV2Request(body);
  if (request.command === 'ls-refs') {
    return { status: 200, contentType: smartType(UPLOAD_SERVICE, 'result'), body: handleLsRefs(repo, request.args) };
  }
  if (request.command === 'fetch') {
    return { status: 200, contentType: smartType(UPLOAD_SERVICE, 'result'), body: handleFetch(repo, request.args) };
  }
  throw new WireError(`unknown upload-pack command ${JSON.stringify(request.command ?? '')}`);
}

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

interface RefResult {
  ref: string;
  ok: boolean;
  reason: string;
}

/** True when `ancestor` is `descendant` or one of its forebears. */
function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const queue = [descendant];
  while (queue.length) {
    const oid = queue.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (oid === ancestor) return true;
    const object = repo.objects.get(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queue.push(parent);
  }
  return false;
}

/** Split the command list from the pack that follows it. */
function parseCommandList(body: Buffer): { commands: PushCommand[]; capabilities: Set<string>; packStart: number } {
  const commands: PushCommand[] = [];
  const capabilities = new Set<string>();
  let offset = 0;
  let first = true;
  let packStart = body.length;
  for (;;) {
    let packet: Packet | null;
    try {
      packet = readPacketAt(body, offset);
    } catch (err) {
      throw new WireError(err instanceof Error ? err.message : 'malformed command list');
    }
    if (packet === null) throw new WireError('the push command list has no flush packet');
    if (packet.kind === 'flush') {
      packStart = packet.next;
      break;
    }
    if (packet.kind !== 'data') throw new WireError('unexpected control packet in a push');
    let line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (first) {
      first = false;
      const nul = line.indexOf('\u0000');
      if (nul >= 0) {
        for (const token of line.slice(nul + 1).split(' ')) {
          if (token) capabilities.add(token.split('=')[0] as string);
        }
        line = line.slice(0, nul);
      }
    }
    const parts = line.split(' ');
    if (parts.length !== 3 || !isOid(parts[0] as string) || !isOid(parts[1] as string)) {
      throw new WireError(`bad push command ${JSON.stringify(line.slice(0, 80))}`);
    }
    commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts[2] as string });
    offset = packet.next;
  }
  return { commands, capabilities, packStart };
}

/** Apply one command, mutating the repository only when it is accepted. */
function applyCommand(repo: Repo, command: PushCommand): RefResult {
  const current = repo.refs.get(command.ref);
  if (command.newOid === ZERO_OID) {
    if (current === undefined) return { ref: command.ref, ok: false, reason: 'no such ref' };
    if (current !== command.oldOid) return { ref: command.ref, ok: false, reason: 'stale info' };
    repo.refs.delete(command.ref);
    return { ref: command.ref, ok: true, reason: '' };
  }
  if (command.oldOid === ZERO_OID) {
    if (current !== undefined) return { ref: command.ref, ok: false, reason: 'already exists' };
    repo.refs.set(command.ref, command.newOid);
    return { ref: command.ref, ok: true, reason: '' };
  }
  if (current !== command.oldOid) return { ref: command.ref, ok: false, reason: 'stale info' };
  if (!isAncestor(repo, command.oldOid, command.newOid)) return { ref: command.ref, ok: false, reason: 'non-fast-forward' };
  repo.refs.set(command.ref, command.newOid);
  return { ref: command.ref, ok: true, reason: '' };
}

/** POST /:owner/:repo.git/git-receive-pack. */
export function handleReceivePack(repo: Repo, body: Buffer): WireResponse {
  const { commands, capabilities, packStart } = parseCommandList(body);
  if (commands.length === 0) throw new WireError('a push needs at least one command');

  const packBytes = body.subarray(packStart);
  if (packBytes.length > 0) {
    const read = readPack(packBytes, Forge.lookup(repo));
    Forge.store(repo, read.objects.values());
  }

  const results = commands.map((command) => applyCommand(repo, command));

  const wantsReport = capabilities.has('report-status') || capabilities.has('report-status-v2');
  if (!wantsReport) return { status: 200, contentType: smartType(RECEIVE_SERVICE, 'result'), body: Buffer.alloc(0) };

  const lines = ['unpack ok\n'];
  for (const result of results) lines.push(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${result.reason}\n`);
  const report = Buffer.concat([...lines.map((line) => pktLine(line)), PKT_FLUSH]);

  const frames: Buffer[] = [];
  if (capabilities.has('side-band-64k')) {
    for (let offset = 0; offset < report.length; offset += SIDEBAND_CHUNK) {
      const chunk = report.subarray(offset, offset + SIDEBAND_CHUNK);
      frames.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
    }
    frames.push(PKT_FLUSH);
  } else {
    frames.push(report);
  }
  return { status: 200, contentType: smartType(RECEIVE_SERVICE, 'result'), body: Buffer.concat(frames) };
}

/** Re-exported for callers that need an object id from bytes (kept close to the wire code). */
export { hashObject };
