// mcp.ts — the Model Context Protocol endpoint at POST /mcp: JSON-RPC 2.0 over the Streamable HTTP
// transport, modern era (2026-07-28), where `server/discover` answers the server's identity, its
// capabilities and the revisions it speaks, and every request is self-describing.
//
// The tools are the GitHub ones an agent needs to drive a forge: create_repo, get_repo, list_branches,
// create_issue, list_issues, add_issue_comment, create_pull_request, list_pull_requests,
// get_pull_request, merge_pull_request. Every tool answers one JSON text block plus the same value as
// structuredContent, so a client may read either.

import { STATE_EPOCH_ISO, type Forge, type Pull } from './gitstore.ts';
import { mergePullInto } from './restapi.ts';

/** The revision this server speaks, and the ones it accepts. */
const PREFERRED_REVISION = '2026-07-28';
const SUPPORTED_REVISIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
/** The server's own identity. */
const SERVER_INFO = { name: 'ghforge-mcp', version: '1.0.0', title: 'GitHub forge MCP server' };

const JSON_RPC_PARSE_ERROR = -32700;
const JSON_RPC_INVALID_REQUEST = -32600;
const JSON_RPC_METHOD_NOT_FOUND = -32601;
const JSON_RPC_INVALID_PARAMS = -32602;

export interface McpReply {
  status: number;
  body: unknown;
}

interface ToolDef {
  name: string;
  title: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

function schema(properties: Record<string, unknown>, required: string[]): Record<string, unknown> {
  return { type: 'object', properties, required, additionalProperties: false };
}

const text = (description: string): Record<string, unknown> => ({ type: 'string', description });
const bool = (description: string): Record<string, unknown> => ({ type: 'boolean', description });
const num = (description: string): Record<string, unknown> => ({ type: 'integer', description });

/** Every tool this server offers, in a fixed order (two tools/list calls must match byte for byte). */
const TOOLS: readonly ToolDef[] = [
  {
    name: 'create_repo',
    title: 'Create repository',
    description: 'Create a repository owned by the authenticated account.',
    inputSchema: schema({ name: text('Repository name'), description: text('Repository description'), private: bool('Whether the repository is private') }, ['name']),
  },
  {
    name: 'get_repo',
    title: 'Get repository',
    description: 'Read one repository by owner and name.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name') }, ['owner', 'repo']),
  },
  {
    name: 'list_branches',
    title: 'List branches',
    description: 'List the branches of a repository with the commit each one points at.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name') }, ['owner', 'repo']),
  },
  {
    name: 'create_issue',
    title: 'Create issue',
    description: 'Open an issue in a repository.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name'), title: text('Issue title'), body: text('Issue body') }, ['owner', 'repo', 'title']),
  },
  {
    name: 'list_issues',
    title: 'List issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name'), state: text('open, closed or all') }, ['owner', 'repo']),
  },
  {
    name: 'add_issue_comment',
    title: 'Add issue comment',
    description: 'Add a comment to an existing issue.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name'), issue_number: num('Issue number'), body: text('Comment body') }, ['owner', 'repo', 'issue_number', 'body']),
  },
  {
    name: 'create_pull_request',
    title: 'Create pull request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name'), title: text('Pull request title'), body: text('Pull request body'), head: text('Head branch'), base: text('Base branch') }, ['owner', 'repo', 'title', 'head', 'base']),
  },
  {
    name: 'list_pull_requests',
    title: 'List pull requests',
    description: 'List the pull requests of a repository.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name'), state: text('open, closed or all') }, ['owner', 'repo']),
  },
  {
    name: 'get_pull_request',
    title: 'Get pull request',
    description: 'Read one pull request by number.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name'), pull_number: num('Pull request number') }, ['owner', 'repo', 'pull_number']),
  },
  {
    name: 'merge_pull_request',
    title: 'Merge pull request',
    description: 'Merge a pull request into its base branch.',
    inputSchema: schema({ owner: text('Repository owner login'), repo: text('Repository name'), pull_number: num('Pull request number'), merge_method: text('merge, squash or rebase') }, ['owner', 'repo', 'pull_number']),
  },
];

const TOOL_JSON = JSON.stringify({ tools: TOOLS });

function rpcResult(id: unknown, result: unknown): unknown {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function rpcError(id: unknown, code: number, message: string): unknown {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

function textResult(value: unknown): unknown {
  return { content: [{ type: 'text', text: JSON.stringify(value) }], structuredContent: value, isError: false };
}

function toolError(message: string): unknown {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function argsOf(params: Record<string, unknown>): Record<string, unknown> {
  const args = params.arguments;
  return args !== null && typeof args === 'object' && !Array.isArray(args) ? (args as Record<string, unknown>) : {};
}

function str(args: Record<string, unknown>, key: string): string | undefined {
  const value = args[key];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function int(args: Record<string, unknown>, key: string): number | undefined {
  const value = args[key];
  if (typeof value === 'number' && Number.isInteger(value)) return value;
  if (typeof value === 'string' && /^\d+$/.test(value)) return Number(value);
  return undefined;
}

/** One repository view: everything an agent needs to address it again. */
function repoView(repo: { id: number; owner: string; name: string; description: string; isPrivate: boolean; defaultBranch: string; refs: Map<string, string>; issues: Map<number, unknown> }): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: repo.owner,
    description: repo.description,
    private: repo.isPrivate,
    default_branch: repo.defaultBranch,
    branches: [...repo.refs.keys()].filter((ref) => ref.startsWith('refs/heads/')).map((ref) => ref.slice('refs/heads/'.length)),
    open_issues: repo.issues.size,
    html_url: `/${repo.owner}/${repo.name}`,
  };
}

function issueView(issue: { number: number; id: number; title: string; body: string; state: string; labels: string[]; comments: unknown[] }): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels,
    comments: issue.comments.length,
    created_at: STATE_EPOCH_ISO,
  };
}

function pullView(pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.merged ? 'merged' : pull.state,
    merged: pull.merged,
    head: { ref: pull.headRef, sha: pull.headSha },
    base: { ref: pull.baseRef, sha: pull.baseSha },
    merge_commit_sha: pull.mergeCommitSha,
  };
}

/** Call one tool; answers the JSON-RPC `result` payload or an error code with its message. */
function callTool(forge: Forge, login: string, name: string, args: Record<string, unknown>): { result?: unknown; code?: number; message?: string } {
  const owner = str(args, 'owner') ?? login;
  const repoName = str(args, 'repo');
  const needRepo = (): { repo: ReturnType<Forge['get']>; missing?: string[] } => {
    if (repoName === undefined) return { repo: undefined, missing: ['repo'] };
    return { repo: forge.get(owner, repoName) };
  };

  switch (name) {
    case 'create_repo': {
      const repoName2 = str(args, 'name');
      if (repoName2 === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: 'create_repo needs a name' };
      const existing = forge.get(login, repoName2);
      if (existing !== undefined) return { result: textResult(repoView(existing)) };
      const created = forge.create(login, repoName2, { description: str(args, 'description') ?? '', isPrivate: args.private === true });
      if (created === null) return { code: JSON_RPC_INVALID_PARAMS, message: `the repository ${login}/${repoName2} could not be created` };
      return { result: textResult(repoView(created)) };
    }
    case 'get_repo': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      return { result: textResult(repoView(found.repo)) };
    }
    case 'list_branches': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const branches = [...found.repo.refs.keys()].filter((ref) => ref.startsWith('refs/heads/')).sort().map((ref) => ({ name: ref.slice('refs/heads/'.length), sha: found.repo?.refs.get(ref) }));
      return { result: textResult({ branches }) };
    }
    case 'create_issue': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const title = str(args, 'title');
      if (title === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: 'create_issue needs a title' };
      const repo = found.repo;
      const number = repo.nextIssueNumber;
      repo.nextIssueNumber += 1;
      const issue = { number, id: forge.issueId(), title, body: str(args, 'body') ?? '', state: 'open' as const, labels: [] as string[], user: login, comments: [] as { id: number; body: string; user: string }[] };
      repo.issues.set(number, issue);
      return { result: textResult(issueView(issue)) };
    }
    case 'list_issues': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const wanted = str(args, 'state') ?? 'open';
      const issues = [...found.repo.issues.values()].filter((issue) => wanted === 'all' || issue.state === wanted).sort((a, b) => b.number - a.number).map((issue) => issueView(issue));
      return { result: textResult({ total: issues.length, issues }) };
    }
    case 'add_issue_comment': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const number = int(args, 'issue_number');
      const issue = number === undefined ? undefined : found.repo.issues.get(number);
      if (issue === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: 'add_issue_comment needs an existing issue_number' };
      const body = str(args, 'body');
      if (body === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: 'add_issue_comment needs a body' };
      const comment = { id: found.repo.nextCommentId, body, user: login };
      found.repo.nextCommentId += 1;
      issue.comments.push(comment);
      return { result: textResult({ id: comment.id, body: comment.body, issue_number: issue.number }) };
    }
    case 'create_pull_request': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const head = str(args, 'head');
      const base = str(args, 'base');
      const title = str(args, 'title');
      if (head === undefined || base === undefined || title === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: 'create_pull_request needs head, base and title' };
      const repo = found.repo;
      const headSha = repo.refs.get(`refs/heads/${head}`);
      const baseSha = repo.refs.get(`refs/heads/${base}`);
      if (headSha === undefined || baseSha === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `the branches ${head} and ${base} must both exist` };
      const number = repo.nextPullNumber;
      repo.nextPullNumber += 1;
      const pull: Pull = { number, id: forge.pullId(), title, body: str(args, 'body') ?? '', state: 'open', headRef: head, headSha, baseRef: base, baseSha, merged: false, mergeCommitSha: null, mergedAt: null };
      repo.pulls.set(number, pull);
      return { result: textResult(pullView(pull)) };
    }
    case 'list_pull_requests': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const wanted = str(args, 'state') ?? 'open';
      const pulls = [...found.repo.pulls.values()].filter((pull) => wanted === 'all' || (wanted === 'open' ? pull.state === 'open' : pull.state === 'closed')).sort((a, b) => b.number - a.number).map((pull) => pullView(pull));
      return { result: textResult({ total: pulls.length, pull_requests: pulls }) };
    }
    case 'get_pull_request': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const number = int(args, 'pull_number');
      const pull = number === undefined ? undefined : found.repo.pulls.get(number);
      if (pull === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: 'get_pull_request needs an existing pull_number' };
      return { result: textResult(pullView(pull)) };
    }
    case 'merge_pull_request': {
      const found = needRepo();
      if (found.repo === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: `no repository ${owner}/${repoName ?? ''}` };
      const number = int(args, 'pull_number');
      const pull = number === undefined ? undefined : found.repo.pulls.get(number);
      if (pull === undefined) return { code: JSON_RPC_INVALID_PARAMS, message: 'merge_pull_request needs an existing pull_number' };
      const sha = mergePullInto(found.repo, pull, login);
      if (sha === null) return { result: toolError(`pull request #${pull.number} is not mergeable`) };
      return { result: textResult({ ...pullView(pull), merge_commit_sha: sha, merged: true, state: 'merged' }) };
    }
    default:
      return { code: JSON_RPC_INVALID_PARAMS, message: `unknown tool ${JSON.stringify(name)}` };
  }
}

/** Handle one POST /mcp: a JSON-RPC request, notification or a body that is not JSON at all. */
export function handleMcp(rawBody: string, forge: Forge, login: string | null): McpReply {
  if (login === null) return { status: 401, body: { jsonrpc: '2.0', id: null, error: { code: JSON_RPC_INVALID_REQUEST, message: 'Authorization is required' } } };

  let message: unknown;
  try {
    message = JSON.parse(rawBody);
  } catch {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: JSON_RPC_PARSE_ERROR, message: 'Parse error: the body is not JSON' } } };
  }
  if (message === null || typeof message !== 'object' || Array.isArray(message)) {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: JSON_RPC_INVALID_REQUEST, message: 'Invalid Request' } } };
  }
  const request = message as { id?: unknown; method?: unknown; params?: unknown };
  const hasId = Object.prototype.hasOwnProperty.call(request, 'id');
  const id = request.id ?? null;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = request.params !== null && typeof request.params === 'object' && !Array.isArray(request.params) ? (request.params as Record<string, unknown>) : {};

  // A notification has no id and takes no answer body.
  if (!hasId) {
    if (method === 'notifications/initialized' || method.startsWith('notifications/')) return { status: 202, body: null };
    return { status: 202, body: null };
  }

  switch (method) {
    case 'server/discover':
      return {
        status: 200,
        body: rpcResult(id, {
          resultType: 'complete',
          supportedVersions: SUPPORTED_REVISIONS,
          capabilities: { tools: { listChanged: false } },
          serverInfo: SERVER_INFO,
          instructions: 'A GitHub-compatible forge: repositories, issues and pull requests.',
        }),
      };
    case 'initialize': {
      const asked = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const version = SUPPORTED_REVISIONS.includes(asked) ? asked : PREFERRED_REVISION;
      return {
        status: 200,
        body: rpcResult(id, {
          protocolVersion: version,
          capabilities: { tools: { listChanged: false } },
          serverInfo: SERVER_INFO,
        }),
      };
    }
    case 'ping':
      return { status: 200, body: rpcResult(id, {}) };
    case 'tools/list':
      return { status: 200, body: { jsonrpc: '2.0', id, result: JSON.parse(TOOL_JSON) } };
    case 'tools/call': {
      const name = typeof params.name === 'string' ? params.name : '';
      if (name === '') return { status: 200, body: rpcError(id, JSON_RPC_INVALID_PARAMS, 'tools/call needs a tool name') };
      const answer = callTool(forge, login, name, argsOf(params));
      if (answer.code !== undefined) return { status: 200, body: rpcError(id, answer.code, answer.message ?? 'invalid params') };
      return { status: 200, body: rpcResult(id, answer.result) };
    }
    case 'resources/list':
      return { status: 200, body: rpcResult(id, { resources: [] }) };
    case 'prompts/list':
      return { status: 200, body: rpcResult(id, { prompts: [] }) };
    default:
      return { status: 200, body: rpcError(id, JSON_RPC_METHOD_NOT_FOUND, `Method not found: ${JSON.stringify(method)}`) };
  }
}