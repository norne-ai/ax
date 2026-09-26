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

package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/agent-substrate/env/guest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"
)

// Server is the cloud-style metadata server running inside the task actor container,
// multiplexing HTTP metadata endpoints and guest gRPC daemon services on a single port.
type Server struct {
	port           int
	server         *http.Server
	grpcServer     *grpc.Server
	grpcCleanup    func()
	mu             sync.RWMutex
	task           *v1alpha1.Task
	workspaces     []*v1alpha1.Workspace
	workspaceReady bool
}

// ServerOptions configures optional settings for the metadata and guest server.
type ServerOptions struct {
	WorkspacePath string
	LogDir        string
	// HTTPProxyTarget receives HTTP requests that do not match the runner's
	// health or metadata endpoints. This lets an agent UI share port 80 with
	// the runner and guest gRPC service.
	HTTPProxyTarget string
	// HTTPProxyBearer is injected into proxied requests and is never returned
	// to the caller. It is intended for a loopback agent daemon such as Qwen
	// Serve whose API must remain authenticated inside the task.
	HTTPProxyBearer string
}

// NewServer creates a new metadata and guest server serving the task and its
// bound workspaces, in the task's declaration order. Nil workspaces are dropped.
func NewServer(port int, task *v1alpha1.Task, workspaces []*v1alpha1.Workspace, opts ...ServerOptions) *Server {
	if port <= 0 {
		port = 9999
	}
	s := &Server{
		port:       port,
		task:       task,
		workspaces: compactWorkspaces(workspaces),
	}

	var opt ServerOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	// Guest services expose process execution and file access inside the container,
	// so they are only served when the task opts in via spec.debug.
	if task.GetSpec().GetDebug() {
		guestCfg := guest.DefaultConfig()
		if opt.WorkspacePath != "" {
			guestCfg.Workspace = opt.WorkspacePath
		}
		if opt.LogDir != "" {
			guestCfg.LogDir = opt.LogDir
		}

		grpcServer, grpcCleanup, err := guest.NewServer(guestCfg)
		if err != nil {
			slog.Error("failed to initialize guest gRPC server", "error", err)
		} else {
			s.grpcServer = grpcServer
			s.grpcCleanup = grpcCleanup
			slog.Info("guest services enabled (spec.debug is true)")
		}
	} else {
		slog.Info("guest services disabled; set spec.debug: true on the Task to enable ax ssh")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)

	// Metadata endpoints:
	// /metadata/v1alpha1/ax/task        the Task
	// /metadata/v1alpha1/ax/workspaces  every bound Workspace, as a YAML stream
	mux.HandleFunc("/metadata/v1alpha1/ax/task", s.handleTask)
	mux.HandleFunc("/metadata/v1alpha1/ax/workspaces", s.handleWorkspaces)
	if opt.HTTPProxyTarget != "" {
		target, err := url.Parse(opt.HTTPProxyTarget)
		if err != nil || target.Scheme == "" || target.Host == "" {
			slog.Error("invalid HTTP proxy target; UI proxy disabled", "target", opt.HTTPProxyTarget, "error", err)
		} else {
			proxy := httputil.NewSingleHostReverseProxy(target)
			originalDirector := proxy.Director
			proxy.Director = func(r *http.Request) {
				originalDirector(r)
				// The router-only header should not escape into the agent daemon.
				r.Header.Del("ate-target-actor")
				if opt.HTTPProxyBearer != "" {
					r.Header.Set("Authorization", "Bearer "+opt.HTTPProxyBearer)
				}
			}
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, proxyErr error) {
				slog.Warn("agent UI proxy request failed", "path", r.URL.Path, "error", proxyErr)
				http.Error(w, "agent UI is not ready", http.StatusBadGateway)
			}
			mux.Handle("/", proxy)
			slog.Info("agent UI proxy enabled", "target", target.Redacted())
		}
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.grpcServer != nil && r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			s.grpcServer.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: handler,
	}
	s.server.Protocols = new(http.Protocols)
	s.server.Protocols.SetHTTP1(true)
	s.server.Protocols.SetUnencryptedHTTP2(true)

	return s
}

// Start begins serving the metadata HTTP and guest gRPC service in a background goroutine.
func (s *Server) Start() error {
	lis, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("metadata server listening on %s: %w", s.server.Addr, err)
	}

	slog.Info("metadata and guest server started", "addr", s.server.Addr)

	go func() {
		if err := s.server.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metadata server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully stops the metadata and guest server.
func (s *Server) Stop(ctx context.Context) error {
	if s.grpcCleanup != nil {
		s.grpcCleanup()
	}
	return s.server.Shutdown(ctx)
}

// UpdateState allows dynamic updates to the current Task and Workspaces.
func (s *Server) UpdateState(task *v1alpha1.Task, workspaces []*v1alpha1.Workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task = task
	s.workspaces = compactWorkspaces(workspaces)
}

// compactWorkspaces returns a copy of workspaces without nil entries.
func compactWorkspaces(workspaces []*v1alpha1.Workspace) []*v1alpha1.Workspace {
	out := make([]*v1alpha1.Workspace, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws != nil {
			out = append(out, ws)
		}
	}
	return out
}

// SetWorkspaceReady updates whether the maiden run workspace setup has completed.
func (s *Server) SetWorkspaceReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaceReady = ready
}

// IsWorkspaceReady returns whether the workspace setup has completed.
func (s *Server) IsWorkspaceReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workspaceReady
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.workspaceReady
	s.mu.RUnlock()

	if !ready {
		http.Error(w, "workspace initializing", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	task := s.task
	s.mu.RUnlock()

	if task == nil {
		http.Error(w, "task metadata not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/yaml")
	if err := yaml.NewEncoder(w).Encode(task); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleWorkspaces serves every bound workspace as a multi-document YAML stream
// in the task's declaration order.
func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	workspaces := s.workspaces
	s.mu.RUnlock()

	if len(workspaces) == 0 {
		http.Error(w, "workspace metadata not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/yaml")
	enc := yaml.NewEncoder(w)
	for _, ws := range workspaces {
		if err := enc.Encode(ws); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := enc.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
