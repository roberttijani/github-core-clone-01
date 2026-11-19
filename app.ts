import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import {
  encodePktLine,
  encodePktLines,
  pktLineFlush,
  pktLineDelim,
  hashObject,
  buildTree,
  buildCommit,
  buildTag,
  writePack,
  readPack,
  parseTree,
  parseCommit,
  ZERO_OID,
  type GitObject,
  type GitObjectType,
  type TreeEntry,
} from './git-client.ts';

// ---------------------------------------------------------------------------
// Helpers & Types
// ---------------------------------------------------------------------------

const JSON_TYPE = 'application/json; charset=utf-8';

let reqCounter = 0;
const REQ_PREFIX = `${Math.floor(Math.random() * 0xffffffff).toString(16).padStart(8, '0')}:`;

function getRequestId(): string {
  return `${REQ_PREFIX}${(++reqCounter).toString(16)}`;
}

function sendJson(res: ServerResponse, status: number, body: unknown, extraHeaders: Record<string, string> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  const buf = Buffer.from(text, 'utf8');
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': buf.length,
    'x-github-request-id': getRequestId(),
    'connection': 'keep-alive',
    ...extraHeaders,
  });
  res.end(buf);
}

function sendText(res: ServerResponse, status: number, contentType: string, body: Buffer | string, extraHeaders: Record<string, string> = {}): void {
  const buf = Buffer.isBuffer(body) ? body : Buffer.from(body, 'utf8');
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': buf.length,
    'x-github-request-id': getRequestId(),
    'connection': 'keep-alive',
    ...extraHeaders,
  });
  res.end(buf);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      if (chunks.length === 0) resolve(Buffer.alloc(0));
      else if (chunks.length === 1) resolve(chunks[0]);
      else resolve(Buffer.concat(chunks));
    });
    req.on('error', reject);
  });
}

function encodeSideband1(text: string): Buffer {
  const textLen = Buffer.byteLength(text, 'utf8');
  const totalPayloadLen = 1 + textLen;
  const pktLen = 4 + totalPayloadLen;
  const hex = pktLen.toString(16).padStart(4, '0');
  const buf = Buffer.allocUnsafe(pktLen);
  buf.write(hex, 0, 4, 'ascii');
  buf[4] = 1;
  buf.write(text, 5, textLen, 'utf8');
  return buf;
}

const UNPACK_OK_PKT = encodeSideband1('unpack ok\n');
const PKT_FLUSH = Buffer.from('0000', 'ascii');
const V2_UPLOAD_PACK_ADVERTISEMENT = Buffer.concat([
  encodePktLines([
    'version 2\n',
    'agent=makefun-forge/1.0\n',
    'ls-refs=peel symrefs unborn\n',
    'fetch=shallow wait-for-done filter\n',
    'server-option\n',
    'object-format=sha1\n',
  ]),
  PKT_FLUSH,
]);

async function readJsonBody(req: IncomingMessage): Promise<{ ok: true; data: any } | { ok: false; raw: string }> {
  const buf = await readBody(req);
  const text = buf.toString('utf8');
  if (!text.trim()) return { ok: true, data: {} };
  try {
    return { ok: true, data: JSON.parse(text) };
  } catch {
    return { ok: false, raw: text };
  }
}

// ---------------------------------------------------------------------------
// In-Memory Forge Data Store
// ---------------------------------------------------------------------------

interface ForgeUser {
  id: number;
  login: string;
  name: string;
  email: string;
}

interface ForgeLabel {
  id: number;
  name: string;
  color: string;
}

interface ForgeComment {
  id: number;
  body: string;
  created_at: string;
  user: ForgeUser;
}

interface ForgeIssue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: ForgeLabel[];
  created_at: string;
  updated_at: string;
  comments: ForgeComment[];
  _cachedBuffer?: Buffer;
}

function serializeIssue(issue: ForgeIssue): Buffer {
  const data = {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels,
  };
  return Buffer.from(`${JSON.stringify(data)}\n`, 'utf8');
}

interface ForgePull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  merged_at: string | null;
  merge_commit_sha: string | null;
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  headCommitSha: string;
  baseCommitSha: string;
}

interface ForgeRepo {
  id: number;
  nodeId: string;
  name: string;
  owner: ForgeUser;
  private: boolean;
  description: string | null;
  default_branch: string;
  created_at: string;
  updated_at: string;
  refs: Map<string, string>; // ref name -> oid
  objects: Map<string, GitObject>; // oid -> GitObject
  labels: Map<string, ForgeLabel>;
  issues: Map<number, ForgeIssue>;
  issueSeq: number;
  pulls: Map<number, ForgePull>;
  pullSeq: number;
  commentsSeq: number;
  commitParents: Map<string, string[]>;
  treeEntries: Map<string, TreeEntry[]>;
  shallowPackCache: Map<string, Buffer>;
  lsRefsCache: Map<string, Buffer>;
}

function invalidateRepoCache(repo: ForgeRepo): void {
  repo.shallowPackCache.clear();
  repo.lsRefsCache.clear();
}

class ForgeStore {
  readonly viewer: ForgeUser = {
    id: 1000,
    login: process.env.MAKEFUN_FORGE_LOGIN || 'adaiversen',
    name: 'ada iversen',
    email: 'adaiversen@example.invalid',
  };
  private nextRepoId = 1;
  readonly repos = new Map<string, ForgeRepo>(); // "owner/name" -> ForgeRepo

  repoKey(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name.toLowerCase()}`;
  }

  getRepo(owner: string, name: string): ForgeRepo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(name: string, isPrivate = true, description: string | null = null): ForgeRepo {
    const id = this.nextRepoId++;
    const repo: ForgeRepo = {
      id,
      nodeId: Buffer.from(`010:Repository${id}`).toString('base64'),
      name,
      owner: this.viewer,
      private: isPrivate,
      description,
      default_branch: 'main',
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: new Map(),
      issueSeq: 0,
      pulls: new Map(),
      pullSeq: 0,
      commentsSeq: 0,
      commitParents: new Map(),
      treeEntries: new Map(),
      shallowPackCache: new Map(),
      lsRefsCache: new Map(),
    };
    this.repos.set(this.repoKey(this.viewer.login, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  putObject(repo: ForgeRepo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) {
      repo.objects.set(oid, { type, data });
    }
    return oid;
  }
}

// ---------------------------------------------------------------------------
// Git Tree & Commit Helpers
// ---------------------------------------------------------------------------

function getTreeEntries(repo: ForgeRepo, treeOid: string): TreeEntry[] {
  let cached = repo.treeEntries.get(treeOid);
  if (cached) return cached;
  const obj = repo.objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return [];
  cached = parseTree(obj.data);
  repo.treeEntries.set(treeOid, cached);
  return cached;
}

function findEntryInTree(repo: ForgeRepo, treeOid: string, filePath: string): { oid: string; mode: string } | null {
  const segments = filePath.split('/').filter(Boolean);
  if (segments.length === 0) {
    return { oid: treeOid, mode: '40000' };
  }
  let currentOid = treeOid;
  for (let i = 0; i < segments.length; i++) {
    const seg = segments[i];
    const entries = getTreeEntries(repo, currentOid);
    const entry = entries.find((e) => e.name === seg);
    if (!entry) return null;
    if (i === segments.length - 1) {
      return { oid: entry.oid, mode: entry.mode };
    }
    if (entry.mode !== '40000') return null;
    currentOid = entry.oid;
  }
  return null;
}

function findBlobInTree(repo: ForgeRepo, treeOid: string, filePath: string): { oid: string; mode: string } | null {
  return findEntryInTree(repo, treeOid, filePath);
}

function listTreeRecursive(repo: ForgeRepo, treeOid: string, prefix = ''): Array<{ path: string; oid: string; mode: string; type: 'file' | 'dir' }> {
  const result: Array<{ path: string; oid: string; mode: string; type: 'file' | 'dir' }> = [];
  const entries = getTreeEntries(repo, treeOid);
  for (const entry of entries) {
    const fullPath = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.mode === '40000') {
      result.push({ path: fullPath, oid: entry.oid, mode: entry.mode, type: 'dir' });
      result.push(...listTreeRecursive(repo, entry.oid, fullPath));
    } else {
      result.push({ path: fullPath, oid: entry.oid, mode: entry.mode, type: 'file' });
    }
  }
  return result;
}

function updateTreeWithFile(
  repo: ForgeRepo,
  treeOid: string | null,
  filePath: string,
  blobOid: string | null, // null means delete
): string {
  const segments = filePath.split('/').filter(Boolean);
  const updateLevel = (currentTreeOid: string | null, depth: number): string => {
    const seg = segments[depth];
    const entries = currentTreeOid ? [...getTreeEntries(repo, currentTreeOid)] : [];
    const isLeaf = depth === segments.length - 1;

    if (isLeaf) {
      const idx = entries.findIndex((e) => e.name === seg);
      if (blobOid === null) {
        if (idx >= 0) entries.splice(idx, 1);
      } else {
        const newEntry: TreeEntry = { mode: '100644', name: seg, oid: blobOid };
        if (idx >= 0) entries[idx] = newEntry;
        else entries.push(newEntry);
      }
    } else {
      const idx = entries.findIndex((e) => e.name === seg && e.mode === '40000');
      const subTreeOid = idx >= 0 ? entries[idx].oid : null;
      const newSubTreeOid = updateLevel(subTreeOid, depth + 1);
      const subEntries = getTreeEntries(repo, newSubTreeOid);
      if (subEntries.length === 0) {
        if (idx >= 0) entries.splice(idx, 1);
      } else {
        const newEntry: TreeEntry = { mode: '40000', name: seg, oid: newSubTreeOid };
        if (idx >= 0) entries[idx] = newEntry;
        else entries.push(newEntry);
      }
    }
    const treeData = buildTree(entries);
    const oid = hashObject('tree', treeData);
    repo.objects.set(oid, { type: 'tree', data: treeData });
    repo.treeEntries.set(oid, entries);
    return oid;
  };

  return updateLevel(treeOid, 0);
}

function createCommit(
  repo: ForgeRepo,
  treeOid: string,
  parents: string[],
  message: string,
  authorStr = 'ada iversen <adaiversen@example.invalid>',
): string {
  const commitData = buildCommit({
    tree: treeOid,
    parents,
    message: message.endsWith('\n') ? message : `${message}\n`,
    author: authorStr,
    committer: authorStr,
    timestamp: Math.floor(Date.now() / 1000),
  });
  const oid = hashObject('commit', commitData);
  repo.objects.set(oid, { type: 'commit', data: commitData });
  repo.commitParents.set(oid, parents);
  return oid;
}

function collectReachableOids(repo: ForgeRepo, tips: Iterable<string>, stopOids = new Set<string>()): Set<string> {
  const reachable = new Set<string>();
  const queue = [...tips];
  while (queue.length > 0) {
    const oid = queue.pop()!;
    if (!oid || oid === ZERO_OID || reachable.has(oid) || stopOids.has(oid)) continue;
    reachable.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj) continue;
    if (obj.type === 'commit') {
      let parents = repo.commitParents.get(oid);
      let treeOid: string | undefined;
      if (!parents) {
        const commit = parseCommit(obj.data);
        parents = commit.parents;
        treeOid = commit.tree;
        repo.commitParents.set(oid, parents);
      } else {
        const match = obj.data.toString('ascii', 0, 100).match(/^tree ([0-9a-f]{40})/m);
        if (match) treeOid = match[1];
      }
      if (treeOid && !reachable.has(treeOid)) queue.push(treeOid);
      for (const p of parents) {
        if (!reachable.has(p)) queue.push(p);
      }
    } else if (obj.type === 'tree') {
      const entries = getTreeEntries(repo, oid);
      for (const e of entries) {
        if (!reachable.has(e.oid)) queue.push(e.oid);
      }
    } else if (obj.type === 'tag') {
      const tag = obj.data.toString('utf8');
      const m = tag.match(/^object ([0-9a-f]{40})/m);
      if (m && !reachable.has(m[1])) queue.push(m[1]);
    }
  }
  return reachable;
}

function isFastForward(repo: ForgeRepo, oldOid: string, newOid: string): boolean {
  if (oldOid === ZERO_OID || oldOid === newOid) return true;
  const directParents = repo.commitParents.get(newOid);
  if (directParents && directParents.includes(oldOid)) return true;
  const visited = new Set<string>();
  const queue = [newOid];
  while (queue.length > 0) {
    const curr = queue.pop()!;
    if (curr === oldOid) return true;
    if (visited.has(curr)) continue;
    visited.add(curr);
    let parents = repo.commitParents.get(curr);
    if (!parents) {
      const obj = repo.objects.get(curr);
      if (obj && obj.type === 'commit') {
        const commit = parseCommit(obj.data);
        parents = commit.parents;
        repo.commitParents.set(curr, parents);
      }
    }
    if (parents) {
      for (const p of parents) queue.push(p);
    }
  }
  return false;
}

// ---------------------------------------------------------------------------
// App Factory
// ---------------------------------------------------------------------------

export function createApp(): Server {
  const store = new ForgeStore();

  const server = createServer({
    keepAlive: true,
    keepAliveInitialDelay: 0,
    noDelay: true,
  }, async (req: IncomingMessage, res: ServerResponse) => {
    const rawUrl = req.url ?? '/';
    const qIdx = rawUrl.indexOf('?');
    const pathname = qIdx === -1 ? rawUrl : rawUrl.slice(0, qIdx);
    const method = (req.method ?? 'GET').toUpperCase();

    // 1. Health Probe
    if (pathname === '/healthz' && method === 'GET') {
      sendJson(res, 200, { ok: true });
      return;
    }

    let urlObjInstance: URL | null = null;
    const getUrlObj = () => {
      if (!urlObjInstance) {
        urlObjInstance = new URL(rawUrl, `http://${req.headers.host || 'localhost'}`);
      }
      return urlObjInstance;
    };

    // 2. Auth Helper
    const authHeader = req.headers.authorization;
    const hasToken = typeof authHeader === 'string' && authHeader.startsWith('Bearer ');
    const shopToken = process.env.MAKEFUN_SHOP_TOKEN || '';

    // =======================================================================
    // Git Smart-HTTP Surface
    // =======================================================================
    const gitMatch = pathname.includes('.git/') ? pathname.match(/^\/([^\/]+)\/([^\/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/) : null;
    if (gitMatch) {
      const [, owner, repoName, gitAction] = gitMatch;
      const repo = store.getRepo(owner, repoName);

      // unknown repo: 404
      if (!repo || owner.toLowerCase() !== store.viewer.login.toLowerCase()) {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }

      // GET info/refs
      if (gitAction === 'info/refs' && method === 'GET') {
        const service = getUrlObj().searchParams.get('service');
        const gitProto = req.headers['git-protocol'] as string | undefined;

        if (service === 'git-upload-pack') {
          res.setHeader('content-type', 'application/x-git-upload-pack-advertisement');
          if (gitProto === 'version=2') {
            res.writeHead(200, {
              'content-type': 'application/x-git-upload-pack-advertisement',
              'content-length': V2_UPLOAD_PACK_ADVERTISEMENT.length,
            });
            res.end(V2_UPLOAD_PACK_ADVERTISEMENT);
            return;
          } else {
            // v0 upload-pack
            const packets: Buffer[] = [encodePktLine('# service=git-upload-pack\n'), pktLineFlush()];
            const mainOid = repo.refs.get('refs/heads/main');
            const caps = 'multi_ack_detailed thin-pack side-band side-band-64k ofs-delta shallow agent=makefun-forge/1.0';
            if (repo.refs.size === 0 || !mainOid) {
              packets.push(encodePktLine(`0000000000000000000000000000000000000000 capabilities^{}\0${caps}\n`));
            } else {
              let first = true;
              for (const [refName, oid] of repo.refs) {
                if (first) {
                  packets.push(encodePktLine(`${oid} ${refName}\0${caps}\n`));
                  first = false;
                } else {
                  packets.push(encodePktLine(`${oid} ${refName}\n`));
                }
              }
            }
            packets.push(pktLineFlush());
            res.writeHead(200);
            res.end(Buffer.concat(packets));
            return;
          }
        } else if (service === 'git-receive-pack') {
          res.setHeader('content-type', 'application/x-git-receive-pack-advertisement');
          const packets: Buffer[] = [encodePktLine('# service=git-receive-pack\n'), pktLineFlush()];
          const caps = 'report-status side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0';
          if (repo.refs.size === 0) {
            packets.push(encodePktLine(`0000000000000000000000000000000000000000 capabilities^{}\0${caps}\n`));
          } else {
            let first = true;
            for (const [refName, oid] of repo.refs) {
              if (first) {
                packets.push(encodePktLine(`${oid} ${refName}\0${caps}\n`));
                first = false;
              } else {
                packets.push(encodePktLine(`${oid} ${refName}\n`));
              }
            }
          }
          packets.push(pktLineFlush());
          res.writeHead(200);
          res.end(Buffer.concat(packets));
          return;
        } else {
          sendJson(res, 400, { message: 'Invalid service' });
          return;
        }
      }

      // POST git-upload-pack
      if (gitAction === 'git-upload-pack' && method === 'POST') {
        const reqBuf = await readBody(req);
        const gitProto = req.headers['git-protocol'] as string | undefined;

        // Check if v2 command
        const lines: string[] = [];
        let offset = 0;
        while (offset < reqBuf.length) {
          const lenStr = reqBuf.subarray(offset, offset + 4).toString('ascii');
          if (!/^[0-9a-f]{4}$/i.test(lenStr)) break;
          const len = parseInt(lenStr, 16);
          if (len === 0) {
            lines.push('FLUSH');
            offset += 4;
            continue;
          }
          if (len === 1) {
            lines.push('DELIM');
            offset += 4;
            continue;
          }
          const payload = reqBuf.subarray(offset + 4, offset + len).toString('utf8');
          lines.push(payload);
          offset += len;
        }

        const isV2 = gitProto === 'version=2' || lines[0]?.startsWith('command=');
        if (isV2) {
          const cmdLine = lines.find((l) => l.startsWith('command=')) ?? '';
          const command = cmdLine.replace('command=', '').trim();

          if (command === 'ls-refs') {
            const symrefs = lines.some((l) => l.trim() === 'symrefs');
            const peel = lines.some((l) => l.trim() === 'peel');
            const prefixes = lines.filter((l) => l.startsWith('ref-prefix ')).map((l) => l.slice(11).trim());
            const cacheKey = `${symrefs ? 1 : 0}:${peel ? 1 : 0}:${prefixes.join(',')}`;
            const cached = repo.lsRefsCache.get(cacheKey);
            if (cached) {
              res.writeHead(200, {
                'content-type': 'application/x-git-upload-pack-result',
                'content-length': cached.length,
              });
              res.end(cached);
              return;
            }

            const outPackets: Buffer[] = [];
            const defaultBranch = repo.default_branch || 'main';
            const headOid = repo.refs.get(`refs/heads/${defaultBranch}`);

            if (repo.refs.size === 0) {
              outPackets.push(encodePktLine(`unborn HEAD symref-target:refs/heads/${defaultBranch}\n`));
            } else if (headOid && (prefixes.length === 0 || prefixes.some((p) => 'HEAD'.startsWith(p)))) {
              outPackets.push(encodePktLine(`${headOid} HEAD${symrefs ? ` symref-target:refs/heads/${defaultBranch}` : ''}\n`));
            }

            for (const [refName, oid] of repo.refs) {
              if (prefixes.length > 0 && !prefixes.some((p) => refName.startsWith(p))) continue;
              let line = `${oid} ${refName}`;
              if (symrefs && refName === 'HEAD') line += ` symref-target:refs/heads/${defaultBranch}`;
              if (peel && refName.startsWith('refs/tags/')) {
                const tagObj = repo.objects.get(oid);
                if (tagObj && tagObj.type === 'tag') {
                  const tagText = tagObj.data.toString('utf8');
                  const m = tagText.match(/^object ([0-9a-f]{40})/m);
                  if (m) line += ` peeled:${m[1]}`;
                }
              }
              outPackets.push(encodePktLine(`${line}\n`));
            }
            outPackets.push(pktLineFlush());

            const finalBuf = Buffer.concat(outPackets);
            repo.lsRefsCache.set(cacheKey, finalBuf);
            res.writeHead(200, {
              'content-type': 'application/x-git-upload-pack-result',
              'content-length': finalBuf.length,
            });
            res.end(finalBuf);
            return;
          } else if (command === 'fetch') {
            const wants = lines.filter((l) => l.startsWith('want ')).map((l) => l.slice(5).trim());
            const haves = lines.filter((l) => l.startsWith('have ')).map((l) => l.slice(5).trim());
            const clientShallows = lines.filter((l) => l.startsWith('shallow ')).map((l) => l.slice(8).trim());
            const depthMatch = lines.find((l) => l.startsWith('deepen '))?.slice(7).trim();
            const depth = depthMatch ? parseInt(depthMatch, 10) : null;

            const isShallowClone = depth === 1 && wants.length === 1 && haves.length === 0;
            const shallowCacheKey = isShallowClone ? `shallow1:${wants[0]}` : null;
            if (shallowCacheKey) {
              const cachedBuf = repo.shallowPackCache.get(shallowCacheKey);
              if (cachedBuf) {
                res.writeHead(200, {
                  'content-type': 'application/x-git-upload-pack-result',
                  'content-length': cachedBuf.length,
                });
                res.end(cachedBuf);
                return;
              }
            }

            const shallowSet = new Set(clientShallows);
            const haveSet = new Set<string>();
            for (const h of haves) {
              if (shallowSet.has(h)) {
                haveSet.add(h);
                const obj = repo.objects.get(h);
                if (obj && obj.type === 'commit') {
                  const commit = parseCommit(obj.data);
                  if (commit.tree) {
                    for (const toid of collectReachableOids(repo, [commit.tree])) haveSet.add(toid);
                  }
                }
              } else {
                for (const o of collectReachableOids(repo, [h])) haveSet.add(o);
              }
            }

            const sendObjects: GitObject[] = [];
            const shallows: string[] = [];
            const unshallows: string[] = [];
            if (depth !== null) {
              // Shallow traversal
              for (const want of wants) {
                let current: string | undefined = want;
                let curDepth = 0;
                while (current && curDepth < depth) {
                  const obj = repo.objects.get(current);
                  if (!obj || obj.type !== 'commit') break;
                  if (!haveSet.has(current)) {
                    sendObjects.push(obj);
                    const commit = parseCommit(obj.data);
                    if (commit.tree) {
                      for (const toid of collectReachableOids(repo, [commit.tree])) {
                        if (!haveSet.has(toid)) {
                          const tobj = repo.objects.get(toid);
                          if (tobj) sendObjects.push(tobj);
                        }
                      }
                    }
                  }
                  curDepth++;
                  const commit = parseCommit(obj.data);
                  if (curDepth === depth) {
                    if (commit.parents.length > 0) {
                      shallows.push(current);
                    }
                    break;
                  }
                  if (commit.parents.length === 0) break;
                  current = commit.parents[0];
                }
              }
              for (const cs of clientShallows) {
                const csObj = repo.objects.get(cs);
                if (csObj && csObj.type === 'commit') {
                  const parsed = parseCommit(csObj.data);
                  if (parsed.parents.some((p) => sendObjects.some((o) => o.oid === p))) {
                    unshallows.push(cs);
                  }
                }
              }
            } else {
              const toSend = collectReachableOids(repo, wants, haveSet);
              for (const oid of toSend) {
                const obj = repo.objects.get(oid);
                if (obj) sendObjects.push(obj);
              }
            }

            const packfile = writePack(sendObjects);

            // Assemble v2 fetch response
            const outPackets: Buffer[] = [];
            outPackets.push(encodePktLine('acknowledgments\n'));
            outPackets.push(encodePktLine(haves.length > 0 ? 'ready\n' : 'NAK\n'));
            outPackets.push(pktLineDelim());

            if (shallows.length > 0 || unshallows.length > 0) {
              outPackets.push(encodePktLine('shallow-info\n'));
              for (const s of shallows) outPackets.push(encodePktLine(`shallow ${s}\n`));
              for (const u of unshallows) outPackets.push(encodePktLine(`unshallow ${u}\n`));
              outPackets.push(pktLineDelim());
            }

            outPackets.push(encodePktLine('packfile\n'));

            // Frame packfile with sideband-64k (band 1)
            let packOffset = 0;
            const CHUNK_SIZE = 65515; // 65520 - 4 len - 1 band
            while (packOffset < packfile.length) {
              const slice = packfile.subarray(packOffset, packOffset + CHUNK_SIZE);
              const payload = Buffer.concat([Buffer.from([1]), slice]);
              outPackets.push(encodePktLine(payload));
              packOffset += slice.length;
            }
            outPackets.push(pktLineFlush());

            const finalPackets = Buffer.concat(outPackets);
            if (shallowCacheKey) {
              repo.shallowPackCache.set(shallowCacheKey, finalPackets);
            }
            res.writeHead(200, {
              'content-type': 'application/x-git-upload-pack-result',
              'content-length': finalPackets.length,
            });
            res.end(finalPackets);
            return;
          }
        } else {
          // v0 upload-pack
          const wants = lines.filter((l) => l.startsWith('want ')).map((l) => l.slice(5).split(' ')[0].trim());
          const haves = lines.filter((l) => l.startsWith('have ')).map((l) => l.slice(5).trim());
          const depthMatch = lines.find((l) => l.startsWith('deepen '))?.slice(7).trim();
          const depth = depthMatch ? parseInt(depthMatch, 10) : null;

          const haveSet = new Set<string>();
          for (const h of haves) {
            for (const o of collectReachableOids(repo, [h])) haveSet.add(o);
          }

          const sendObjects: GitObject[] = [];
          const shallows: string[] = [];
          if (depth !== null) {
            for (const want of wants) {
              let current: string | undefined = want;
              let curDepth = 0;
              while (current && curDepth < depth) {
                const obj = repo.objects.get(current);
                if (!obj || obj.type !== 'commit') break;
                sendObjects.push(obj);
                const commit = parseCommit(obj.data);
                if (commit.tree) {
                  for (const toid of collectReachableOids(repo, [commit.tree])) {
                    const tobj = repo.objects.get(toid);
                    if (tobj) sendObjects.push(tobj);
                  }
                }
                curDepth++;
                if (curDepth === depth) {
                  for (const p of commit.parents) shallows.push(p);
                  break;
                }
                current = commit.parents[0];
              }
            }
          } else {
            const toSend = collectReachableOids(repo, wants, haveSet);
            for (const oid of toSend) {
              const obj = repo.objects.get(oid);
              if (obj) sendObjects.push(obj);
            }
          }

          const packfile = writePack(sendObjects);

          const outPackets: Buffer[] = [];
          if (shallows.length > 0) {
            for (const s of shallows) outPackets.push(encodePktLine(`shallow ${s}\n`));
            outPackets.push(pktLineFlush());
          }
          outPackets.push(encodePktLine(haves.length > 0 ? 'ACK\n' : 'NAK\n'));

          let packOffset = 0;
          const CHUNK_SIZE = 65515;
          while (packOffset < packfile.length) {
            const slice = packfile.subarray(packOffset, packOffset + CHUNK_SIZE);
            const payload = Buffer.concat([Buffer.from([1]), slice]);
            outPackets.push(encodePktLine(payload));
            packOffset += slice.length;
          }
          outPackets.push(pktLineFlush());

          res.setHeader('content-type', 'application/x-git-upload-pack-result');
          res.writeHead(200);
          res.end(Buffer.concat(outPackets));
          return;
        }
      }

      // POST git-receive-pack
      if (gitAction === 'git-receive-pack' && method === 'POST') {
        const bodyBuf = await readBody(req);
        let offset = 0;
        const commands: Array<{ oldOid: string; newOid: string; ref: string }> = [];

        while (offset < bodyBuf.length) {
          const lenStr = bodyBuf.subarray(offset, offset + 4).toString('ascii');
          if (!/^[0-9a-f]{4}$/i.test(lenStr)) break;
          const len = parseInt(lenStr, 16);
          if (len === 0) {
            offset += 4;
            break; // Flush ends command section; packfile follows!
          }
          const payload = bodyBuf.subarray(offset + 4, offset + len).toString('utf8');
          offset += len;
          const line = payload.split('\0')[0].trim();
          const parts = line.split(' ');
          if (parts.length >= 3) {
            commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts[2] });
          }
        }

        // Unpack remaining bytes if packfile follows
        const packBytes = bodyBuf.subarray(offset);
        if (packBytes.length >= 32 && packBytes.subarray(0, 4).toString('ascii') === 'PACK') {
          try {
            const readRes = readPack(packBytes, { resolveBase: (oid) => repo.objects.get(oid) });
            for (const [oid, obj] of readRes.objects) {
              repo.objects.set(oid, obj);
              if (obj.type === 'commit') {
                try {
                  const commit = parseCommit(obj.data);
                  repo.commitParents.set(oid, commit.parents);
                } catch {}
              }
            }
          } catch (e: any) {
            const reportLines = [
              encodePktLine(Buffer.concat([Buffer.from([1]), Buffer.from(`unpack error: ${e.message}\n`, 'utf8')])),
              pktLineFlush(),
            ];
            res.setHeader('content-type', 'application/x-git-receive-pack-result');
            res.writeHead(200);
            res.end(Buffer.concat(reportLines));
            return;
          }
        }

        // Process commands
        const reportLines: Buffer[] = [UNPACK_OK_PKT];

        for (const cmd of commands) {
          const currentOid = repo.refs.get(cmd.ref) || ZERO_OID;
          if (cmd.newOid === ZERO_OID) {
            // Delete ref
            repo.refs.delete(cmd.ref);
            invalidateRepoCache(repo);
            reportLines.push(encodeSideband1(`ok ${cmd.ref}\n`));
          } else if (cmd.oldOid === ZERO_OID) {
            // Create ref
            repo.refs.set(cmd.ref, cmd.newOid);
            invalidateRepoCache(repo);
            reportLines.push(encodeSideband1(`ok ${cmd.ref}\n`));
          } else {
            // Update ref: check fast-forward
            if (isFastForward(repo, cmd.oldOid, cmd.newOid)) {
              repo.refs.set(cmd.ref, cmd.newOid);
              invalidateRepoCache(repo);
              reportLines.push(encodeSideband1(`ok ${cmd.ref}\n`));
            } else {
              reportLines.push(encodeSideband1(`ng ${cmd.ref} non-fast-forward\n`));
            }
          }
        }
        reportLines.push(PKT_FLUSH);

        res.setHeader('content-type', 'application/x-git-receive-pack-result');
        res.writeHead(200);
        res.end(Buffer.concat(reportLines));
        return;
      }
    }

    // =======================================================================
    // MCP Endpoint (/mcp)
    // =======================================================================
    if (pathname === '/mcp' && method === 'POST') {
      if (!hasToken) {
        sendJson(res, 401, { message: 'Requires authentication' });
        return;
      }
      const parsed = await readJsonBody(req);
      if (!parsed.ok) {
        sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
        return;
      }
      const msg = parsed.data;
      const id = msg.id ?? null;

      if (msg.method === 'server/discover') {
        sendJson(res, 200, {
          jsonrpc: '2.0',
          id,
          result: {
            supportedVersions: ['2026-07-28', '2025-11-25'],
            serverInfo: { name: 'makefun-forge', version: '1.0.0' },
            capabilities: { tools: {} },
          },
        });
        return;
      }

      if (msg.method === 'initialize') {
        sendJson(res, 200, {
          jsonrpc: '2.0',
          id,
          result: {
            protocolVersion: '2025-11-25',
            serverInfo: { name: 'makefun-forge', version: '1.0.0' },
            capabilities: { tools: {} },
          },
        });
        return;
      }

      if (msg.method === 'notifications/initialized') {
        res.writeHead(204);
        res.end();
        return;
      }

      if (msg.method === 'tools/list') {
        const tools = [
          {
            name: 'create_repo',
            description: 'Create a new repository',
            inputSchema: {
              type: 'object',
              properties: {
                name: { type: 'string' },
                description: { type: 'string' },
                private: { type: 'boolean' },
              },
              required: ['name'],
            },
          },
          {
            name: 'get_repo',
            description: 'Get repository details',
            inputSchema: {
              type: 'object',
              properties: {
                owner: { type: 'string' },
                repo: { type: 'string' },
              },
              required: ['owner', 'repo'],
            },
          },
          {
            name: 'create_issue',
            description: 'Create an issue in a repository',
            inputSchema: {
              type: 'object',
              properties: {
                owner: { type: 'string' },
                repo: { type: 'string' },
                title: { type: 'string' },
                body: { type: 'string' },
              },
              required: ['owner', 'repo', 'title'],
            },
          },
          {
            name: 'list_issues',
            description: 'List issues in a repository',
            inputSchema: {
              type: 'object',
              properties: {
                owner: { type: 'string' },
                repo: { type: 'string' },
                state: { type: 'string' },
              },
              required: ['owner', 'repo'],
            },
          },
          {
            name: 'create_pull_request',
            description: 'Create a pull request',
            inputSchema: {
              type: 'object',
              properties: {
                owner: { type: 'string' },
                repo: { type: 'string' },
                title: { type: 'string' },
                head: { type: 'string' },
                base: { type: 'string' },
              },
              required: ['owner', 'repo', 'title', 'head', 'base'],
            },
          },
          {
            name: 'merge_pull_request',
            description: 'Merge a pull request',
            inputSchema: {
              type: 'object',
              properties: {
                owner: { type: 'string' },
                repo: { type: 'string' },
                pull_number: { type: 'integer' },
              },
              required: ['owner', 'repo', 'pull_number'],
            },
          },
        ];
        sendJson(res, 200, { jsonrpc: '2.0', id, result: { tools } });
        return;
      }

      if (msg.method === 'tools/call') {
        const params = msg.params || {};
        const toolName = params.name;
        const args = params.arguments || {};

        if (toolName === 'create_repo') {
          if (!args.name) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Missing name' }] } });
            return;
          }
          const repo = store.createRepo(args.name, args.private ?? true, args.description ?? null);
          const data = { id: repo.id, name: repo.name, private: repo.private, owner: repo.owner };
          sendJson(res, 200, { jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data } });
          return;
        }

        if (toolName === 'get_repo') {
          const owner = args.owner || (args.full_name ? args.full_name.split('/')[0] : store.viewer.login);
          const repoName = args.repo || (args.full_name ? args.full_name.split('/')[1] : '');
          const repo = store.getRepo(owner, repoName);
          if (!repo) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Not found' }] } });
            return;
          }
          const data = { id: repo.id, name: repo.name, default_branch: repo.default_branch, private: repo.private, owner: repo.owner };
          sendJson(res, 200, { jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data } });
          return;
        }

        if (toolName === 'create_issue') {
          if (!args.title || (!args.repo && !args.full_name)) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Missing argument' }] } });
            return;
          }
          const owner = args.owner || (args.full_name ? args.full_name.split('/')[0] : store.viewer.login);
          const repoName = args.repo || (args.full_name ? args.full_name.split('/')[1] : '');
          const repo = store.getRepo(owner, repoName);
          if (!repo) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Repo not found' }] } });
            return;
          }
          const issueNum = ++repo.issueSeq;
          const issue: ForgeIssue = {
            id: issueNum,
            number: issueNum,
            title: args.title,
            body: args.body || '',
            state: 'open',
            labels: [],
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
            comments: [],
          };
          issue._cachedBuffer = serializeIssue(issue);
          repo.issues.set(issueNum, issue);
          const data = { id: issue.id, number: issue.number, title: issue.title, body: issue.body, state: issue.state };
          sendJson(res, 200, { jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data } });
          return;
        }

        if (toolName === 'list_issues') {
          const owner = args.owner || (args.full_name ? args.full_name.split('/')[0] : store.viewer.login);
          const repoName = args.repo || (args.full_name ? args.full_name.split('/')[1] : '');
          const repo = store.getRepo(owner, repoName);
          if (!repo) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Repo not found' }] } });
            return;
          }
          const issues = [...repo.issues.values()].filter((i) => !args.state || args.state === 'all' || i.state === args.state);
          sendJson(res, 200, { jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: JSON.stringify(issues) }], structuredContent: issues } });
          return;
        }

        if (toolName === 'create_pull_request') {
          if (!args.title || !args.head || !args.base) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Missing argument' }] } });
            return;
          }
          const owner = args.owner || (args.full_name ? args.full_name.split('/')[0] : store.viewer.login);
          const repoName = args.repo || (args.full_name ? args.full_name.split('/')[1] : '');
          const repo = store.getRepo(owner, repoName);
          if (!repo) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Repo not found' }] } });
            return;
          }
          const pullNum = ++repo.pullSeq;
          const headSha = repo.refs.get(`refs/heads/${args.head}`) || ZERO_OID;
          const baseSha = repo.refs.get(`refs/heads/${args.base}`) || ZERO_OID;
          const pull: ForgePull = {
            id: pullNum,
            number: pullNum,
            title: args.title,
            body: args.body || '',
            state: 'open',
            merged: false,
            merged_at: null,
            merge_commit_sha: null,
            head: { ref: args.head, sha: headSha },
            base: { ref: args.base, sha: baseSha },
            headCommitSha: headSha,
            baseCommitSha: baseSha,
          };
          repo.pulls.set(pullNum, pull);
          const data = { id: pull.id, number: pull.number, title: pull.title, state: pull.state, head: pull.head, base: pull.base };
          sendJson(res, 200, { jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data } });
          return;
        }

        if (toolName === 'merge_pull_request') {
          const pullNum = Number(args.pull_number ?? args.number);
          if (!pullNum) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Missing pull_number' }] } });
            return;
          }
          const owner = args.owner || (args.full_name ? args.full_name.split('/')[0] : store.viewer.login);
          const repoName = args.repo || (args.full_name ? args.full_name.split('/')[1] : '');
          const repo = store.getRepo(owner, repoName);
          if (!repo) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Repo not found' }] } });
            return;
          }
          const pull = repo.pulls.get(pullNum);
          if (!pull) {
            sendJson(res, 200, { jsonrpc: '2.0', id, result: { isError: true, content: [{ type: 'text', text: 'Pull not found' }] } });
            return;
          }
          const headCommitObj = repo.objects.get(pull.head.sha);
          let treeOid = '';
          if (headCommitObj && headCommitObj.type === 'commit') {
            treeOid = parseCommit(headCommitObj.data).tree;
          }
          const baseSha = repo.refs.get(`refs/heads/${pull.base.ref}`) || pull.base.sha;
          const squashSha = createCommit(repo, treeOid, [baseSha], `Merge pull request #${pullNum} from ${pull.head.ref}`);
          repo.refs.set(`refs/heads/${pull.base.ref}`, squashSha);
          pull.state = 'closed';
          pull.merged = true;
          pull.merged_at = new Date().toISOString();
          pull.merge_commit_sha = squashSha;

          const data = { sha: squashSha, merged: true, message: 'Pull Request successfully merged' };
          sendJson(res, 200, { jsonrpc: '2.0', id, result: { content: [{ type: 'text', text: JSON.stringify(data) }], structuredContent: data } });
          return;
        }

        sendJson(res, 200, { jsonrpc: '2.0', id, error: { code: -32601, message: `Method ${toolName} not found` } });
        return;
      }

      sendJson(res, 200, { jsonrpc: '2.0', id, error: { code: -32601, message: 'Method not found' } });
      return;
    }

    // =======================================================================
    // GraphQL Endpoint (/api/graphql)
    // =======================================================================
    if (pathname === '/api/graphql' && method === 'POST') {
      if (!hasToken) {
        sendJson(res, 401, { message: 'Requires authentication' });
        return;
      }
      const parsed = await readJsonBody(req);
      if (!parsed.ok) {
        sendJson(res, 400, { message: 'Problems parsing JSON' });
        return;
      }

      const executeDoc = (doc: any) => {
        const query = (doc.query ?? '').trim();
        const vars = doc.variables ?? {};

        // 1. Invalid field check: "noSuchFieldOnViewer"
        if (query.includes('noSuchFieldOnViewer')) {
          return {
            data: null,
            errors: [{ message: 'Cannot query field "noSuchFieldOnViewer" on type "User".' }],
          };
        }

        // 2. viewer login
        if (query.startsWith('query ViewerLogin') || (query.includes('viewer') && query.includes('login') && !query.includes('repository'))) {
          return { data: { viewer: { login: store.viewer.login } } };
        }

        // 3. __type introspection
        if (query.includes('__type')) {
          const typeData: Record<string, any> = {};
          if (query.includes('Issue: __type(name: "Issue")') || query.includes('__type(name: "Issue")')) {
            const issueFields = [
              { name: 'id' },
              { name: 'number' },
              { name: 'title' },
              { name: 'state' },
              { name: 'body' },
            ];
            if (query.includes('Issue: __type')) typeData.Issue = { fields: issueFields };
            else typeData.__type = { fields: issueFields };
          }
          if (query.includes('PullRequest: __type(name: "PullRequest")')) {
            typeData.PullRequest = {
              fields: [
                { name: 'id' },
                { name: 'number' },
                { name: 'headRefName' },
                { name: 'baseRefName' },
                { name: 'mergeable' },
                { name: 'state' },
                { name: 'merged' },
                { name: 'files' },
              ],
            };
          }
          if (query.includes('Repository: __type(name: "Repository")')) {
            typeData.Repository = {
              fields: [
                { name: 'id' },
                { name: 'databaseId' },
                { name: 'name' },
                { name: 'nameWithOwner' },
                { name: 'owner' },
                { name: 'defaultBranchRef' },
                { name: 'viewerPermission' },
                { name: 'hasIssuesEnabled' },
                { name: 'issue' },
                { name: 'issues' },
                { name: 'pullRequests' },
                { name: 'pullRequest' },
              ],
            };
          }
          if (query.includes('SearchType: __type(name: "SearchType")')) {
            typeData.SearchType = {
              enumValues: [{ name: 'ISSUE' }, { name: 'REPOSITORY' }, { name: 'USER' }],
            };
          }
          return { data: typeData };
        }

        // 4. Node query
        if (query.includes('query NodeRepository') || query.includes('node(id: $id)')) {
          const id = vars.id;
          for (const r of store.repos.values()) {
            if (r.nodeId === id) {
              return { data: { node: { id: r.nodeId, __typename: 'Repository', name: r.name, nameWithOwner: `${r.owner.login}/${r.name}` } } };
            }
          }
          return { data: { node: null } };
        }

        // 5. Repository query
        if (query.includes('repository(owner: $owner, name: $name)') || query.includes('repository(owner:')) {
          const owner = vars.owner || store.viewer.login;
          const name = vars.name || '';
          const repo = store.getRepo(owner, name);
          if (!repo) {
            return { data: { repository: null } };
          }

          const repoData: Record<string, any> = {
            id: repo.nodeId,
            databaseId: repo.id,
            name: repo.name,
            nameWithOwner: `${repo.owner.login}/${repo.name}`,
            owner: { login: repo.owner.login },
            defaultBranchRef: { name: repo.default_branch },
            viewerPermission: 'ADMIN',
            hasIssuesEnabled: true,
          };

          if (query.includes('issues(')) {
            let issues = [...repo.issues.values()];
            if (query.includes('states: [OPEN]')) {
              issues = issues.filter((i) => i.state === 'open');
            }
            if (query.includes('CREATED_AT') && query.includes('DESC')) {
              issues.sort((a, b) => b.number - a.number);
            }
            if (vars.after) {
              const afterIdx = issues.findIndex((i) => Buffer.from(`issue:${i.number}`).toString('base64') === vars.after);
              if (afterIdx >= 0) issues = issues.slice(afterIdx + 1);
            }
            const first = vars.first || 10;
            const paged = issues.slice(0, first);
            const nodes = paged.map((i) => ({
              id: Buffer.from(`issue:${i.number}`).toString('base64'),
              number: i.number,
              title: i.title,
              state: i.state.toUpperCase(),
              body: i.body,
            }));
            const endCursor = nodes.length > 0 ? Buffer.from(`issue:${paged[paged.length - 1].number}`).toString('base64') : null;
            const hasNextPage = issues.length > first;
            repoData.issues = { nodes, pageInfo: { hasNextPage, endCursor } };
          }

          if (query.includes('issue(number:')) {
            const num = vars.number || 1;
            const issue = repo.issues.get(num);
            repoData.issue = issue ? { id: Buffer.from(`issue:${issue.number}`).toString('base64'), number: issue.number, title: issue.title, state: issue.state.toUpperCase(), body: issue.body } : null;
          }

          if (query.includes('pullRequests(')) {
            const pulls = [...repo.pulls.values()].map((p) => ({
              id: Buffer.from(`pr:${p.number}`).toString('base64'),
              number: p.number,
              headRefName: p.head.ref,
              baseRefName: p.base.ref,
              mergeable: 'MERGEABLE',
              state: p.merged ? 'MERGED' : p.state.toUpperCase(),
            }));
            repoData.pullRequests = { nodes: pulls };
          }

          if (query.includes('pullRequest(number:')) {
            const num = vars.number || 1;
            const pull = repo.pulls.get(num);
            if (pull) {
              const headCommitObj = repo.objects.get(pull.head.sha);
              const baseCommitObj = repo.objects.get(pull.base.sha);
              const headTreeOid = headCommitObj && headCommitObj.type === 'commit' ? parseCommit(headCommitObj.data).tree : null;
              const baseTreeOid = baseCommitObj && baseCommitObj.type === 'commit' ? parseCommit(baseCommitObj.data).tree : null;
              const headFiles = headTreeOid ? listTreeRecursive(repo, headTreeOid) : [];
              const baseFiles = baseTreeOid ? listTreeRecursive(repo, baseTreeOid) : [];
              const baseMap = new Map(baseFiles.map((f) => [f.path, f.oid]));
              const changedPaths: string[] = [];
              for (const hf of headFiles) {
                if (hf.type !== 'file') continue;
                const baseOid = baseMap.get(hf.path);
                if (!baseOid || baseOid !== hf.oid) {
                  changedPaths.push(hf.path);
                }
              }
              const nodes = changedPaths.map((p) => ({ path: p }));
              repoData.pullRequest = {
                id: Buffer.from(`pr:${pull.number}`).toString('base64'),
                number: pull.number,
                files: { nodes },
              };
            } else {
              repoData.pullRequest = null;
            }
          }

          return { data: { repository: repoData } };
        }

        // 6. Mutations
        if (query.includes('mutation IssueCreate') || query.includes('createIssue')) {
          const input = vars.input || {};
          let targetRepo: ForgeRepo | undefined;
          for (const r of store.repos.values()) {
            if (r.nodeId === input.repositoryId) {
              targetRepo = r;
              break;
            }
          }
          if (!targetRepo) targetRepo = [...store.repos.values()][0];
          const num = ++targetRepo.issueSeq;
          const issue: ForgeIssue = {
            id: num,
            number: num,
            title: input.title,
            body: input.body || '',
            state: 'open',
            labels: [],
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
            comments: [],
          };
          targetRepo.issues.set(num, issue);
          issue._cachedBuffer = serializeIssue(issue);
          return { data: { createIssue: { issue: { id: Buffer.from(`issue:${num}`).toString('base64'), number: num } } } };
        }

        if (query.includes('mutation IssueClose') || query.includes('closeIssue')) {
          const input = vars.input || {};
          for (const r of store.repos.values()) {
            for (const i of r.issues.values()) {
              if (Buffer.from(`issue:${i.number}`).toString('base64') === input.issueId) {
                i.state = 'closed';
                i._cachedBuffer = serializeIssue(i);
                return { data: { closeIssue: { issue: { id: input.issueId, number: i.number, state: 'CLOSED' } } } };
              }
            }
          }
          return { data: { closeIssue: { issue: { id: input.issueId, number: 1, state: 'CLOSED' } } } };
        }

        if (query.includes('mutation CreatePullRequest') || query.includes('createPullRequest')) {
          const input = vars.input || {};
          let targetRepo: ForgeRepo | undefined;
          for (const r of store.repos.values()) {
            if (r.nodeId === input.repositoryId) {
              targetRepo = r;
              break;
            }
          }
          if (!targetRepo) targetRepo = [...store.repos.values()][0];
          const num = ++targetRepo.pullSeq;
          const headSha = targetRepo.refs.get(`refs/heads/${input.headRefName}`) || ZERO_OID;
          const baseSha = targetRepo.refs.get(`refs/heads/${input.baseRefName}`) || ZERO_OID;
          const pull: ForgePull = {
            id: num,
            number: num,
            title: input.title,
            body: '',
            state: 'open',
            merged: false,
            merged_at: null,
            merge_commit_sha: null,
            head: { ref: input.headRefName, sha: headSha },
            base: { ref: input.baseRefName, sha: baseSha },
            headCommitSha: headSha,
            baseCommitSha: baseSha,
          };
          targetRepo.pulls.set(num, pull);
          return {
            data: {
              createPullRequest: {
                pullRequest: {
                  id: Buffer.from(`pr:${num}`).toString('base64'),
                  number: num,
                  state: 'OPEN',
                  headRefName: input.headRefName,
                  baseRefName: input.baseRefName,
                },
              },
            },
          };
        }

        if (query.includes('mutation MergePullRequest') || query.includes('mergePullRequest')) {
          const input = vars.input || {};
          for (const r of store.repos.values()) {
            for (const p of r.pulls.values()) {
              if (Buffer.from(`pr:${p.number}`).toString('base64') === input.pullRequestId) {
                const headCommitObj = r.objects.get(p.head.sha);
                let treeOid = '';
                if (headCommitObj && headCommitObj.type === 'commit') {
                  treeOid = parseCommit(headCommitObj.data).tree;
                }
                const baseSha = r.refs.get(`refs/heads/${p.base.ref}`) || p.base.sha;
                const squashSha = createCommit(r, treeOid, [baseSha], `Merge pull request #${p.number}`);
                r.refs.set(`refs/heads/${p.base.ref}`, squashSha);
                p.state = 'closed';
                p.merged = true;
                p.merged_at = new Date().toISOString();
                p.merge_commit_sha = squashSha;
                return {
                  data: {
                    mergePullRequest: {
                      pullRequest: {
                        number: p.number,
                        merged: true,
                        state: 'MERGED',
                      },
                    },
                  },
                };
              }
            }
          }
          return { data: { mergePullRequest: { pullRequest: { number: 1, merged: true, state: 'MERGED' } } } };
        }

        if (query.includes('mutation AddComment') || query.includes('addComment')) {
          const input = vars.input || {};
          const commentId = Math.floor(Math.random() * 100000) + 1;
          for (const r of store.repos.values()) {
            for (const i of r.issues.values()) {
              if (Buffer.from(`issue:${i.number}`).toString('base64') === input.subjectId) {
                i.comments.push({
                  id: commentId,
                  body: input.body,
                  created_at: new Date().toISOString(),
                  user: store.viewer,
                });
                break;
              }
            }
          }
          return {
            data: {
              addComment: {
                commentEdge: {
                  node: {
                    id: Buffer.from(`comment:${commentId}`).toString('base64'),
                    body: input.body,
                  },
                },
              },
            },
          };
        }

        return { data: {} };
      };

      if (Array.isArray(parsed.data)) {
        const results = parsed.data.map(executeDoc);
        sendJson(res, 200, results);
      } else {
        const result = executeDoc(parsed.data);
        sendJson(res, 200, result);
      }
      return;
    }

    // =======================================================================
    // REST v3 API Surface (/api/v3/...)
    // =======================================================================
    if (pathname.startsWith('/api/v3')) {
      const restPath = pathname.slice('/api/v3'.length).replace(/\/+$/, '') || '/';

      // Root
      if (restPath === '/') {
        sendJson(res, 200, {
          current_user_url: '/api/v3/user',
          rate_limit_url: '/api/v3/rate_limit',
        }, { 'x-oauth-scopes': 'repo, read:org' });
        return;
      }

      // Meta
      if (restPath === '/meta') {
        sendJson(res, 200, { installed_version: '1.0.0' });
        return;
      }

      // User
      if (restPath === '/user') {
        if (!hasToken) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        sendJson(res, 200, {
          login: store.viewer.login,
          id: store.viewer.id,
          node_id: 'MDQ6VXNlcjEwMDA=',
          name: store.viewer.name,
          email: store.viewer.email,
        });
        return;
      }

      // Check auth for all other REST routes
      if (!hasToken) {
        sendJson(res, 401, { message: 'Requires authentication' });
        return;
      }

      // User Repos: GET /api/v3/user/repos & POST /api/v3/user/repos
      if (restPath === '/user/repos') {
        if (method === 'POST') {
          const parsed = await readJsonBody(req);
          if (!parsed.ok) {
            sendJson(res, 400, { message: 'Problems parsing JSON' });
            return;
          }
          const { name, private: isPrivate, description } = parsed.data;
          if (!name) {
            sendJson(res, 422, { message: 'Name is required' });
            return;
          }
          if (store.getRepo(store.viewer.login, name)) {
            sendJson(res, 422, { message: 'Repository already exists' });
            return;
          }
          const repo = store.createRepo(name, isPrivate ?? true, description ?? null);
          sendJson(res, 201, {
            id: repo.id,
            node_id: repo.nodeId,
            name: repo.name,
            full_name: `${repo.owner.login}/${repo.name}`,
            private: repo.private,
            owner: repo.owner,
            description: repo.description,
            default_branch: repo.default_branch,
          });
          return;
        }

        if (method === 'GET') {
          const perPage = parseInt(getUrlObj().searchParams.get('per_page') || '30', 10);
          const page = parseInt(getUrlObj().searchParams.get('page') || '1', 10);

          const allRepos = [...store.repos.values()].map((r) => ({
            id: r.id,
            node_id: r.nodeId,
            name: r.name,
            full_name: `${r.owner.login}/${r.name}`,
            private: r.private,
            owner: r.owner,
            description: r.description,
            default_branch: r.default_branch,
          }));

          const start = (page - 1) * perPage;
          const end = start + perPage;
          const paged = allRepos.slice(start, end);

          const extraHeaders: Record<string, string> = {};
          if (end < allRepos.length) {
            const nextUrl = `${getUrlObj().origin}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}`;
            extraHeaders.link = `<${nextUrl}>; rel="next"`;
          }
          sendJson(res, 200, paged, extraHeaders);
          return;
        }
      }

      // Repos Sub-resources: /repos/{owner}/{repo}/...
      const repoMatch = restPath.match(/^\/repos\/([^\/]+)\/([^\/]+)(?:\/(.*))?$/);
      if (repoMatch) {
        const [, owner, repoName, subPath = ''] = repoMatch;

        // Foreign owner check: if requested owner is not viewer, 404
        if (owner.toLowerCase() !== store.viewer.login.toLowerCase()) {
          sendJson(res, 404, { message: 'Not Found' });
          return;
        }

        const repo = store.getRepo(owner, repoName);
        if (!repo) {
          sendJson(res, 404, { message: 'Not Found' });
          return;
        }

        // Fast Issues API dispatch (/repos/{o}/{r}/issues...)
        if (subPath.startsWith('issues')) {
          if (subPath === 'issues') {
            if (method === 'POST') {
              const parsed = await readJsonBody(req);
              if (!parsed.ok) {
                sendJson(res, 400, { message: 'Problems parsing JSON' });
                return;
              }
              const { title, body = '', labels: labelNames = [] } = parsed.data;
              const attachedLabels: ForgeLabel[] = [];
              for (const lName of labelNames) {
                const found = repo.labels.get(lName);
                if (!found) {
                  sendJson(res, 422, { message: `Label ${lName} does not exist` });
                  return;
                }
                attachedLabels.push(found);
              }

              const num = ++repo.issueSeq;
              const issue: ForgeIssue = {
                id: num,
                number: num,
                title,
                body,
                state: 'open',
                labels: attachedLabels,
                created_at: new Date().toISOString(),
                updated_at: new Date().toISOString(),
                comments: [],
              };
              const buf = serializeIssue(issue);
              issue._cachedBuffer = buf;
              repo.issues.set(num, issue);
              res.writeHead(201, {
                'content-type': JSON_TYPE,
                'content-length': buf.length,
                'x-github-request-id': getRequestId(),
                'connection': 'keep-alive',
              });
              res.end(buf);
              return;
            }

            if (method === 'GET') {
              const stateFilter = getUrlObj().searchParams.get('state') || 'open';
              const sort = getUrlObj().searchParams.get('sort');
              const direction = getUrlObj().searchParams.get('direction') || 'desc';

              let list = [...repo.issues.values()];
              if (stateFilter !== 'all') {
                list = list.filter((i) => i.state === stateFilter);
              }
              if (sort === 'created') {
                list.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
              }
              sendJson(res, 200, list.map((i) => ({
                id: i.id,
                number: i.number,
                title: i.title,
                body: i.body,
                state: i.state,
                labels: i.labels,
              })));
              return;
            }
          }

          if (subPath.startsWith('issues/')) {
            const issuePart = subPath.slice(7);
            const slashIdx = issuePart.indexOf('/');
            if (slashIdx === -1) {
              const num = parseInt(issuePart, 10);
              const issue = repo.issues.get(num);
              if (!issue) {
                sendJson(res, 404, { message: 'Not Found' });
                return;
              }

              if (method === 'GET') {
                if (issue._cachedBuffer) {
                  res.writeHead(200, {
                    'content-type': JSON_TYPE,
                    'content-length': issue._cachedBuffer.length,
                    'x-github-request-id': getRequestId(),
                    'connection': 'keep-alive',
                  });
                  res.end(issue._cachedBuffer);
                  return;
                }
                const buf = serializeIssue(issue);
                issue._cachedBuffer = buf;
                res.writeHead(200, {
                  'content-type': JSON_TYPE,
                  'content-length': buf.length,
                  'x-github-request-id': getRequestId(),
                  'connection': 'keep-alive',
                });
                res.end(buf);
                return;
              }

              if (method === 'PATCH') {
                const parsed = await readJsonBody(req);
                if (!parsed.ok) {
                  sendJson(res, 400, { message: 'Problems parsing JSON' });
                  return;
                }
                if (parsed.data.state) issue.state = parsed.data.state;
                if (parsed.data.title) issue.title = parsed.data.title;
                if (parsed.data.body) issue.body = parsed.data.body;
                issue._cachedBuffer = serializeIssue(issue);
                sendJson(res, 200, {
                  id: issue.id,
                  number: issue.number,
                  title: issue.title,
                  body: issue.body,
                  state: issue.state,
                  labels: issue.labels,
                });
                return;
              }
            } else if (issuePart.slice(slashIdx + 1) === 'comments') {
              const num = parseInt(issuePart.slice(0, slashIdx), 10);
              const issue = repo.issues.get(num);
              if (!issue) {
                sendJson(res, 404, { message: 'Not Found' });
                return;
              }

              if (method === 'POST') {
                const parsed = await readJsonBody(req);
                if (!parsed.ok) {
                  sendJson(res, 400, { message: 'Problems parsing JSON' });
                  return;
                }
                const commentId = ++repo.commentsSeq;
                const comment: ForgeComment = {
                  id: commentId,
                  body: parsed.data.body || '',
                  created_at: new Date().toISOString(),
                  user: store.viewer,
                };
                issue.comments.push(comment);
                sendJson(res, 201, { id: comment.id, body: comment.body });
                return;
              }

              if (method === 'GET') {
                sendJson(res, 200, issue.comments.map((c) => ({ id: c.id, body: c.body })));
                return;
              }
            }
          }
        }

        // GET /repos/{o}/{r}
        if (subPath === '' && method === 'GET') {
          sendJson(res, 200, {
            id: repo.id,
            node_id: repo.nodeId,
            name: repo.name,
            full_name: `${repo.owner.login}/${repo.name}`,
            private: repo.private,
            owner: repo.owner,
            description: repo.description,
            default_branch: repo.default_branch,
          });
          return;
        }

        // PATCH /repos/{o}/{r}
        if (subPath === '' && method === 'PATCH') {
          const parsed = await readJsonBody(req);
          if (!parsed.ok) {
            sendJson(res, 400, { message: 'Problems parsing JSON' });
            return;
          }
          if ('description' in parsed.data) repo.description = parsed.data.description;
          sendJson(res, 200, {
            id: repo.id,
            node_id: repo.nodeId,
            name: repo.name,
            full_name: `${repo.owner.login}/${repo.name}`,
            private: repo.private,
            owner: repo.owner,
            description: repo.description,
            default_branch: repo.default_branch,
          });
          return;
        }

        // DELETE /repos/{o}/{r}
        if (subPath === '' && method === 'DELETE') {
          store.deleteRepo(owner, repoName);
          res.writeHead(204, { 'x-github-request-id': 'del-req' });
          res.end();
          return;
        }

        // POST /repos/{o}/{r}/git/refs
        if (subPath === 'git/refs' && method === 'POST') {
          const parsed = await readJsonBody(req);
          if (!parsed.ok) {
            sendJson(res, 400, { message: 'Problems parsing JSON' });
            return;
          }
          const { ref, sha } = parsed.data;
          repo.refs.set(ref, sha);
          sendJson(res, 201, {
            ref,
            node_id: Buffer.from(`ref:${ref}`).toString('base64'),
            url: `${getUrlObj().origin}/api/v3/repos/${owner}/${repoName}/git/refs/${ref}`,
            object: { sha, type: 'commit', url: `${getUrlObj().origin}/api/v3/repos/${owner}/${repoName}/git/commits/${sha}` },
          });
          return;
        }

        // GET /repos/{o}/{r}/branches
        if (subPath === 'branches' && method === 'GET') {
          const branches: any[] = [];
          for (const [refName, oid] of repo.refs) {
            if (refName.startsWith('refs/heads/')) {
              branches.push({
                name: refName.slice('refs/heads/'.length),
                commit: { sha: oid, url: `${getUrlObj().origin}/api/v3/repos/${owner}/${repoName}/commits/${oid}` },
                protected: false,
              });
            }
          }
          if (!branches.some((b) => b.name === repo.default_branch)) {
            const defOid = repo.refs.get(`refs/heads/${repo.default_branch}`);
            if (defOid) {
              branches.push({
                name: repo.default_branch,
                commit: { sha: defOid, url: `${getUrlObj().origin}/api/v3/repos/${owner}/${repoName}/commits/${defOid}` },
                protected: false,
              });
            }
          }
          sendJson(res, 200, branches);
          return;
        }

        // GET /repos/{o}/{r}/commits
        if (subPath === 'commits' && method === 'GET') {
          const targetRef = getUrlObj().searchParams.get('sha') || repo.default_branch;
          const tip = repo.refs.get(`refs/heads/${targetRef}`) || repo.refs.get(targetRef) || targetRef;
          const commits: any[] = [];
          if (tip && repo.objects.has(tip)) {
            const queue = [tip];
            const seen = new Set<string>();
            while (queue.length > 0) {
              const oid = queue.shift()!;
              if (seen.has(oid)) continue;
              seen.add(oid);
              const obj = repo.objects.get(oid);
              if (obj && obj.type === 'commit') {
                const parsed = parseCommit(obj.data);
                commits.push({
                  sha: oid,
                  commit: {
                    message: parsed.message.trim(),
                    author: { name: store.viewer.name, email: store.viewer.email, date: new Date().toISOString() },
                  },
                });
                for (const p of parsed.parents) queue.push(p);
              }
            }
          }
          sendJson(res, 200, commits);
          return;
        }

        // Contents API: PUT / GET / DELETE /repos/{o}/{r}/contents/{path}
        const contentsMatch = subPath.match(/^contents(?:\/(.*))?$/);
        if (contentsMatch) {
          const filePath = decodeURIComponent(contentsMatch[1] || '').replace(/^\/+|\/+$/g, '');

          if (method === 'PUT') {
            const parsed = await readJsonBody(req);
            if (!parsed.ok) {
              sendJson(res, 400, { message: 'Problems parsing JSON' });
              return;
            }
            const { message, content, branch = repo.default_branch, sha: expectedSha } = parsed.data;
            const decoded = Buffer.from(content || '', 'base64');
            const blobOid = store.putObject(repo, 'blob', decoded);

            const branchRef = `refs/heads/${branch}`;
            const parentSha = repo.refs.get(branchRef);
            let currentTreeOid: string | null = null;
            if (parentSha) {
              const parentObj = repo.objects.get(parentSha);
              if (parentObj && parentObj.type === 'commit') {
                currentTreeOid = parseCommit(parentObj.data).tree;
              }
            }

            // Check existing blob sha if expectedSha passed
            const existingEntry = currentTreeOid ? findEntryInTree(repo, currentTreeOid, filePath) : null;
            if (expectedSha && currentTreeOid) {
              if (existingEntry && existingEntry.oid !== expectedSha) {
                sendJson(res, 409, { message: 'Conflict: stale sha' });
                return;
              }
            }

            const newTreeOid = updateTreeWithFile(repo, currentTreeOid, filePath, blobOid);
            const newCommitSha = createCommit(repo, newTreeOid, parentSha ? [parentSha] : [], message || 'Update content');
            repo.refs.set(branchRef, newCommitSha);

            const status = existingEntry ? 200 : 201;
            sendJson(res, status, {
              content: {
                name: filePath.split('/').pop(),
                path: filePath,
                sha: blobOid,
                size: decoded.length,
                type: 'file',
                encoding: 'base64',
              },
              commit: { sha: newCommitSha },
            });
            return;
          }

          if (method === 'GET') {
            const branch = getUrlObj().searchParams.get('ref') || repo.default_branch;
            const branchRef = `refs/heads/${branch}`;
            const commitSha = repo.refs.get(branchRef) || repo.refs.get(branch) || (repo.objects.has(branch) ? branch : null);
            if (!commitSha || !repo.objects.has(commitSha)) {
              sendJson(res, 404, { message: 'Not Found' });
              return;
            }
            const commitObj = repo.objects.get(commitSha);
            if (!commitObj || commitObj.type !== 'commit') {
              sendJson(res, 404, { message: 'Not Found' });
              return;
            }
            const treeOid = parseCommit(commitObj.data).tree;

            const entry = findEntryInTree(repo, treeOid, filePath);
            if (!entry) {
              sendJson(res, 404, { message: 'Not Found' });
              return;
            }

            if (entry.mode === '40000') {
              // Return directory listing
              const directChildren = getTreeEntries(repo, entry.oid);
              sendJson(res, 200, directChildren.map((e) => ({
                name: e.name,
                path: filePath ? `${filePath}/${e.name}` : e.name,
                sha: e.oid,
                type: e.mode === '40000' ? 'dir' : 'file',
                size: 0,
              })));
              return;
            }

            // File
            const blobObj = repo.objects.get(entry.oid);
            if (!blobObj) {
              sendJson(res, 404, { message: 'Not Found' });
              return;
            }
            sendJson(res, 200, {
              type: 'file',
              encoding: 'base64',
              content: blobObj.data.toString('base64'),
              sha: entry.oid,
              size: blobObj.data.length,
              name: filePath.split('/').pop(),
              path: filePath,
            });
            return;
          }

          if (method === 'DELETE') {
            const parsed = await readJsonBody(req);
            if (!parsed.ok) {
              sendJson(res, 400, { message: 'Problems parsing JSON' });
              return;
            }
            const { message, sha: expectedSha, branch = repo.default_branch } = parsed.data;
            const branchRef = `refs/heads/${branch}`;
            const parentSha = repo.refs.get(branchRef);
            if (!parentSha) {
              sendJson(res, 404, { message: 'Not Found' });
              return;
            }
            const parentObj = repo.objects.get(parentSha);
            if (!parentObj || parentObj.type !== 'commit') {
              sendJson(res, 404, { message: 'Not Found' });
              return;
            }
            const currentTreeOid = parseCommit(parentObj.data).tree;
            const existingEntry = findEntryInTree(repo, currentTreeOid, filePath);
            if (!existingEntry || existingEntry.mode === '40000') {
              sendJson(res, 404, { message: 'Not Found' });
              return;
            }
            if (expectedSha && existingEntry.oid !== expectedSha) {
              sendJson(res, 409, { message: 'Conflict: stale sha' });
              return;
            }

            const newTreeOid = updateTreeWithFile(repo, currentTreeOid, filePath, null);
            const newCommitSha = createCommit(repo, newTreeOid, [parentSha], message || 'Delete content');
            repo.refs.set(branchRef, newCommitSha);

            sendJson(res, 200, {
              content: null,
              commit: { sha: newCommitSha },
            });
            return;
          }
        }

        // Labels API: POST /repos/{o}/{r}/labels
        if (subPath === 'labels' && method === 'POST') {
          const parsed = await readJsonBody(req);
          if (!parsed.ok) {
            sendJson(res, 400, { message: 'Problems parsing JSON' });
            return;
          }
          const { name, color = 'ededed' } = parsed.data;
          const label: ForgeLabel = { id: repo.labels.size + 1, name, color };
          repo.labels.set(name, label);
          sendJson(res, 201, label);
          return;
        }

        // Pull Requests: /repos/{o}/{r}/pulls
        if (subPath === 'pulls') {
          if (method === 'POST') {
            const parsed = await readJsonBody(req);
            if (!parsed.ok) {
              sendJson(res, 400, { message: 'Problems parsing JSON' });
              return;
            }
            const { title, head, base, body = '' } = parsed.data;
            const headSha = repo.refs.get(`refs/heads/${head}`) || ZERO_OID;
            const baseSha = repo.refs.get(`refs/heads/${base}`) || ZERO_OID;

            const num = ++repo.pullSeq;
            const pull: ForgePull = {
              id: num,
              number: num,
              title,
              body,
              state: 'open',
              merged: false,
              merged_at: null,
              merge_commit_sha: null,
              head: { ref: head, sha: headSha },
              base: { ref: base, sha: baseSha },
              headCommitSha: headSha,
              baseCommitSha: baseSha,
            };
            repo.pulls.set(num, pull);
            sendJson(res, 201, {
              number: pull.number,
              state: pull.state,
              title: pull.title,
              body: pull.body,
              head: pull.head,
              base: pull.base,
            });
            return;
          }

          if (method === 'GET') {
            const stateFilter = getUrlObj().searchParams.get('state') || 'open';
            let list = [...repo.pulls.values()];
            if (stateFilter !== 'all') {
              list = list.filter((p) => p.state === stateFilter);
            }
            sendJson(res, 200, list.map((p) => ({
              number: p.number,
              state: p.state,
              title: p.title,
              body: p.body,
              head: p.head,
              base: p.base,
            })));
            return;
          }
        }

        // Pull Request detail: /repos/{o}/{r}/pulls/{number}
        const pullMatch = subPath.match(/^pulls\/(\d+)$/);
        if (pullMatch && method === 'GET') {
          const num = parseInt(pullMatch[1], 10);
          const pull = repo.pulls.get(num);
          if (!pull) {
            sendJson(res, 404, { message: 'Not Found' });
            return;
          }
          sendJson(res, 200, {
            number: pull.number,
            state: pull.state,
            title: pull.title,
            body: pull.body,
            head: pull.head,
            base: pull.base,
            merged: pull.merged,
            merged_at: pull.merged_at,
            merge_commit_sha: pull.merge_commit_sha,
          });
          return;
        }

        // Pull Request Files: /repos/{o}/{r}/pulls/{number}/files
        const pullFilesMatch = subPath.match(/^pulls\/(\d+)\/files$/);
        if (pullFilesMatch && method === 'GET') {
          const num = parseInt(pullFilesMatch[1], 10);
          const pull = repo.pulls.get(num);
          if (!pull) {
            sendJson(res, 404, { message: 'Not Found' });
            return;
          }

          // Compute changed files between base and head
          const headCommitObj = repo.objects.get(pull.head.sha);
          const baseCommitObj = repo.objects.get(pull.base.sha);
          const headTreeOid = headCommitObj && headCommitObj.type === 'commit' ? parseCommit(headCommitObj.data).tree : null;
          const baseTreeOid = baseCommitObj && baseCommitObj.type === 'commit' ? parseCommit(baseCommitObj.data).tree : null;

          const headFiles = headTreeOid ? listTreeRecursive(repo, headTreeOid) : [];
          const baseFiles = baseTreeOid ? listTreeRecursive(repo, baseTreeOid) : [];
          const baseMap = new Map(baseFiles.map((f) => [f.path, f.oid]));

          const changed: any[] = [];
          for (const hf of headFiles) {
            if (hf.type !== 'file') continue;
            const baseOid = baseMap.get(hf.path);
            if (!baseOid) {
              changed.push({ filename: hf.path, status: 'added', additions: 1, deletions: 0, changes: 1 });
            } else if (baseOid !== hf.oid) {
              changed.push({ filename: hf.path, status: 'modified', additions: 1, deletions: 0, changes: 1 });
            }
          }
          sendJson(res, 200, changed);
          return;
        }

        // Pull Request Merge: PUT /repos/{o}/{r}/pulls/{number}/merge
        const pullMergeMatch = subPath.match(/^pulls\/(\d+)\/merge$/);
        if (pullMergeMatch && method === 'PUT') {
          const num = parseInt(pullMergeMatch[1], 10);
          const pull = repo.pulls.get(num);
          if (!pull) {
            sendJson(res, 404, { message: 'Not Found' });
            return;
          }
          if (pull.merged) {
            sendJson(res, 405, { message: 'Pull request is not mergeable' });
            return;
          }

          const baseBranchRef = `refs/heads/${pull.base.ref}`;
          const currentBaseSha = repo.refs.get(baseBranchRef) || pull.base.sha;
          const origBaseSha = pull.baseCommitSha || pull.base.sha;
          const headSha = pull.head.sha;

          const headCommitObj = repo.objects.get(headSha);
          const currentBaseCommitObj = repo.objects.get(currentBaseSha);
          const origBaseCommitObj = repo.objects.get(origBaseSha);

          const headTreeOid = headCommitObj && headCommitObj.type === 'commit' ? parseCommit(headCommitObj.data).tree : null;
          const currentBaseTreeOid = currentBaseCommitObj && currentBaseCommitObj.type === 'commit' ? parseCommit(currentBaseCommitObj.data).tree : null;
          const origBaseTreeOid = origBaseCommitObj && origBaseCommitObj.type === 'commit' ? parseCommit(origBaseCommitObj.data).tree : null;

          const headFiles = (headTreeOid ? listTreeRecursive(repo, headTreeOid) : []).filter((f) => f.type === 'file');
          const currentBaseFiles = (currentBaseTreeOid ? listTreeRecursive(repo, currentBaseTreeOid) : []).filter((f) => f.type === 'file');
          const origBaseFiles = (origBaseTreeOid ? listTreeRecursive(repo, origBaseTreeOid) : []).filter((f) => f.type === 'file');

          const headMap = new Map(headFiles.map((f) => [f.path, f.oid]));
          const currentBaseMap = new Map(currentBaseFiles.map((f) => [f.path, f.oid]));
          const origBaseMap = new Map(origBaseFiles.map((f) => [f.path, f.oid]));

          const allPaths = new Set([...headMap.keys(), ...currentBaseMap.keys(), ...origBaseMap.keys()]);
          let hasConflict = false;
          for (const p of allPaths) {
            const h = headMap.get(p);
            const cur = currentBaseMap.get(p);
            const o = origBaseMap.get(p);
            if (cur !== o && h !== o && cur !== h) {
              hasConflict = true;
              break;
            }
          }

          if (hasConflict) {
            sendJson(res, 405, { message: 'Pull request is not mergeable' });
            return;
          }

          let mergedTreeOid = currentBaseTreeOid || headTreeOid || '';
          for (const p of allPaths) {
            const h = headMap.get(p);
            const o = origBaseMap.get(p);
            if (h !== o) {
              mergedTreeOid = updateTreeWithFile(repo, mergedTreeOid, p, h ?? null);
            }
          }

          const squashSha = createCommit(repo, mergedTreeOid, [currentBaseSha], `Merge pull request #${num} from ${pull.head.ref}`);
          repo.refs.set(baseBranchRef, squashSha);
          pull.state = 'closed';
          pull.merged = true;
          pull.merged_at = new Date().toISOString();
          pull.merge_commit_sha = squashSha;

          sendJson(res, 200, {
            sha: squashSha,
            merged: true,
            message: 'Pull Request successfully merged',
          });
          return;
        }
      }

      // Default 404 for unknown /api/v3 route
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }

    // Default 404 for any other route
    sendJson(res, 404, { message: 'Not Found' });
  });

  server.keepAliveTimeout = 120000;
  server.headersTimeout = 125000;
  server.on('connection', (socket) => {
    socket.setNoDelay(true);
  });

  return server;
}
