import { mkdir, writeFile } from 'node:fs/promises';

const origin = 'http://127.0.0.1:4170';
const token = process.env.QWEN_SERVER_TOKEN;
const prompt = process.env.AX_QWEN_PROMPT;

if (!token) throw new Error('QWEN_SERVER_TOKEN is not set');
if (!prompt) throw new Error('AX_QWEN_PROMPT is empty');

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

const sessionResponse = await request('/session', {
  method: 'POST',
  body: '{}',
});
const session = await sessionResponse.json();
if (!session.sessionId) throw new Error('Qwen Serve did not return a sessionId');

await mkdir('/workspace/.ax', { recursive: true });
await writeFile('/workspace/.ax/qwen-session-id', `${session.sessionId}\n`, { mode: 0o600 });
process.stdout.write(`Qwen session ${session.sessionId} created; submitting initial prompt\n`);

const result = await request(`/session/${encodeURIComponent(session.sessionId)}/prompt`, {
  method: 'POST',
  body: JSON.stringify({ prompt: [{ type: 'text', text: prompt }] }),
});
process.stdout.write(`${await result.text()}\n`);
