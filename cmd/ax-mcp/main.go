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
	"fmt"
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
	writes          writesConfig
}

// defaultWaModels is the launcher's model allowlist: only the cheap cloud
// flash model (defaulting to medium reasoning effort instead of the image's
// very-high, because flash bills every thought) and the local 27B may run
// assistant-launched tasks. Operator flags can widen or narrow it; the MCP
// caller cannot.
const defaultWaModels = "modelstudio=qwen3.8-flash:medium,ninfer=qwen3.8-27b"

const defaultRunnerImage = "localhost:5001/ax-qwen-task-runner@sha256:2f6a95d84435706a174e5e097443d8394d2e1898c4b2c03faacf75af6329bfb5"

func main() {
	var (
		listenAddr      string
		axServerAddr    string
		refresh         time.Duration
		listLimit       int64
		dashboardDomain string

		allowWrites    bool
		waAtespace     string
		waRunnerImage  string
		waDefaultModel string
		waModelCatalog string
		waMaxActive    int
	)

	flag.StringVar(&listenAddr, "addr", ":8080", "HTTP listen address")
	flag.StringVar(&axServerAddr, "ax-server", "ax-server.ax-system.svc.cluster.local:8080", "AX API server gRPC address")
	flag.DurationVar(&refresh, "refresh", 10*time.Second, "how often the task list is re-read from the AX server")
	flag.Int64Var(&listLimit, "list-limit", 500, "maximum number of tasks to list from the AX server")
	flag.StringVar(&dashboardDomain, "dashboard-domain", "norne", "base domain the ax-dashboard serves task Web Shells under; links are built as http://<task>.<domain>")

	flag.BoolVar(&allowWrites, "allow-writes", false, "enable the ax_launch_task and ax_delete_task mutating tools; off by default")
	flag.StringVar(&waAtespace, "wa-atespace", "default", "atespace the assistant-launched tasks are created in; it must hold the git and model Secrets")
	flag.StringVar(&waRunnerImage, "wa-runner-image", defaultRunnerImage, "pinned runner image the launcher always uses")
	flag.StringVar(&waDefaultModel, "wa-default-model", "qwen3.8-flash", "model used when the caller does not name one; must appear in --wa-models")
	flag.StringVar(&waModelCatalog, "wa-models", defaultWaModels, "comma-separated provider=model[:default-effort] allowlist for launched tasks")
	flag.IntVar(&waMaxActive, "wa-max-active", 3, "maximum assistant-launched tasks allowed to be active at once")
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
	if v := os.Getenv("AX_MCP_ALLOW_WRITES"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			slog.Error("invalid AX_MCP_ALLOW_WRITES", "value", v, "error", err)
			os.Exit(2)
		}
		allowWrites = parsed
	}
	if v := os.Getenv("AX_MCP_WA_ATESPACE"); v != "" {
		waAtespace = v
	}
	if v := os.Getenv("AX_MCP_WA_RUNNER_IMAGE"); v != "" {
		waRunnerImage = v
	}
	if v := os.Getenv("AX_MCP_WA_DEFAULT_MODEL"); v != "" {
		waDefaultModel = v
	}
	if v := os.Getenv("AX_MCP_WA_MODELS"); v != "" {
		waModelCatalog = v
	}
	if v := os.Getenv("AX_MCP_WA_MAX_ACTIVE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			slog.Error("invalid AX_MCP_WA_MAX_ACTIVE", "value", v, "error", err)
			os.Exit(2)
		}
		waMaxActive = n
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := newConfig(listenAddr, axServerAddr, refresh, listLimit, dashboardDomain,
		allowWrites, waAtespace, waRunnerImage, waDefaultModel, waModelCatalog, waMaxActive)
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

	srv := newMCPServer(tasks, cfg.dashboardDomain, cfg.writes)
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
		"allowWrites", cfg.writes.enabled,
		"waAtespace", cfg.writes.atespace,
		"waMaxActive", cfg.writes.maxActive,
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
func newConfig(listenAddr, axServerAddr string, refresh time.Duration, listLimit int64, dashboardDomain string,
	allowWrites bool, waAtespace, waRunnerImage, waDefaultModel, waModelCatalog string, waMaxActive int) (*config, error) {
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

	models, err := parseAllowedModels(waModelCatalog)
	if err != nil {
		return nil, err
	}
	if allowWrites {
		if waAtespace == "" {
			return nil, errors.New("wa-atespace cannot be empty when writes are enabled")
		}
		if waRunnerImage == "" {
			return nil, errors.New("wa-runner-image cannot be empty when writes are enabled")
		}
		if len(models) == 0 {
			return nil, errors.New("wa-models cannot be empty when writes are enabled")
		}
		var def allowedModel
		var found bool
		for _, m := range models {
			if strings.EqualFold(m.Model, waDefaultModel) {
				def, found = m, true
			}
		}
		if !found {
			return nil, errors.New("wa-default-model " + waDefaultModel + " is not in the wa-models allowlist")
		}
		if def.Provider == "ninfer" && len(models) > 1 {
			return nil, errors.New("wa-default-model must be a paid cloud model unless the allowlist is deliberately local-only")
		}
		if waMaxActive < 1 {
			return nil, errors.New("wa-max-active must be at least 1")
		}
	}

	return &config{
		listenAddr:      listenAddr,
		axServerAddr:    strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(axServerAddr), "http://"), "https://"),
		refresh:         refresh,
		listLimit:       listLimit,
		dashboardDomain: strings.ToLower(strings.Trim(strings.TrimSpace(dashboardDomain), ".")),
		writes: writesConfig{
			enabled:      allowWrites,
			runnerImage:  strings.TrimSpace(waRunnerImage),
			atespace:     strings.TrimSpace(waAtespace),
			maxActive:    waMaxActive,
			defaultModel: strings.TrimSpace(waDefaultModel),
			models:       models,
		},
	}, nil
}

// parseAllowedModels parses a comma-separated provider=model[:effort]
// allowlist. Entries are lowercased; an empty list is only acceptable when
// writes are off, which the caller checks. The effort suffix sets the
// per-model default applied when the caller does not name one.
func parseAllowedModels(value string) ([]allowedModel, error) {
	var out []allowedModel
	for _, part := range strings.Split(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		provider, rest, ok := strings.Cut(part, "=")
		if !ok || rest == "" {
			return nil, fmt.Errorf("invalid wa-models entry %q: expected provider=model[:effort]", part)
		}
		switch provider {
		case "modelstudio", "ninfer":
		default:
			return nil, fmt.Errorf("invalid wa-models entry %q: unknown provider %q", part, provider)
		}
		model, effort, _ := strings.Cut(rest, ":")
		if model == "" {
			return nil, fmt.Errorf("invalid wa-models entry %q: empty model id", part)
		}
		switch effort {
		case "", "none", "low", "medium", "xhigh":
		default:
			return nil, fmt.Errorf("invalid wa-models entry %q: unknown default effort %q", part, effort)
		}
		out = append(out, allowedModel{Model: model, Provider: provider, DefaultEffort: effort})
	}
	return out, nil
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
