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
	baseDomain   string
	dashboardURL string
	refresh      time.Duration
	listLimit    int64
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
	flag.StringVar(&baseDomain, "base-domain", "norne", "parent domain; a task is served at <task-name>.<base-domain>")
	flag.StringVar(&dashboardURL, "dashboard-host", "ax.norne", "hostname that serves the task list instead of a session")
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
		"baseDomain", cfg.baseDomain,
		"dashboardHosts", strings.Join(cfg.dashboardHosts(), ", "),
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
	baseDomain = strings.ToLower(strings.TrimSpace(baseDomain))
	baseDomain = strings.TrimPrefix(baseDomain, ".")
	if baseDomain == "" {
		return nil, errors.New("base domain cannot be empty")
	}
	if !isDNSLabel(baseDomain) {
		return nil, errors.New("base domain must be a single DNS label: " + baseDomain)
	}

	dashboardHost = strings.ToLower(strings.TrimSpace(dashboardHost))
	if dashboardHost == "" {
		return nil, errors.New("dashboard host cannot be empty")
	}
	if !strings.HasSuffix(dashboardHost, "."+baseDomain) && dashboardHost != baseDomain {
		return nil, errors.New("dashboard host " + dashboardHost + " is not inside base domain " + baseDomain)
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
		baseDomain:   baseDomain,
		dashboardURL: dashboardHost,
		refresh:      refresh,
		listLimit:    listLimit,
	}, nil
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

// dashboardHosts returns every hostname that serves the task list rather than a
// session: the configured dashboard host plus the bare base domain, so that
// http://norne/ works as well as http://ax.norne/.
func (c *config) dashboardHosts() []string {
	hosts := []string{c.dashboardURL}
	if c.dashboardURL != c.baseDomain {
		hosts = append(hosts, c.baseDomain)
	}
	return hosts
}

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
