#!/usr/bin/env node
// Share a task's single routed port with a preview app.
//
// The atenet router only ever reaches an actor on port 80, and the AX runner
// owns that port with exactly one HTTPProxyTarget. So a dev server running
// beside the Qwen daemon cannot get its own port: it has to share the task's
// single routed port, and this mux is the thing that decides who gets a request.
//
// Routes, in priority order:
//   Host starts with AX_PREVIEW_HOST_PREFIX  -> AX_PREVIEW_TARGET, whole origin
//   AX_PREVIEW_BASE/*                        -> AX_PREVIEW_TARGET   (bearer stripped)
//   /__qwen-preview.json                     -> where the app is reachable, as JSON
//   everything else                          -> AX_PREVIEW_DAEMON_TARGET (bearer kept)
//
// The hostname route is the one a browser preview panel needs: an app under a
// path of the Web Shell's own hostname is the same origin, and the Web Shell
// refuses to preview its own origin. A dedicated preview- hostname gives the app
// a real origin and lets it be served from / with no base path at all.
//
// /__qwen-preview.json is how the forked Web Shell learns that it has a preview
// to open on load, so the panel appears with the address already filled in. It
// publishes nothing secret: the same URL is already visible in the dashboard's
// task list, and Access still fronts the tunnel hostnames.
//
// The path route remains as the fallback for reaching the app by URL bar or a
// separate tab when no preview hostname is available. The app is expected to
// serve itself under AX_PREVIEW_BASE there (Next.js `basePath`), so paths are
// forwarded unchanged rather than rewritten.
// WebSocket upgrades (HMR, terminals) are piped to whichever route owns them.

import http from 'node:http';
import net from 'node:net';

const env = process.env;

function required(name) {
  const value = env[name];
  if (!value) {
    process.stderr.write(`qwen-preview-mux: ${name} is not set\n`);
    process.exit(2);
  }
  return value;
}

const base = (() => {
  // `??` and not `||`: a Task that sets AX_PREVIEW_BASE="" is a manifest error,
  // and silently routing the whole origin to the app would be a bad way to
  // swallow it.
  const raw = env.AX_PREVIEW_BASE ?? '/preview';
  if (!raw.startsWith('/') || raw === '/' || raw.endsWith('/')) {
    process.stderr.write(
      `qwen-preview-mux: AX_PREVIEW_BASE must be a path prefix starting with "/" ` +
      `and not "/" or ending with "/" (got "${raw}")\n`,
    );
    process.exit(2);
  }
  return raw;
})();

const port = Number(env.AX_PREVIEW_PORT ?? 8099);
if (!Number.isInteger(port) || port <= 0 || port > 65535) {
  process.stderr.write(`qwen-preview-mux: invalid AX_PREVIEW_PORT "${env.AX_PREVIEW_PORT}"\n`);
  process.exit(2);
}

// The dashboard routes preview-<task>.<base> to this same actor and preserves
// the browser's Host on the way in, so this prefix is what separates the app's
// origin from the Web Shell's. "" disables hostname routing and leaves the path
// prefix only.
const hostPrefix = (env.AX_PREVIEW_HOST_PREFIX ?? 'preview-').toLowerCase();

function parseTarget(name) {
  let url;
  try {
    url = new URL(required(name));
  } catch {
    process.stderr.write(`qwen-preview-mux: ${name} is not an absolute URL\n`);
    process.exit(2);
  }
  // Both targets are loopback listeners in this container. TLS upstreams would
  // need certificate handling the task cannot satisfy, and the upgrade path
  // below speaks plain TCP.
  if (url.protocol !== 'http:') {
    process.stderr.write(`qwen-preview-mux: ${name} must be an http:// loopback URL\n`);
    process.exit(2);
  }
  return url;
}

const app = parseTarget('AX_PREVIEW_TARGET');
const daemon = parseTarget('AX_PREVIEW_DAEMON_TARGET');

// Hop-by-hop headers belong to one transport leg, so they must never cross the
// proxy. Chunked framing in particular: Node re-frames the body itself.
const HOP_BY_HOP = new Set([
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
]);

function filterHeaders(source, { dropAuthorization }) {
  const out = {};
  for (const [name, value] of Object.entries(source)) {
    if (HOP_BY_HOP.has(name)) continue;
    if (dropAuthorization && name === 'authorization') continue;
    out[name] = value;
  }
  return out;
}

// A protocol switch is the exception to the hop-by-hop rule: `Upgrade` and
// `Connection` are the request, so forwarding an upgrade means copying the
// client headers as they arrived and touching only the credential.
function filterUpgradeHeaders(source, { dropAuthorization }) {
  const out = { ...source };
  if (dropAuthorization) delete out.authorization;
  return out;
}

function upstreamPort(url) {
  return Number(url.port) || 80;
}

// A redirect out of the prefix would land the browser on the Web Shell, so any
// app-originated absolute path gets the prefix back.
function rewriteLocation(location) {
  if (typeof location !== 'string') return location;
  if (location.startsWith(base)) return location;
  if (location.startsWith('/')) return base + location;
  return location;
}

function forward(req, res, target, { dropAuthorization, prefixRedirects }) {
  const upstream = http.request(
    {
      protocol: target.protocol,
      hostname: target.hostname,
      port: upstreamPort(target),
      // No agent pooling: a dev server restart must not be masked by a stale
      // keep-alive socket. The browser-visible Host must survive instead of
      // becoming the upstream's, because the daemon checks Origin against it
      // and Next.js derives its own origin from it.
      agent: false,
      path: req.url,
      method: req.method,
      headers: {
        ...filterHeaders(req.headers, { dropAuthorization }),
        host: req.headers.host,
      },
    },
    (response) => {
      const headers = { ...response.headers };
      if (prefixRedirects && headers.location) headers.location = rewriteLocation(headers.location);
      res.writeHead(response.statusCode ?? 502, filterHeaders(headers, { dropAuthorization: false }));
      response.pipe(res);
    },
  );

  upstream.on('error', (error) => {
    if (res.headersSent) {
      res.destroy();
      return;
    }
    const hint =
      target === app
        ? `Start the app there, for example: next dev -H 0.0.0.0 -p ${upstreamPort(app)}`
        : 'The Qwen daemon is not up yet; retry once the task has finished starting.';
    res.writeHead(502, { 'content-type': 'text/plain; charset=utf-8', 'x-preview-upstream': target.href });
    res.end(`upstream ${target.href} is not reachable (${error.code ?? error.message}).\n${hint}\n`);
  });

  // Piping a bodiless GET makes Node frame it as `Transfer-Encoding: chunked`,
  // which strict upstreams reject. End the request unless the client declared a
  // body, and let the pipe carry it when there is one.
  const hasBody =
    req.headers['content-length'] !== undefined || req.headers['transfer-encoding'] !== undefined;
  if (hasBody) {
    req.pipe(upstream);
  } else {
    upstream.end();
  }
}

function hostnameOf(host) {
  if (!host) return '';
  const value = host.toLowerCase();
  // An IPv6 literal is bracketed, so its port colon is not the last one.
  if (value.startsWith('[')) {
    const end = value.indexOf(']');
    return end === -1 ? value : value.slice(0, end + 1);
  }
  const colon = value.lastIndexOf(':');
  return colon === -1 ? value : value.slice(0, colon);
}

// 'host' means the request arrived on the task's preview hostname, so the app
// owns the whole origin and needs no base path. 'path' means it is under the
// base prefix on the shared hostname. null means the Web Shell owns it.
function previewRoute(req) {
  if (hostPrefix && hostnameOf(req.headers.host).startsWith(hostPrefix)) return 'host';
  const url = req.url;
  if (url === base || url.startsWith(`${base}/`) || url.startsWith(`${base}?`)) return 'path';
  return null;
}

const configPath = '/__qwen-preview.json';

// What scheme a browser should use is not visible from in here: the runner is
// reached over mTLS and rewrites X-Forwarded-Proto, so a plain HTTP request on
// the LAN arrives marked https. By default this therefore returns a bare
// hostname, which the page fills in with the scheme and port it was itself
// loaded over. AX_PREVIEW_URL remains an explicit escape hatch for Tasks whose
// public preview address cannot be derived from the incoming Host; callers must
// ensure such an absolute address is valid in every access zone they support.
function previewAddressFor(req) {
  if (env.AX_PREVIEW_URL) return { url: env.AX_PREVIEW_URL };
  if (!hostPrefix) return undefined;
  const host = hostnameOf(req.headers.host);
  // A bare address cannot take a label, so an IP Host has no preview origin to
  // name; only a dns name with a zone to hang the prefix on works.
  if (!host || net.isIP(host) || !host.includes('.')) return undefined;
  return { host: `${hostPrefix}${host}` };
}

function previewConfig(res, address) {
  if (!address) {
    res.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' });
    res.end('no preview hostname for this request\n');
    return;
  }
  res.writeHead(200, {
    'content-type': 'application/json',
    'cache-control': 'no-store',
  });
  res.end(`${JSON.stringify(address)}\n`);
}

const server = http.createServer((req, res) => {
  const route = previewRoute(req);
  if (route) {
    // The daemon's code-execution bearer has no business reaching an app that
    // serves the whole LAN and, through the tunnel, the internet.
    forward(req, res, app, { dropAuthorization: true, prefixRedirects: route === 'path' });
  } else if (req.url === configPath) {
    previewConfig(res, req.method === 'GET' ? previewAddressFor(req) : undefined);
  } else {
    // The shell's own redirects are already correct for its origin, which this
    // mux shares, so they must pass through untouched.
    forward(req, res, daemon, { dropAuthorization: false, prefixRedirects: false });
  }
});

// The daemon's session event stream is server-sent and never ends, so a request
// timeout would cut it every N seconds. Node defaults requestTimeout to 300s.
server.requestTimeout = 0;
server.headersTimeout = 120_000;
server.keepAliveTimeout = 60_000;

server.on('upgrade', (req, socket, head) => {
  const route = previewRoute(req);
  const target = route ? app : daemon;
  const headers = filterUpgradeHeaders(req.headers, { dropAuthorization: !!route });
  const upstream = net.connect(upstreamPort(target), target.hostname, () => {
    const lines = Object.entries(headers).map(([name, value]) => `${name}: ${value}`);
    upstream.write(`${req.method} ${req.url} HTTP/1.1\r\n${lines.join('\r\n')}\r\n\r\n`);
    if (head?.length) upstream.write(head);
    socket.pipe(upstream).pipe(socket);
  });
  upstream.on('error', () => socket.destroy());
  socket.on('error', () => upstream.destroy());
});

server.listen(port, '127.0.0.1', () => {
  process.stdout.write(
    `qwen-preview-mux on 127.0.0.1:${port}: ` +
    `${hostPrefix ? `host ${hostPrefix}* -> ` : ''}${app.href} ` +
    `| ${base}/* -> ${app.href} | * -> ${daemon.href}\n`,
  );
});
