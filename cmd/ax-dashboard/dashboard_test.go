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
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// stubLister returns a canned ListTasks response so the cache and the routing
// layer can be exercised without an AX server.
type stubLister struct {
	resp    *v1alpha1.ListTasksResponse
	err     error
	calls   int
	lastReq *v1alpha1.ListTasksRequest
}

func (s *stubLister) ListTasks(_ context.Context, in *v1alpha1.ListTasksRequest, _ ...grpc.CallOption) (*v1alpha1.ListTasksResponse, error) {
	s.calls++
	s.lastReq = in
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func newTask(name, atespace, phase, actor string) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{
			Name:              name,
			Atespace:          atespace,
			CreationTimestamp: timestamppb.New(time.Now().Add(-90 * time.Second)),
		},
		Status: &v1alpha1.TaskStatus{
			Phase: phase,
			Actor: actor,
			Usage: &v1alpha1.UsageStats{PromptTokens: 1200, CompletionTokens: 340},
		},
	}
}

func listerWith(tasks ...*v1alpha1.Task) *stubLister {
	return &stubLister{resp: &v1alpha1.ListTasksResponse{Tasks: tasks}}
}

func mustConfig(t *testing.T, routerURL string) *config {
	t.Helper()
	cfg, err := newConfig(":0", "ax-server.example:8080", routerURL, "norne", "ax.norne", 3*time.Second, 500)
	if err != nil {
		t.Fatalf("newConfig: %v", err)
	}
	return cfg
}

// requestFor builds a server-side request with an explicit Host header, which is
// how the ingress controller delivers every *.norne hostname to this service.
func requestFor(t *testing.T, method, host, target string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	return req
}

func TestNewConfig(t *testing.T) {
	tests := []struct {
		name          string
		router        string
		baseDomain    string
		dashboardHost string
		wantErr       bool
		wantRouter    string
	}{
		{name: "bare host port gets http scheme", router: "atenet-router.ate-system.svc.cluster.local:80", baseDomain: "norne", dashboardHost: "ax.norne", wantRouter: "http://atenet-router.ate-system.svc.cluster.local:80"},
		{name: "explicit scheme preserved", router: "http://router.local:80", baseDomain: "norne", dashboardHost: "ax.norne", wantRouter: "http://router.local:80"},
		{name: "uppercase domains normalised", router: "http://r:80", baseDomain: "NORNE", dashboardHost: "AX.NORNE", wantRouter: "http://r:80"},
		{name: "apex dashboard host allowed", router: "http://r:80", baseDomain: "norne", dashboardHost: "norne", wantRouter: "http://r:80"},
		{name: "leading dot tolerated", router: "http://r:80", baseDomain: ".norne", dashboardHost: "ax.norne", wantRouter: "http://r:80"},
		{name: "dashboard host outside base domain", router: "http://r:80", baseDomain: "norne", dashboardHost: "ax.other", wantErr: true},
		{name: "multi label base domain allowed", router: "http://r:80", baseDomain: "example.com", dashboardHost: "ax.example.com", wantRouter: "http://r:80"},
		{name: "several base domains", router: "http://r:80", baseDomain: "norne,example.com", dashboardHost: "ax.norne, ax.example.com", wantRouter: "http://r:80"},
		{name: "base domain with an invalid label", router: "http://r:80", baseDomain: "exa_mple.com", dashboardHost: "ax.norne", wantErr: true},
		{name: "one dashboard host outside the list", router: "http://r:80", baseDomain: "norne,example.com", dashboardHost: "ax.norne,ax.other", wantErr: true},
		{name: "empty base domain", router: "http://r:80", baseDomain: "", dashboardHost: "ax.norne", wantErr: true},
		{name: "empty router", router: "", baseDomain: "norne", dashboardHost: "ax.norne", wantErr: true},
		{name: "unsupported scheme", router: "grpc://r:80", baseDomain: "norne", dashboardHost: "ax.norne", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := newConfig(":0", "http://ax-server:8080", tc.router, tc.baseDomain, tc.dashboardHost, 3*time.Second, 500)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got config %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.routerURL.String(); got != tc.wantRouter {
				t.Errorf("routerURL = %q, want %q", got, tc.wantRouter)
			}
			if got := cfg.axServerAddr; got != "ax-server:8080" {
				t.Errorf("axServerAddr = %q, want the http:// prefix stripped", got)
			}
		})
	}
}

func TestNewConfigRejectsShortRefresh(t *testing.T) {
	if _, err := newConfig(":0", "ax:8080", "http://r:80", "norne", "ax.norne", 100*time.Millisecond, 500); err == nil {
		t.Fatal("expected a sub-second refresh interval to be rejected")
	}
}

func TestIsDNSLabel(t *testing.T) {
	valid := []string{"a", "task-1", "qwen-modelstudio-snake-1790427538", "norne", "a1", strings.Repeat("a", 63)}
	for _, s := range valid {
		if !isDNSLabel(s) {
			t.Errorf("isDNSLabel(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "-lead", "trail-", "has.dot", "UPPER", "under_score", strings.Repeat("a", 64), "sp ace"}
	for _, s := range invalid {
		if isDNSLabel(s) {
			t.Errorf("isDNSLabel(%q) = true, want false", s)
		}
	}
}

func TestSessionLabel(t *testing.T) {
	d := &dashboard{cfg: &config{baseDomains: []string{"norne"}, dashHosts: []string{"ax.norne", "norne"}}}

	tests := []struct {
		host     string
		want     string
		wantOK   bool
		comments string
	}{
		{host: "qwen-wa-observer-1790410920.norne", want: "qwen-wa-observer-1790410920", wantOK: true},
		{host: "task.norne", want: "task", wantOK: true},
		{host: "deeper.sub.norne", wantOK: false, comments: "task names are single labels"},
		{host: "norne", wantOK: false},
		{host: "task.otherdomain", wantOK: false},
		{host: "notnorne", wantOK: false, comments: "suffix must be preceded by a dot"},
		{host: ".norne", wantOK: false},
		{host: "Task.Norne", want: "task.norne", wantOK: false, comments: "input is already lowercased by ServeHTTP"},
	}

	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			got, ok := d.sessionLabel(tc.host)
			if ok != tc.wantOK {
				t.Fatalf("sessionLabel(%q) ok = %v, want %v (%s)", tc.host, ok, tc.wantOK, tc.comments)
			}
			if ok && got != tc.want {
				t.Errorf("sessionLabel(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

// TestMultipleBaseDomains covers serving the LAN wildcard and a Cloudflare
// Tunnel hostname from one deployment: both must dispatch, and session links
// must stay in whichever domain the browser is currently using.
func TestMultipleBaseDomains(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		w.Header().Set("X-Seen-Actor", r.Header.Get(targetActorHeader))
	}))
	defer router.Close()

	cfg, err := newConfig(":0", "ax:8080", router.URL, "norne,example.com", "ax.norne,ax.example.com", 3*time.Second, 500)
	if err != nil {
		t.Fatalf("newConfig: %v", err)
	}
	// Longest first so the more specific domain wins a match.
	if cfg.baseDomains[0] != "example.com" || cfg.baseDomains[1] != "norne" {
		t.Errorf("baseDomains = %v, want [example.com norne]", cfg.baseDomains)
	}
	// The apex of each base domain is added automatically.
	for _, want := range []string{"ax.norne", "ax.example.com", "example.com", "norne"} {
		if !slices.Contains(cfg.dashHosts, want) {
			t.Errorf("dashHosts = %v, want it to include %q", cfg.dashHosts, want)
		}
	}
	if got := cfg.primaryHost(); got != "ax.norne" {
		t.Errorf("primaryHost() = %q, want ax.norne", got)
	}

	lister := listerWith(newTask("t1", "default", "Running", "t1"))
	d := newDashboard(cfg, newTaskSource(lister, 500, 3*time.Second))
	if err := d.tasks.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	for _, host := range []string{"ax.norne", "norne", "ax.example.com", "example.com"} {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, requestFor(t, "GET", host, "/api/tasks"))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", host, rec.Code)
			continue
		}
		var resp tasksResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decoding: %v", host, err)
		}
		wantBase := "norne"
		if strings.HasSuffix(host, "example.com") {
			wantBase = "example.com"
		}
		if resp.BaseDomain != wantBase {
			t.Errorf("%s: baseDomain = %q, want %q", host, resp.BaseDomain, wantBase)
		}
		if want := "http://t1." + wantBase + "/"; resp.Tasks[0].URL != want {
			t.Errorf("%s: URL = %q, want %q", host, resp.Tasks[0].URL, want)
		}
		if resp.DashboardHost != host {
			t.Errorf("%s: dashboardHost = %q, want the request's own host", host, resp.DashboardHost)
		}
	}

	// Both domains reach the same actor, with Host preserved in each case.
	for _, host := range []string{"t1.norne", "t1.example.com"} {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, requestFor(t, "GET", host, "/"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", host, rec.Code)
		}
		if got := rec.Header().Get("X-Seen-Actor"); got != "default/t1" {
			t.Errorf("%s: actor = %q, want default/t1", host, got)
		}
		if got := rec.Header().Get("X-Seen-Host"); got != host {
			t.Errorf("%s: upstream Host = %q, want it preserved", host, got)
		}
	}

	// A task name is still exactly one label, in either domain.
	for _, host := range []string{"a.b.norne", "a.b.example.com"} {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, requestFor(t, "GET", host, "/"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", host, rec.Code)
		}
	}
}

func TestHostOnlyAndPortOf(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPort string
	}{
		{in: "task.norne", wantHost: "task.norne", wantPort: ""},
		{in: "task.norne:8080", wantHost: "task.norne", wantPort: "8080"},
		{in: "[::1]:8080", wantHost: "::1", wantPort: "8080"},
		{in: "", wantHost: "", wantPort: ""},
	}
	for _, tc := range tests {
		if got := hostOnly(tc.in); got != tc.wantHost {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.wantHost)
		}
		if got := portOf(tc.in); got != tc.wantPort {
			t.Errorf("portOf(%q) = %q, want %q", tc.in, got, tc.wantPort)
		}
	}
}

// TestSessionProxySetsActorAndPreservesHost covers the two properties the whole
// design rests on: the router selects a target from ate-target-actor alone, and
// the Web Shell rejects requests whose Origin does not match Host. SetURL
// rewrites the outbound Host, so it has to be restored explicitly.
func TestSessionProxySetsActorAndPreservesHost(t *testing.T) {
	type seen struct {
		host  string
		actor string
		path  string
		xff   string
	}
	got := make(chan seen, 1)

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{
			host:  r.Host,
			actor: r.Header.Get(targetActorHeader),
			path:  r.URL.RequestURI(),
			xff:   r.Header.Get("X-Forwarded-Host"),
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("shell"))
	}))
	defer router.Close()

	lister := listerWith(newTask("my-task", "default", "Running", "my-task"))
	d := newDashboard(mustConfig(t, router.URL), newTaskSource(lister, 500, 3*time.Second))
	if err := d.tasks.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, requestFor(t, "GET", "my-task.norne", "/assets/index.js?v=1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "shell" {
		t.Errorf("body = %q, want the upstream response", body)
	}

	select {
	case s := <-got:
		if s.actor != "default/my-task" {
			t.Errorf("%s = %q, want %q", targetActorHeader, s.actor, "default/my-task")
		}
		if s.host != "my-task.norne" {
			t.Errorf("upstream Host = %q, want the browser hostname preserved, not the router's", s.host)
		}
		if s.path != "/assets/index.js?v=1" {
			t.Errorf("upstream path = %q, want the original path and query", s.path)
		}
		if s.xff != "my-task.norne" {
			t.Errorf("X-Forwarded-Host = %q, want %q", s.xff, "my-task.norne")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the request never reached the router")
	}
}

func TestSessionProxyRejectsNonRunningTask(t *testing.T) {
	reached := false
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer router.Close()

	tests := []struct {
		name     string
		task     *v1alpha1.Task
		wantCode int
	}{
		{name: "suspended", task: newTask("susp-task", "default", "Suspended", "susp-task"), wantCode: http.StatusConflict},
		{name: "failed", task: newTask("failed-task", "default", "Failed", "failed-task"), wantCode: http.StatusConflict},
		{name: "terminating", task: newTask("gone-task", "default", "Terminating", "gone-task"), wantCode: http.StatusConflict},
		{name: "running without actor", task: newTask("no-actor", "default", "Running", ""), wantCode: http.StatusConflict},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			lister := listerWith(tc.task)
			d := newDashboard(mustConfig(t, router.URL), newTaskSource(lister, 500, 3*time.Second))
			if err := d.tasks.refresh(context.Background()); err != nil {
				t.Fatalf("refresh: %v", err)
			}

			rec := httptest.NewRecorder()
			d.ServeHTTP(rec, requestFor(t, "GET", tc.task.Metadata.Name+".norne", "/"))

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if reached {
				t.Error("the router was called for a task that must not be proxied")
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Errorf("Content-Type = %q, want an HTML explanation a browser can render", ct)
			}
		})
	}
}

func TestUnknownTaskReturns404(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the router must not be called for an unknown task")
	}))
	defer router.Close()

	d := newDashboard(mustConfig(t, router.URL), newTaskSource(listerWith(), 500, 3*time.Second))
	if err := d.tasks.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, requestFor(t, "GET", "nope.norne", "/"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestLookupForcesRefreshOnMiss covers a task created after the last poll: the
// first request for it must not 404 just because the cache is a few seconds old.
func TestLookupForcesRefreshOnMiss(t *testing.T) {
	lister := listerWith()
	d := newDashboard(mustConfig(t, "http://127.0.0.1:1"), newTaskSource(lister, 500, time.Hour))
	if err := d.tasks.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if lister.calls != 1 {
		t.Fatalf("calls = %d, want 1", lister.calls)
	}

	// The task appears in the AX server after the first poll.
	lister.resp = &v1alpha1.ListTasksResponse{Tasks: []*v1alpha1.Task{newTask("fresh-task", "default", "Running", "fresh-task")}}

	info, ok := d.tasks.lookup(context.Background(), "fresh-task")
	if !ok {
		t.Fatal("lookup missed a task the server already knows about")
	}
	if info.Phase != "Running" {
		t.Errorf("phase = %q, want Running", info.Phase)
	}
	if lister.calls != 2 {
		t.Errorf("calls = %d, want 2 (the miss should force exactly one refresh)", lister.calls)
	}

	// A second hit is served from the cache.
	before := lister.calls
	if _, ok := d.tasks.lookup(context.Background(), "fresh-task"); !ok {
		t.Fatal("lookup missed a cached task")
	}
	if lister.calls != before {
		t.Errorf("calls = %d, want no extra refresh for a cached name", lister.calls)
	}
}

func TestRefreshPropagatesError(t *testing.T) {
	lister := &stubLister{err: errors.New("ax-server unavailable")}
	src := newTaskSource(lister, 500, 3*time.Second)

	if err := src.refresh(context.Background()); err == nil {
		t.Fatal("expected the ListTasks error to surface")
	}
	if ok, err := src.health(); ok || err == nil {
		t.Errorf("health = (%v, %v), want a failure", ok, err)
	}

	// The dashboard still renders, but reports the problem instead of showing an
	// empty list that looks like "no tasks".
	d := newDashboard(mustConfig(t, "http://127.0.0.1:1"), src)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, requestFor(t, "GET", "ax.norne", "/api/tasks"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp tasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.AXServerError == "" {
		t.Error("axServerError is empty, want the failure reported to the page")
	}
}

func TestListTasksAsksForEveryAtespace(t *testing.T) {
	lister := listerWith(newTask("t1", "default", "Running", "t1"))
	src := newTaskSource(lister, 500, 3*time.Second)
	if err := src.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// An empty atespace is what makes the AX server return tasks from all
	// atespaces rather than only the default one.
	if got := lister.lastReq.GetAtespace(); got != "" {
		t.Errorf("Atespace = %q, want empty", got)
	}
	if got := lister.lastReq.GetLimit(); got != 500 {
		t.Errorf("Limit = %d, want 500", got)
	}
}

func TestTaskInfoMapping(t *testing.T) {
	task := newTask("map-task", "team-a", "Running", "map-task")
	task.Status.PendingApproval = &v1alpha1.PendingApproval{Id: "ap-1", Action: "shell"}
	task.Status.Conditions = []*v1alpha1.Condition{
		{Type: "Ready", Status: "True"},
		{Type: "GatewayReady", Status: "False", Reason: "NoGateway", Message: "no gateway bound"},
	}

	info := toTaskInfo(task)

	if !info.running() {
		t.Error("running() = false, want true")
	}
	if got, want := info.targetActor(), "team-a/map-task"; got != want {
		t.Errorf("targetActor() = %q, want %q", got, want)
	}
	if info.PromptTokens != 1200 || info.CompletionTokens != 340 {
		t.Errorf("usage = (%d, %d), want (1200, 340)", info.PromptTokens, info.CompletionTokens)
	}
	if info.PendingAction != "shell" {
		t.Errorf("PendingAction = %q, want shell", info.PendingAction)
	}
	if len(info.Conditions) != 2 {
		t.Fatalf("conditions = %d, want 2", len(info.Conditions))
	}
	if info.Created.IsZero() {
		t.Error("Created is zero, want the metadata timestamp")
	}
}

func TestAtespaceDefaultsWhenEmpty(t *testing.T) {
	info := toTaskInfo(newTask("no-atespace", "", "Running", "no-atespace"))
	if info.Atespace != "default" {
		t.Errorf("Atespace = %q, want the default applied", info.Atespace)
	}
	if got := info.targetActor(); got != "default/no-atespace" {
		t.Errorf("targetActor() = %q, want default/no-atespace", got)
	}
}

func TestDashboardHostServesUIAndAPI(t *testing.T) {
	lister := listerWith(
		newTask("run-task", "default", "Running", "run-task"),
		newTask("old-task", "default", "Failed", "old-task"),
	)
	d := newDashboard(mustConfig(t, "http://127.0.0.1:1"), newTaskSource(lister, 500, 3*time.Second))
	if err := d.tasks.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	for _, host := range []string{"ax.norne", "ax.norne:8080", "norne"} {
		t.Run("index/"+host, func(t *testing.T) {
			rec := httptest.NewRecorder()
			d.ServeHTTP(rec, requestFor(t, "GET", host, "/"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html", ct)
			}
			if !strings.Contains(rec.Body.String(), "AX sessions") {
				t.Error("the index page did not render")
			}
		})
	}

	t.Run("api", func(t *testing.T) {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, requestFor(t, "GET", "ax.norne", "/api/tasks"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var resp tasksResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if resp.Count != 2 || resp.Running != 1 {
			t.Errorf("count/running = %d/%d, want 2/1", resp.Count, resp.Running)
		}
		if resp.BaseDomain != "norne" || resp.DashboardHost != "ax.norne" {
			t.Errorf("baseDomain/dashboardHost = %q/%q", resp.BaseDomain, resp.DashboardHost)
		}
		if resp.RefreshMs != 3000 {
			t.Errorf("refreshMs = %d, want 3000", resp.RefreshMs)
		}
		// Running tasks sort ahead of terminal ones so the page leads with what
		// is actionable.
		if resp.Tasks[0].Name != "run-task" {
			t.Errorf("first task = %q, want the Running one", resp.Tasks[0].Name)
		}
		if got := resp.Tasks[0].URL; got != "http://run-task.norne/" {
			t.Errorf("URL = %q, want http://run-task.norne/", got)
		}
		if resp.Tasks[0].AgeSeconds <= 0 {
			t.Errorf("AgeSeconds = %d, want a positive age", resp.Tasks[0].AgeSeconds)
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, requestFor(t, "GET", "ax.norne", "/nope"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func TestSessionURLHonoursSchemeAndPort(t *testing.T) {
	d := newDashboard(mustConfig(t, "http://127.0.0.1:1"), newTaskSource(listerWith(), 500, 3*time.Second))
	task := toTaskInfo(newTask("t", "default", "Running", "t"))

	tests := []struct {
		name      string
		host      string
		forwarded string
		tls       bool
		want      string
	}{
		{name: "plain http on 80", host: "ax.norne", want: "http://t.norne/"},
		{name: "non standard port preserved", host: "ax.norne:8080", want: "http://t.norne:8080/"},
		{name: "explicit 80 dropped", host: "ax.norne:80", want: "http://t.norne/"},
		{name: "forwarded proto wins", host: "ax.norne", forwarded: "https", want: "https://t.norne/"},
		{name: "first forwarded proto only", host: "ax.norne", forwarded: "https, http", want: "https://t.norne/"},
		{name: "terminated tls", host: "ax.norne", tls: true, want: "https://t.norne/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := requestFor(t, "GET", tc.host, "/api/tasks")
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if got := d.sessionURL(req, task); got != tc.want {
				t.Errorf("sessionURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbesAnswerOnAnyHost(t *testing.T) {
	d := newDashboard(mustConfig(t, "http://127.0.0.1:1"), newTaskSource(listerWith(), 500, 3*time.Second))
	_ = d.tasks.refresh(context.Background())

	// The kubelet addresses probes to the pod IP, which matches neither the
	// dashboard host nor a session host, so they must be answered before the
	// hostname dispatch rejects the request.
	for _, host := range []string{"10.0.0.42:8080", "ax.norne", "localhost"} {
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, requestFor(t, "GET", host, "/healthz"))
		if rec.Code != http.StatusOK {
			t.Errorf("healthz on %s = %d, want 200", host, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, requestFor(t, "GET", "10.0.0.42:8080", "/"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("index on a pod IP = %d, want 404", rec.Code)
	}
}

// TestSessionHostKeepsRunnerEndpoints confirms the dashboard does not shadow the
// task's own /healthz: the AX task runner serves that path on the actor's port
// 80, so on a session hostname it must be proxied rather than answered locally.
func TestSessionHostKeepsRunnerEndpoints(t *testing.T) {
	gotPath := make(chan string, 1)
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
	}))
	defer router.Close()

	lister := listerWith(newTask("probe-task", "default", "Running", "probe-task"))
	d := newDashboard(mustConfig(t, router.URL), newTaskSource(lister, 500, 3*time.Second))
	if err := d.tasks.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, requestFor(t, "GET", "probe-task.norne", "/healthz"))

	select {
	case path := <-gotPath:
		if path != "/healthz" {
			t.Errorf("proxied path = %q, want /healthz", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/healthz on a session host was answered locally instead of proxied")
	}
}

// TestWebSocketUpgradeIsProxied is the load-bearing check for the Web Shell:
// terminals, voice and ACP all run over WebSocket, and a proxy that buffers or
// downgrades them yields a page that loads but never streams.
func TestWebSocketUpgradeIsProxied(t *testing.T) {
	upgraded := make(chan string, 1)

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: test\r\n\r\n")
		_ = buf.Flush()
		upgraded <- r.Header.Get(targetActorHeader) + "|" + r.Host
	}))
	defer router.Close()

	lister := listerWith(newTask("ws-task", "default", "Running", "ws-task"))
	d := newDashboard(mustConfig(t, router.URL), newTaskSource(lister, 500, 3*time.Second))
	if err := d.tasks.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	front := httptest.NewServer(d)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatalf("dialing the dashboard: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	_, err = conn.Write([]byte("GET /terminal?terminalId=1&replay=1 HTTP/1.1\r\n" +
		"Host: ws-task.norne\r\n" +
		"Origin: http://ws-task.norne\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"))
	if err != nil {
		t.Fatalf("writing the upgrade request: %v", err)
	}

	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q, want a 101 Switching Protocols", strings.TrimSpace(statusLine))
	}

	select {
	case seen := <-upgraded:
		if want := "default/ws-task|ws-task.norne"; seen != want {
			t.Errorf("router saw %q, want actor and preserved host %q", seen, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the upgrade never reached the router")
	}
}
