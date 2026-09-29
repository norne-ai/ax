# Runners

A runner is the program that AX starts as PID 1 inside every task container. It is the bridge between the control plane and whatever your agent actually is: the controller hands it the `Task` and `Workspace` specs, and the runner turns them into a prepared workspace, a running command, and a small HTTP surface that the rest of AX uses to observe the sandbox.

AX ships a default runner, `ax-task-runner`, baked into the default task image. You do not have to use it. Any binary that honors the contract below can be packaged into a container image and named in `spec.image`, and the control plane will treat it exactly like the default.

This page describes what a runner must do. For what the default runner exposes to your command once it is up, see [Sandbox](sandbox.md).

## How a runner is launched

The controller does not run `spec.command` as the container entrypoint. It always starts the container with a fixed command and lets the runner take it from there:

| What the controller sets | Value |
|---|---|
| Container image | `spec.image`, or the default `ax-task-runner` image when unset |
| Container command | `/usr/local/bin/ax-task-runner`, always |
| `AX_TASK_YAML` | The full `Task` resource as YAML, including status |
| `AX_WORKSPACES_YAML` | Every bound `Workspace` resource as a multi-document YAML stream, in the task's binding order |
| `spec.env` entries | Each one set directly in the container environment |
| `spec.secretEnv` entries | Kubernetes Secret keys resolved in the task's atespace and set in the container environment |
| `GEMINI_API_KEY` | Set when the atespace has a Gemini credential configured |
| Volume | A durable directory mounted at `/workspace` |
| Readiness probe | `GET /readyz` on port 80 |

Two consequences follow from that table. Your image must contain an executable at `/usr/local/bin/ax-task-runner`, even if it is a symlink or a shell wrapper around something else. And `spec.command` reaches the runner only through `AX_TASK_YAML`; the runner is responsible for parsing it and starting it.

The `/workspace` volume is what survives suspend and resume. Agent Substrate snapshots it when a task is suspended and restores it into a fresh container when the task is resumed, so the runner will see the same files but a new process tree.

## What a runner must do

**Serve HTTP on port 80.** Both Agent Substrate and the AX controller probe the container on this port. The paths that matter:

| Path | Behavior |
|---|---|
| `/healthz` | Return `200` as soon as the runner is alive. |
| `/readyz` | Return `503` until the workspace is prepared, then `200`. The controller polls this to set the task's `WorkspaceReady` condition, and `ax watch` shows the transition. |
| `/metadata/v1alpha1/ax/task` | Return the `Task` as `application/yaml`. Optional, but your command and `ax` tooling may expect it. |
| `/metadata/v1alpha1/ax/workspaces` | Return every bound `Workspace` as a multi-document YAML stream. Optional, as above. |

**Prepare each workspace once.** A task binds workspaces through `spec.workspaces`. For each binding, at its path, clone the Git repos from `spec.git`, create the skills path, write any MCP configuration, and run any environment bootstrap the binding asks for through its `goal`. A binding without a path lands at `/workspace/<name>`. Record that setup happened somewhere on the durable volume or in a known location, per workspace, then skip the work on later boots. Resume restarts the container, and re-cloning into a restored workspace would destroy the agent's state. The default runner writes a marker file under `/ax` for each workspace path.

**Run the command and supervise it.** Start `spec.command` as a child process with the first workspace as its working directory. Give it `AX_METADATA_URL` pointing at your own HTTP server plus every `spec.env` entry; controller-resolved `spec.secretEnv` values are already inherited from the container environment. Put it in its own process group so you can signal everything it spawns.

**Stay up after the command exits.** The runner is PID 1, and the container lives as long as it does. If the runner exits when the command does, the metadata server goes with it and `ax ssh` stops working. Log the exit status and keep serving until you are told to stop. The control plane does not currently read the command's exit status back from the container.

**Shut down cleanly on `SIGTERM`.** Stop and suspend both deliver `SIGTERM` to PID 1. Forward it to the command's process group, wait a bounded grace period, then `SIGKILL` whatever is left. Flush anything the agent needs to survive a resume before you exit.

**Serve guest services only when asked.** When `spec.debug` is true, the runner should also serve the [Agent Substrate guest services](https://github.com/agent-substrate/env) over gRPC on port 80, multiplexed with the HTTP endpoints using `h2c`. This is what `ax ssh` connects to. When `spec.debug` is false, leave them off. They allow arbitrary process execution and file access inside the sandbox, and `ax ssh` refuses to connect to a task that has not opted in.

## The default runner

`ax-task-runner` lives in `cmd/ax-task-runner` and is a thin wrapper over the `runner` Go package. It implements everything above and is documented from the inside in [Sandbox](sandbox.md). Its image, built from `Dockerfile.task-runner`, is Python 3.12 with `git`, `curl`, `openssh-client`, and the Antigravity agent installed, because the goal-based workspace bootstrap hands the goal to Antigravity.

```bash
make build-task-runner     # cross-compile for linux/amd64 and build the image
make push-task-runner      # push it; set TASK_RUNNER_REPO to choose the registry
```

### Local Qwen Code runner

The repository also includes `Dockerfile.qwen-task-runner`, which packages the
same AX lifecycle runner with Qwen Code. It defaults to the local
OpenAI-compatible Responses API at
`http://ninfer.hermes-agent.svc.cluster.local:8000/v1`, model
`qwen3.8-27b`, with a 132,000-token context window. `OPENAI_BASE_URL`,
`OPENAI_MODEL`, and `OPENAI_API_KEY` are ordinary task environment variables,
so a manifest can override the baked-in local defaults without rebuilding.
AX and Hermes use the same Kubernetes Service and single host NInfer instance.

Build and push it to the kubeadm host's loopback registry, then apply the
example task:

```bash
make push-qwen-task-runner
make build install
make apply-qwen-example
ax watch task qwen-local
```

The Qwen runner also carries the backend development and verification tools an
autonomous task needs: Go, build-essential, GitHub CLI, kubectl, ko, buf,
protoc, grpcurl, Dapr and NATS CLIs, PostgreSQL client tools, jq/yq, ShellCheck,
golangci-lint, staticcheck, govulncheck, gosec, and kubeconform. Versions for
Go-native tools are pinned in `Dockerfile.qwen-task-runner`.

Verify the installed toolchain after a build:

```bash
make verify-qwen-task-runner
```

The image deliberately does not include a container daemon or a host
`kubeconfig`. Supply scoped registry credentials and namespace-limited cluster
credentials to individual tasks when their workflow requires them.

The example runs a persistent Qwen Serve daemon without a wall-time or
session-turn cap. It uses `approvalMode: yolo` because an AX task already runs
inside a dedicated sandbox; use a stricter mode if the workspace or image is
not trusted. Change `AX_QWEN_PROMPT` in `examples/qwen-task.yaml` to the coding
task you want performed.

For an ad-hoc prompt, the helper script creates a uniquely named Qwen Serve
Task that checks out a repository you name, from the private `norne-ai` GitHub
organisation. `--repo` takes a bare repository name (or `norne-ai/<name>`) and
rejects URLs and other organisations, so the injected credential can never be
aimed at a remote the operator did not approve. Omit it and the task runs in
`norne-ai/experiments`. Create the scoped Git credential once in the task's
atespace:

```bash
kubectl -n default create secret generic experiments-git \
  --from-file=token=/path/to/github-token
```

Then launch a task without putting the token in the prompt or Task:

```bash
scripts/run-qwen-task.sh --repo work-coordinator --experiment parser-benchmark --watch \
  "Build and verify the parser benchmark"

# Multiline prompts and automation can use stdin.
printf '%s\n' "Review this implementation" | \
  scripts/run-qwen-task.sh --repo work-coordinator --experiment parser-review

# Or load the prompt from a file, based on a branch other than main.
scripts/run-qwen-task.sh --repo work-coordinator --base-branch experiment/wa-observer \
  --prompt-file task-prompt.md --watch

# Select the model reasoning effort for this task only.
scripts/run-qwen-task.sh --repo work-coordinator --experiment quick-check \
  --reasoning-effort low "Run the focused verification"
```

`--repo` and `--experiment` are both optional: an omitted repository is
`norne-ai/experiments`, and an omitted experiment seed names the task after the
repository instead.

`--reasoning-effort` writes a task-local Qwen Code
`model.generationConfig.extra_body.reasoning.effort` setting. This forwards the
effort without Qwen's optional reasoning-summary request, which the local
NInfer endpoint does not expose. Accepted values are `none`, `low`, `medium`,
and `xhigh`; omit the option to retain the Qwen/NInfer default. The equivalent
environment override is `AX_QWEN_REASONING_EFFORT`.

Each task gets its own workspace object holding the chosen repository, checked
out at `--base-branch` (default `main`) before Qwen starts. `norne-ai/experiments`
has no `main` branch, so a task that should build on it must say
`--base-branch experiment/wa-observer`; without that, the clone fetches a branch
that does not exist and Qwen starts in an empty workspace. The launcher creates
a unique `qwen/<task-name>` branch from it and hands Qwen the whole repository,
instructing it to commit and push that branch. The runner uses Git's askpass
protocol for both the private checkout and Qwen's later pushes, so the token is
not written to `.git/config` or the remote URL. Deleting a task through the AX
MCP server reclaims that per-task workspace; workspaces an operator mounted by
hand are left alone.

The default persistent Qwen daemon submits the initial prompt and exposes its
Web Shell through AX (`--serve` may still be passed explicitly):

```bash
scripts/run-qwen-task.sh --repo work-coordinator --serve --prompt-file task-prompt.md
ax qwen-ui <task-name> --host 0.0.0.0
# Open http://norne:8787 and keep the command running.
```

The bridge forwards HTTP, SSE, and WebSocket traffic through `atenet-router`
while adding the task-routing header. Qwen's bearer stays inside the task and
is injected by the runner's internal proxy. The default bind is the safer
`127.0.0.1`; passing `--host 0.0.0.0` exposes the UI to the LAN and grants code
execution access to anyone who can reach that port. Use `--port` when port 8787
is already occupied.

With `--headless`, Qwen's stream-JSON output is saved in the durable workspace
at `/workspace/qwen-output.jsonl` and can be followed with `ax ssh` using the
command printed by the script. Use `--dry-run` to inspect the generated Task.

## Replacing the default runner

There are three levels of customization. Pick the shallowest one that solves your problem.

### 1. Extend the default image

If the runner behavior is fine and you only need different tools in the sandbox, build on top of the default image and keep its entrypoint:

```dockerfile
# Pin the same digest the examples use so the runner behavior is reproducible.
FROM gcr.io/ax-substrate/ate-images/ax-task-runner@sha256:69b764607ec7f1e433d83d2eca17dccfa04f663b43f071fd376e2dd716a57f8c

RUN apt-get update && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g my-agent
```

The runner binary stays at `/usr/local/bin/ax-task-runner`, so nothing else changes.

### 2. Embed the runner package in your own binary

If you want the standard lifecycle but need to run code around it, import `github.com/google/ax/runner` and call `runner.Run` yourself. This gives you a hook for the command's exit and a place to do your own setup before or after the metadata server starts:

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	"gopkg.in/yaml.v3"
)

func main() {
	var task v1alpha1.Task
	_ = yaml.Unmarshal([]byte(os.Getenv("AX_TASK_YAML")), &task)

	// AX_WORKSPACES_YAML is a multi-document stream, one Workspace per document.
	var workspaces []*v1alpha1.Workspace
	dec := yaml.NewDecoder(strings.NewReader(os.Getenv("AX_WORKSPACES_YAML")))
	for {
		var ws v1alpha1.Workspace
		if err := dec.Decode(&ws); err != nil {
			break
		}
		workspaces = append(workspaces, &ws)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := runner.Run(ctx, runner.Config{
		Task:       &task,
		Workspaces: workspaces,
		OnCommandExit: func(exit runner.CommandExit) {
			slog.Info("agent finished", "exitCode", exit.ExitCode)
			// Upload artifacts, notify a webhook, and so on.
		},
	})
	if err != nil {
		slog.Error("runner failed", "error", err)
		os.Exit(1)
	}
}
```

Cross-compile it for `linux/amd64` with `CGO_ENABLED=0` and copy it into your image at `/usr/local/bin/ax-task-runner`.

### 3. Write a runner from scratch

If the default lifecycle does not fit, for example because your agent framework already supervises processes or you want a different workspace layout, write your own in any language. Read `AX_TASK_YAML` and `AX_WORKSPACES_YAML`, satisfy the contract in the previous section, and install the result at `/usr/local/bin/ax-task-runner`. The `ax.io/v1alpha1` schema is defined in `pkg/apis/v1alpha1/ax.proto`, and [Manifests](manifests.md) walks through every field.

### Ship it

Whichever route you take, push the image to a registry the cluster can pull from and reference it in the task:

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: custom-runner
spec:
  image: "ghcr.io/my-org/my-runner@sha256:..."
  command: ["my-agent", "--goal", "fix the flaky test"]
  debug: true
```

The controller provisions a dedicated Agent Substrate actor template for each distinct image and environment, so different tasks can run different runners side by side in the same atespace.

## Testing a runner locally

The default runner accepts its specs from files as well as the environment, which makes it easy to run outside a cluster. Your own runner should offer something similar:

```bash
ax-task-runner --task-file task.yaml --workspace-file code.yaml --workspace-file tools.yaml --port 8080

# In another shell:
curl -i http://127.0.0.1:8080/readyz
curl -s http://127.0.0.1:8080/metadata/v1alpha1/ax/task
```

Once the image is built, the fastest end-to-end check is a task with `debug: true` and `ax ssh` into it to confirm the workspace, the command, and the environment look the way you expect.

## Checklist

- Executable present at `/usr/local/bin/ax-task-runner`
- Reads `AX_TASK_YAML` and `AX_WORKSPACES_YAML`
- Serves `/healthz` and `/readyz` on port 80, with `/readyz` returning `503` until every workspace is ready
- Prepares each workspace exactly once across restarts and resumes, at its own path
- Starts `spec.command` in the first workspace with `AX_METADATA_URL`, `spec.env`, and resolved `spec.secretEnv`
- Keeps running after the command exits
- Forwards `SIGTERM` to the command's process group and exits after a grace period
- Serves guest gRPC services on port 80 only when `spec.debug` is true
