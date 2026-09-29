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
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
)

// The write path deliberately mirrors scripts/run-qwen-task.sh in Model Studio
// serve mode: one pinned runner image, one Git workspace, the experiment's
// runs/<name>/ confinement, and credentials that arrive only as Kubernetes
// Secret references. The MCP client can supply text, never a spec.
const (
	waPrefix = "wa-"

	// modelStudioEndpoint is the Alibaba Token Plan compatible-mode endpoint
	// the runner image's Model Studio profile is written against.
	modelStudioEndpoint  = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
	modelStudioSettings  = "/etc/qwen-code/settings-modelstudio.json"
	modelStudioKeyEnv    = "MODELSTUDIO_API_KEY"
	modelStudioSecret    = "modelstudio-api"
	modelStudioSecretKey = "api-key"

	// ninferEndpoint is the shared local inference Service, spelled exactly
	// as examples/qwen-task.yaml spells it. Tasks launched on it draw from
	// the same process that answers the assistant itself, so the operator
	// keeps them few by policy; the launcher only speaks the OpenAI-
	// compatible API and never reads or writes NInfer configuration.
	ninferEndpoint = "http://ninfer.hermes-agent.svc.cluster.local:8000/v1"
	ninferKeyEnv   = "OPENAI_API_KEY"
	// ninferPlaceholderKey matches examples/qwen-task.yaml: the local
	// endpoint accepts any non-empty key. It is not a secret.
	ninferPlaceholderKey = "ollama"

	gitWorkspaceName   = "qwen-experiments"
	gitWorkspaceRepo   = "https://github.com/norne-ai/experiments.git"
	gitWorkspaceBranch = "main"
	gitTokenEnv        = "GITHUB_TOKEN"
	gitSecret          = "experiments-git"
	gitSecretKey       = "token"
	previewTarget      = "http://127.0.0.1:3000"

	maxPromptChars   = 8000
	maxExperimentLen = 80
	maxSlugLen       = 32
)

var experimentPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// allowedModel binds a model id the launcher may use to its provider backend.
type allowedModel struct {
	Model    string
	Provider string // "modelstudio" or "ninfer"
	// DefaultEffort is the reasoning effort applied when the caller does not
	// name one; empty means the runner image's own default (very high for
	// flash, which bills every thought).
	DefaultEffort string
}

// writesConfig bounds the mutating tools. Every field is operator-set (flags
// or env); none are reachable by the MCP caller.
type writesConfig struct {
	enabled      bool
	runnerImage  string
	atespace     string
	maxActive    int
	defaultModel string
	models       []allowedModel
}

// findModel resolves a requested model id against the operator's allowlist.
func (w writesConfig) findModel(model string) (allowedModel, bool) {
	for _, m := range w.models {
		if strings.EqualFold(m.Model, model) {
			return m, true
		}
	}
	return allowedModel{}, false
}

// modelList renders the allowlist for tool descriptions and error messages.
func (w writesConfig) modelList() string {
	parts := make([]string, 0, len(w.models))
	for _, m := range w.models {
		part := m.Model + " (" + m.Provider
		if m.DefaultEffort != "" {
			part += ", default effort " + m.DefaultEffort
		}
		parts = append(parts, part+")")
	}
	return strings.Join(parts, ", ")
}

// --- ax_launch_task ---

type launchTaskInput struct {
	Experiment      string `json:"experiment" jsonschema:"Short kebab-case name for the run; becomes runs/<experiment>/ in the experiments repo and part of the task name. Lowercase letters, digits, dots, underscores, hyphens."`
	Prompt          string `json:"prompt" jsonschema:"What to ask the agent to do, verbatim. The delivery rules (work under runs/<experiment>/, commit and push the branch) are appended by the server and cannot be overridden."`
	Model           string `json:"model,omitempty" jsonschema:"Model id; must be one of the operator-allowlisted models named in the tool description. Omit to use the default."`
	ReasoningEffort string `json:"reasoningEffort,omitempty" jsonschema:"Optional reasoning effort: none, low, medium or xhigh."`
	Preview         *bool  `json:"preview,omitempty" jsonschema:"Preview is enabled when omitted. Set false only for a non-web task. When enabled, the app must listen on 0.0.0.0:3000 and the Web Shell opens a hot-reloading preview panel automatically."`
}

type launchTaskOutput struct {
	Name        string `json:"name"`
	Atespace    string `json:"atespace"`
	Phase       string `json:"phase"`
	Branch      string `json:"branch"`
	Experiment  string `json:"experiment"`
	Model       string `json:"model"`
	WebShell    string `json:"webShell,omitempty"`
	PreviewURL  string `json:"previewURL,omitempty"`
	ActiveTasks int    `json:"activeTasks"`
	Note        string `json:"note"`
}

func (ax *axTools) launchTask(ctx context.Context, _ *mcp.CallToolRequest, in launchTaskInput) (*mcp.CallToolResult, launchTaskOutput, error) {
	w := ax.writes
	experiment, slug, err := validateExperiment(in.Experiment)
	if err != nil {
		return nil, launchTaskOutput{}, err
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, launchTaskOutput{}, fmt.Errorf("prompt is required")
	}
	if len(prompt) > maxPromptChars {
		return nil, launchTaskOutput{}, fmt.Errorf("prompt is %d characters, over the %d limit", len(prompt), maxPromptChars)
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = w.defaultModel
	}
	profile, allowed := w.findModel(model)
	if !allowed {
		return nil, launchTaskOutput{}, fmt.Errorf("model %q is not allowed; this launcher offers: %s",
			model, w.modelList())
	}
	// Normalize to the allowlist's spelling before it reaches the spec.
	model = profile.Model
	effort := strings.ToLower(strings.TrimSpace(in.ReasoningEffort))
	switch effort {
	case "", "none", "low", "medium", "xhigh":
	default:
		return nil, launchTaskOutput{}, fmt.Errorf("invalid reasoningEffort %q: expected none, low, medium or xhigh", in.ReasoningEffort)
	}
	if effort == "" {
		effort = profile.DefaultEffort
	}

	// The cap counts wa- tasks that could still burn tokens or hold a worker:
	// everything except Failed and Suspended.
	tasks, _, err := ax.requireTasks(ctx)
	if err != nil {
		return nil, launchTaskOutput{}, err
	}
	active := activeWaTasks(tasks)
	if len(active) >= w.maxActive {
		return nil, launchTaskOutput{}, fmt.Errorf("launcher is at its cap: %d assistant-launched task(s) are already active (%s). Delete one with ax_delete_task first",
			len(active), strings.Join(active, ", "))
	}

	ts := time.Now().Unix()
	name := waPrefix + slug + "-" + timeSuffix(ts)
	if _, exists := ax.tasks.cached(name); exists {
		return nil, launchTaskOutput{}, fmt.Errorf("task %s already exists; wait a second and retry", name)
	}

	branch := "qwen/" + name
	ws := experimentsWorkspace(w.atespace)
	preview := in.Preview == nil || *in.Preview
	task := waTask(w, profile, name, branch, experiment, prompt, model, effort, preview)

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := ax.tasks.client.UpdateWorkspace(callCtx, &v1alpha1.UpdateWorkspaceRequest{Workspace: ws}); err != nil {
		return nil, launchTaskOutput{}, fmt.Errorf("updating workspace %s: %w", gitWorkspaceName, err)
	}
	created, err := ax.tasks.client.UpdateTask(callCtx, &v1alpha1.UpdateTaskRequest{Task: task}, grpc.WaitForReady(false))
	if err != nil {
		return nil, launchTaskOutput{}, fmt.Errorf("launching task %s: %w", name, err)
	}

	// Refresh immediately so follow-up list/get calls see the new task
	// without waiting for the next poll tick.
	if err := ax.tasks.refresh(ctx); err != nil {
		slog.Warn("post-launch task list refresh failed", "task", name, "error", err)
	}

	slog.Info("launched assistant task", "name", name, "atespace", w.atespace, "branch", branch, "model", model, "effort", effort, "promptChars", len(prompt))

	out := launchTaskOutput{
		Name:        name,
		Atespace:    w.atespace,
		Phase:       created.GetStatus().GetPhase(),
		Branch:      branch,
		Experiment:  experiment,
		Model:       model,
		ActiveTasks: len(active) + 1,
		Note:        "The agent will commit its work and push branch " + branch + " to norne-ai/experiments before declaring completion.",
	}
	if ax.dashboardDomain != "" {
		out.WebShell = "http://" + name + "." + ax.dashboardDomain
	}
	if out.Phase == "" {
		out.Phase = "Pending"
	}
	if preview && ax.dashboardDomain != "" {
		// This is an operator-facing link for the configured dashboard zone. It
		// does not become AX_PREVIEW_URL in the Task: the in-panel URL still
		// derives its scheme and hostname from the Shell origin.
		out.PreviewURL = "http://preview-" + name + "." + ax.dashboardDomain
		out.Note += " The Web Shell will open the app preview from " + out.PreviewURL + "."
	}
	return nil, out, nil
}

// validateExperiment checks the raw name and derives the DNS-label-safe slug
// used in the task name.
func validateExperiment(raw string) (experiment, slug string, err error) {
	experiment = strings.TrimSpace(raw)
	if experiment == "" {
		return "", "", fmt.Errorf("experiment is required")
	}
	if len(experiment) > maxExperimentLen {
		return "", "", fmt.Errorf("experiment %q exceeds %d characters", experiment, maxExperimentLen)
	}
	if !experimentPattern.MatchString(experiment) {
		return "", "", fmt.Errorf("experiment %q must be lowercase letters, digits, dots, underscores or hyphens, starting and ending alphanumeric", experiment)
	}
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r == '.', r == '_':
			return '-'
		}
		return -1
	}, experiment)
	slug = strings.Trim(strings.TrimLeft(slug, "-"), "-")
	if len(slug) > maxSlugLen {
		slug = strings.TrimRight(slug[:maxSlugLen], "-")
	}
	if slug == "" {
		return "", "", fmt.Errorf("experiment %q yields no usable task-name characters", experiment)
	}
	return experiment, slug, nil
}

func timeSuffix(ts int64) string {
	return fmt.Sprintf("%d", ts)
}

func activeWaTasks(tasks []taskInfo) []string {
	var names []string
	for _, t := range tasks {
		if !strings.HasPrefix(strings.ToLower(t.Name), waPrefix) {
			continue
		}
		switch t.Phase {
		case "Failed", "Suspended":
			continue
		}
		names = append(names, t.Name)
	}
	return names
}

func experimentsWorkspace(atespace string) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		ApiVersion: "ax.io/v1alpha1",
		Kind:       "Workspace",
		Metadata:   &v1alpha1.ObjectMeta{Name: gitWorkspaceName, Atespace: atespace},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{{
				Name:   "origin",
				Repo:   gitWorkspaceRepo,
				Branch: gitWorkspaceBranch,
				Dir:    ".",
			}},
		},
	}
}

// waPrepareScript recreates the checked-out branch and confines new work to
// runs/<experiment>/, exactly as scripts/run-qwen-task.sh does.
const waPrepareScript = `set -euo pipefail
git config --global user.name "${AX_GIT_AUTHOR_NAME:-Norne Qwen}"
git config --global user.email "${AX_GIT_AUTHOR_EMAIL:-qwen@norne.local}"
cd /workspace
if git show-ref --verify --quiet "refs/heads/$AX_GIT_BRANCH"; then
  git switch "$AX_GIT_BRANCH"
else
  git switch -c "$AX_GIT_BRANCH"
fi
mkdir -p "runs/$AX_EXPERIMENT_NAME"
`

func wrappedPrompt(branch, experiment, userPrompt string, preview bool) string {
	previewNote := ""
	if preview {
		previewNote = `
- The app must listen on 0.0.0.0:3000 inside the task and keep running after you finish. For Next.js, use NEXT_TELEMETRY_DISABLED=1 npx next dev -H 0.0.0.0 -p 3000 in the background, with no basePath. The preview panel opens itself beside the chat; confirm the app answers on http://127.0.0.1:3000 before calling the work done.`
	}
	return fmt.Sprintf(`You are working in the norne-ai/experiments Git repository on branch %s.

Mandatory delivery rules:
- Put every new or modified project file under runs/%s/.
- Do not modify files outside runs/%s/.
- Complete and verify the requested work.
- Before declaring the task complete, commit all changes on %s with a meaningful commit message and push that branch to origin.
- In your final response, report the branch name, commit SHA, verification performed, and any remaining issues.%s

User task:
%s`, branch, experiment, experiment, branch, previewNote, userPrompt)
}

// waTask renders the task spec for one allowlisted model profile. The base
// env and the Git Secret reference are identical across providers; the
// provider decides the endpoint, where the model key comes from, and whether
// a settings profile is selected at all (the NInfer default is baked into the
// runner image unchanged).
func waTask(w writesConfig, profile allowedModel, name, branch, experiment, prompt, model, effort string, preview bool) *v1alpha1.Task {
	env := []*v1alpha1.EnvVar{
		{Name: "AX_QWEN_PROMPT", Value: wrappedPrompt(branch, experiment, prompt, preview)},
		{Name: "OPENAI_MODEL", Value: model},
		{Name: "AX_EXPERIMENT_NAME", Value: experiment},
		{Name: "AX_GIT_BRANCH", Value: branch},
	}
	secrets := []*v1alpha1.SecretEnvVar{
		{Name: gitTokenEnv, SecretKeyRef: &v1alpha1.SecretKeyRef{Name: gitSecret, Key: gitSecretKey}},
	}

	switch profile.Provider {
	case "ninfer":
		env = append(env,
			&v1alpha1.EnvVar{Name: "OPENAI_BASE_URL", Value: ninferEndpoint},
			&v1alpha1.EnvVar{Name: ninferKeyEnv, Value: ninferPlaceholderKey},
		)
	case "modelstudio":
		env = append(env,
			&v1alpha1.EnvVar{Name: "OPENAI_BASE_URL", Value: modelStudioEndpoint},
			&v1alpha1.EnvVar{Name: "QWEN_CODE_SYSTEM_SETTINGS_PATH", Value: modelStudioSettings},
		)
		// The key arrives as a Secret reference only: no credential value
		// ever passes through the MCP server, let alone the model.
		secrets = append(secrets, &v1alpha1.SecretEnvVar{
			Name:         modelStudioKeyEnv,
			SecretKeyRef: &v1alpha1.SecretKeyRef{Name: modelStudioSecret, Key: modelStudioSecretKey},
		})
	default:
		// findModel only yields profiles built from the parsed flag, whose
		// providers are validated there; this keeps a misparsed config from
		// ever launching against an unintended backend.
		panic("ax-mcp: unknown model provider " + profile.Provider)
	}
	if effort != "" {
		env = append(env, &v1alpha1.EnvVar{Name: "AX_QWEN_REASONING_EFFORT", Value: effort})
	}
	if preview {
		// The mux answers /__qwen-preview.json with a hostname derived from the
		// incoming Shell Host. Do not set AX_PREVIEW_URL: an absolute LAN URL is
		// mixed content through the HTTPS tunnel, and the reverse is invalid TLS.
		env = append(env, &v1alpha1.EnvVar{Name: "AX_PREVIEW_TARGET", Value: previewTarget})
	}

	return &v1alpha1.Task{
		ApiVersion: "ax.io/v1alpha1",
		Kind:       "Task",
		Metadata:   &v1alpha1.ObjectMeta{Name: name, Atespace: w.atespace},
		Spec: &v1alpha1.TaskSpec{
			Image: w.runnerImage,
			Command: []string{
				"bash", "-lc",
				waPrepareScript + "exec /usr/local/bin/ax-qwen-serve",
			},
			Env: env,
			Resources: &v1alpha1.ResourceReqs{
				Requests: &v1alpha1.ResourceList{Cpu: "500m", Memory: "1Gi"},
				Limits:   &v1alpha1.ResourceList{Cpu: "4", Memory: "8Gi"},
			},
			Workspaces: []*v1alpha1.WorkspaceRef{{Name: gitWorkspaceName, Path: "/workspace"}},
			SecretEnv:  secrets,
			Debug:      true,
		},
	}
}

// --- ax_delete_task ---

type deleteTaskInput struct {
	Name        string `json:"name" jsonschema:"Task name to delete. Only tasks starting with wa- (launched through this server) can be deleted."`
	ConfirmName string `json:"confirmName,omitempty" jsonschema:"Set to exactly the same value as name to confirm the deletion. Without it the server only reports the task and refuses, so deletion is always a deliberate second call."`
}

type deleteTaskOutput struct {
	Deleted           bool      `json:"deleted"`
	NeedsConfirmation bool      `json:"needsConfirmation"`
	Reason            string    `json:"reason,omitempty"`
	Task              *taskItem `json:"task,omitempty"`
}

func (ax *axTools) deleteTask(ctx context.Context, _ *mcp.CallToolRequest, in deleteTaskInput) (*mcp.CallToolResult, deleteTaskOutput, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, deleteTaskOutput{}, fmt.Errorf("name is required")
	}

	info, found := ax.tasks.lookup(ctx, name)
	if !found {
		return nil, deleteTaskOutput{}, fmt.Errorf("no task %q found in the current task list", name)
	}

	// Policy refusal comes back as a normal result, not an error: the caller
	// needs the explanation verbatim, not as a tool failure.
	if !strings.HasPrefix(strings.ToLower(info.Name), waPrefix) {
		return nil, deleteTaskOutput{
			Reason: fmt.Sprintf("task %q was not launched through this server; only %s-prefixed tasks can be deleted from chat. Ask an operator to remove it with the ax CLI.", info.Name, waPrefix),
		}, nil
	}

	item := ax.toItem(info, time.Now())
	if !strings.EqualFold(strings.TrimSpace(in.ConfirmName), info.Name) {
		return nil, deleteTaskOutput{
			NeedsConfirmation: true,
			Task:              &item,
			Reason:            fmt.Sprintf("deleting %q is final: the sandbox and any work it has not pushed to git is lost. Call ax_delete_task again with confirmName set to exactly %q to go through with it.", info.Name, info.Name),
		}, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := ax.tasks.client.DeleteTask(callCtx, &v1alpha1.DeleteTaskRequest{Atespace: info.Atespace, Name: info.Name}, grpc.WaitForReady(false)); err != nil {
		return nil, deleteTaskOutput{}, fmt.Errorf("deleting task %s: %w", info.Name, err)
	}
	if err := ax.tasks.refresh(ctx); err != nil {
		slog.Warn("post-delete task list refresh failed", "task", info.Name, "error", err)
	}

	slog.Info("deleted assistant task", "name", info.Name, "atespace", info.Atespace, "phase", info.Phase, "ageSeconds", item.AgeSeconds)
	return nil, deleteTaskOutput{Deleted: true, Task: &item}, nil
}
