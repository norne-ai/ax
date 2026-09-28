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
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultToolLimit = 50
	maxToolLimit     = 200
)

// readOnly is the annotation set shared by all three tools. AX mutations
// (apply, suspend, resume, delete) are deliberately not exposed: this server
// fronts a family chat assistant, so it answers questions and changes
// nothing.
func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: ptrFalse(),
		IdempotentHint:  true,
		OpenWorldHint:   ptrFalse(),
	}
}

func ptrFalse() *bool {
	f := false
	return &f
}

func ptrTrue() *bool {
	t := true
	return &t
}

type axTools struct {
	tasks           *taskSource
	dashboardDomain string
	writes          writesConfig
}

// newMCPServer builds the MCP server and registers the AX task tools on it.
// The three read-only tools are always present; the two mutating tools are
// registered only when the operator enabled writes, so a client can never
// discover a tool the deployment did not opt into.
func newMCPServer(tasks *taskSource, dashboardDomain string, writes writesConfig) *mcp.Server {
	ax := &axTools{tasks: tasks, dashboardDomain: dashboardDomain, writes: writes}

	instructions := "Read-only queries against the AX orchestrator on this cluster: ax_list_tasks, ax_get_task and ax_cluster_summary."
	if writes.enabled {
		instructions += " It can also launch a confined coding task (ax_launch_task) and delete an assistant-launched task (ax_delete_task); both are guarded and require an explicit confirm."
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ax-mcp", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ax_list_tasks",
		Description: "List AX tasks, newest first. Optionally filter by atespace and by phase (e.g. Running, Suspended, Failed, Terminating). Returns name, phase, age, token usage, any pending approval, and any failed condition.",
		Annotations: readOnly(),
	}, ax.listTasks)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ax_get_task",
		Description: "Describe one AX task in full: phase, token usage, conditions, pending approval, runner image and command, and each workspace's goal. Reads live from the AX server, so the answer is current.",
		Annotations: readOnly(),
	}, ax.getTask)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ax_cluster_summary",
		Description: "Summarise all AX tasks on the cluster in one shot: how many are in each phase, total token usage, how many await approval, and how old the underlying task list is.",
		Annotations: readOnly(),
	}, ax.clusterSummary)

	if writes.enabled {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "ax_launch_task",
			Description: "Launch a confined Qwen coding task in the norne-ai/experiments repository. The agent works only under runs/<experiment>/, then commits and pushes a branch. The runner image, model catalog, secrets, gateway and resource limits are fixed by the operator; you only supply the experiment name, the prompt, and optionally the model and reasoning effort. Requires an operator-enabled launcher and there is a cap on concurrent assistant tasks.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				DestructiveHint: ptrFalse(),
				IdempotentHint:  false,
				OpenWorldHint:   ptrTrue(),
			},
		}, ax.launchTask)

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "ax_delete_task",
			Description: "Delete an assistant-launched task whose name starts with wa-. Destructive and final: the sandbox and any unpushed work are lost. The first call only reports the task and refuses; call again with confirmName equal to the name to actually delete. Tasks not launched through this server cannot be deleted here.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				DestructiveHint: ptrTrue(),
				IdempotentHint:  false,
				OpenWorldHint:   ptrTrue(),
			},
		}, ax.deleteTask)
	}

	return srv
}

// --- ax_list_tasks ---

type listTasksInput struct {
	Atespace string `json:"atespace,omitempty" jsonschema:"Only tasks in this atespace. Empty lists across every atespace."`
	Phase    string `json:"phase,omitempty" jsonschema:"Only tasks in this phase, case-insensitive, e.g. Running or Suspended."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum number of tasks to return. Defaults to 50, caps at 200."`
}

type taskItem struct {
	Name             string `json:"name"`
	Atespace         string `json:"atespace"`
	Phase            string `json:"phase"`
	Created          string `json:"created,omitempty"`
	AgeSeconds       int64  `json:"ageSeconds"`
	PromptTokens     int32  `json:"promptTokens"`
	CompletionTokens int32  `json:"completionTokens"`
	PendingAction    string `json:"pendingAction,omitempty"`
	Problem          string `json:"problem,omitempty"`
	WebShell         string `json:"webShell,omitempty"`
}

type listTasksOutput struct {
	Tasks           []taskItem `json:"tasks"`
	TotalCached     int        `json:"totalCached"`
	Truncated       bool       `json:"truncated"`
	CacheAgeSeconds float64    `json:"cacheAgeSeconds"`
}

func (ax *axTools) listTasks(ctx context.Context, _ *mcp.CallToolRequest, in listTasksInput) (*mcp.CallToolResult, listTasksOutput, error) {
	tasks, fetched, err := ax.requireTasks(ctx)
	if err != nil {
		return nil, listTasksOutput{}, err
	}
	if in.Limit <= 0 {
		in.Limit = defaultToolLimit
	}
	if in.Limit > maxToolLimit {
		in.Limit = maxToolLimit
	}

	ateespace := strings.ToLower(strings.TrimSpace(in.Atespace))
	phase := strings.ToLower(strings.TrimSpace(in.Phase))

	out := listTasksOutput{
		Tasks:           []taskItem{},
		TotalCached:     len(tasks),
		CacheAgeSeconds: cacheAge(fetched),
	}
	for _, t := range tasks {
		if ateespace != "" && strings.ToLower(t.Atespace) != ateespace {
			continue
		}
		if phase != "" && strings.ToLower(t.Phase) != phase {
			continue
		}
		if len(out.Tasks) >= in.Limit {
			out.Truncated = true
			break
		}
		out.Tasks = append(out.Tasks, ax.toItem(t, time.Now()))
	}
	return nil, out, nil
}

// --- ax_get_task ---

type getTaskInput struct {
	Name     string `json:"name" jsonschema:"Task name, exactly as ax_list_tasks returns it."`
	Atespace string `json:"atespace,omitempty" jsonschema:"Atespace of the task. Omit to resolve it from the current task list."`
}

type workspaceGoal struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
	Goal string `json:"goal,omitempty"`
}

type getTaskOutput struct {
	Name             string          `json:"name"`
	Atespace         string          `json:"atespace"`
	Phase            string          `json:"phase"`
	Actor            string          `json:"actor,omitempty"`
	Created          string          `json:"created,omitempty"`
	AgeSeconds       int64           `json:"ageSeconds"`
	PromptTokens     int32           `json:"promptTokens"`
	CompletionTokens int32           `json:"completionTokens"`
	PendingAction    string          `json:"pendingAction,omitempty"`
	Image            string          `json:"image,omitempty"`
	Command          string          `json:"command,omitempty"`
	Debug            bool            `json:"debug,omitempty"`
	SuspendRequested bool            `json:"suspendRequested,omitempty"`
	Goals            []workspaceGoal `json:"goals,omitempty"`
	Conditions       []conditionInfo `json:"conditions,omitempty"`
	WebShell         string          `json:"webShell,omitempty"`
}

func (ax *axTools) getTask(ctx context.Context, _ *mcp.CallToolRequest, in getTaskInput) (*mcp.CallToolResult, getTaskOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, getTaskOutput{}, fmt.Errorf("name is required")
	}

	ateespace := strings.TrimSpace(in.Atespace)
	if ateespace == "" {
		if info, ok := ax.tasks.lookup(ctx, name); ok {
			ateespace = info.Atespace
		} else {
			// GetTask scopes to one atespace, so an unknown name can only be
			// looked up in the default atespace. A miss there is reported as
			// not-found rather than a guess across atespaces.
			ateespace = "default"
		}
	}

	task, err := ax.tasks.taskDetail(ctx, ateespace, name)
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			return nil, getTaskOutput{}, fmt.Errorf("no task %q in atespace %q", name, ateespace)
		}
		return nil, getTaskOutput{}, fmt.Errorf("asking the AX server for task %q: %w", name, err)
	}

	return nil, ax.toDetail(task, time.Now()), nil
}

func (ax *axTools) toDetail(task *v1alpha1.Task, now time.Time) getTaskOutput {
	info := toTaskInfo(task)
	out := getTaskOutput{
		Name:             info.Name,
		Atespace:         info.Atespace,
		Phase:            info.Phase,
		Actor:            info.Actor,
		Created:          formatTime(info.Created),
		AgeSeconds:       ageSeconds(info.Created, now),
		PromptTokens:     info.PromptTokens,
		CompletionTokens: info.CompletionTokens,
		PendingAction:    info.PendingAction,
		Conditions:       info.Conditions,
		WebShell:         ax.webShellURL(info),
	}
	spec := task.GetSpec()
	out.Image = spec.GetImage()
	out.Command = strings.Join(spec.GetCommand(), " ")
	out.Debug = spec.GetDebug()
	out.SuspendRequested = spec.GetSuspend()
	for _, ws := range spec.GetWorkspaces() {
		out.Goals = append(out.Goals, workspaceGoal{
			Name: ws.GetName(),
			Path: ws.GetPath(),
			Goal: ws.GetGoal(),
		})
	}
	return out
}

// --- ax_cluster_summary ---

type clusterSummaryOutput struct {
	TotalTasks       int            `json:"totalTasks"`
	Phases           map[string]int `json:"phases"`
	PromptTokens     int64          `json:"promptTokens"`
	CompletionTokens int64          `json:"completionTokens"`
	PendingApprovals int            `json:"pendingApprovals"`
	Failing          []string       `json:"failing"`
	Atespaces        []string       `json:"atespaces"`
	CacheAgeSeconds  float64        `json:"cacheAgeSeconds"`
}

type clusterSummaryInput struct{}

func (ax *axTools) clusterSummary(ctx context.Context, _ *mcp.CallToolRequest, _ clusterSummaryInput) (*mcp.CallToolResult, clusterSummaryOutput, error) {
	tasks, fetched, err := ax.requireTasks(ctx)
	if err != nil {
		return nil, clusterSummaryOutput{}, err
	}

	out := clusterSummaryOutput{
		TotalTasks:      len(tasks),
		Phases:          map[string]int{},
		Failing:         []string{},
		Atespaces:       []string{},
		CacheAgeSeconds: cacheAge(fetched),
	}
	ateespaces := map[string]bool{}
	for _, t := range tasks {
		out.Phases[t.Phase]++
		out.PromptTokens += int64(t.PromptTokens)
		out.CompletionTokens += int64(t.CompletionTokens)
		if t.PendingAction != "" {
			out.PendingApprovals++
		}
		if t.problem() != "" {
			out.Failing = append(out.Failing, t.Name)
		}
		ateespaces[t.Atespace] = true
	}
	for a := range ateespaces {
		out.Atespaces = append(out.Atespaces, a)
	}
	sort.Strings(out.Atespaces)
	return nil, out, nil
}

// --- shared helpers ---

// requireTasks returns the cached task list, forcing a refresh when the cache
// has never been populated so the first query after an AX-server outage does
// not have to wait for the next poll tick.
func (ax *axTools) requireTasks(ctx context.Context) ([]taskInfo, time.Time, error) {
	tasks, fetched, err := ax.tasks.snapshot()
	if fetched.IsZero() {
		if rerr := ax.tasks.refresh(ctx); rerr != nil {
			return nil, time.Time{}, fmt.Errorf("AX server unreachable: %w", rerr)
		}
		tasks, fetched, err = ax.tasks.snapshot()
	}
	if err != nil {
		return tasks, fetched, fmt.Errorf("task list is stale (last refresh failed: %w)", err)
	}
	return tasks, fetched, nil
}

func (ax *axTools) toItem(t taskInfo, now time.Time) taskItem {
	return taskItem{
		Name:             t.Name,
		Atespace:         t.Atespace,
		Phase:            t.Phase,
		Created:          formatTime(t.Created),
		AgeSeconds:       ageSeconds(t.Created, now),
		PromptTokens:     t.PromptTokens,
		CompletionTokens: t.CompletionTokens,
		PendingAction:    t.PendingAction,
		Problem:          t.problem(),
		WebShell:         ax.webShellURL(t),
	}
}

// webShellURL points at the ax-dashboard session for a running task. The
// dashboard serves each running task at <task-name>.<base-domain>.
func (ax *axTools) webShellURL(t taskInfo) string {
	if t.Phase != "Running" || ax.dashboardDomain == "" {
		return ""
	}
	return "http://" + strings.ToLower(t.Name) + "." + ax.dashboardDomain
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func ageSeconds(created time.Time, now time.Time) int64 {
	if created.IsZero() {
		return 0
	}
	d := now.Sub(created)
	if d < 0 {
		d = 0
	}
	return int64(d.Seconds())
}

func cacheAge(fetched time.Time) float64 {
	if fetched.IsZero() {
		return -1
	}
	return time.Since(fetched).Round(time.Millisecond).Seconds()
}
