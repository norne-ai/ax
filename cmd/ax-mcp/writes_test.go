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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func envValue(task *v1alpha1.Task, name string) (string, bool) {
	for _, e := range task.GetSpec().GetEnv() {
		if e.GetName() == name {
			return e.GetValue(), true
		}
	}
	return "", false
}

func secretRef(task *v1alpha1.Task, name string) *v1alpha1.SecretKeyRef {
	for _, s := range task.GetSpec().GetSecretEnv() {
		if s.GetName() == name {
			return s.GetSecretKeyRef()
		}
	}
	return nil
}

func TestWritesDisabledHidesMutatingTools(t *testing.T) {
	session, _ := newTestSession(t, sampleTasks()) // writes off
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "ax_launch_task" || tool.Name == "ax_delete_task" {
			t.Errorf("mutating tool %q registered while writes are disabled", tool.Name)
		}
	}
}

func TestWritesEnabledRegistersFiveTools(t *testing.T) {
	session, _ := newSessionWithWrites(t, sampleTasks(), enabledWrites())
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 5 {
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("registered %d tools (%v), want 5", len(names), names)
	}
}

func TestLaunchTaskBuildsRepositorySpec(t *testing.T) {
	fake := sampleTasks()
	fake.getTask = nil
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ax_launch_task",
		Arguments: map[string]any{
			"repository": "work-coordinator",
			"experiment": "fix.parse_bug-2",
			"prompt":     "Write a parser regression test and make it pass.",
			"preview":    true,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out launchTaskOutput
	decodeStructured(t, res.StructuredContent, &out)

	if !strings.HasPrefix(out.Name, "wa-fix-parse-bug-2-") {
		t.Errorf("name %q does not carry the wa- prefix and slugified experiment", out.Name)
	}
	if out.Atespace != "default" {
		t.Errorf("atespace = %q, want default", out.Atespace)
	}
	if out.Repository != gitHubOrg+"/work-coordinator" {
		t.Errorf("repository = %q, want %s/work-coordinator", out.Repository, gitHubOrg)
	}
	if out.BaseBranch != "main" {
		t.Errorf("baseBranch = %q, want the main default", out.BaseBranch)
	}
	if !strings.Contains(out.Note, gitHubOrg+"/work-coordinator") {
		t.Errorf("note = %q, want it to name the target repository", out.Note)
	}

	if len(fake.wsCalls) != 1 || len(fake.taskCalls) != 1 {
		t.Fatalf("workspace/task update calls = %d/%d, want 1/1", len(fake.wsCalls), len(fake.taskCalls))
	}
	task := fake.taskCalls[0].GetTask()
	spec := task.GetSpec()

	if spec.GetImage() != enabledWrites().runnerImage {
		t.Errorf("image = %q, want the pinned runner image", spec.GetImage())
	}
	if !slices.Contains(spec.GetCommand(), "exec /usr/local/bin/ax-qwen-serve") {
		// The command is one combined bash string, so look for the suffix.
		if len(spec.GetCommand()) == 0 || !strings.Contains(strings.Join(spec.GetCommand(), " "), "ax-qwen-serve") {
			t.Errorf("command does not launch the Qwen runner: %v", spec.GetCommand())
		}
	}
	if strings.Contains(spec.GetCommand()[2], "runs/$AX_EXPERIMENT_NAME") {
		t.Error("prepare script still confines the agent to runs/<experiment>/")
	}
	if task.GetMetadata().GetAtespace() != "default" {
		t.Errorf("task atespace = %q", task.GetMetadata().GetAtespace())
	}

	// The caller-selected repository becomes a per-task workspace cloned at the
	// base branch, and the task binds that workspace instead of the shared one.
	workspace := fake.wsCalls[0].GetWorkspace()
	gitRepo := workspace.GetSpec().GetGit()[0]
	if gitRepo.GetRepo() != "https://github.com/"+gitHubOrg+"/work-coordinator.git" {
		t.Errorf("workspace repo = %q, want the caller's repository under %s", gitRepo.GetRepo(), gitHubOrg)
	}
	if gitRepo.GetBranch() != "main" {
		t.Errorf("workspace branch = %q, want the defaulted base branch", gitRepo.GetBranch())
	}
	if !strings.HasPrefix(workspace.GetMetadata().GetName(), "ws-"+out.Name) {
		t.Errorf("workspace name = %q, want one per task (ws-%s)", workspace.GetMetadata().GetName(), out.Name)
	}
	if ref := spec.GetWorkspaces()[0]; ref.GetName() != workspace.GetMetadata().GetName() || ref.GetPath() != "/workspace" {
		t.Errorf("task workspace ref = %+v, want %s at /workspace", ref, workspace.GetMetadata().GetName())
	}

	// The rendered prompt names the repository and branch, never a subdirectory.
	prompt, _ := envValue(task, "AX_QWEN_PROMPT")
	if !strings.Contains(prompt, gitHubOrg+"/work-coordinator") || !strings.Contains(prompt, out.Branch) {
		t.Errorf("wrapped prompt missing repository or branch:\n%s", prompt)
	}
	if strings.Contains(prompt, "runs/fix.parse_bug-2/") {
		t.Error("wrapped prompt still confines the agent to runs/<experiment>/")
	}
	if _, set := envValue(task, "AX_EXPERIMENT_NAME"); set {
		t.Error("AX_EXPERIMENT_NAME is no longer part of a task spec")
	}
	if !strings.Contains(prompt, "Write a parser regression test") {
		t.Error("wrapped prompt lost the user task")
	}
	if !strings.Contains(prompt, "0.0.0.0:3000") || !strings.Contains(prompt, "preview panel opens itself") {
		t.Error("preview task prompt does not tell the agent how to serve the app")
	}
	if target, ok := envValue(task, "AX_PREVIEW_TARGET"); !ok || target != previewTarget {
		t.Errorf("AX_PREVIEW_TARGET = %q/%v, want %q", target, ok, previewTarget)
	}
	if _, set := envValue(task, "AX_PREVIEW_URL"); set {
		t.Error("preview task must derive its hostname from the Shell origin, not pin AX_PREVIEW_URL")
	}
	wantPreviewURL := "http://preview-" + out.Name + ".norne"
	if out.PreviewURL != wantPreviewURL {
		t.Errorf("previewURL = %q, want %q", out.PreviewURL, wantPreviewURL)
	}
	if !strings.Contains(out.Note, wantPreviewURL) {
		t.Errorf("note = %q, want preview URL %q", out.Note, wantPreviewURL)
	}
	if wantWebShell := "http://" + out.Name + ".norne"; out.WebShell != wantWebShell {
		t.Errorf("webShell = %q, want %q", out.WebShell, wantWebShell)
	}

	// Credentials must be Secret references, never plaintext values, and the
	// Model Studio key env must not also appear as a plaintext env var.
	if ref := secretRef(task, "GITHUB_TOKEN"); ref == nil || ref.GetName() != gitSecret || ref.GetKey() != gitSecretKey {
		t.Errorf("GITHUB_TOKEN secret ref = %+v, want %s/%s", ref, gitSecret, gitSecretKey)
	}
	if ref := secretRef(task, modelStudioKeyEnv); ref == nil || ref.GetName() != modelStudioSecret {
		t.Errorf("model key secret ref = %+v, want the %s secret", ref, modelStudioSecret)
	}
	if _, leaked := envValue(task, modelStudioKeyEnv); leaked {
		t.Error("model studio key must not be a plaintext env var")
	}
	if model, _ := envValue(task, "OPENAI_MODEL"); model != "qwen3.8-flash" {
		t.Errorf("default model = %q, want qwen3.8-flash", model)
	}
	if ep, _ := envValue(task, "OPENAI_BASE_URL"); ep != modelStudioEndpoint {
		t.Errorf("endpoint = %q, want the Model Studio endpoint", ep)
	}
	// Resource limits are the operator's, not the caller's.
	if spec.GetResources().GetLimits().GetMemory() != "8Gi" {
		t.Errorf("memory limit = %q", spec.GetResources().GetLimits().GetMemory())
	}
}

// A caller may spell the target with the organisation prefix or a .git suffix,
// and may base the task on a branch other than main.
func TestLaunchAcceptsOrgPrefixedRepoAndExplicitBaseBranch(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ax_launch_task",
		Arguments: map[string]any{
			"repository": gitHubOrg + "/experiments.git",
			"baseBranch": "experiment/wa-observer",
			"experiment": "reuse",
			"prompt":     "Continue the observer work.",
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out launchTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	if out.Repository != gitHubOrg+"/experiments" {
		t.Errorf("repository = %q, want the normalized %s/experiments", out.Repository, gitHubOrg)
	}
	if out.BaseBranch != "experiment/wa-observer" {
		t.Errorf("baseBranch = %q, want the requested base branch", out.BaseBranch)
	}

	gitRepo := fake.wsCalls[0].GetWorkspace().GetSpec().GetGit()[0]
	if gitRepo.GetRepo() != "https://github.com/norne-ai/experiments.git" {
		t.Errorf("workspace repo = %q, want the canonical org URL", gitRepo.GetRepo())
	}
	if gitRepo.GetBranch() != "experiment/wa-observer" {
		t.Errorf("workspace branch = %q, want the requested base branch", gitRepo.GetBranch())
	}
	prompt, _ := envValue(fake.taskCalls[0].GetTask(), "AX_QWEN_PROMPT")
	if !strings.Contains(prompt, "created from experiment/wa-observer") {
		t.Errorf("prompt does not name the base branch:\n%s", prompt)
	}
}

// Omitting the repository targets norne-ai/experiments at the branch default,
// and a blank value counts as an omission rather than a missing required field.
func TestLaunchOmittedRepositoryTargetsExperiments(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"experiment": "default-a", "prompt": "go"},
	})
	if err != nil || res.IsError {
		t.Fatalf("omitted call: %v / %s", err, callText(res))
	}
	var out launchTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	wantRepository := gitHubOrg + "/" + defaultRepository
	if out.Repository != wantRepository {
		t.Errorf("repository = %q, want the default target %q", out.Repository, wantRepository)
	}
	if out.BaseBranch != defaultBaseBranch {
		t.Errorf("baseBranch = %q, want %q", out.BaseBranch, defaultBaseBranch)
	}

	res, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"repository": "   ", "experiment": "default-b", "prompt": "go"},
	})
	if err != nil || res.IsError {
		t.Fatalf("blank call: %v / %s", err, callText(res))
	}
	var blankOut launchTaskOutput
	decodeStructured(t, res.StructuredContent, &blankOut)
	if blankOut.Repository != wantRepository {
		t.Errorf("blank repository = %q, want the default target %q", blankOut.Repository, wantRepository)
	}

	for _, wsCall := range fake.wsCalls {
		gitRepo := wsCall.GetWorkspace().GetSpec().GetGit()[0]
		if gitRepo.GetRepo() != "https://github.com/"+wantRepository+".git" || gitRepo.GetBranch() != defaultBaseBranch {
			t.Errorf("workspace git = %q at %q, want the defaulted repository at the defaulted branch", gitRepo.GetRepo(), gitRepo.GetBranch())
		}
	}
}

func TestLaunchExplicitlyWithoutPreviewDoesNotOpenPanel(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ax_launch_task",
		Arguments: map[string]any{
			"repository": "work-coordinator",
			"experiment": "docs-only",
			"prompt":     "Improve the documentation.",
			"preview":    false,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out launchTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	task := fake.taskCalls[0].GetTask()
	if _, set := envValue(task, "AX_PREVIEW_TARGET"); set {
		t.Error("non-preview task unexpectedly enables the preview mux")
	}
	if out.PreviewURL != "" || strings.Contains(out.Note, "app preview") {
		t.Errorf("non-preview task note unexpectedly advertises a preview: %q", out.Note)
	}
	prompt, _ := envValue(task, "AX_QWEN_PROMPT")
	if strings.Contains(prompt, "0.0.0.0:3000") {
		t.Error("non-preview task prompt contains web-server instructions")
	}
}

func TestLaunchDefaultsToPreview(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ax_launch_task",
		Arguments: map[string]any{
			"repository": "work-coordinator",
			"experiment": "web-by-default",
			"prompt":     "Build a web app.",
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out launchTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	task := fake.taskCalls[0].GetTask()
	if target, ok := envValue(task, "AX_PREVIEW_TARGET"); !ok || target != previewTarget {
		t.Errorf("default AX_PREVIEW_TARGET = %q/%v, want %q", target, ok, previewTarget)
	}
	wantPreviewURL := "http://preview-" + out.Name + ".norne"
	if out.PreviewURL != wantPreviewURL || !strings.Contains(out.Note, wantPreviewURL) {
		t.Errorf("default preview output = URL %q note %q, want %q", out.PreviewURL, out.Note, wantPreviewURL)
	}
}

func TestLaunchRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty prompt", map[string]any{"repository": "work-coordinator", "experiment": "ok", "prompt": "  "}, "prompt is required"},
		{"bad model", map[string]any{"repository": "work-coordinator", "experiment": "ok", "prompt": "do it", "model": "gpt-5"}, "is not allowed"},
		{"bad effort", map[string]any{"repository": "work-coordinator", "experiment": "ok", "prompt": "do it", "reasoningEffort": "ultra"}, "invalid reasoningEffort"},
		{"uppercase experiment", map[string]any{"repository": "work-coordinator", "experiment": "Bad_Name", "prompt": "do it"}, "must be lowercase"},
		// A missing required field is rejected by the MCP input schema before
		// the handler runs; both paths mention "experiment".
		{"missing experiment", map[string]any{"repository": "work-coordinator", "prompt": "do it"}, "experiment"},
		// The launcher must not let a caller point the injected Git credential
		// anywhere outside the norne-ai organisation.
		{"other organisation", map[string]any{"repository": "QwenLM/qwen-code", "experiment": "ok", "prompt": "do it"}, "norne-ai"},
		{"raw git url", map[string]any{"repository": "https://github.com/evil/experiments.git", "experiment": "ok", "prompt": "do it"}, "norne-ai"},
		{"path traversal", map[string]any{"repository": "../secrets", "experiment": "ok", "prompt": "do it"}, "norne-ai"},
		{"option-like repository", map[string]any{"repository": "--no-verify", "experiment": "ok", "prompt": "do it"}, "norne-ai"},
		{"range base branch", map[string]any{"repository": "work-coordinator", "baseBranch": "main..other", "experiment": "ok", "prompt": "do it"}, "invalid baseBranch"},
		{"option-like base branch", map[string]any{"repository": "work-coordinator", "baseBranch": "--upload-pack=evil", "experiment": "ok", "prompt": "do it"}, "invalid baseBranch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, _ := newSessionWithWrites(t, sampleTasks(), enabledWrites())
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ax_launch_task", Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !res.IsError || !strings.Contains(callText(res), tc.want) {
				t.Errorf("isError=%v text=%q, want it to mention %q", res.IsError, callText(res), tc.want)
			}
		})
	}
}

func TestLaunchRespectsConcurrencyCap(t *testing.T) {
	now := time.Now()
	fake := &fakeClient{listTasks: []*v1alpha1.Task{
		pbTask("wa-one", "default", "Running", now),
		pbTask("wa-two", "default", "Running", now),
		pbTask("wa-old", "default", "Suspended", now), // does not count
		pbTask("user-task", "default", "Running", now),
	}}
	writes := enabledWrites()
	writes.maxActive = 2
	session, _ := newSessionWithWrites(t, fake, writes)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"repository": "work-coordinator", "experiment": "three", "prompt": "go"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(callText(res), "at its cap") {
		t.Fatalf("expected cap refusal, got isError=%v text=%q", res.IsError, callText(res))
	}
	if len(fake.taskCalls) != 0 {
		t.Error("no task should be created once at the cap")
	}
}

func TestLaunchLocalModelUsesNinferProfile(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"repository": "work-coordinator", "experiment": "local-run", "prompt": "summarize the repo", "model": "qwen3.8-27b"},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	if len(fake.taskCalls) != 1 {
		t.Fatalf("task calls = %d, want 1", len(fake.taskCalls))
	}
	task := fake.taskCalls[0].GetTask()

	if model, _ := envValue(task, "OPENAI_MODEL"); model != "qwen3.8-27b" {
		t.Errorf("OPENAI_MODEL = %q", model)
	}
	if ep, _ := envValue(task, "OPENAI_BASE_URL"); ep != ninferEndpoint {
		t.Errorf("endpoint = %q, want the local NInfer service", ep)
	}
	// Local model: key is the non-secret placeholder, and the Model Studio
	// secret and profile must be absent.
	if key, ok := envValue(task, ninferKeyEnv); !ok || key != ninferPlaceholderKey {
		t.Errorf("OPENAI_API_KEY = %q/%v, want the local placeholder", key, ok)
	}
	if ref := secretRef(task, modelStudioKeyEnv); ref != nil {
		t.Error("local NInfer task must not reference the Model Studio secret")
	}
	if _, set := envValue(task, "QWEN_CODE_SYSTEM_SETTINGS_PATH"); set {
		t.Error("local NInfer task must not override the settings profile")
	}
	// The Git credential is still a Secret reference, never a value.
	if ref := secretRef(task, gitTokenEnv); ref == nil || ref.GetName() != gitSecret {
		t.Errorf("GITHUB_TOKEN secret ref = %+v", ref)
	}
}

func TestLaunchPaidModelUsesModelStudioProfile(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"repository": "work-coordinator", "experiment": "paid-run", "prompt": "go", "model": "qwen3.8-flash"},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	task := fake.taskCalls[0].GetTask()
	if ep, _ := envValue(task, "OPENAI_BASE_URL"); ep != modelStudioEndpoint {
		t.Errorf("endpoint = %q, want Model Studio", ep)
	}
	if ref := secretRef(task, modelStudioKeyEnv); ref == nil {
		t.Error("paid model must reference the Model Studio secret")
	}
	if _, leaked := envValue(task, modelStudioKeyEnv); leaked {
		t.Error("Model Studio key must not be a plaintext env var")
	}
}

func TestWaModelsAllowlistParsing(t *testing.T) {
	cfg, err := newConfig(":1", "ax:8080", 10*time.Second, 500, "norne",
		true, "default", defaultRunnerImage, "qwen3.8-flash", defaultWaModels, 3)
	if err != nil {
		t.Fatalf("newConfig with defaults: %v", err)
	}
	got := cfg.writes.models
	want := []allowedModel{
		{Model: "qwen3.8-flash", Provider: "modelstudio", DefaultEffort: "medium"},
		{Model: "qwen3.8-27b", Provider: "ninfer"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A paid default is required when the allowlist is not local-only: mixing
	// a NInfer default with a paid model would silently route to local.
	if _, err := newConfig(":1", "ax:8080", 10*time.Second, 500, "norne",
		true, "default", defaultRunnerImage, "qwen3.8-27b", "modelstudio=qwen3.8-flash,ninfer=qwen3.8-27b", 3); err == nil {
		t.Error("newConfig accepted a NInfer default while a paid model was allowlisted")
	}
	// But a local-only allowlist with a NInfer default is fine.
	if _, err := newConfig(":1", "ax:8080", 10*time.Second, 500, "norne",
		true, "default", defaultRunnerImage, "qwen3.8-27b", "ninfer=qwen3.8-27b", 3); err != nil {
		t.Errorf("local-only allowlist rejected: %v", err)
	}
	// A default not in the allowlist is rejected.
	if _, err := newConfig(":1", "ax:8080", 10*time.Second, 500, "norne",
		true, "default", defaultRunnerImage, "qwen3.8-max", defaultWaModels, 3); err == nil {
		t.Error("newConfig accepted a default model outside the allowlist")
	}
	// A malformed entry is rejected.
	if _, err := newConfig(":1", "ax:8080", 10*time.Second, 500, "norne",
		true, "default", defaultRunnerImage, "qwen3.8-flash", "qwen3.8-flash", 3); err == nil {
		t.Error("newConfig accepted a provider-less model entry")
	}
	// An unknown default-effort suffix is rejected.
	if _, err := newConfig(":1", "ax:8080", 10*time.Second, 500, "norne",
		true, "default", defaultRunnerImage, "qwen3.8-flash", "modelstudio=qwen3.8-flash:ultra", 3); err == nil {
		t.Error("newConfig accepted an invalid effort suffix")
	}
}

func TestLaunchDefaultAndExplicitEffort(t *testing.T) {
	writes := enabledWrites()
	writes.defaultModel = "qwen3.8-flash"
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, writes)

	// Omitted effort -> the flash model's allowlist default (medium).
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"repository": "work-coordinator", "experiment": "one", "prompt": "go"},
	})
	if err != nil || res.IsError {
		t.Fatalf("default call: %v / %s", err, callText(res))
	}
	if e, ok := envValue(fake.taskCalls[0].GetTask(), "AX_QWEN_REASONING_EFFORT"); !ok || e != "medium" {
		t.Errorf("default effort env = %q/%v, want medium", e, ok)
	}

	// Explicit effort overrides the allowlist default.
	res, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"repository": "work-coordinator", "experiment": "two", "prompt": "go", "reasoningEffort": "xhigh"},
	})
	if err != nil || res.IsError {
		t.Fatalf("explicit call: %v / %s", err, callText(res))
	}
	if e, _ := envValue(fake.taskCalls[1].GetTask(), "AX_QWEN_REASONING_EFFORT"); e != "xhigh" {
		t.Errorf("explicit effort = %q, want xhigh", e)
	}

	// The local model has no allowlist default: the env stays unset and the
	// runner's own profile decides.
	res, err = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_launch_task",
		Arguments: map[string]any{"repository": "work-coordinator", "experiment": "three", "prompt": "go", "model": "qwen3.8-27b"},
	})
	if err != nil || res.IsError {
		t.Fatalf("local call: %v / %s", err, callText(res))
	}
	if _, set := envValue(fake.taskCalls[2].GetTask(), "AX_QWEN_REASONING_EFFORT"); set {
		t.Error("ninfer model without an allowlist default must not set AX_QWEN_REASONING_EFFORT")
	}
}

func TestDeleteTwoStepConfirmation(t *testing.T) {
	now := time.Now()
	fake := &fakeClient{listTasks: []*v1alpha1.Task{
		pbTask("wa-experiment-123", "default", "Running", now.Add(-time.Minute)),
		pbTask("important-long-run", "default", "Running", now),
	}}
	// The live spec is what the delete path reads: one workspace the launcher
	// created for this task alone, plus a shared one it must leave mounted.
	fake.getTask = &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "wa-experiment-123", Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{
			{Name: "ws-wa-experiment-123", Path: "/workspace"},
			{Name: "qwen-experiments", Path: "/workspace/shared"},
		}},
	}
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	// Step 1: no confirmName -> reports and refuses.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_delete_task",
		Arguments: map[string]any{"name": "wa-experiment-123"},
	})
	if err != nil || res.IsError {
		t.Fatalf("first call: %v / %s", err, callText(res))
	}
	var out1 deleteTaskOutput
	decodeStructured(t, res.StructuredContent, &out1)
	if !out1.NeedsConfirmation || out1.Deleted {
		t.Errorf("first call = %+v, wants needsConfirmation", out1)
	}
	if out1.Task == nil || out1.Task.Name != "wa-experiment-123" {
		t.Errorf("first call did not echo the task: %+v", out1.Task)
	}
	if len(fake.deleteCalls) != 0 || len(fake.deleteWsCalls) != 0 {
		t.Fatal("an unconfirmed call must not delete the task or its workspace")
	}

	// Step 2: matching confirmName -> deletes.
	res2, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_delete_task",
		Arguments: map[string]any{"name": "wa-experiment-123", "confirmName": "wa-experiment-123"},
	})
	if err != nil || res2.IsError {
		t.Fatalf("confirmed call: %v / %s", err, callText(res2))
	}
	var out2 deleteTaskOutput
	decodeStructured(t, res2.StructuredContent, &out2)
	if !out2.Deleted {
		t.Errorf("second call did not delete: %+v", out2)
	}
	if len(fake.deleteCalls) != 1 || fake.deleteCalls[0].GetName() != "wa-experiment-123" {
		t.Errorf("DeleteTask calls = %+v", fake.deleteCalls)
	}
	// Only the launcher's own workspace is reclaimed; the shared one survives
	// because other tasks mount it.
	if len(fake.deleteWsCalls) != 1 || fake.deleteWsCalls[0].GetName() != "ws-wa-experiment-123" {
		t.Errorf("DeleteWorkspace calls = %+v, want only ws-wa-experiment-123", fake.deleteWsCalls)
	}
	if len(out2.CleanedWorkspaces) != 1 || out2.CleanedWorkspaces[0] != "ws-wa-experiment-123" {
		t.Errorf("cleanedWorkspaces = %v, want [ws-wa-experiment-123]", out2.CleanedWorkspaces)
	}
	if out2.CleanupWarning != "" {
		t.Errorf("cleanupWarning = %q, want none", out2.CleanupWarning)
	}
}

// A workspace that refuses to delete must not make the deletion look failed:
// the task is gone and the operator needs the survivor named explicitly.
func TestDeleteReportsWorkspaceCleanupFailure(t *testing.T) {
	fake := &fakeClient{
		listTasks: []*v1alpha1.Task{pbTask("wa-drop-9", "default", "Running", time.Now())},
		getTask: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "wa-drop-9", Atespace: "default"},
			Spec:     &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "ws-wa-drop-9", Path: "/workspace"}}},
		},
		deleteWsErr: status.Error(codes.Unavailable, "ax-server down"),
	}
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_delete_task",
		Arguments: map[string]any{"name": "wa-drop-9", "confirmName": "wa-drop-9"},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out deleteTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	if !out.Deleted {
		t.Fatal("the task delete succeeded, so the result must report it deleted")
	}
	if len(out.CleanedWorkspaces) != 0 {
		t.Errorf("cleanedWorkspaces = %v, want nothing reported as reclaimed", out.CleanedWorkspaces)
	}
	if !strings.Contains(out.CleanupWarning, "ws-wa-drop-9") {
		t.Errorf("cleanupWarning = %q, want it to name the surviving workspace", out.CleanupWarning)
	}
}

func TestDeleteRefusesNonAssistantTask(t *testing.T) {
	fake := &fakeClient{listTasks: []*v1alpha1.Task{
		pbTask("important-long-run", "default", "Running", time.Now()),
	}}
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_delete_task",
		Arguments: map[string]any{"name": "important-long-run", "confirmName": "important-long-run"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out deleteTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	if out.Deleted {
		t.Error("a task without the wa- prefix must never be deleted through MCP")
	}
	if !strings.Contains(out.Reason, "not launched through this server") {
		t.Errorf("reason = %q, want the policy explanation", out.Reason)
	}
	if len(fake.deleteCalls) != 0 {
		t.Error("policy refusal must not call DeleteTask")
	}
}

func TestDeleteUnknownTaskErrors(t *testing.T) {
	fake := sampleTasks() // no wa- tasks, lookup for a fresh name forces a refresh but finds nothing
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_delete_task",
		Arguments: map[string]any{"name": "wa-does-not-exist", "confirmName": "wa-does-not-exist"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("deleting an unknown task should be a tool error")
	}
}

func TestDeleteReportsRPCFailure(t *testing.T) {
	fake := &fakeClient{
		listTasks: []*v1alpha1.Task{pbTask("wa-drop-1", "default", "Running", time.Now())},
		deleteErr: status.Error(codes.Unavailable, "ax-server down"),
	}
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ax_delete_task",
		Arguments: map[string]any{"name": "wa-drop-1", "confirmName": "wa-drop-1"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(callText(res), "deleting task") {
		t.Errorf("expected the RPC failure surfaced, got isError=%v text=%q", res.IsError, callText(res))
	}
}

func TestLaunchDefaultsToReviewPhase(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ax_launch_task",
		Arguments: map[string]any{
			"repository": "work-coordinator",
			"experiment": "reviewed",
			"prompt":     "Add the retry helper.",
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out launchTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	task := fake.taskCalls[0].GetTask()
	if got, ok := envValue(task, "AX_QWEN_REVIEW"); !ok || got != "1" {
		t.Errorf("AX_QWEN_REVIEW = %q (set=%v), want the review phase on by default", got, ok)
	}
	if got, ok := envValue(task, "AX_GIT_BASE_BRANCH"); !ok || got != defaultBaseBranch {
		t.Errorf("AX_GIT_BASE_BRANCH = %q (set=%v), want it passed so prepare can recover an empty tree", got, ok)
	}
	// The review reads the uncommitted tree, so committing early would starve it.
	prompt, _ := envValue(task, "AX_QWEN_PROMPT")
	if !strings.Contains(prompt, "Do not commit or push yet") {
		t.Error("reviewed task prompt does not hold delivery back for the review phase")
	}
	if strings.Contains(prompt, "push that branch to origin") {
		t.Error("reviewed task prompt still asks the agent to push before the review")
	}
	if !out.Review || !strings.Contains(out.Note, "review cycle") {
		t.Errorf("output should advertise the review cycle, got review=%v note=%q", out.Review, out.Note)
	}
}

func TestLaunchReviewFalseDeliversInOnePass(t *testing.T) {
	fake := sampleTasks()
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ax_launch_task",
		Arguments: map[string]any{
			"repository": "work-coordinator",
			"experiment": "unreviewed",
			"prompt":     "Add the retry helper.",
			"review":     false,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("call: %v / %s", err, callText(res))
	}
	var out launchTaskOutput
	decodeStructured(t, res.StructuredContent, &out)
	task := fake.taskCalls[0].GetTask()
	if got, _ := envValue(task, "AX_QWEN_REVIEW"); got != "0" {
		t.Errorf("AX_QWEN_REVIEW = %q, want 0", got)
	}
	prompt, _ := envValue(task, "AX_QWEN_PROMPT")
	if !strings.Contains(prompt, "push that branch to origin") || strings.Contains(prompt, "Do not commit or push yet") {
		t.Error("single-pass task must keep the original commit-and-push delivery rule")
	}
	if out.Review || !strings.Contains(out.Note, "before declaring completion") {
		t.Errorf("single-pass task should not advertise a review cycle: review=%v note=%q", out.Review, out.Note)
	}
}
