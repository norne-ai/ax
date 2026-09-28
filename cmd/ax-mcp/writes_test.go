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

func TestLaunchTaskBuildsConfinedSpec(t *testing.T) {
	fake := sampleTasks()
	fake.getTask = nil
	session, _ := newSessionWithWrites(t, fake, enabledWrites())

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ax_launch_task",
		Arguments: map[string]any{
			"experiment": "fix.parse_bug-2",
			"prompt":     "Write a parser regression test and make it pass.",
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
	if !strings.Contains(spec.GetCommand()[2], "runs/$AX_EXPERIMENT_NAME") {
		t.Error("command does not create the runs/<experiment>/ directory")
	}
	if task.GetMetadata().GetAtespace() != "default" {
		t.Errorf("task atespace = %q", task.GetMetadata().GetAtespace())
	}

	// Prompt confinement must be baked into the rendered prompt.
	prompt, _ := envValue(task, "AX_QWEN_PROMPT")
	if !strings.Contains(prompt, "runs/fix.parse_bug-2/") || !strings.Contains(prompt, out.Branch) {
		t.Errorf("wrapped prompt missing confinement or branch:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Write a parser regression test") {
		t.Error("wrapped prompt lost the user task")
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

func TestLaunchRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty prompt", map[string]any{"experiment": "ok", "prompt": "  "}, "prompt is required"},
		{"bad model", map[string]any{"experiment": "ok", "prompt": "do it", "model": "gpt-5"}, "is not allowed"},
		{"bad effort", map[string]any{"experiment": "ok", "prompt": "do it", "reasoningEffort": "ultra"}, "invalid reasoningEffort"},
		{"uppercase experiment", map[string]any{"experiment": "Bad_Name", "prompt": "do it"}, "must be lowercase"},
		// A missing required field is rejected by the MCP input schema before
		// the handler runs; both paths mention "experiment".
		{"missing experiment", map[string]any{"prompt": "do it"}, "experiment"},
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
		Arguments: map[string]any{"experiment": "three", "prompt": "go"},
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
		Arguments: map[string]any{"experiment": "local-run", "prompt": "summarize the repo", "model": "qwen3.8-27b"},
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
		Arguments: map[string]any{"experiment": "paid-run", "prompt": "go", "model": "qwen3.8-flash"},
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
		{Model: "qwen3.8-flash", Provider: "modelstudio"},
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
}

func TestDeleteTwoStepConfirmation(t *testing.T) {
	now := time.Now()
	fake := &fakeClient{listTasks: []*v1alpha1.Task{
		pbTask("wa-experiment-123", "default", "Running", now.Add(-time.Minute)),
		pbTask("important-long-run", "default", "Running", now),
	}}
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
	if len(fake.deleteCalls) != 0 {
		t.Fatal("an unconfirmed call must not delete")
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
