import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import net from 'node:net';
import http from 'node:http';
import test from 'node:test';

const MUX_PORT = 8123;
const MUX = new URL('./qwen-preview-mux.mjs', import.meta.url).pathname;

// Every test here waits on sockets, so a hang must fail loudly instead of
// taking the whole run down with it.
const guarded = (name, fn) => test(name, { timeout: 25_000 }, fn);

// Records the last request each fake upstream saw, so header policy is checkable.
function upstream(name) {
  const seen = [];
  const server = http.createServer((req, res) => {
    const headers = { ...req.headers, host: req.headers.host };
    let body = '';
    req.on('data', (chunk) => { body += chunk; });
    req.on('end', () => {
      seen.push({ url: req.url, method: req.method, headers, body });
      if (req.url === '/redirect') {
        res.writeHead(302, { location: '/login', 'transfer-encoding': 'chunked' });
        return res.end();
      }
      if (req.url === '/preview/redirect') {
        res.writeHead(302, { location: '/preview/elsewhere' });
        return res.end();
      }
      if (req.url === '/stream') {
        res.writeHead(200, { 'content-type': 'text/event-stream' });
        res.write('data: one\n\n');
        setTimeout(() => { res.write('data: two\n\n'); res.end(); }, 20);
        return;
      }
      res.writeHead(200, { 'content-type': 'text/plain', 'x-upstream': name });
      res.end(`${name}:${req.url}`);
    });
  });
  server.on('upgrade', (req, socket) => {
    seen.push({ url: req.url, method: 'UPGRADE', headers: { ...req.headers, host: req.headers.host } });
    socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: upgrade\r\n\r\n');
    socket.on('data', (chunk) => socket.write(Buffer.concat([Buffer.from('echo:'), chunk])));
  });
  return { server, seen, port: () => server.address().port };
}

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
}

async function waitForPort(port) {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    const socket = net.connect(port, '127.0.0.1');
    const ok = await new Promise((resolve) => {
      socket.once('connect', () => resolve(true));
      socket.once('error', () => resolve(false));
    });
    socket.destroy();
    if (ok) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error(`mux never listened on ${port}`);
}

function request(path, { headers = {}, method = 'GET', body = '' } = {}) {
  return new Promise((resolve, reject) => {
    const req = http.request(
      {
        port: MUX_PORT,
        path,
        method,
        // Stand in for the browser-visible hostname the dashboard preserves,
        // which must not become the upstream's own address on the last hop.
        headers: { host: 'task.norne', ...headers },
      },
      (res) => {
        let text = '';
        const chunks = [];
        res.on('data', (chunk) => { chunks.push(chunk); text += chunk; });
        res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, text, chunks }));
      },
    );
    req.on('error', reject);
    if (body) req.write(body);
    req.end();
  });
}

function rawUpgrade(path, payload, host = 'task.norne') {
  return new Promise((resolve, reject) => {
    const socket = net.connect(MUX_PORT, '127.0.0.1');
    let buffered = Buffer.alloc(0);
    let headerLength = -1;
    const done = (value) => {
      socket.destroy();
      resolve(value);
    };
    socket.on('error', reject);
    socket.setTimeout(15_000, () => {
      socket.destroy();
      reject(new Error('upgrade timed out'));
    });
    socket.once('connect', () => {
      socket.write(
        `GET ${path} HTTP/1.1\r\nHost: ${host}\r\nUpgrade: websocket\r\n` +
        `Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer secret-token\r\n\r\n`,
      );
    });
    socket.on('data', (chunk) => {
      buffered = Buffer.concat([buffered, chunk]);
      const head = buffered.toString('latin1');
      if (headerLength === -1) {
        const end = head.indexOf('\r\n\r\n');
        if (end === -1) return;
        if (!head.startsWith('HTTP/1.1 101')) {
          socket.destroy();
          return reject(new Error(`bad handshake: ${head.slice(0, end)}`));
        }
        headerLength = end + 4;
        const rest = buffered.subarray(headerLength);
        if (rest.length) return done({ head: head.slice(0, headerLength), body: rest });
        socket.write(payload);
        return;
      }
      done({ head: head.slice(0, headerLength), body: buffered.subarray(headerLength) });
    });
  });
}

async function withMux(appTarget, daemonTarget, run, extraEnv = {}) {
  const child = spawn(
    process.execPath,
    [MUX],
    {
      env: {
        ...process.env,
        AX_PREVIEW_TARGET: appTarget,
        AX_PREVIEW_DAEMON_TARGET: daemonTarget,
        AX_PREVIEW_BASE: '/preview',
        AX_PREVIEW_PORT: String(MUX_PORT),
        ...extraEnv,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    },
  );
  const stderr = [];
  child.stderr.on('data', (chunk) => stderr.push(chunk.toString()));
  try {
    await waitForPort(MUX_PORT);
    await run();
  } finally {
    child.kill();
    await new Promise((resolve) => child.once('exit', resolve));
  }
  return stderr.join('');
}

guarded('routes the prefix to the app and everything else to the daemon', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  await withMux(
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      const preview = await request('/preview/_next/webpack-hmr/x?y=1');
      assert.equal(preview.headers['x-upstream'], 'app');
      assert.equal(preview.text, 'app:/preview/_next/webpack-hmr/x?y=1');

      const shell = await request('/session/abc/prompt');
      assert.equal(shell.headers['x-upstream'], 'daemon');
      assert.equal(shell.text, 'daemon:/session/abc/prompt');

      const root = await request('/');
      assert.equal(root.headers['x-upstream'], 'daemon');

      // A sibling path that merely starts with the prefix word stays with the daemon.
      const neighbour = await request('/previewer');
      assert.equal(neighbour.headers['x-upstream'], 'daemon');
    },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});

guarded('keeps the daemon bearer and strips it from the app', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  const auth = { authorization: 'Bearer secret-token' };
  await withMux(
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      await request('/preview/', { headers: auth });
      await request('/health', { headers: auth });

      assert.equal(appUpstream.seen.at(-1).headers.authorization, undefined);
      assert.equal(daemonUpstream.seen.at(-1).headers.authorization, 'Bearer secret-token');
      // The browser-visible hostname has to survive on both hops: the daemon
      // checks Origin against it and Next derives its origin from it.
      assert.equal(appUpstream.seen.at(-1).headers.host, 'task.norne');
      assert.equal(daemonUpstream.seen.at(-1).headers.host, 'task.norne');
      // Hop-by-hop framing must not cross the proxy twice, and a bodiless GET
      // must not be re-framed as a chunked request body.
      assert.equal(appUpstream.seen.at(-1).headers['transfer-encoding'], undefined);
      assert.equal(daemonUpstream.seen.at(-1).headers['transfer-encoding'], undefined);
    },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});

guarded('rewrites app redirects back under the prefix and preserves method and body', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  await withMux(
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      const posted = await request('/preview/api/save', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: '{"a":1}',
      });
      assert.equal(appUpstream.seen.at(-1).method, 'POST');
      assert.equal(appUpstream.seen.at(-1).body, '{"a":1}');
      assert.equal(posted.text, 'app:/preview/api/save');

      // An app that redirects to an app-rooted path would otherwise escape onto
      // the Web Shell, because both share one origin.
      const alreadyPrefixed = await request('/preview/redirect');
      assert.equal(alreadyPrefixed.headers.location, '/preview/elsewhere');

      const redirect = await request('/redirect');
      assert.equal(redirect.status, 302);
      assert.equal(redirect.headers.location, '/login');
    },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});

guarded('pipes upgrades to whichever upstream owns the path', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  await withMux(
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      const preview = await rawUpgrade('/preview/_next/webpack-hmr', Buffer.from('hmr-frame'));
      assert.match(preview.head, /^HTTP\/1\.1 101/);
      assert.equal(preview.body.toString(), 'echo:hmr-frame');
      assert.equal(appUpstream.seen.at(-1).url, '/preview/_next/webpack-hmr');
      assert.equal(appUpstream.seen.at(-1).method, 'UPGRADE');
      assert.equal(appUpstream.seen.at(-1).headers.authorization, undefined, 'no bearer to the app');
      assert.equal(appUpstream.seen.at(-1).headers.upgrade, 'websocket');

      const shell = await rawUpgrade('/session/ws', Buffer.from('shell-frame'));
      assert.match(shell.head, /^HTTP\/1\.1 101/);
      assert.equal(daemonUpstream.seen.at(-1).headers.authorization, 'Bearer secret-token');
    },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});

guarded('streams a server-sent response and 502s a dead app without touching the daemon', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);
  const appPort = appUpstream.port();
  await new Promise((resolve) => appUpstream.server.close(resolve));

  const stderr = await withMux(
    `http://127.0.0.1:${appPort}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      const dead = await request('/preview/');
      assert.equal(dead.status, 502);
      assert.match(dead.text, /next dev -H 0\.0\.0\.0/);
      assert.equal(daemonUpstream.seen.length, 0, 'a dead app must not fall through to the daemon');

      const shell = await request('/stream');
      assert.equal(shell.headers['content-type'], 'text/event-stream');
      assert.equal(shell.text, 'data: one\n\ndata: two\n\n');
    },
  );

  assert.equal(stderr, '', `mux logged to stderr: ${stderr}`);
  daemonUpstream.server.close();
});

guarded('refuses to start with a prefix that cannot be routed safely', async () => {
  for (const bad of ['/', 'preview', '/preview/', '']) {
    const code = await new Promise((resolve) => {
      const child = spawn(process.execPath, [MUX], {
        env: {
          ...process.env,
          AX_PREVIEW_TARGET: 'http://127.0.0.1:1',
          AX_PREVIEW_DAEMON_TARGET: 'http://127.0.0.1:1',
          AX_PREVIEW_BASE: bad,
          AX_PREVIEW_PORT: String(MUX_PORT + 1),
        },
        stdio: 'ignore',
      });
      child.on('exit', resolve);
    });
    assert.equal(code, 2, `AX_PREVIEW_BASE="${bad}" should exit 2`);
  }
});

// The hostname route is what makes the app embeddable at all: the Web Shell
// refuses to preview its own origin, so the app needs preview-<task> instead of
// <task>/preview. Owning an origin means being served from / with no base path.
guarded('gives a preview hostname the whole origin, bearer-free', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  const preview = { host: 'preview-my-task.norne' };
  await withMux(
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      const root = await request('/', {
        headers: { ...preview, authorization: 'Bearer secret-token' },
      });
      assert.equal(root.headers['x-upstream'], 'app');
      assert.equal(root.text, 'app:/');
      assert.equal(appUpstream.seen.at(-1).headers.authorization, undefined);
      assert.equal(appUpstream.seen.at(-1).headers.host, 'preview-my-task.norne');
      assert.equal(daemonUpstream.seen.length, 0, 'the preview hostname never reaches the daemon');

      // Absolute asset URLs work untouched, which is the point of the dedicated host.
      const asset = await request('/_next/static/chunks/app/layout.js', { headers: preview });
      assert.equal(asset.text, 'app:/_next/static/chunks/app/layout.js');

      // The app owns this origin, so a redirect to / stays /: there is no prefix
      // left to put it under.
      const redirect = await request('/redirect', { headers: preview });
      assert.equal(redirect.headers.location, '/login');

      const upgrade = await rawUpgrade('/_next/webpack-hmr', Buffer.from('hmr'), 'preview-my-task.norne');
      assert.match(upgrade.head, /^HTTP\/1\.1 101/);
      assert.equal(appUpstream.seen.at(-1).headers.authorization, undefined, 'no bearer to the app');

      // The task's own hostname keeps serving the shell.
      const shell = await request('/', { headers: { host: 'my-task.norne' } });
      assert.equal(shell.headers['x-upstream'], 'daemon');
    },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});

guarded('hostname routing is opt-out and the prefix is configurable', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  const targets = [
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
  ];

  await withMux(
    ...targets,
    async () => {
      // With hostname routing disabled the path prefix is the only way in, even on
      // a preview-shaped hostname.
      const root = await request('/', { headers: { host: 'preview-my-task.norne' } });
      assert.equal(root.headers['x-upstream'], 'daemon');
      const viaPath = await request('/preview/', { headers: { host: 'preview-my-task.norne' } });
      assert.equal(viaPath.headers['x-upstream'], 'app');
    },
    { AX_PREVIEW_HOST_PREFIX: '' },
  );

  await withMux(
    ...targets,
    async () => {
      const dev = await request('/', { headers: { host: 'dev-my-task.norne' } });
      assert.equal(dev.headers['x-upstream'], 'app');
      const notOurs = await request('/', { headers: { host: 'preview-my-task.norne' } });
      assert.equal(notOurs.headers['x-upstream'], 'daemon');
    },
    { AX_PREVIEW_HOST_PREFIX: 'dev-' },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});

guarded('tells the Web Shell which preview origin to open', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  await withMux(
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      // The name is derived from the Host the browser used; the scheme is left
      // to the page, because the runner's mTLS hop rewrites X-Forwarded-Proto
      // and a LAN request would otherwise be told it came in over https.
      const lan = await request('/__qwen-preview.json', { headers: { host: 'my-task.norne' } });
      assert.equal(lan.status, 200);
      assert.equal(lan.headers['cache-control'], 'no-store');
      assert.deepEqual(JSON.parse(lan.text), { host: 'preview-my-task.norne' });
      assert.equal(daemonUpstream.seen.length, 0, 'the mux answers this path itself');

      const tunneled = await request('/__qwen-preview.json', {
        headers: { host: 'my-task.norne.app', 'x-forwarded-proto': 'https' },
      });
      assert.deepEqual(
        JSON.parse(tunneled.text),
        { host: 'preview-my-task.norne.app' },
        'the forwarded protocol is not trusted',
      );

      // A Host that cannot take a label has no preview origin to name, so the
      // shell gets its 404 and opens no panel rather than a broken one.
      const bare = await request('/__qwen-preview.json', { headers: { host: '192.168.1.73' } });
      assert.equal(bare.status, 404);

      // Only a GET asks; anything else is not a document the shell would read.
      const posted = await request('/__qwen-preview.json', {
        method: 'POST',
        body: 'x',
        headers: { host: 'my-task.norne' },
      });
      assert.equal(posted.status, 404);

      // On the app's own hostname every path belongs to the app.
      const onApp = await request('/__qwen-preview.json', { headers: { host: 'preview-my-task.norne' } });
      assert.equal(onApp.headers['x-upstream'], 'app');
    },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});

guarded('allows a task to explicitly override the derived hostname', async () => {
  const appUpstream = upstream('app');
  const daemonUpstream = upstream('daemon');
  await listen(appUpstream.server);
  await listen(daemonUpstream.server);

  await withMux(
    `http://127.0.0.1:${appUpstream.port()}`,
    `http://127.0.0.1:${daemonUpstream.port()}`,
    async () => {
      const reachedByIp = await request('/__qwen-preview.json', { headers: { host: '192.168.1.73' } });
      assert.equal(JSON.parse(reachedByIp.text).url, 'https://preview-my-task.norne.app/');
    },
    { AX_PREVIEW_URL: 'https://preview-my-task.norne.app/' },
  );

  appUpstream.server.close();
  daemonUpstream.server.close();
});
