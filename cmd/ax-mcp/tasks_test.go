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
	"errors"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeClient is an in-memory taskClient. Responses are held in new-old-first
// order, the same newest-first spelling the AX server uses.
type fakeClient struct {
	listTasks []*v1alpha1.Task
	listErr   error
	getTask   *v1alpha1.Task
	getErr    error

	// Write-path records and injected failures.
	wsCalls       []*v1alpha1.UpdateWorkspaceRequest
	taskCalls     []*v1alpha1.UpdateTaskRequest
	deleteCalls   []*v1alpha1.DeleteTaskRequest
	deleteWsCalls []*v1alpha1.DeleteWorkspaceRequest
	writeErr      error
	deleteErr     error
	deleteWsErr   error

	listCalls int
	getCalls  int
}

func (f *fakeClient) ListTasks(_ context.Context, _ *v1alpha1.ListTasksRequest, _ ...grpc.CallOption) (*v1alpha1.ListTasksResponse, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &v1alpha1.ListTasksResponse{Tasks: f.listTasks}, nil
}

func (f *fakeClient) GetTask(_ context.Context, _ *v1alpha1.GetTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getTask, nil
}

func (f *fakeClient) UpdateWorkspace(_ context.Context, in *v1alpha1.UpdateWorkspaceRequest, _ ...grpc.CallOption) (*v1alpha1.Workspace, error) {
	f.wsCalls = append(f.wsCalls, in)
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return in.GetWorkspace(), nil
}

func (f *fakeClient) UpdateTask(_ context.Context, in *v1alpha1.UpdateTaskRequest, _ ...grpc.CallOption) (*v1alpha1.Task, error) {
	f.taskCalls = append(f.taskCalls, in)
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return in.GetTask(), nil
}

func (f *fakeClient) DeleteTask(_ context.Context, in *v1alpha1.DeleteTaskRequest, _ ...grpc.CallOption) (*v1alpha1.DeleteTaskResponse, error) {
	f.deleteCalls = append(f.deleteCalls, in)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (f *fakeClient) DeleteWorkspace(_ context.Context, in *v1alpha1.DeleteWorkspaceRequest, _ ...grpc.CallOption) (*v1alpha1.DeleteWorkspaceResponse, error) {
	f.deleteWsCalls = append(f.deleteWsCalls, in)
	if f.deleteWsErr != nil {
		return nil, f.deleteWsErr
	}
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}

func pbTask(name, ateespace, phase string, created time.Time) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{
			Name:              name,
			Atespace:          ateespace,
			CreationTimestamp: timestamppb.New(created),
		},
		Status: &v1alpha1.TaskStatus{
			Phase: phase,
			Usage: &v1alpha1.UsageStats{PromptTokens: 100, CompletionTokens: 25},
			Conditions: []*v1alpha1.Condition{
				{Type: "Ready", Status: "True"},
			},
		},
	}
}

func TestRefreshMapsTasks(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fake := &fakeClient{listTasks: []*v1alpha1.Task{
		pbTask("Alpha", "", "Running", now),
		pbTask("beta", "team", "Suspended", now.Add(-time.Hour)),
	}}
	src := newTaskSource(fake, 100, time.Minute)

	if err := src.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	tasks, fetched, err := src.snapshot()
	if err != nil {
		t.Fatalf("snapshot error: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if fetched.IsZero() {
		t.Error("fetched timestamp not recorded")
	}
	if tasks[0].Name != "Alpha" || tasks[0].Atespace != "default" || tasks[0].Phase != "Running" {
		t.Errorf("task 0 = %+v, want Alpha/default/Running with empty atespace defaulted", tasks[0])
	}
	if tasks[1].Atespace != "team" || tasks[1].Phase != "Suspended" {
		t.Errorf("task 1 = %+v, want team/Suspended", tasks[1])
	}

	// Lookup is case-insensitive on the cached key.
	if info, ok := src.cached("ALPHA"); !ok || info.Name != "Alpha" {
		t.Errorf("cached(ALPHA) = %+v, %v; want the Alpha task", info, ok)
	}
}

func TestRefreshAmbiguousNameKeepsNewest(t *testing.T) {
	now := time.Now()
	fake := &fakeClient{listTasks: []*v1alpha1.Task{
		pbTask("dup", "newer", "Running", now),
		pbTask("dup", "older", "Failed", now.Add(-time.Hour)),
	}}
	src := newTaskSource(fake, 100, time.Minute)

	if err := src.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	info, ok := src.cached("dup")
	if !ok {
		t.Fatal("dup missing from cache")
	}
	if info.Atespace != "newer" {
		t.Errorf("dup resolved to atespace %q, want the newest entry (newer)", info.Atespace)
	}
}

func TestLookupMissForcesRefreshRateLimited(t *testing.T) {
	fake := &fakeClient{listTasks: []*v1alpha1.Task{
		pbTask("known", "default", "Running", time.Now()),
	}}
	src := newTaskSource(fake, 100, time.Minute)
	if err := src.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	before := fake.listCalls

	// A miss forces one refresh; the unknown task never appears, so the
	// second miss inside the rate window must not force another.
	if _, ok := src.lookup(context.Background(), "unknown"); ok {
		t.Fatal("lookup found a task that does not exist")
	}
	if _, ok := src.lookup(context.Background(), "unknown2"); ok {
		t.Fatal("lookup found a task that does not exist")
	}
	if got := fake.listCalls - before; got != 1 {
		t.Errorf("two cache misses forced %d refreshes, want 1 (rate-limited)", got)
	}

	// A hit never refreshes.
	if _, ok := src.lookup(context.Background(), "known"); !ok {
		t.Fatal("lookup missed a cached task")
	}
	if got := fake.listCalls - before; got != 1 {
		t.Errorf("cache hit forced extra refreshes: %d calls", got)
	}
}

func TestRefreshFailureRecorded(t *testing.T) {
	boom := errors.New("boom")
	fake := &fakeClient{listErr: boom}
	src := newTaskSource(fake, 100, time.Minute)

	if err := src.refresh(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("refresh error = %v, want boom", err)
	}
	if ok, err := src.health(); ok || !errors.Is(err, boom) {
		t.Errorf("health = %v, %v; want the refresh error surfaced", ok, err)
	}
	// A later success clears the recorded error.
	fake.listErr = nil
	fake.listTasks = []*v1alpha1.Task{pbTask("x", "default", "Running", time.Now())}
	if err := src.refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if ok, err := src.health(); !ok || err != nil {
		t.Errorf("health after recovery = %v, %v; want true, nil", ok, err)
	}
}

func TestTaskDetailCallsGetTask(t *testing.T) {
	fake := &fakeClient{getTask: pbTask("solo", "default", "Running", time.Now())}
	src := newTaskSource(fake, 100, time.Minute)

	task, err := src.taskDetail(context.Background(), "default", "solo")
	if err != nil {
		t.Fatalf("taskDetail: %v", err)
	}
	if task.GetMetadata().GetName() != "solo" {
		t.Errorf("got task %q, want solo", task.GetMetadata().GetName())
	}
	if fake.getCalls != 1 {
		t.Errorf("GetTask called %d times, want 1", fake.getCalls)
	}
}
