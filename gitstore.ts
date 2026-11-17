// gitstore.ts — the forge's state: one record per repository, holding its git objects (in memory,
// keyed by SHA-1) and its loose refs, plus the resources the REST surface exposes.
//
// Determinism: object ids are content hashes, refs are updated only by explicit commands, and every
// id or timestamp this file hands out is a pure function of creation order (never a clock).

import { hashObject, type GitObject, type ObjectLookup, type ObjectType } from './gitcore.ts';

/** The fixed instant every stored timestamp names (2025-01-01T00:00:00Z): no clock, so a re-run matches. */
export const STATE_EPOCH_SEC = 1_735_689_600;
/** STATE_EPOCH_SEC as an ISO string. */
export const STATE_EPOCH_ISO = new Date(STATE_EPOCH_SEC * 1000).toISOString();
/** The default branch every repository is born with. */
export const DEFAULT_BRANCH = 'main';

export interface IssueComment {
  id: number;
  body: string;
  user: string;
}

export interface Issue {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  user: string;
  comments: IssueComment[];
}

/** One pull request: the two refs it names, frozen shas, and its merge state. */
export interface Pull {
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  headSha: string;
  baseRef: string;
  baseSha: string;
  merged: boolean;
  mergeCommitSha: string | null;
  mergedAt: string | null;
}

/** One repository: its objects, its refs and the REST resources hanging off it. */
export interface Repo {
  readonly id: number;
  readonly owner: string;
  readonly name: string;
  description: string;
  isPrivate: boolean;
  readonly defaultBranch: string;
  readonly objects: Map<string, GitObject>;
  readonly refs: Map<string, string>;
  /** The next issue number (1-based, per repository). */
  nextIssueNumber: number;
  /** The 1-based comment id counter, shared by every issue in the repository. */
  nextCommentId: number;
  /** The next pull request number (1-based, per repository). */
  nextPullNumber: number;
  readonly issues: Map<number, Issue>;
  readonly pulls: Map<number, Pull>;
  readonly labels: Set<string>;
}

/** The whole forge. One process, one of these. */
export class Forge {
  private readonly repos = new Map<string, Repo>();
  private nextRepoId = 1;
  private nextIssueId = 1;
  private nextPullId = 1;

  /** The next globally unique issue id. */
  issueId(): number {
    const id = this.nextIssueId;
    this.nextIssueId += 1;
    return id;
  }

  /** The next globally unique pull request id. */
  pullId(): number {
    const id = this.nextPullId;
    this.nextPullId += 1;
    return id;
  }

  private static key(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name.toLowerCase()}`;
  }

  /** The repository `owner/name`, or undefined. */
  get(owner: string, name: string): Repo | undefined {
    return this.repos.get(Forge.key(owner, name));
  }

  /** The repository with numeric id `id`, or undefined. */
  findById(id: number): Repo | undefined {
    for (const repo of this.repos.values()) if (repo.id === id) return repo;
    return undefined;
  }

  /** Every repository `owner` owns, in creation order. */
  listByOwner(owner: string): Repo[] {
    const wanted = owner.toLowerCase();
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner.toLowerCase() === wanted) out.push(repo);
    return out;
  }

  /** Create `owner/name`; null when it already exists. */
  create(owner: string, name: string, options: { description?: string; isPrivate?: boolean } = {}): Repo | null {
    const key = Forge.key(owner, name);
    if (this.repos.has(key)) return null;
    const repo: Repo = {
      id: this.nextRepoId,
      owner,
      name,
      description: options.description ?? '',
      isPrivate: options.isPrivate ?? false,
      defaultBranch: DEFAULT_BRANCH,
      objects: new Map(),
      refs: new Map(),
      nextIssueNumber: 1,
      nextCommentId: 1,
      nextPullNumber: 1,
      issues: new Map(),
      pulls: new Map(),
      labels: new Set(),
    };
    this.nextRepoId += 1;
    this.repos.set(key, repo);
    return repo;
  }

  /** Delete `owner/name`; false when it was not there. */
  remove(owner: string, name: string): boolean {
    return this.repos.delete(Forge.key(owner, name));
  }

  /** A lookup into one repository's object store. */
  static lookup(repo: Repo): ObjectLookup {
    return (oid: string) => repo.objects.get(oid);
  }

  /** Store objects, skipping any already present; a no-op when the pack was empty. */
  static store(repo: Repo, objects: Iterable<GitObject>): void {
    for (const object of objects) {
      const oid = hashObject(object.type, object.data);
      if (!repo.objects.has(oid)) repo.objects.set(oid, object);
    }
  }

  /** The type of `oid`, or undefined. */
  static typeOf(repo: Repo, oid: string): ObjectType | undefined {
    return repo.objects.get(oid)?.type;
  }
}
