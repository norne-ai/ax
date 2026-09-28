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

// Command ax-mcp serves the AX task list to MCP clients, so an agent such as
// the Hermes family assistant can ask "what is running?" without an AX CLI or
// cluster credentials.
//
// Two constraints shape this service, both shared with ax-dashboard. Task
// state lives in the AX server rather than in Kubernetes, so it comes from
// the AX gRPC API; and the server is stateless per MCP request, because
// Hermes connects over Streamable HTTP from another namespace and no session
// needs to survive between tool calls.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// config holds the resolved command-line and environment settings.
type config struct {
	listenAddr      string
	axServerAddr    string
	refresh         time.Duration
	listLimit       int64
	dashboardDomain string
}

func main() {
	var (
		listenAddr      string
		axServerAddr    string
		refresh         time.Duration
		listLimit       int64
		dashboardDomain string
	)

	flag.StringVar(&listenAddr, "addr", ":8080", "HTTP listen address")
	flag.StringVar(&axServerAddr, "ax-server", "ax-server.ax-system.svc.cluster.local:8080", "AX API server gRPC address")
	flag.DurationVar(&refresh, "refresh", 10*time.Second, "how often the task list is re-read from the AX server")
	flag.Int64Var(&listLimit, "list-limit", 500, "maximum number of tasks to list from the AX server")
	flag.StringVar(&dashboardDomain, "dashboard-domain", "norne", "base domain the ax-dashboard serves task Web Shells under; links are built as http://<task>.<domain>")
	flag.Parse()

	if v := os.Getenv("ADDR"); v != "" {
		listenAddr = v
	}
	if v := os.Getenv("AX_SERVER_ADDR"); v != "" {
		axServerAddr = v
	}
	if v := os.Getenv("AX_MCP_REFRESH"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			slog.Error("invalid AX_MCP_REFRESH", "value", v, "error", err)
			os.Exit(2)
		}
		refresh = parsed
	}
	if v := os.Getenv("AX_MCP_LIST_LIMIT"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			slog.Error("invalid AX_MCP_LIST_LIMIT", "value", v, "error", err)
			os.Exit(2)
		}
		listLimit = n
	}
	if v := os.Getenv("AX_MCP_DASHBOARD_DOMAIN"); v != "" {
		dashboardDomain = v
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := newConfig(listenAddr, axServerAddr, refresh, listLimit, dashboardDomain)
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

	// Warm the cache before serving so the first tool call does not race the
	// initial ListTasks. A failure here is not fatal: the AX server may still
	// be starting, and the poller retries.
	if err := tasks.refresh(ctx); err != nil {
		slog.Warn("initial task list failed; will retry", "error", err)
	}
	go tasks.poll(ctx)

	srv := newMCPServer(tasks, cfg.dashboardDomain)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	))
	mux.HandleFunc("/healthz", healthHandler(tasks))

	slog.Info("starting ax-mcp",
		"listenAddr", cfg.listenAddr,
		"axServer", cfg.axServerAddr,
		"refresh", cfg.refresh,
		"dashboardDomain", cfg.dashboardDomain,
	)

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
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
		slog.Info("shutting down ax-mcp")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("graceful shutdown incomplete", "error", err)
		}
	}
}

// newConfig validates the raw settings and resolves them into a config.
func newConfig(listenAddr, axServerAddr string, refresh time.Duration, listLimit int64, dashboardDomain string) (*config, error) {
	if listenAddr == "" {
		return nil, errors.New("listen address cannot be empty")
	}
	if axServerAddr == "" {
		return nil, errors.New("AX server address cannot be empty")
	}
	if refresh < time.Second {
		return nil, errors.New("refresh interval must be at least 1s")
	}
	if listLimit < 1 {
		return nil, errors.New("list limit must be at least 1")
	}
	return &config{
		listenAddr:      listenAddr,
		axServerAddr:    strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(axServerAddr), "http://"), "https://"),
		refresh:         refresh,
		listLimit:       listLimit,
		dashboardDomain: strings.ToLower(strings.Trim(strings.TrimSpace(dashboardDomain), ".")),
	}, nil
}

// healthHandler reports process liveness plus task-cache state. The AX server
// being unreachable does not fail the probe: the pod is still the right place
// to answer MCP requests once AX recovers, and the cache degrades visibly in
// each tool response instead.
func healthHandler(tasks *taskSource) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		cached, fetched, err := tasks.snapshot()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"ok":            err == nil,
			"tasks":         len(cached),
			"fetchedAt":     formatTime(fetched),
			"lastListError": errString(err),
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
