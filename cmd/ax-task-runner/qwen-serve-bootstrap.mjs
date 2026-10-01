import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const run = promisify(execFile);

const origin = process.env.AX_QWEN_DAEMON_ORIGIN ?? 'http://127.0.0.1:4170';
const workspaceDir = process.env.AX_TASK_WORKSPACE ?? '/workspace';
const branch = process.env.AX_GIT_BRANCH ?? '';
const token = process.env.QWEN_SERVER_TOKEN;
const prompt = process.env.AX_QWEN_PROMPT;

const intEnv = (name, fallback) => {
  const parsed = Number.parseInt(process.env[name] ?? '', 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
};

const reviewEnabled = !['0', 'false', 'no', 'off'].includes(
  (process.env.AX_QWEN_REVIEW ?? '1').toLowerCase(),
);
// Measured 2026-10-01: medium effort spent ~678k input tokens over 25 minutes on an
// 84-line Go change and did not finish. low is the single-pass review - no subagent
// fan-out, no build/test - which is what a per-task gate should cost.
const reviewEffort = (() => {
  const raw = (process.env.AX_QWEN_REVIEW_EFFORT ?? 'low').toLowerCase();
  return ['low', 'medium', 'high'].includes(raw) ? raw : 'low';
})();
// A fresh session costs ~20k input tokens before it reads a line of code, so a
// change this small is not worth paying a reviewer fan-out for.
const reviewMinLines = intEnv('AX_QWEN_REVIEW_MIN_LINES', 25);
// Measured 2026-10-01: a low-effort pass over a 1,824-line / 15-file feature took
// 11.9 minutes and finished inside a 15 minute wall by three minutes. The budget is
// the thing that fails open (no findings, straight to delivery), so it is set above
// the slowest honest run rather than at it.
const reviewDeadlineMs = intEnv('AX_QWEN_REVIEW_DEADLINE_MINUTES', 25) * 60_000;
// The implementation phase is the task itself and is not cut off by default.
const implementDeadlineMs = intEnv('AX_QWEN_IMPLEMENT_DEADLINE_MINUTES', 1440) * 60_000;
const findingsPath = `${workspaceDir}/.ax/review-findings.md`;
const pollIntervalMs = 2_000;

if (!token) throw new Error('QWEN_SERVER_TOKEN is not set');
if (!prompt) throw new Error('AX_QWEN_PROMPT is empty');

function log(message) {
  process.stdout.write(`${new Date().toISOString()} ${message}\n`);
}

const headers = {
  Authorization: `Bearer ${token}`,
  'Content-Type': 'application/json',
};

async function request(path, init = {}) {
  const response = await fetch(origin + path, {
    ...init,
    headers: { ...headers, ...(init.headers ?? {}) },
  });
  if (!response.ok) {
    const body = await response.text();
    throw new Error(`${init.method ?? 'GET'} ${path}: HTTP ${response.status}: ${body}`);
  }
  return response;
}

async function createSession(body) {
  const session = await (
    await request('/session', { method: 'POST', body: JSON.stringify(body) })
  ).json();
  if (!session.sessionId) throw new Error('Qwen Serve did not return a sessionId');
  return session;
}

// POST /session/:id/prompt answers 202 with a promptId and the turn keeps running,
// so every phase is "submit, then wait for the session to go quiet".
async function submitPrompt(sessionId, text, deadlineMs) {
  const accepted = await (
    await request(`/session/${encodeURIComponent(sessionId)}/prompt`, {
      method: 'POST',
      body: JSON.stringify({
        prompt: [{ type: 'text', text }],
        ...(deadlineMs ? { deadlineMs } : {}),
      }),
    })
  ).json();
  if (!accepted.promptId) throw new Error(`No promptId returned for session ${sessionId}`);
  return accepted.promptId;
}

// A turn is over when the daemon has no active prompt and is not parked on a human.
//
// `activeWorkState` is deliberately not consulted. The daemon derives it from
// `pendingPromptCount || pendingAgentNotificationCount || backgroundTurn`, so a session
// that leaves a dev server running - which is exactly what a preview task is told to do -
// reports "active" for the rest of its life and the phase runner would never advance.
const isBusy = (status) =>
  status.hasActivePrompt === true ||
  status.isWaitingForPermission === true ||
  status.isWaitingForUserQuestion === true ||
  (Number.isFinite(status.pendingInteractionCount) ? status.pendingInteractionCount : 0) > 0;

async function waitForIdle(sessionId, label, deadlineMs) {
  const startedAt = Date.now();
  // A prompt that has just been accepted is not marked busy yet, so an idle reading
  // only counts once we have seen it busy or given it a chance to become busy.
  let sawBusy = false;
  while (true) {
    const status = await (
      await request(`/session/${encodeURIComponent(sessionId)}/status`)
    ).json();
    if (isBusy(status)) {
      sawBusy = true;
    } else if (sawBusy || Date.now() - startedAt >= 15_000) {
      return status;
    }
    if (Date.now() - startedAt >= deadlineMs) {
      throw new Error(
        `${label} exceeded its ${Math.round(deadlineMs / 60_000)} minute budget`,
      );
    }
    await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
  }
}

async function gitText(...args) {
  const { stdout } = await run('git', args, {
    cwd: workspaceDir,
    maxBuffer: 8 * 1024 * 1024,
  });
  return stdout;
}

// The task branch has no commit of its own until delivery, and a workspace whose base
// was never materialized has no HEAD at all, so the change set is measured from the
// index and the untracked list rather than from a diff against HEAD.
//
// Runner and review scratch space is filtered here as well as in .git/info/exclude:
// counting the runner's own logs as the agent's work would trigger a review of nothing.
const runnerState = (path) => path === '.ax' || path.startsWith('.ax/') || path === '.qwen' || path.startsWith('.qwen/');

// Lockfiles, build output, minified bundles and generated clients. The reviewer is
// better off not spending its wall on them, and `qwen review capture-local` offers no
// way to drop them from the capture (no exclusion flag, no such setting or env var),
// so the only lever this runner has is the decision of whether a review is worth
// starting: generated lines never count toward the size threshold. A dependency bump
// with five lines of real code no longer buys a full review pass.
const generatedPath = (path) => {
  const p = path.toLowerCase().replace(/^"|"$/g, '');
  if (/(^|\/)(package-lock\.json|npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|bun\.lockb?|cargo\.lock|gemfile\.lock|poetry\.lock|uv\.lock|composer\.lock|go\.sum)$/.test(p)) return true;
  if (/(^|\/)(dist|build|out|\.next|\.nuxt|\.svelte-kit|\.turbo|\.vercel|node_modules|vendor|target|__pycache__|generated)\//.test(p)) return true;
  return /\.(min\.js|min\.css|map|snap|pb\.go|pb\.ts|lock)$/.test(p);
};

async function describeChanges() {
  let lines = 0;
  let generatedLines = 0;
  let generatedPaths = 0;

  const numstat = `${await gitText('diff', '--cached', '--numstat')}
${await gitText('diff', '--numstat')}`;
  for (const row of numstat.split('\n').filter(Boolean)) {
    const [added, deleted, path = ''] = row.split('\t');
    if (runnerState(path)) continue;
    // A binary file reports "-" for both counts; it still deserves a look.
    const count =
      (added === '-' ? 1 : Number.parseInt(added, 10) || 0) +
      (deleted === '-' ? 1 : Number.parseInt(deleted, 10) || 0);
    if (generatedPath(path)) {
      generatedLines += count;
      generatedPaths += 1;
    } else {
      lines += count;
    }
  }

  const untracked = (await gitText('ls-files', '--others', '--exclude-standard'))
    .split('\n')
    .filter(Boolean)
    .filter((path) => !runnerState(path));
  if (untracked.length > 0) {
    const { stdout } = await run('wc', ['-l', ...untracked], {
      cwd: workspaceDir,
      maxBuffer: 8 * 1024 * 1024,
    });
    // With several inputs wc appends a "total" row whose name is not a path.
    for (const row of stdout.trim().split('\n')) {
      const [count, path = ''] = row.trim().split(/\s+/);
      if (!path || path === 'total') continue;
      const parsed = Number.parseInt(count, 10) || 0;
      if (generatedPath(path)) {
        generatedLines += parsed;
        generatedPaths += 1;
      } else {
        lines += parsed;
      }
    }
  }

  const paths = (await gitText('status', '--porcelain'))
    .split('\n')
    .filter(Boolean)
    .filter((entry) => !runnerState(entry.slice(3).replace(/^"|"$/g, '')));
  return { pathCount: paths.length, lines, generatedLines, generatedPaths };
}

async function readFindings() {
  try {
    return (await readFile(findingsPath, 'utf8')).trim();
  } catch {
    return '';
  }
}

let implementationSessionId = '';

// The work is only safe once it is pushed. If the review cycle cannot finish for any
// reason, delivery still has to happen, so hand it back rather than leaving the task
// holding an uncommitted tree.
async function deliverAnyway(reason) {
  log(`handing delivery back to the implementation session: ${reason}`);
  try {
    await submitPrompt(
      implementationSessionId,
      `The automated review phase did not complete (${reason}). Skip it and deliver now: ` +
        `commit every change on ${branch || 'the current branch'} with a meaningful commit ` +
        'message, push that branch to origin, and report the branch, the commit SHA, and ' +
        'anything still unverified.',
      implementDeadlineMs,
    );
    await waitForIdle(implementationSessionId, 'delivery fallback', implementDeadlineMs);
  } catch (error) {
    log(`delivery fallback did not complete: ${error.message}`);
  }
}

const deadline = Date.now() + 120_000;
while (true) {
  try {
    await request('/health');
    break;
  } catch (error) {
    if (Date.now() >= deadline) throw error;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
}

const session = await createSession({});
implementationSessionId = session.sessionId;

await mkdir(`${workspaceDir}/.ax`, { recursive: true });
await writeFile(`${workspaceDir}/.ax/qwen-session-id`, `${implementationSessionId}\n`, {
  mode: 0o600,
});
log(`Qwen session ${implementationSessionId} created; submitting initial prompt`);

await submitPrompt(implementationSessionId, prompt, implementDeadlineMs);
const implemented = await waitForIdle(implementationSessionId, 'implementation', implementDeadlineMs);
log('implementation turn settled');
if (implemented.hasRunningBackgroundTasks === true) {
  log('a background task is still running (its preview stays up); proceeding to review');
}

if (!reviewEnabled) {
  log('review phase disabled (AX_QWEN_REVIEW)');
  process.exit(0);
}

const changes = await describeChanges().catch((error) => {
  log(`could not inspect the working tree: ${error.message}`);
  return null;
});
if (!changes) process.exit(0);
log(
  `working tree: ${changes.pathCount} changed path(s), ~${changes.lines} reviewable line(s)` +
    (changes.generatedPaths
      ? ` (+${changes.generatedPaths} generated path(s), ${changes.generatedLines} line(s), not counted)`
      : ''),
);

if (changes.pathCount === 0) {
  log('nothing uncommitted to review');
  process.exit(0);
}
if (changes.lines < reviewMinLines) {
  await deliverAnyway(`the change is ${changes.lines} reviewable line(s), under the ${reviewMinLines} line threshold`);
  process.exit(0);
}

const reviewSession = await createSession({ sessionScope: 'thread' });
const reviewSessionId = reviewSession.sessionId;
await writeFile(`${workspaceDir}/.ax/qwen-review-session-id`, `${reviewSessionId}\n`, {
  mode: 0o600,
});
log(`review session ${reviewSessionId} created (fresh context, same workspace)`);

try {
  // The review skill parses its argument string verbatim, so the slash command gets a
  // turn to itself; anything else it must do is a separate turn.
  await submitPrompt(reviewSessionId, `/review --effort ${reviewEffort}`, reviewDeadlineMs);
  await waitForIdle(reviewSessionId, `review (${reviewEffort})`, reviewDeadlineMs);
  log('review turn settled');

  await submitPrompt(
    reviewSessionId,
    'Do not review anything again, run no tools that modify files, and do not commit. ' +
      `Write every finding from the review you just completed into ${findingsPath} as ` +
      'markdown, one section per finding: severity, file and line, what is wrong, and the ' +
      'concrete fix. If the review found nothing, write exactly "No findings." and nothing ' +
      'else in the file.',
    reviewDeadlineMs,
  );
  await waitForIdle(reviewSessionId, 'findings write', reviewDeadlineMs);
} catch (error) {
  await deliverAnyway(error.message);
  process.exit(0);
}

const findings = await readFindings();
if (!findings) {
  await deliverAnyway('the review session left no findings file');
  process.exit(0);
}
if (/^no findings\.?$/i.test(findings)) {
  log('review found nothing to change');
  await deliverAnyway('the review reported no findings, so apply nothing');
  process.exit(0);
}
log(`review produced ${findings.length} character(s) of findings; handing back`);

await submitPrompt(
  implementationSessionId,
  'A code review of your uncommitted work ran in its own session and wrote its findings to ' +
    `${findingsPath}. Read that file and apply every finding that is genuinely wrong in this ` +
    'change - blockers and majors at minimum. Do not re-run the review and do not widen the ' +
    `work beyond the findings. Then commit all changes on ${branch || 'the current branch'} ` +
    'with a meaningful commit message, push that branch to origin, and report the branch, the ' +
    'commit SHA, the verification you ran, which findings you applied, which you rejected and ' +
    'why, and any remaining issues.',
  implementDeadlineMs,
);
await waitForIdle(implementationSessionId, 'apply findings', implementDeadlineMs);

const remaining = await describeChanges().catch(() => null);
if (remaining && remaining.pathCount > 0) {
  await deliverAnyway('changes were still uncommitted after the apply phase');
}
log('phase runner complete');
