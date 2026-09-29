// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"
)

// targetActorHeader selects the destination actor in the atenet router. The
// router ignores Host and :authority entirely, so this header alone decides
// which task a request reaches.
const targetActorHeader = "ate-target-actor"

// actorKey carries the resolved actor from the dispatch handler into the
// reverse proxy's Rewrite hook.
type actorKey struct{}

// previewHostPrefix turns a task's hostname into the hostname its preview app
// is served on: preview-<task>.<base> reaches the same actor as <task>.<base>.
//
// The app needs a hostname of its own rather than a path under the task's. A
// browser treats scheme+host+port as the origin, so an app mounted at
// <task>.<base>/preview is the Web Shell's own origin, and the Web Shell's
// preview panel refuses to embed its own origin by design. One label in front of
// the task name buys the app a separate origin, which is also what lets the app
// be served from / instead of a base path. Inside the task the preview mux
// separates the two by this same Host prefix, because the atenet router only
// ever reaches the actor's port 80 and the runner forwards that one port to one
// target.
const previewHostPrefix = "preview-"

// dashboard routes an incoming request either to the task list or to a single
// task's Qwen Web Shell, based purely on the Host header. Every hostname under
// the base domain arrives at this one service, which is what lets an ingress
// rule for *.norne cover tasks that do not exist yet.
type dashboard struct {
	cfg   *config
	tasks *taskSource
	proxy *httputil.ReverseProxy
}

func newDashboard(cfg *config, tasks *taskSource) *dashboard {
	return &dashboard{
		cfg:   cfg,
		tasks: tasks,
		proxy: newSessionProxy(cfg),
	}
}

// newSessionProxy builds the reverse proxy that relays Web Shell traffic to the
// atenet router.
func newSessionProxy(cfg *config) *httputil.ReverseProxy {
	routerURL := cfg.routerURL
	return &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
			// No ResponseHeaderTimeout or TLSHandshakeTimeout: Web Shell
			// responses include long-lived server-sent event streams.
		},
		// Flush every write so streaming responses and terminal output reach the
		// browser immediately instead of being buffered.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(routerURL)
			// SetURL rewrites the outbound Host to the router's own host. The
			// browser-visible hostname has to survive instead: the Web Shell
			// rejects any request whose Origin does not match Host, including
			// every WebSocket upgrade. The router does not need Host, so
			// preserving it costs nothing.
			pr.Out.Host = pr.In.Host
			normalizeSelfOrigin(pr)
			if actor, ok := pr.In.Context().Value(actorKey{}).(string); ok && actor != "" {
				pr.Out.Header.Set(targetActorHeader, actor)
			}
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			host := hostOnly(r.Host)
			slog.Error("session proxy failed", "host", host, "error", err)
			renderError(cfg, w, r, http.StatusBadGateway, "Web Shell unreachable",
				"The task "+host+" could not be reached through the atenet router.", err.Error())
		},
	}
}

// normalizeSelfOrigin rewrites an https self-Origin to http before relaying to
// the task's qwen daemon. The daemon only strips the Origin header (and thus
// admits the request past its CORS gate) when Origin literally equals a URL it
// builds from its own plaintext socket — always "http://<Host>". Behind a
// TLS-terminating proxy such as a Cloudflare tunnel, the browser correctly
// sends "https://<host>"; same-origin by the browser's definition, the daemon
// still reads it as a cross-origin request and answers 403. Only an Origin
// whose authority matches the browser-visible hostname is rewritten, so true
// third-party origins stay exactly as they are and keep being rejected.
func normalizeSelfOrigin(pr *httputil.ProxyRequest) {
	origin := pr.Out.Header.Get("Origin")
	if origin == "" {
		return
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" {
		return
	}
	if !strings.EqualFold(hostOnly(u.Host), hostOnly(pr.In.Host)) {
		return
	}
	pr.Out.Header.Set("Origin", "http://"+u.Host)
}

func (d *dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(hostOnly(r.Host))

	if d.isDashboardHost(host) {
		d.serveDashboard(w, r)
		return
	}
	if label, ok := d.sessionLabel(host); ok {
		d.serveSession(w, r, label)
		return
	}
	// Anything else is an unrecognised hostname: still answer the probes, since
	// the kubelet addresses them to the pod IP rather than to a real hostname.
	d.serveUnroutedHost(w, r)
}

func (d *dashboard) isDashboardHost(host string) bool {
	return slices.Contains(d.cfg.dashHosts, host)
}

// sessionLabel extracts the task name from a <task-name>.<base-domain> host,
// matching against every configured base domain.
func (d *dashboard) sessionLabel(host string) (string, bool) {
	for _, base := range d.cfg.baseDomains {
		suffix := "." + base
		if !strings.HasSuffix(host, suffix) {
			continue
		}
		label := strings.TrimSuffix(host, suffix)
		// A task name is a single DNS-1123 label, so a hostname with another dot
		// in it is not something this dashboard can route to an actor.
		if strings.Contains(label, ".") || !isDNSLabel(label) {
			continue
		}
		return label, true
	}
	return "", false
}

// resolveTask maps a session hostname label to the task that answers it. A plain
// label is the task name. A preview- label is the preview hostname of the task
// named by the rest of the label.
//
// The order is deliberate: a task whose real name starts with preview- is found
// first, so an operator who names a task that way keeps their hostname, and the
// alias only applies when no such task exists.
func (d *dashboard) resolveTask(ctx context.Context, label string) (taskInfo, bool) {
	if task, ok := d.tasks.lookup(ctx, label); ok {
		return task, true
	}
	base, isPreview := strings.CutPrefix(label, previewHostPrefix)
	if !isPreview || !isDNSLabel(base) {
		return taskInfo{}, false
	}
	// A cache miss on the alias still has to force a refresh, the same way a
	// miss on a direct task name does, or a task created moments ago previews 404.
	return d.tasks.lookup(ctx, base)
}

// baseDomainFor returns the base domain a request arrived on, so that session
// links stay in the same domain the browser is already using: the LAN wildcard
// and a tunnel hostname both work, and neither leaks into the other.
func (d *dashboard) baseDomainFor(host string) string {
	host = strings.ToLower(hostOnly(host))
	for _, base := range d.cfg.baseDomains {
		if host == base || strings.HasSuffix(host, "."+base) {
			return base
		}
	}
	return d.cfg.baseDomains[0]
}

// serveSession proxies every path on a task's hostname to that task's Web Shell.
// The Web Shell serves absolute /assets URLs and has no base-path option, so it
// must be mounted at the root of its own hostname rather than under a prefix.
//
// A preview hostname is routed here identically: separating the app from the
// shell is the task's job (see previewHostPrefix), and the browser-visible Host
// is preserved on the way in, which is what the mux inside the task routes on.
func (d *dashboard) serveSession(w http.ResponseWriter, r *http.Request, label string) {
	task, ok := d.resolveTask(r.Context(), label)
	if !ok {
		message := "There is no AX task named " + label + " in the " + d.baseDomainFor(r.Host) + " domain."
		detail := "It may have been deleted, or the AX server may be unreachable."
		if base, isPreview := strings.CutPrefix(label, previewHostPrefix); isPreview {
			message = "No task to preview: there is no AX task named " + base + " in the " +
				d.baseDomainFor(r.Host) + " domain."
			detail = "A preview hostname is " + previewHostPrefix + "<task-name>." + d.baseDomainFor(r.Host) + "."
		}
		renderError(d.cfg, w, r, http.StatusNotFound, "Unknown task", message, detail)
		return
	}
	if !task.running() {
		renderError(d.cfg, w, r, http.StatusConflict, "Task is not running",
			"Task "+task.Name+" is in phase "+task.Phase+".",
			"Only Running tasks serve a Web Shell. Resume the task first if it is suspended.")
		return
	}
	if task.Actor == "" {
		renderError(d.cfg, w, r, http.StatusConflict, "Task has no actor",
			"Task "+task.Name+" is Running but has no actor assigned yet.", "")
		return
	}

	ctx := context.WithValue(r.Context(), actorKey{}, task.targetActor())
	d.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (d *dashboard) serveUnroutedHost(w http.ResponseWriter, r *http.Request) {
	if d.serveProbe(w, r) {
		return
	}
	renderError(d.cfg, w, r, http.StatusNotFound, "Not a dashboard or session host",
		"Hostname "+hostOnly(r.Host)+" does not match "+d.cfg.primaryHost()+
			" or <task-name>.{"+strings.Join(d.cfg.baseDomains, "|")+"}.", "")
}

// serveProbe answers the liveness and readiness endpoints and reports whether
// it handled the request.
func (d *dashboard) serveProbe(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
		return true
	case "/readyz":
		ok, err := d.tasks.health()
		if !ok {
			http.Error(w, "task list unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return true
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
		return true
	}
	return false
}

// isDNSLabel reports whether s is a valid DNS-1123 label, which is what AX task
// names and atenet actor names are required to be.
func isDNSLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return s[0] != '-' && s[len(s)-1] != '-'
}

// requestScheme reports the scheme the browser used, honouring the forwarding
// header an ingress controller adds.
func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = proto[:i]
		}
		if proto = strings.TrimSpace(proto); proto != "" {
			return proto
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// sessionURL builds the browser-facing URL for a task's Web Shell, in the same
// base domain the request arrived on.
func (d *dashboard) sessionURL(r *http.Request, task taskInfo) string {
	return d.urlForLabel(r, task.Name)
}

// previewURL is the task's preview hostname, or "" when the name is too long to
// carry the prefix and still be one DNS label. Returning "" rather than a broken
// hostname keeps the dashboard's links honest: the ingress wildcard and the
// router both demand a single label of at most 63 characters.
func (d *dashboard) previewURL(r *http.Request, task taskInfo) string {
	label := previewHostPrefix + task.Name
	if len(label) > 63 || !isDNSLabel(label) {
		return ""
	}
	return d.urlForLabel(r, label)
}

func (d *dashboard) urlForLabel(r *http.Request, label string) string {
	host := strings.ToLower(label) + "." + d.baseDomainFor(r.Host)
	if port := portOf(r.Host); port != "" && port != "80" && port != "443" {
		host = net.JoinHostPort(host, port)
	}
	return requestScheme(r) + "://" + host + "/"
}

// portOf returns the port from an HTTP Host header, or "" if there is none.
func portOf(hostport string) string {
	if hostport == "" {
		return ""
	}
	if _, port, err := net.SplitHostPort(hostport); err == nil {
		return port
	}
	return ""
}
