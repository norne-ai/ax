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

// Package runner implements the logic that runs inside every AX task container.
//
// Given a Task and its Workspace, a run starts the metadata and guest server,
// performs the maiden-run workspace setup, and then starts the task's command
// as a supervised child process. The runner stays up as the container's main
// process until it is told to stop, so the sandbox remains inspectable after
// the command has finished. The ax-task-runner binary is a thin wrapper around
// [Run]; custom images can embed the same behavior by calling it directly.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/google/ax/internal/metadata"
	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

const (
	// DefaultPort is where the metadata and guest server listens when Config.Port
	// is zero. It matches the port Agent Substrate probes for readiness.
	DefaultPort = 80
	// DefaultWorkspacePath is the command's working directory when the task
	// binds no workspaces.
	DefaultWorkspacePath = v1alpha1.DefaultWorkspacePath

	// stopGracePeriod is how long a running task command gets to exit after
	// SIGTERM before it is killed during shutdown.
	stopGracePeriod = 10 * time.Second
)

// Config controls a single Run.
type Config struct {
	// Port is where the metadata HTTP endpoints and guest gRPC services listen.
	// Zero means DefaultPort.
	Port int
	// Task is the task to run. It supplies the command, its environment, and
	// the workspace bindings. Nil runs with no command and default settings.
	Task *v1alpha1.Task
	// Workspaces are the Workspace resources the task binds. Each is matched to
	// an entry of the task's spec.workspaces by name.
	Workspaces []*v1alpha1.Workspace
	// OnCommandExit, when set, is called once the task command has exited.
	OnCommandExit func(CommandExit)
	// ExitOnCommandDone ends Run as soon as the task command finishes, so a
	// headless task's container exit status can mirror the command's own.
	// False — the default every interactive task relies on — keeps the
	// sandbox up and inspectable after the command exits.
	ExitOnCommandDone bool
}

// mount pairs one workspace binding from the task spec with the Workspace
// resource that satisfies it and the path it is set up at.
type mount struct {
	ref  *v1alpha1.WorkspaceRef
	ws   *v1alpha1.Workspace
	path string
}

// resolveMounts lines up the task's workspace bindings with the Workspace
// resources in cfg. Bindings are matched by name; a binding whose Workspace was
// not supplied is set up as an empty directory.
func resolveMounts(cfg Config) []mount {
	byName := make(map[string]*v1alpha1.Workspace, len(cfg.Workspaces))
	for _, ws := range cfg.Workspaces {
		if ws != nil {
			byName[ws.GetMetadata().GetName()] = ws
		}
	}

	spec := cfg.Task.GetSpec()
	refs := spec.WorkspaceRefs()
	paths := spec.WorkspacePaths()
	mounts := make([]mount, len(refs))
	for i, ref := range refs {
		mounts[i] = mount{ref: ref, ws: byName[ref.GetName()], path: paths[i]}
	}
	return mounts
}

// CommandExit describes how the task command finished.
type CommandExit struct {
	// Pid of the command's process.
	Pid int
	// ExitCode is the command's exit status, or -1 if it was killed by a signal.
	ExitCode int
	// Err is nil when the command exited with status zero.
	Err error
}

// Run executes the task-runner lifecycle and blocks until ctx is cancelled.
//
// Every workspace the task binds is set up in declaration order, each at its
// own path. The task's spec.command, if any, is then started as a child process
// in its own process group with the first workspace as its working directory
// and AX_METADATA_URL plus spec.env in its environment. The metadata and guest
// server keeps serving whether or not the command is still running. When ctx is
// cancelled a running command is sent SIGTERM, given a grace period, and then
// killed.
//
// Workspace setup failures are logged but do not abort the run; the task simply
// never reports ready. Run returns an error only when the runner itself cannot
// start, for example when the metadata server or the command fails to launch.
func Run(ctx context.Context, cfg Config) error {
	port := cfg.Port
	if port <= 0 {
		port = DefaultPort
	}
	mounts := resolveMounts(cfg)
	if len(mounts) == 0 {
		// Nothing bound: still provide an empty working directory at the default path.
		mounts = []mount{{ref: &v1alpha1.WorkspaceRef{}, path: DefaultWorkspacePath}}
	}
	wsPath := mounts[0].path
	workspaces := make([]*v1alpha1.Workspace, 0, len(mounts))
	paths := make([]string, len(mounts))
	for i, m := range mounts {
		paths[i] = m.path
		if m.ws != nil {
			workspaces = append(workspaces, m.ws)
		}
	}
	slog.Info("initializing AX task runner", "port", port, "wsPath", wsPath, "workspaces", paths)

	metadataURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	_ = os.Setenv("AX_METADATA_URL", metadataURL)
	for _, e := range cfg.Task.GetSpec().GetEnv() {
		_ = os.Setenv(e.GetName(), e.GetValue())
	}

	metaServer := metadata.NewServer(port, cfg.Task, workspaces, metadata.ServerOptions{
		WorkspacePath:   wsPath,
		HTTPProxyTarget: os.Getenv("AX_HTTP_PROXY_TARGET"),
		HTTPProxyBearer: os.Getenv("AX_HTTP_PROXY_BEARER"),
	})
	if err := metaServer.Start(); err != nil {
		return fmt.Errorf("starting metadata server: %w", err)
	}
	defer func() {
		slog.Info("shutting down metadata server")
		_ = metaServer.Stop(context.Background())
	}()

	ready := true
	for _, m := range mounts {
		if _, err := workspace.SetupWorkspace(ctx, m.ws, m.path, m.ref.GetGoal()); err != nil {
			slog.Error("workspace maiden run setup failed", "workspace", m.ref.GetName(), "path", m.path, "error", err)
			ready = false
		}
	}
	if ready {
		metaServer.SetWorkspaceReady(true)
		slog.Info("workspace maiden run setup marked ready", "count", len(mounts))
	}

	cmdArgs := cfg.Task.GetSpec().GetCommand()
	if len(cmdArgs) == 0 {
		slog.Info("no task command specified; serving metadata until stopped")
		<-ctx.Done()
		return nil
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = wsPath
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = commandEnv(cfg.Task, port)
	// A dedicated process group lets shutdown signal the command together with
	// anything it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting task command %v: %w", cmdArgs, err)
	}
	slog.Info("started task command", "pid", cmd.Process.Pid, "command", cmdArgs)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		reportExit(cfg, cmd, err)
		if cfg.ExitOnCommandDone {
			// Headless lifecycle: the command *is* the task. Return so the
			// container's exit status can mirror the command's, and the
			// platform records a terminal phase instead of holding an actor
			// that serves nothing.
			return nil
		}
		// Keep the sandbox up and inspectable until told to stop.
		<-ctx.Done()
	case <-ctx.Done():
		reportExit(cfg, cmd, stopCommand(cmd, exited))
	}
	return nil
}

// stopCommand asks the command's process group to terminate, kills it if it has
// not exited within the grace period, and returns the command's exit result.
func stopCommand(cmd *exec.Cmd, exited <-chan error) error {
	pid := cmd.Process.Pid
	slog.Info("stopping task command", "pid", pid)
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	timer := time.NewTimer(stopGracePeriod)
	defer timer.Stop()
	select {
	case err := <-exited:
		return err
	case <-timer.C:
		slog.Warn("task command did not exit within grace period; killing", "pid", pid, "gracePeriod", stopGracePeriod)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-exited
	}
}

// reportExit logs how the command finished and notifies the OnCommandExit hook.
func reportExit(cfg Config, cmd *exec.Cmd, err error) {
	exit := CommandExit{Pid: cmd.Process.Pid, ExitCode: -1, Err: err}
	if cmd.ProcessState != nil {
		exit.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		slog.Error("task command exited with error", "pid", exit.Pid, "exitCode", exit.ExitCode, "error", err)
	} else {
		slog.Info("task command completed successfully", "pid", exit.Pid)
	}
	if cfg.OnCommandExit != nil {
		cfg.OnCommandExit(exit)
	}
}

// commandEnv builds the environment for the task command: the runner's own
// environment, the metadata URL, and the Task's spec.env entries.
func commandEnv(task *v1alpha1.Task, port int) []string {
	env := append(os.Environ(), fmt.Sprintf("AX_METADATA_URL=http://127.0.0.1:%d", port))
	for _, e := range task.GetSpec().GetEnv() {
		env = append(env, fmt.Sprintf("%s=%s", e.GetName(), e.GetValue()))
	}
	return env
}
