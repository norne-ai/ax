import assert from 'node:assert/strict';
import http from 'node:http';
import { mkdir, mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn } from 'node:child_process';
import test from 'node:test';

const BOOTSTRAP = new URL('./qwen-serve-bootstrap.mjs', import.meta.url).pathname;

// A stand-in for `qwen serve`: it records the call sequence and settles each prompt on
// the second status poll, which is how the real daemon behaves (POST /prompt answers
// 202 immediately and /status reports the turn afterwards).
function fakeDaemon(onPrompt = () => {}, options = {}) {
  const calls = [];
  const sessions = new Map();
  const pending = new Map();
  let nextSession = 0;

  // A session that leaves a dev server running keeps reporting activeWorkState "active"
  // through backgroundTurn even though its turn is over, which is how a preview task
  // looks to the daemon for the rest of its life.
  const lingering = options.lingeringBackgroundTask
    ? { activeWorkState: 'active', hasRunningBackgroundTasks: true }
    : { activeWorkState: 'idle' };

  const server = http.createServer((req, res) => {
    let body = '';
    req.on('data', (chunk) => { body += chunk; });
    req.on('end', async () => {
      const url = new URL(req.url, 'http://127.0.0.1');
      const send = (code, payload) => {
        res.writeHead(code, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(payload));
      };
      if (url.pathname === '/health') return send(200, { status: 'ok' });

      if (url.pathname === '/session' && req.method === 'POST') {
        const requested = body ? JSON.parse(body) : {};
        const fresh = requested.sessionScope === 'thread';
        const sessionId = `session-${nextSession++}`;
        calls.push({ kind: 'session', fresh, body: requested });
        sessions.set(sessionId, requested);
        return send(200, { sessionId, workspaceCwd: '/workspace', attached: !fresh });
      }

      const promptMatch = url.pathname.match(/^\/session\/([^/]+)\/prompt$/);
      if (promptMatch && req.method === 'POST') {
        const text = JSON.parse(body).prompt[0].text;
        calls.push({ kind: 'prompt', sessionId: promptMatch[1], text, deadlineMs: JSON.parse(body).deadlineMs });
        pending.set(promptMatch[1], 0);
        await onPrompt(promptMatch[1], text);
        return send(202, { promptId: `prompt-${calls.length}` });
      }

      const statusMatch = url.pathname.match(/^\/session\/([^/]+)\/status$/);
      if (statusMatch) {
        const sessionId = statusMatch[1];
        const seen = pending.get(sessionId);
        if (seen === undefined) return send(200, { sessionId, hasActivePrompt: false, ...lingering });
        if (seen === 0) {
          pending.set(sessionId, 1);
          return send(200, { sessionId, hasActivePrompt: true, activeWorkState: 'active' });
        }
        pending.set(sessionId, undefined);
        return send(200, { sessionId, hasActivePrompt: false, ...lingering });
      }

      calls.push({ kind: 'other', method: req.method, path: url.pathname });
      return send(404, { error: 'not found' });
    });
  });

  return {
    calls,
    listen: () => new Promise((resolve) => server.listen(0, '127.0.0.1', resolve)),
    close: () => new Promise((resolve) => server.close(resolve)),
    port: () => server.address().port,
  };
}

// The bootstrap shells out to git, so each case runs against a throwaway repository.
async function workspaceWith(changes) {
  const dir = await mkdtemp(join(tmpdir(), 'ax-bootstrap-'));
  const sh = (cmd) =>
    new Promise((resolve, reject) => {
      const child = spawn('sh', ['-c', cmd], { cwd: dir });
      let err = '';
      child.stderr.on('data', (chunk) => { err += chunk; });
      child.on('exit', (code) =>
        code === 0 ? resolve() : reject(new Error(`git setup failed (${code}): ${err}`)),
      );
    });
  await sh('git init -q . && git -c user.email=t@t -c user.name=t commit -q --allow-empty -m init');
  for (const [name, content] of Object.entries(changes)) {
    await writeFile(join(dir, name), content);
  }
  await sh('git add -A');
  return dir;
}

const manyLines = Array.from({ length: 40 }, (_, i) => `line ${i}`).join('\n') + '\n';

async function runBootstrap(env, workspace, options = {}) {
  // The review session's findings file is the hand-off the apply phase reads, so the
  // fake daemon produces it exactly when the real reviewer would have.
  const daemon = fakeDaemon(async (_sessionId, text) => {
    if (options.findings === undefined) return;
    if (!/Do not review anything again/.test(text)) return;
    await mkdir(join(workspace, '.ax'), { recursive: true });
    await writeFile(join(workspace, '.ax', 'review-findings.md'), options.findings);
  }, options);
  await daemon.listen();
  const logs = [];
  const child = spawn(
    process.execPath,
    [BOOTSTRAP],
    {
      env: {
        ...env,
        QWEN_SERVER_TOKEN: 'test-token',
        AX_QWEN_PROMPT: 'implement the thing',
        AX_QWEN_DAEMON_ORIGIN: `http://127.0.0.1:${daemon.port()}`,
        AX_TASK_WORKSPACE: workspace,
        AX_GIT_BRANCH: 'qwen/test-task',
        AX_GIT_BASE_BRANCH: 'main',
        // Keep every poll inside the test's patience.
        AX_QWEN_IMPLEMENT_DEADLINE_MINUTES: '1',
        AX_QWEN_REVIEW_DEADLINE_MINUTES: '1',
      },
    },
  );
  child.stdout.on('data', (chunk) => logs.push(String(chunk)));
  child.stderr.on('data', (chunk) => logs.push(String(chunk)));
  const code = await new Promise((resolve) => child.on('exit', resolve));
  await daemon.close();
  await rm(workspace, { recursive: true, force: true });
  return { code, calls: daemon.calls, output: logs.join('') };
}

const promptTexts = (calls) => calls.filter((call) => call.kind === 'prompt').map((call) => call.text);

test('review enabled runs implement, review, findings write, then apply', async () => {
  const workspace = await workspaceWith({ 'big.go': manyLines });
  const { code, calls, output } = await runBootstrap(
    { AX_QWEN_REVIEW: '1', AX_QWEN_REVIEW_EFFORT: 'medium' },
    workspace,
    { findings: '## Blocker\n\n`big.go:12` - shadowed error. Return it.\n' },
  );
  const texts = promptTexts(calls);
  assert.equal(code, 0, output);
  assert.deepEqual(
    calls.filter((call) => call.kind === 'session').map((call) => call.fresh),
    [false, true],
    'one implementation session plus one fresh review session',
  );
  assert.equal(texts[0], 'implement the thing');
  // The review skill parses its argument string verbatim, so the slash command must
  // arrive alone on its turn.
  assert.equal(texts[1], '/review --effort medium');
  assert.match(texts[2], /Do not review anything again/);
  assert.match(texts[3], /review-findings\.md/);
  assert.match(texts[3], /push that branch to origin/);
  // Only the review turn is wall-clock capped; the task itself is not cut off.
  assert.equal(calls.filter((call) => call.kind === 'prompt')[1].deadlineMs, 60_000);
});

test('review disabled delivers in a single prompt', async () => {
  const workspace = await workspaceWith({ 'big.go': manyLines });
  const { code, calls } = await runBootstrap({ AX_QWEN_REVIEW: '0' }, workspace);
  assert.equal(code, 0);
  assert.equal(calls.filter((call) => call.kind === 'session').length, 1);
  assert.deepEqual(promptTexts(calls), ['implement the thing']);
});

test('the default review effort is the single-pass low tier', async () => {
  const workspace = await workspaceWith({ 'big.go': manyLines });
  const { calls } = await runBootstrap({ AX_QWEN_REVIEW: '1' }, workspace, {
    findings: '## Major\n\n`big.go:3` - unchecked error.\n',
  });
  assert.equal(
    promptTexts(calls)[1],
    '/review --effort low',
    'medium measured 678k input tokens on an 84-line change; the default must not fan out',
  );
});

test('an unchanged tree never opens a review session', async () => {
  const workspace = await workspaceWith({});
  const { code, calls, output } = await runBootstrap({ AX_QWEN_REVIEW: '1' }, workspace);
  assert.equal(code, 0, output);
  assert.equal(calls.filter((call) => call.kind === 'session').length, 1);
  assert.match(output, /nothing uncommitted to review/);
});

test('a change under the size threshold skips review but still delivers', async () => {
  const workspace = await workspaceWith({ 'tiny.go': 'one\ntwo\n' });
  const { code, calls, output } = await runBootstrap(
    { AX_QWEN_REVIEW: '1', AX_QWEN_REVIEW_MIN_LINES: '25' },
    workspace,
  );
  assert.equal(code, 0, output);
  assert.equal(calls.filter((call) => call.kind === 'session').length, 1, 'no review session');
  assert.match(output, /under the 25 line threshold/);
  const texts = promptTexts(calls);
  assert.equal(texts.length, 2, 'the fallback delivery prompt runs so the work is pushed');
  assert.match(texts[1], /Skip it and deliver now/);
});

test('a review session that writes no findings file falls back to delivery', async () => {
  const workspace = await workspaceWith({ 'big.go': manyLines });
  const { code, calls, output } = await runBootstrap({ AX_QWEN_REVIEW: '1' }, workspace);
  assert.equal(code, 0, output);
  // The fake daemon never creates .ax/review-findings.md, so the apply phase must not
  // run and delivery must still happen.
  const texts = promptTexts(calls);
  assert.ok(!texts.some((text) => /Read that file and apply/.test(text)), 'no apply phase');
  assert.match(texts[texts.length - 1], /Skip it and deliver now/);
});

// Measured 2026-10-01 on wa-guestbook-nextjs-1790878150: the agent left `next dev`
// running as a background task, the daemon kept reporting activeWorkState "active",
// and the review phase never fired. Sequencing may only depend on the turn being over.
test('a session that leaves a background task running still reviews and delivers', async () => {
  const workspace = await workspaceWith({ 'big.go': manyLines });
  const { code, calls, output } = await runBootstrap(
    { AX_QWEN_REVIEW: '1' },
    workspace,
    {
      findings: '## Major\n\n`big.go:7` - unhandled error. Return it.\n',
      lingeringBackgroundTask: true,
    },
  );
  const texts = promptTexts(calls);
  assert.equal(code, 0, output);
  assert.equal(texts[1], '/review --effort low', 'the dev server must not wedge the phase runner');
  assert.match(texts[3], /review-findings\.md/, 'findings are handed back to the task session');
  assert.match(output, /a background task is still running/);
});

// The guestbook diff was 1,824 lines but only 766 of them were source, and a single
// 36k-char package-lock.json chunk was paged and re-sent across 21 reviewer calls.
// Generated volume must not decide whether a review is worth starting.
test('generated bulk does not drag a small change into a review', async () => {
  const bulk = (n) => Array.from({ length: n }, (_, i) => `line ${i}`).join('\n') + '\n';
  const workspace = await workspaceWith({
    'package-lock.json': bulk(300),
    'tiny.go': 'one\ntwo\n',
  });
  // Written after the helper staged everything, so these land in the untracked list
  // that describeChanges counts with wc instead of the diff numstat.
  await mkdir(join(workspace, 'dist'), { recursive: true });
  await writeFile(join(workspace, 'dist', 'bundle.min.js'), bulk(100));
  await writeFile(join(workspace, 'notes.md'), 'a\nb\nc\n');

  const { code, calls, output } = await runBootstrap(
    { AX_QWEN_REVIEW: '1', AX_QWEN_REVIEW_MIN_LINES: '25' },
    workspace,
  );
  const texts = promptTexts(calls);
  assert.equal(code, 0, output);
  assert.match(output, /5 reviewable line\(s\)/, 'tiny.go (2) + notes.md (3)');
  assert.match(output, /2 generated path\(s\), 400 line\(s\), not counted/);
  assert.equal(calls.filter((call) => call.kind === 'session').length, 1, 'no review session');
  assert.match(output, /under the 25 line threshold/);
  assert.match(texts[texts.length - 1], /Skip it and deliver now/, 'work is still delivered');
});
