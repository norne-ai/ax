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
	"strings"
	"sync"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
)

// conditionInfo is the MCP-facing view of a task condition.
type conditionInfo struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// taskInfo is the cached view of an AX task. It carries only fields safe to
// return to an MCP client: the task spec's environment variables are never
// included, because a task's values may hold credentials.
type taskInfo struct {
	Name             string
	Atespace         string
	Phase            string
	Actor            string
	Created          time.Time
	PromptTokens     int32
	CompletionTokens int32
	PendingAction    string
	Conditions       []conditionInfo
}

// problem returns a one-line description of the first failed condition, or ""
// when the task shows no failure.
func (t taskInfo) problem() string {
	for _, c := range t.Conditions {
		if strings.EqualFold(c.Status, "False") {
			msg := strings.TrimSpace(c.Reason + " " + c.Message)
			if msg == "" {
				msg = c.Type
			}
			return msg
		}
	}
	return ""
}

// taskClient is the slice of the AX gRPC API the MCP server needs. Depending
// on the narrow interface rather than v1alpha1.AXClient keeps the cache and
// tools testable without stubbing twenty unrelated RPCs.
type taskClient interface {
	ListTasks(ctx context.Context, in *v1alpha1.ListTasksRequest, opts ...grpc.CallOption) (*v1alpha1.ListTasksResponse, error)
	GetTask(ctx context.Context, in *v1alpha1.GetTaskRequest, opts ...grpc.CallOption) (*v1alpha1.Task, error)
	UpdateWorkspace(ctx context.Context, in *v1alpha1.UpdateWorkspaceRequest, opts ...grpc.CallOption) (*v1alpha1.Workspace, error)
	UpdateTask(ctx context.Context, in *v1alpha1.UpdateTaskRequest, opts ...grpc.CallOption) (*v1alpha1.Task, error)
	DeleteTask(ctx context.Context, in *v1alpha1.DeleteTaskRequest, opts ...grpc.CallOption) (*v1alpha1.DeleteTaskResponse, error)
}

// taskSource caches the AX task list. The AX server has no list-watch RPC and
// WatchTask is per-task and self-terminating, so the server polls instead.
// Single-task detail deliberately bypasses this cache and calls GetTask, so an
// answer about one task is never older than the request that asked for it.
type taskSource struct {
	client taskClient
	limit  int64
	ttl    time.Duration

	mu      sync.RWMutex
	tasks   []taskInfo
	byName  map[string]taskInfo
	fetched time.Time
	err     error

	// refreshMu serialises on-demand refreshes so a burst of lookups for an
	// unknown task cannot each trigger their own ListTasks call.
	refreshMu  sync.Mutex
	lastForced time.Time
}

func newTaskSource(client taskClient, limit int64, ttl time.Duration) *taskSource {
	return &taskSource{
		client: client,
		limit:  limit,
		ttl:    ttl,
		byName: map[string]taskInfo{},
	}
}

// poll refreshes the cache on the configured interval until ctx is done.
func (s *taskSource) poll(ctx context.Context) {
	ticker := time.NewTicker(s.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.refresh(ctx); err != nil {
				slog.Warn("task list refresh failed", "error", err)
			}
		}
	}
}

// refresh re-reads the task list. An empty atespace asks the AX server for
// every atespace rather than just the default one.
func (s *taskSource) refresh(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := s.client.ListTasks(callCtx, &v1alpha1.ListTasksRequest{Atespace: "", Limit: s.limit})

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		s.err = err
		return err
	}

	tasks := make([]taskInfo, 0, len(resp.GetTasks()))
	byName := make(map[string]taskInfo, len(resp.GetTasks()))
	for _, task := range resp.GetTasks() {
		info := toTaskInfo(task)
		if info.Name == "" {
			continue
		}
		tasks = append(tasks, info)
		// The server returns tasks newest-first, which makes the first entry
		// win if two atespaces ever reuse a name.
		key := strings.ToLower(info.Name)
		if existing, dup := byName[key]; dup && existing.Atespace != info.Atespace {
			slog.Warn("task name is ambiguous across atespaces; using the newest",
				"name", info.Name, "atespace", existing.Atespace, "shadowedAtespace", info.Atespace)
			continue
		}
		byName[key] = info
	}

	s.tasks = tasks
	s.byName = byName
	s.fetched = time.Now()
	s.err = nil
	return nil
}

func toTaskInfo(task *v1alpha1.Task) taskInfo {
	status := task.GetStatus()
	meta := task.GetMetadata()

	info := taskInfo{
		Name:             meta.GetName(),
		Atespace:         meta.GetAtespace(),
		Phase:            status.GetPhase(),
		Actor:            status.GetActor(),
		PromptTokens:     status.GetUsage().GetPromptTokens(),
		CompletionTokens: status.GetUsage().GetCompletionTokens(),
		PendingAction:    status.GetPendingApproval().GetAction(),
	}
	if ts := meta.GetCreationTimestamp(); ts != nil {
		info.Created = ts.AsTime()
	}
	for _, cond := range status.GetConditions() {
		info.Conditions = append(info.Conditions, conditionInfo{
			Type:    cond.GetType(),
			Status:  cond.GetStatus(),
			Reason:  cond.GetReason(),
			Message: cond.GetMessage(),
		})
	}
	if info.Atespace == "" {
		info.Atespace = "default"
	}
	return info
}

// snapshot returns the cached task list along with when it was read and the
// last error seen, if any.
func (s *taskSource) snapshot() ([]taskInfo, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tasks := make([]taskInfo, len(s.tasks))
	copy(tasks, s.tasks)
	return tasks, s.fetched, s.err
}

// lookup finds a task by name, case-insensitively, and returns its atespace.
// A miss forces one refresh so that a task created seconds ago is reachable
// immediately; the refresh is rate-limited to at most one per second. The
// bool is false when no atespace is known yet, in which case callers fall
// back to the default atespace.
func (s *taskSource) lookup(ctx context.Context, name string) (taskInfo, bool) {
	if info, ok := s.cached(name); ok {
		return info, true
	}

	s.refreshMu.Lock()
	if time.Since(s.lastForced) >= time.Second {
		s.lastForced = time.Now()
		if err := s.refresh(ctx); err != nil {
			slog.Warn("on-demand task list refresh failed", "task", name, "error", err)
		}
	}
	s.refreshMu.Unlock()

	info, ok := s.cached(name)
	return info, ok
}

func (s *taskSource) cached(name string) (taskInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.byName[strings.ToLower(name)]
	return info, ok
}

// taskDetail fetches one task straight from the AX server and adds what only
// the spec carries: the runner image, command, and per-workspace goals.
func (s *taskSource) taskDetail(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return s.client.GetTask(callCtx, &v1alpha1.GetTaskRequest{Atespace: atespace, Name: name})
}

// health reports whether the cache has ever been populated and, if not, why.
func (s *taskSource) health() (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil {
		return false, s.err
	}
	return true, nil
}
