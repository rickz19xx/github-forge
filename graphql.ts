// graphql.ts — GitHub's GraphQL v4 wire on POST /api/graphql: a document in, `{data, errors}` out.
//
// No GraphQL library: the lexer, the recursive-descent selection parser and the executor below cover
// exactly the shapes gh and github-mcp-server send — named queries and mutations, string/int variables,
// inline enum lists, inline input objects, field aliases, inline fragments and operation batching.
//
// Two rules this file keeps:
//   - a GraphQL-level failure (an unknown field, a bad argument) is a 200 whose body carries `errors`;
//     an absent token is the only HTTP-level failure (401);
//   - global ids are opaque strings that round-trip through node(id:) and the mutation inputs.

import { STATE_EPOCH_ISO, type Forge, type Issue, type Pull, type Repo } from './gitstore.ts';
import { commitTree, diffTrees, makeCommit, mergePullInto, writePath } from './restapi.ts';

/** The changed paths between two trees. */
function diffTreePaths(repo: Repo, from: string | null, to: string | null): string[] {
  const diff = diffTrees(repo, from, to);
  return [...diff.added, ...diff.modified, ...diff.removed].sort();
}

/** The tree a commit names. */
const commitTreeOf = commitTree;

/** Write an existing blob id at a path. */
const writeTreePath = writePath;

// ---------------------------------------------------------------------------------------------
// lexer
// ---------------------------------------------------------------------------------------------

type Token = { t: 'name' | 'num' | 'str' | 'punc'; v: string };

function lex(source: string): Token[] {
  const out: Token[] = [];
  let i = 0;
  while (i < source.length) {
    const c = source[i] as string;
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (source.startsWith('...', i)) {
      out.push({ t: 'punc', v: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!$=@|'.includes(c)) {
      out.push({ t: 'punc', v: c });
      i += 1;
      continue;
    }
    if (c === '"') {
      if (source.startsWith('"""', i)) {
        const end = source.indexOf('"""', i + 3);
        const stop = end < 0 ? source.length : end;
        out.push({ t: 'str', v: source.slice(i + 3, stop) });
        i = stop + 3;
        continue;
      }
      let j = i + 1;
      let value = '';
      while (j < source.length && source[j] !== '"') {
        if (source[j] === '\\') {
          value += source[j + 1] ?? '';
          j += 2;
          continue;
        }
        value += source[j];
        j += 1;
      }
      out.push({ t: 'str', v: value });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(c)) {
      let j = i;
      while (j < source.length && /[0-9.eE+-]/.test(source[j] as string)) j += 1;
      out.push({ t: 'num', v: source.slice(i, j) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < source.length && /[A-Za-z0-9_]/.test(source[j] as string)) j += 1;
      out.push({ t: 'name', v: source.slice(i, j) });
      i = j;
      continue;
    }
    i += 1;
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------------------------

type Value =
  | { k: 'var'; name: string }
  | { k: 'lit'; v: unknown }
  | { k: 'enum'; v: string }
  | { k: 'list'; items: Value[] }
  | { k: 'obj'; fields: Map<string, Value> };

interface Selection {
  name: string;
  alias: string | null;
  args: Map<string, Value>;
  selections: Selection[];
  fragmentOn: string | null;
}

interface Document {
  op: 'query' | 'mutation';
  selections: Selection[];
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(offset = 0): Token | undefined {
    return this.tokens[this.pos + offset];
  }

  private next(): Token {
    const token = this.tokens[this.pos];
    this.pos += 1;
    if (token === undefined) throw new Error('the document ended early');
    return token;
  }

  private expect(value: string): void {
    const token = this.next();
    if (token.v !== value) throw new Error(`expected ${value}, found ${token.v}`);
  }

  private skipGroup(open: string, close: string): void {
    this.expect(open);
    let depth = 1;
    while (depth > 0) {
      const token = this.next();
      if (token.v === open) depth += 1;
      else if (token.v === close) depth -= 1;
    }
  }

  parseDocument(): Document {
    let op: 'query' | 'mutation' = 'query';
    const head = this.peek();
    if (head !== undefined && (head.v === 'query' || head.v === 'mutation') && head.t === 'name') {
      op = this.next().v as 'query' | 'mutation';
      if (this.peek()?.t === 'name') this.next();
      if (this.peek()?.v === '(') this.skipGroup('(', ')');
    }
    while (this.peek()?.v === '@') {
      this.next();
      this.next();
    }
    return { op, selections: this.parseSelectionSet() };
  }

  private parseSelectionSet(): Selection[] {
    this.expect('{');
    const selections: Selection[] = [];
    while (this.peek() !== undefined && this.peek()?.v !== '}') selections.push(this.parseSelection());
    this.expect('}');
    return selections;
  }

  private parseSelection(): Selection {
    if (this.peek()?.v === '...') {
      this.next();
      let onType: string | null = null;
      if (this.peek()?.v === 'on') {
        this.next();
        onType = this.next().v;
      } else if (this.peek()?.t === 'name') {
        this.next();
      }
      return { name: '...', alias: null, args: new Map(), selections: this.parseSelectionSet(), fragmentOn: onType };
    }
    let name = this.next().v;
    let alias: string | null = null;
    if (this.peek()?.v === ':') {
      this.next();
      alias = name;
      name = this.next().v;
    }
    const args = new Map<string, Value>();
    if (this.peek()?.v === '(') {
      this.next();
      while (this.peek() !== undefined && this.peek()?.v !== ')') {
        const argName = this.next().v;
        this.expect(':');
        args.set(argName, this.parseValue());
      }
      this.expect(')');
    }
    const selections = this.peek()?.v === '{' ? this.parseSelectionSet() : [];
    return { name, alias, args, selections, fragmentOn: null };
  }

  private parseValue(): Value {
    const token = this.next();
    if (token.v === '$') return { k: 'var', name: this.next().v };
    if (token.v === '[') {
      const items: Value[] = [];
      while (this.peek() !== undefined && this.peek()?.v !== ']') items.push(this.parseValue());
      this.expect(']');
      return { k: 'list', items };
    }
    if (token.v === '{') {
      const fields = new Map<string, Value>();
      while (this.peek() !== undefined && this.peek()?.v !== '}') {
        const name = this.next().v;
        this.expect(':');
        fields.set(name, this.parseValue());
      }
      this.expect('}');
      return { k: 'obj', fields };
    }
    if (token.t === 'str') return { k: 'lit', v: token.v };
    if (token.t === 'num') return { k: 'lit', v: Number(token.v) };
    if (token.v === 'true') return { k: 'lit', v: true };
    if (token.v === 'false') return { k: 'lit', v: false };
    if (token.v === 'null') return { k: 'lit', v: null };
    return { k: 'enum', v: token.v };
  }
}

// ---------------------------------------------------------------------------------------------
// global ids
// ---------------------------------------------------------------------------------------------

export function repositoryGlobalId(repo: Repo): string {
  return `R_${repo.id}`;
}

export function issueGlobalId(repo: Repo, issue: Issue): string {
  return `I_${repo.id}_${issue.number}`;
}

export function pullGlobalId(repo: Repo, pull: Pull): string {
  return `P_${repo.id}_${pull.number}`;
}

function numbersIn(id: unknown): number[] {
  if (typeof id !== 'string') return [];
  return id
    .split('_')
    .slice(1)
    .map((part) => Number.parseInt(part, 10))
    .filter((value) => Number.isFinite(value));
}

// ---------------------------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------------------------

interface GraphQLError {
  message: string;
  path?: (string | number)[];
}

/** A resolved node: a kind the field resolver understands, plus its payload. */
interface Node {
  kind: string;
  repo?: Repo;
  issue?: Issue;
  pull?: Pull;
  comment?: { id: number; body: string };
  name?: string;
  list?: Node[];
  hasNextPage?: boolean;
  endCursor?: string | null;
  value?: unknown;
}

const SCHEMA: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'comments', 'createdAt', 'updatedAt', 'closed'],
  PullRequest: ['id', 'number', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'title', 'body'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'isPrivate',
  ],
  SearchType: [],
};

const SEARCH_TYPE_VALUES = ['ISSUE', 'ISSUE_ADVANCED', 'REPOSITORY', 'USER', 'DISCUSSION'];

class Executor {
  readonly errors: GraphQLError[] = [];

  private readonly forge: Forge;
  private readonly login: string;
  private readonly vars: Record<string, unknown>;
  private readonly origin: string;

  constructor(forge: Forge, login: string, vars: Record<string, unknown>, origin: string) {
    this.forge = forge;
    this.login = login;
    this.vars = vars;
    this.origin = origin;
  }

  private resolve(value: Value | undefined): unknown {
    if (value === undefined) return undefined;
    if (value.k === 'var') return this.vars[value.name];
    if (value.k === 'lit') return value.v;
    if (value.k === 'enum') return value.v;
    if (value.k === 'list') return value.items.map((item) => this.resolve(item));
    const out: Record<string, unknown> = {};
    for (const [key, item] of value.fields) out[key] = this.resolve(item);
    return out;
  }

  private arg(selection: Selection, name: string): unknown {
    return this.resolve(selection.args.get(name));
  }

  /** A selection's argument by name, resolved against the variables. */
  value(selection: Selection, name: string): unknown {
    return this.arg(selection, name);
  }

  private fail(message: string, path?: (string | number)[]): null {
    this.errors.push(path === undefined ? { message } : { message, path });
    return null;
  }

  /** Resolve a selection set against one node into a plain JSON object. */
  object(node: Node, selections: readonly Selection[], path: (string | number)[]): Record<string, unknown> | null {
    if (node.kind === 'null') return null;
    const out: Record<string, unknown> = {};
    for (const selection of selections) {
      if (selection.fragmentOn !== null) {
        if (matchesFragment(node.kind, selection.fragmentOn)) {
          const nested = this.object(node, selection.selections, path);
          if (nested !== null) Object.assign(out, nested);
        }
        continue;
      }
      const key = selection.alias ?? selection.name;
      const fieldPath = [...path, key];
      const child = this.field(node, selection, fieldPath);
      out[key] = child;
    }
    return out;
  }

  private field(node: Node, selection: Selection, path: (string | number)[]): unknown {
    const name = selection.name;
    const repo = node.repo;
    const issue = node.issue;
    const pull = node.pull;
    switch (node.kind) {
      case 'user':
        if (name === 'login') return this.login;
        if (name === 'name') return this.login;
        if (name === 'id') return `U_${this.login}`;
        return this.fail(`Field '${name}' doesn't exist on type 'User'`, path);
      case 'repo': {
        if (repo === undefined) return null;
        if (name === 'id') return repositoryGlobalId(repo);
        if (name === 'databaseId') return repo.id;
        if (name === 'name') return repo.name;
        if (name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
        if (name === 'description') return repo.description;
        if (name === 'isPrivate') return repo.isPrivate;
        if (name === 'owner') return this.object({ kind: 'user', repo }, selection.selections, path);
        if (name === 'viewerPermission') return 'ADMIN';
        if (name === 'hasIssuesEnabled') return true;
        if (name === 'defaultBranchRef') {
          if (!repo.refs.has(`refs/heads/${repo.defaultBranch}`)) return null;
          return this.object({ kind: 'ref', repo }, selection.selections, path);
        }
        if (name === 'issue') {
          const number = Number(this.arg(selection, 'number'));
          const found = repo.issues.get(number);
          if (found === undefined) return null;
          return this.object({ kind: 'issue', repo, issue: found }, selection.selections, path);
        }
        if (name === 'issues') return this.issuesConnection(repo, selection, path);
        if (name === 'pullRequest') {
          const number = Number(this.arg(selection, 'number'));
          const found = repo.pulls.get(number);
          if (found === undefined) return null;
          return this.object({ kind: 'pr', repo, pull: found }, selection.selections, path);
        }
        if (name === 'pullRequests') {
          const first = Number(this.arg(selection, 'first') ?? 30);
          const open = [...repo.pulls.values()].filter((entry) => entry.state === 'open').sort((a, b) => b.number - a.number);
          const list = open.slice(0, Number.isFinite(first) ? first : 30).map((entry) => ({ kind: 'pr', repo, pull: entry }));
          return this.object({ kind: 'prConn', repo, list }, selection.selections, path);
        }
        return this.fail(`Field '${name}' doesn't exist on type 'Repository'`, path);
      }
      case 'ref':
        if (name === 'name') return node.repo?.defaultBranch ?? null;
        return this.fail(`Field '${name}' doesn't exist on type 'Ref'`, path);
      case 'issue':
        if (issue === undefined) return null;
        if (name === 'id') return issueGlobalId(node.repo as Repo, issue);
        if (name === 'number') return issue.number;
        if (name === 'title') return issue.title;
        if (name === 'body') return issue.body;
        if (name === 'state') return issue.state === 'open' ? 'OPEN' : 'CLOSED';
        if (name === 'closed') return issue.state !== 'open';
        if (name === 'createdAt') return STATE_EPOCH_ISO;
        if (name === 'updatedAt') return STATE_EPOCH_ISO;
        if (name === 'comments') {
          const list = issue.comments.map((comment) => ({ kind: 'comment', comment }));
          return this.object({ kind: 'commentConn', list }, selection.selections, path);
        }
        return this.fail(`Field '${name}' doesn't exist on type 'Issue'`, path);
      case 'pr':
        if (pull === undefined) return null;
        if (name === 'id') return pullGlobalId(node.repo as Repo, pull);
        if (name === 'number') return pull.number;
        if (name === 'title') return pull.title;
        if (name === 'body') return pull.body;
        if (name === 'state') return pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED';
        if (name === 'merged') return pull.merged;
        if (name === 'mergeable') return 'MERGEABLE';
        if (name === 'headRefName') return pull.headRef;
        if (name === 'baseRefName') return pull.baseRef;
        if (name === 'files') {
          const paths = pullFilePaths(node.repo as Repo, pull);
          const first = Number(this.arg(selection, 'first') ?? 100);
          const list = paths.slice(0, Number.isFinite(first) ? first : 100).map((pathName) => ({ kind: 'file', name: pathName }));
          return this.object({ kind: 'fileConn', list }, selection.selections, path);
        }
        return this.fail(`Field '${name}' doesn't exist on type 'PullRequest'`, path);
      case 'file':
        if (name === 'path') return node.name;
        return this.fail(`Field '${name}' doesn't exist on type 'PullRequestChangedFile'`, path);
      case 'fileConn':
      case 'prConn':
      case 'commentConn': {
        const list = node.list ?? [];
        if (name === 'nodes') return list.map((entry) => this.object(entry, selection.selections, path));
        if (name === 'totalCount') return list.length;
        return this.fail(`Field '${name}' doesn't exist on type 'Connection'`, path);
      }
      case 'issueConn': {
        const list = node.list ?? [];
        if (name === 'nodes') return list.map((entry) => this.object(entry, selection.selections, path));
        if (name === 'totalCount') return list.length;
        if (name === 'pageInfo') {
          return this.object({ kind: 'pageInfo', hasNextPage: node.hasNextPage, endCursor: node.endCursor ?? null }, selection.selections, path);
        }
        return this.fail(`Field '${name}' doesn't exist on type 'IssueConnection'`, path);
      }
      case 'pageInfo':
        if (name === 'hasNextPage') return node.hasNextPage === true;
        if (name === 'endCursor') return node.endCursor ?? null;
        if (name === 'startCursor') return node.endCursor ?? null;
        return this.fail(`Field '${name}' doesn't exist on type 'PageInfo'`, path);
      case 'comment':
        if (name === 'id') return `C_${node.comment?.id ?? 0}`;
        if (name === 'body') return node.comment?.body ?? '';
        return this.fail(`Field '${name}' doesn't exist on type 'IssueComment'`, path);
      case 'commentEdge':
        if (name === 'node') return this.object({ kind: 'comment', comment: node.comment }, selection.selections, path);
        return this.fail(`Field '${name}' doesn't exist on type 'IssueCommentEdge'`, path);
      case 'type': {
        const typeName = node.name ?? '';
        if (name === 'name') return typeName;
        if (name === 'fields' || name === 'enumValues') {
          // __Type.fields and __Type.enumValues are plain lists of named entries, not connections.
          const values = name === 'fields' ? (SCHEMA[typeName] ?? []) : typeName === 'SearchType' ? SEARCH_TYPE_VALUES : [];
          return values.map((entry) => this.object({ kind: 'field', name: entry }, selection.selections, path));
        }
        return this.fail(`Field '${name}' doesn't exist on type '__Type'`, path);
      }
      case 'field':
        if (name === 'name') return node.name;
        return this.fail(`Field '${name}' doesn't exist on type '__Field'`, path);
      case 'payload': {
        const payload = node.value as Record<string, Node> | undefined;
        if (payload === undefined) return null;
        const child = payload[name];
        if (child === undefined) return this.fail(`Field '${name}' doesn't exist on type 'Payload'`, path);
        return this.object(child, selection.selections, path);
      }
      default:
        return null;
    }
  }

  /** issues(first, states, orderBy, after) as a connection node. */
  private issuesConnection(repo: Repo, selection: Selection, path: (string | number)[]): unknown {
    const firstRaw = this.arg(selection, 'first');
    const first = Number.isFinite(Number(firstRaw)) ? Number(firstRaw) : 30;
    const states = this.arg(selection, 'states');
    const wanted = Array.isArray(states) ? states.map((entry) => String(entry)) : null;
    const after = this.arg(selection, 'after');
    const orderBy = this.arg(selection, 'orderBy') as Record<string, unknown> | undefined;
    const direction = typeof orderBy?.direction === 'string' ? orderBy.direction : 'DESC';
    let list = [...repo.issues.values()];
    if (wanted !== null && !wanted.includes('ALL')) {
      list = list.filter((issue) => (wanted.includes('OPEN') && issue.state === 'open') || (wanted.includes('CLOSED') && issue.state === 'closed'));
    }
    list.sort((a, b) => (direction === 'ASC' ? a.number - b.number : b.number - a.number));
    if (typeof after === 'string' && after !== '') {
      const cursor = Number.parseInt(after, 10);
      if (Number.isFinite(cursor)) list = list.filter((issue) => (direction === 'ASC' ? issue.number > cursor : issue.number < cursor));
    }
    const page = list.slice(0, first);
    const endCursor = page.length > 0 ? String((page[page.length - 1] as Issue).number) : null;
    const node: Node = {
      kind: 'issueConn',
      repo,
      list: page.map((issue) => ({ kind: 'issue', repo, issue })),
      hasNextPage: list.length > page.length,
      endCursor,
    };
    return this.object(node, selection.selections, path);
  }

  /** A repository by global id, or by owner/name. */
  repoById(id: unknown): Repo | undefined {
    const [numeric] = numbersIn(id);
    if (numeric === undefined) return undefined;
    return this.forge.findById(numeric);
  }

  root(node: Node, selections: readonly Selection[]): Record<string, unknown> | null {
    return this.object(node, selections, []);
  }

  mutate(selections: readonly Selection[]): Record<string, unknown> | null {
    const out: Record<string, unknown> = {};
    for (const selection of selections) {
      const key = selection.alias ?? selection.name;
      const input = (this.arg(selection, 'input') ?? {}) as Record<string, unknown>;
      switch (selection.name) {
        case 'createIssue': {
          const repo = this.repoById(input.repositoryId);
          if (repo === undefined) {
            out[key] = this.fail('Could not resolve to a Repository with the global id', [key]);
            break;
          }
          const number = repo.nextIssueNumber;
          repo.nextIssueNumber += 1;
          const issue: Issue = {
            number,
            id: this.forge.issueId(),
            title: String(input.title ?? ''),
            body: String(input.body ?? ''),
            state: 'open',
            labels: [],
            user: this.login,
            comments: [],
          };
          repo.issues.set(number, issue);
          out[key] = this.object({ kind: 'payload', value: { issue: { kind: 'issue', repo, issue } } }, selection.selections, [key]);
          break;
        }
        case 'closeIssue': {
          const target = this.findIssue(input.issueId);
          if (target === undefined) {
            out[key] = this.fail('Could not resolve to an Issue with the global id', [key]);
            break;
          }
          target.issue.state = 'closed';
          out[key] = this.object({ kind: 'payload', value: { issue: { kind: 'issue', repo: target.repo, issue: target.issue } } }, selection.selections, [key]);
          break;
        }
        case 'createPullRequest': {
          const repo = this.repoById(input.repositoryId);
          const headRef = String(input.headRefName ?? '');
          const baseRef = String(input.baseRefName ?? '');
          const headSha = repo?.refs.get(`refs/heads/${headRef}`);
          const baseSha = repo?.refs.get(`refs/heads/${baseRef}`);
          if (repo === undefined || headSha === undefined || baseSha === undefined) {
            out[key] = this.fail('Could not resolve the head or base ref', [key]);
            break;
          }
          const number = repo.nextPullNumber;
          repo.nextPullNumber += 1;
          const pull: Pull = {
            number,
            id: this.forge.pullId(),
            title: String(input.title ?? ''),
            body: String(input.body ?? ''),
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
          out[key] = this.object({ kind: 'payload', value: { pullRequest: { kind: 'pr', repo, pull } } }, selection.selections, [key]);
          break;
        }
        case 'mergePullRequest': {
          const target = this.findPull(input.pullRequestId);
          if (target === undefined || target.pull.merged) {
            out[key] = this.fail('Pull request is not mergeable', [key]);
            break;
          }
          mergePullInto(target.repo, target.pull, this.login);
          out[key] = this.object({ kind: 'payload', value: { pullRequest: { kind: 'pr', repo: target.repo, pull: target.pull } } }, selection.selections, [key]);
          break;
        }
        case 'addComment': {
          const target = this.findIssue(input.subjectId);
          if (target === undefined) {
            out[key] = this.fail('Could not resolve to an Issue with the global id', [key]);
            break;
          }
          const comment = { id: target.repo.nextCommentId, body: String(input.body ?? ''), user: this.login };
          target.repo.nextCommentId += 1;
          target.issue.comments.push(comment);
          out[key] = this.object({ kind: 'payload', value: { commentEdge: { kind: 'commentEdge', comment } } }, selection.selections, [key]);
          break;
        }
        default:
          out[key] = this.fail(`Field '${selection.name}' doesn't exist on type 'Mutation'`, [key]);
      }
    }
    return out;
  }

  findIssue(id: unknown): { repo: Repo; issue: Issue } | undefined {
    const [repoId, number] = numbersIn(id);
    if (repoId === undefined || number === undefined) return undefined;
    const repo = this.forge.findById(repoId);
    const issue = repo?.issues.get(number);
    return repo !== undefined && issue !== undefined ? { repo, issue } : undefined;
  }

  findPull(id: unknown): { repo: Repo; pull: Pull } | undefined {
    const [repoId, number] = numbersIn(id);
    if (repoId === undefined || number === undefined) return undefined;
    const repo = this.forge.findById(repoId);
    const pull = repo?.pulls.get(number);
    return repo !== undefined && pull !== undefined ? { repo, pull } : undefined;
  }
}

function matchesFragment(kind: string, onType: string): boolean {
  if (onType === 'Repository') return kind === 'repo' || kind === 'node';
  if (onType === 'Issue') return kind === 'issue' || kind === 'node';
  if (onType === 'PullRequest') return kind === 'pr' || kind === 'node';
  if (onType === 'User') return kind === 'user';
  return kind === 'node';
}

/** The changed paths of a pull request, base tree to head tree. */
function pullFilePaths(repo: Repo, pull: Pull): string[] {
  const baseTree = commitTreeOf(repo, pull.baseSha);
  const headTree = commitTreeOf(repo, pull.headSha);
  return diffTreePaths(repo, baseTree, headTree);
}

// ---------------------------------------------------------------------------------------------
// the root
// ---------------------------------------------------------------------------------------------

/** Execute one operation document against the forge. */
export function executeDocument(
  document: Document,
  forge: Forge,
  login: string,
  origin: string,
  variables: Record<string, unknown>,
): { data: unknown; errors?: GraphQLError[] } {
  const executor = new Executor(forge, login, variables, origin);
  if (document.op === 'mutation') {
    const data = executor.mutate(document.selections);
    return executor.errors.length > 0 ? { data, errors: executor.errors } : { data };
  }
  const out: Record<string, unknown> = {};
  for (const selection of document.selections) {
    const key = selection.alias ?? selection.name;
    switch (selection.name) {
      case 'viewer':
        out[key] = executor.root({ kind: 'user' }, selection.selections);
        break;
      case 'repository': {
        const owner = String(executor.value(selection, 'owner') ?? '');
        const name = String(executor.value(selection, 'name') ?? '');
        const repo = forge.get(owner, name);
        out[key] = repo === undefined ? null : executor.root({ kind: 'repo', repo }, selection.selections);
        break;
      }
      case 'node': {
        const repo = executor.repoById(executor.value(selection, 'id'));
        out[key] = repo === undefined ? null : executor.root({ kind: 'repo', repo }, selection.selections);
        break;
      }
      case '__type': {
        const typeName = String(executor.value(selection, 'name') ?? '');
        out[key] = SCHEMA[typeName] === undefined ? null : executor.root({ kind: 'type', name: typeName }, selection.selections);
        break;
      }
      default:
        out[key] = null;
        executor.errors.push({ message: `Field '${selection.name}' doesn't exist on type 'Query'`, path: [key] });
    }
  }
  return executor.errors.length > 0 ? { data: out, errors: executor.errors } : { data: out };
}

/** Parse one document's text into the operation this module executes. */
export function parseGraphQL(query: string): Document {
  return new Parser(lex(query)).parseDocument();
}
