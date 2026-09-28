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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestSession starts the MCP endpoint over httptest with a populated task
// cache and connects a real SDK client to it, exercising the stateless
// Streamable HTTP handshake for every test. Writes are off, so only the three
// read-only tools are registered.
func newTestSession(t *testing.T, fake *fakeClient) (*mcp.ClientSession, *taskSource) {
	t.Helper()
	return newSessionWithWrites(t, fake, writesConfig{})
}

// newSessionWithWrites is newTestSession with an explicit write config, used
// by the launch/delete tests to register the mutating tools.
func newSessionWithWrites(t *testing.T, fake *fakeClient, writes writesConfig) (*mcp.ClientSession, *taskSource) {
	t.Helper()
	ctx := context.Background()

	src := newTaskSource(fake, 500, time.Minute)
	if err := src.refresh(ctx); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	srv := newMCPServer(src, "norne", writes)
	httpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	ts := httptest.NewServer(httpHandler)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, src
}

// enabledWrites returns a valid write config for tests that opt into the
// mutating tools.
func enabledWrites() writesConfig {
	return writesConfig{
		enabled:      true,
		runnerImage:  "localhost:5001/ax-qwen-task-runner@sha256:test",
		atespace:     "default",
		maxActive:    3,
		defaultModel: "qwen3.8-flash",
		models: []allowedModel{
			{Model: "qwen3.8-flash", Provider: "modelstudio"},
			{Model: "qwen3.8-max", Provider: "modelstudio"},
			{Model: "qwen3.8-27b", Provider: "ninfer"},
		},
	}
}

func decodeStructured(t *testing.T, raw any, dst any) {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}

func callText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func sampleTasks() *fakeClient {
	now := time.Now().UTC().Truncate(time.Second)
	running := pbTask("fix-build", "default", "Running", now.Add(-90*time.Second))
	running.Status.PendingApproval = &v1alpha1.PendingApproval{Action: "exec: rm -rf"}
	suspended := pbTask("nightly", "team", "Suspended", now.Add(-3*time.Hour))
	failed := pbTask("spike", "team", "Failed", now.Add(-time.Hour))
	failed.Status.Conditions = []*v1alpha1.Condition{
		{Type: "Ready", Status: "False", Reason: "SecretResolutionFailed", Message: "secret missing"},
	}
	return &fakeClient{listTasks: []*v1alpha1.Task{running, suspended, failed}}
}

func TestListTools(t *testing.T) {
	session, _ := newTestSession(t, sampleTasks())

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := []string{"ax_cluster_summary", "ax_get_task", "ax_list_tasks"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q is not annotated read-only", tool.Name)
		}
	}
}

func TestListTasksFiltersAndTruncates(t *testing.T) {
	session, _ := newTestSession(t, sampleTasks())
	ctx := context.Background()

	cases := []struct {
		name string
		args map[string]any
		want []string // task names, in listing order
		trun bool
	}{
		{"all", nil, []string{"fix-build", "nightly", "spike"}, false},
		{"phase", map[string]any{"phase": "running"}, []string{"fix-build"}, false},
		{"atespace", map[string]any{"atespace": "TEAM"}, []string{"nightly", "spike"}, false},
		{"limit", map[string]any{"limit": 2}, []string{"fix-build", "nightly"}, true},
		{"both", map[string]any{"phase": "Failed", "atespace": "team"}, []string{"spike"}, false},
	}
	for _, tc := range cases {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ax_list_tasks", Arguments: tc.args})
		if err != nil {
			t.Fatalf("%s: CallTool: %v", tc.name, err)
		}
		if res.IsError {
			t.Fatalf("%s: tool error: %s", tc.name, callText(res))
		}
		var out listTasksOutput
		decodeStructured(t, res.StructuredContent, &out)
		var got []string
		for _, item := range out.Tasks {
			got = append(got, item.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: tasks = %v, want %v", tc.name, got, tc.want)
		}
		if out.Truncated != tc.trun {
			t.Errorf("%s: truncated = %v, want %v", tc.name, out.Truncated, tc.trun)
		}
	}
}

func TestListTaskItems(t *testing.T) {
	session, _ := newTestSession(t, sampleTasks())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ax_list_tasks"})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out listTasksOutput
	decodeStructured(t, res.StructuredContent, &out)

	first := out.Tasks[0]
	if first.WebShell != "http://fix-build.norne" {
		t.Errorf("running task webShell = %q, want the dashboard link", first.WebShell)
	}
	if first.PendingAction == "" {
		t.Error("pending approval not surfaced")
	}
	if out.Tasks[2].Problem == "" {
		t.Error("failed condition not surfaced as problem")
	}
	// Suspended tasks have no Web Shell.
	if out.Tasks[1].WebShell != "" {
		t.Errorf("suspended task should not advertise a web shell, got %q", out.Tasks[1].WebShell)
	}
}

func TestGetTaskDetail(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	task := pbTask("experiment-7", "default", "Running", now)
	task.Spec = &v1alpha1.TaskSpec{
		Image:   "localhost:5001/qwen-task-runner",
		Command: []string{"/bin/sh", "-c", "make test"},
		Workspaces: []*v1alpha1.WorkspaceRef{
			{Name: "experiments", Path: "/workspace/experiments", Goal: "fix the flaky test"},
		},
		Debug: true,
		// Env values must never reach the MCP output.
		Env: []*v1alpha1.EnvVar{{Name: "TOKEN", Value: "hunter2"}},
	}
	fake := sampleTasks()
	fake.getTask = task
	session, _ := newTestSession(t, fake)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_get_task",
		Arguments: map[string]any{"name": "experiment-7"},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out getTaskOutput
	decodeStructured(t, res.StructuredContent, &out)

	if out.Name != "experiment-7" || out.Atespace != "default" {
		t.Errorf("identity = %q/%q", out.Atespace, out.Name)
	}
	if out.Command != "/bin/sh -c make test" || out.Image == "" {
		t.Errorf("spec fields not surfaced: command=%q image=%q", out.Command, out.Image)
	}
	if len(out.Goals) != 1 || out.Goals[0].Goal != "fix the flaky test" {
		t.Errorf("goals = %+v, want the workspace goal", out.Goals)
	}
	if out.WebShell != "http://experiment-7.norne" {
		t.Errorf("webShell = %q", out.WebShell)
	}
	if strings.Contains(callText(res), "hunter2") {
		t.Error("environment values must not leak into tool output")
	}
	// Atespace resolved from the cache without an explicit argument: the
	// cache has no experiment-7, so the default atespace was used for GetTask.
	if fake.getCalls != 1 {
		t.Errorf("GetTask calls = %d, want 1", fake.getCalls)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	fake := sampleTasks()
	fake.getErr = status.Error(codes.NotFound, "no such task")
	session, _ := newTestSession(t, fake)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_get_task",
		Arguments: map[string]any{"name": "ghost", "atespace": "team"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool-level error for an unknown task")
	}
	text := callText(res)
	if !strings.Contains(text, `no task "ghost" in atespace "team"`) {
		t.Errorf("error text = %q, want the not-found message", text)
	}
}

func TestClusterSummary(t *testing.T) {
	session, _ := newTestSession(t, sampleTasks())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ax_cluster_summary"})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out clusterSummaryOutput
	decodeStructured(t, res.StructuredContent, &out)

	if out.TotalTasks != 3 {
		t.Errorf("totalTasks = %d, want 3", out.TotalTasks)
	}
	if out.Phases["Running"] != 1 || out.Phases["Suspended"] != 1 || out.Phases["Failed"] != 1 {
		t.Errorf("phases = %v", out.Phases)
	}
	if out.PendingApprovals != 1 {
		t.Errorf("pendingApprovals = %d, want 1", out.PendingApprovals)
	}
	if !slices.Equal(out.Failing, []string{"spike"}) {
		t.Errorf("failing = %v, want [spike]", out.Failing)
	}
	if !slices.Equal(out.Atespaces, []string{"default", "team"}) {
		t.Errorf("atespaces = %v", out.Atespaces)
	}
	if out.PromptTokens != 300 || out.CompletionTokens != 75 {
		t.Errorf("tokens = %d/%d, want 300/75", out.PromptTokens, out.CompletionTokens)
	}
}

func TestToolsFailWhenAXUnreachable(t *testing.T) {
	fake := &fakeClient{listErr: status.Error(codes.Unavailable, "ax-server down")}
	src := newTaskSource(fake, 500, time.Hour)

	srv := newMCPServer(src, "norne", writesConfig{})
	httpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	ts := httptest.NewServer(httpHandler)
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ax_list_tasks"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(callText(res), "AX server unreachable") {
		t.Errorf("expected a visible outage error, got isError=%v text=%q", res.IsError, callText(res))
	}
}
