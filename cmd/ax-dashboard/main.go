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

// Command ax-dashboard lists the AX tasks running on a cluster and proxies
// each running task's Qwen Web Shell to its own hostname.
//
// Two constraints shape this service. AX keeps task state in Redis rather than
// in Kubernetes, so the task list comes from the AX gRPC API and not from the
// API server. And the worker pool's NetworkPolicy admits only the Substrate
// atenet router, so session traffic must be relayed through the router's
// ate-target-actor header instead of dialing a task's worker IP directly.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// config holds the resolved command-line and environment settings.
type config struct {
	listenAddr   string
	axServerAddr string
	routerURL    *url.URL
	// baseDomains lists every parent domain a task hostname may live under, so
	// the LAN wildcard and a Cloudflare Tunnel hostname can be served at once.
	// Sorted longest-first so the most specific domain wins a match.
	baseDomains []string
	// dashHosts are the hostnames that serve the task list, including the bare
	// apex of each base domain.
	dashHosts []string
	refresh   time.Duration
	listLimit int64
}

func main() {
	var (
		listenAddr   string
		axServerAddr string
		routerAddr   string
		baseDomain   string
		dashboardURL string
		refresh      time.Duration
		listLimit    int64
	)

	flag.StringVar(&listenAddr, "addr", ":8080", "HTTP listen address")
	flag.StringVar(&axServerAddr, "ax-server", "ax-server.ax-system.svc.cluster.local:8080", "AX API server gRPC address")
	flag.StringVar(&routerAddr, "router", "atenet-router.ate-system.svc.cluster.local:80", "atenet router address used to reach task actors")
	flag.StringVar(&baseDomain, "base-domain", "norne", "comma-separated parent domains; a task is served at <task-name>.<domain>")
	flag.StringVar(&dashboardURL, "dashboard-host", "ax.norne", "comma-separated hostnames that serve the task list instead of a session")
	flag.DurationVar(&refresh, "refresh", 3*time.Second, "how often the task list is re-read from the AX server")
	flag.Int64Var(&listLimit, "list-limit", 500, "maximum number of tasks to list from the AX server")
	flag.Parse()

	if v := os.Getenv("ADDR"); v != "" {
		listenAddr = v
	}
	if v := os.Getenv("AX_SERVER_ADDR"); v != "" {
		axServerAddr = v
	}
	if v := os.Getenv("ATENET_ROUTER_ADDR"); v != "" {
		routerAddr = v
	}
	if v := os.Getenv("AX_DASHBOARD_BASE_DOMAIN"); v != "" {
		baseDomain = v
	}
	if v := os.Getenv("AX_DASHBOARD_HOST"); v != "" {
		dashboardURL = v
	}
	if v := os.Getenv("AX_DASHBOARD_REFRESH"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			slog.Error("invalid AX_DASHBOARD_REFRESH", "value", v, "error", err)
			os.Exit(2)
		}
		refresh = parsed
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := newConfig(listenAddr, axServerAddr, routerAddr, baseDomain, dashboardURL, refresh, listLimit)
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	conn, err := grpc.NewClient(cfg.axServerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("connecting to AX server", "addr", cfg.axServerAddr, "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	tasks := newTaskSource(v1alpha1.NewAXClient(conn), cfg.listLimit, cfg.refresh)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Warm the cache before serving so the first browser request does not race
	// the initial ListTasks call. A failure here is not fatal: the AX server may
	// still be starting, and the poller retries.
	if err := tasks.refresh(ctx); err != nil {
		slog.Warn("initial task list failed; will retry", "error", err)
	}
	go tasks.poll(ctx)

	dash := newDashboard(cfg, tasks)

	slog.Info("starting ax-dashboard",
		"listenAddr", cfg.listenAddr,
		"axServer", cfg.axServerAddr,
		"router", cfg.routerURL.String(),
		"baseDomains", strings.Join(cfg.baseDomains, ", "),
		"dashboardHosts", strings.Join(cfg.dashHosts, ", "),
	)

	httpServer := &http.Server{
		Addr:    cfg.listenAddr,
		Handler: dash,
		// Deliberately no ReadTimeout or WriteTimeout: Web Shell sessions hold
		// WebSocket upgrades and server-sent event streams open indefinitely.
		ReadHeaderTimeout: 30 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		slog.Error("HTTP server failed", "error", err)
		stop()
		os.Exit(1)
	case <-ctx.Done():
		slog.Info("shutting down ax-dashboard")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Active Web Shell sessions are long-lived by design, so Shutdown waits
		// for them only up to the timeout above and then returns.
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("graceful shutdown incomplete", "error", err)
		}
	}
}

// newConfig validates the raw settings and resolves them into a config.
func newConfig(listenAddr, axServerAddr, routerAddr, baseDomain, dashboardHost string, refresh time.Duration, listLimit int64) (*config, error) {
	baseDomains := splitList(baseDomain)
	if len(baseDomains) == 0 {
		return nil, errors.New("base domain cannot be empty")
	}
	for _, domain := range baseDomains {
		if !isDomain(domain) {
			return nil, errors.New("base domain is not a valid hostname: " + domain)
		}
	}
	// Longest first so that when one base domain is a suffix of another, the
	// more specific one is matched first.
	sort.SliceStable(baseDomains, func(i, j int) bool {
		return len(baseDomains[i]) > len(baseDomains[j])
	})

	dashHosts := splitList(dashboardHost)
	if len(dashHosts) == 0 {
		return nil, errors.New("dashboard host cannot be empty")
	}
	for _, host := range dashHosts {
		if !isDomain(host) {
			return nil, errors.New("dashboard host is not a valid hostname: " + host)
		}
		if !insideAnyDomain(host, baseDomains) {
			return nil, errors.New("dashboard host " + host + " is not inside any base domain")
		}
	}
	// The bare apex of each base domain also serves the list, so http://norne/
	// works as well as http://ax.norne/.
	for _, domain := range baseDomains {
		if !slices.Contains(dashHosts, domain) {
			dashHosts = append(dashHosts, domain)
		}
	}

	routerURL, err := parseRouterAddr(routerAddr)
	if err != nil {
		return nil, err
	}

	if refresh < time.Second {
		return nil, errors.New("refresh interval must be at least 1s")
	}
	if listLimit < 1 {
		return nil, errors.New("list limit must be at least 1")
	}

	return &config{
		listenAddr:   listenAddr,
		axServerAddr: strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(axServerAddr), "http://"), "https://"),
		routerURL:    routerURL,
		baseDomains:  baseDomains,
		dashHosts:    dashHosts,
		refresh:      refresh,
		listLimit:    listLimit,
	}, nil
}

// splitList parses a comma-separated flag value into trimmed, lowercased,
// non-empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		part = strings.TrimPrefix(part, ".")
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// insideAnyDomain reports whether host is the domain itself or a name under it.
func insideAnyDomain(host string, domains []string) bool {
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// isDomain reports whether s is one or more DNS-1123 labels separated by dots.
func isDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !isDNSLabel(label) {
			return false
		}
	}
	return true
}

// parseRouterAddr accepts the bare host:port form used by ATENET_ROUTER_ADDR
// elsewhere in AX as well as an explicit http:// or https:// URL.
func parseRouterAddr(addr string) (*url.URL, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("router address cannot be empty")
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil, errors.New("invalid router address: " + err.Error())
	}
	if u.Host == "" {
		return nil, errors.New("router address must include a host")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("router address must be http or https")
	}
	return u, nil
}

// primaryHost is the first configured dashboard hostname, used for links and
// messages where the request's own host is not usable.
func (c *config) primaryHost() string { return c.dashHosts[0] }

// hostOnly strips the port from an HTTP Host header value.
func hostOnly(hostport string) string {
	if hostport == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}
