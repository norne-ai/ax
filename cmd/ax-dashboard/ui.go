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
	_ "embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// dashboardPage is the single-page task list. It polls /api/tasks and renders
// client-side, which keeps session tabs open across refreshes instead of
// reloading the whole document.
//
//go:embed ui.html
var dashboardPage []byte

// errorPage renders browser-facing failures. A plain http.Error would be enough
// for a CLI proxy, but these pages are visited in a browser and need a way back
// to the task list.
var errorPage = template.Must(template.New("error").Parse(errorPageHTML))

// taskView is the JSON shape consumed by the dashboard page.
type taskView struct {
	Name             string          `json:"name"`
	Atespace         string          `json:"ateespace"`
	Phase            string          `json:"phase"`
	Actor            string          `json:"actor"`
	WorkerIP         string          `json:"workerIP,omitempty"`
	AgeSeconds       int64           `json:"ageSeconds"`
	PromptTokens     int32           `json:"promptTokens"`
	CompletionTokens int32           `json:"completionTokens"`
	PendingAction    string          `json:"pendingAction,omitempty"`
	Conditions       []conditionInfo `json:"conditions,omitempty"`
	URL              string          `json:"url"`
	// PreviewURL is the task's preview hostname, empty when the task is not
	// running or its name leaves no room for the prefix in one DNS label. The
	// page does not link to it (a card is already an anchor, and nesting links
	// is not legal HTML), but /api/tasks is a fine place to read the convention.
	PreviewURL string `json:"previewURL,omitempty"`
}

// tasksResponse is the /api/tasks payload.
type tasksResponse struct {
	Tasks         []taskView `json:"tasks"`
	Count         int        `json:"count"`
	Running       int        `json:"running"`
	BaseDomain    string     `json:"baseDomain"`
	DashboardHost string     `json:"dashboardHost"`
	GeneratedAt   time.Time  `json:"generatedAt"`
	Stale         bool       `json:"stale"`
	// RefreshMs tells the page how often to re-poll so it matches the server's
	// own cache interval instead of hardcoding a second cadence.
	RefreshMs     int64  `json:"refreshMs"`
	AXServerError string `json:"axServerError,omitempty"`
}

func (d *dashboard) serveDashboard(w http.ResponseWriter, r *http.Request) {
	if d.serveProbe(w, r) {
		return
	}
	switch r.URL.Path {
	case "/":
		d.serveIndex(w, r)
	case "/api/tasks":
		d.serveTasksJSON(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (d *dashboard) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardPage)
}

func (d *dashboard) serveTasksJSON(w http.ResponseWriter, r *http.Request) {
	tasks, fetched, listErr := d.tasks.snapshot()
	now := time.Now()

	views := make([]taskView, 0, len(tasks))
	running := 0
	for _, task := range tasks {
		if task.running() {
			running++
		}
		view := taskView{
			Name:             task.Name,
			Atespace:         task.Atespace,
			Phase:            task.Phase,
			Actor:            task.Actor,
			WorkerIP:         task.WorkerIP,
			PromptTokens:     task.PromptTokens,
			CompletionTokens: task.CompletionTokens,
			PendingAction:    task.PendingAction,
			Conditions:       task.Conditions,
			URL:              d.sessionURL(r, task),
		}
		// Only a Running task can be routed to at all, and the preview hostname
		// is meaningless without the mux an app is running behind.
		if task.running() {
			view.PreviewURL = d.previewURL(r, task)
		}
		if !task.Created.IsZero() {
			view.AgeSeconds = int64(now.Sub(task.Created).Seconds())
		}
		views = append(views, view)
	}

	sort.SliceStable(views, func(i, j int) bool {
		if pi, pj := phaseRank(views[i].Phase), phaseRank(views[j].Phase); pi != pj {
			return pi < pj
		}
		// The AX server already returns newest-first, but sorting here keeps the
		// order stable if that ever changes.
		return views[i].AgeSeconds < views[j].AgeSeconds
	})

	resp := tasksResponse{
		Tasks: views,
		Count: len(views),
		// Reported in terms of the hostname being browsed, so the page renders
		// session links in the same domain the user arrived on.
		Running:       running,
		BaseDomain:    d.baseDomainFor(r.Host),
		DashboardHost: hostOnly(r.Host),
		GeneratedAt:   now.UTC(),
		// A cache older than three refresh intervals means the poller is not
		// keeping up, so the page should say the data may be out of date rather
		// than silently showing a stale list.
		Stale: !fetched.IsZero() && now.Sub(fetched) > 3*d.cfg.refresh,
	}
	resp.RefreshMs = d.cfg.refresh.Milliseconds()
	if listErr != nil {
		resp.AXServerError = listErr.Error()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("writing task list response failed", "error", err)
	}
}

// phaseRank orders the task list so that actionable phases come first.
func phaseRank(phase string) int {
	switch phase {
	case "Running":
		return 0
	case "Pending":
		return 1
	case "Suspended":
		return 2
	case "Failed":
		return 3
	case "Terminating":
		return 4
	default:
		return 5
	}
}

// errorData drives errorPage.
type errorData struct {
	Status int
	Title  string
	Detail string
	Extra  string
	Home   string
}

// renderError writes a styled error page and links back to the dashboard.
func renderError(cfg *config, w http.ResponseWriter, r *http.Request, status int, title, detail, extra string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	data := errorData{
		Status: status,
		Title:  title,
		Detail: detail,
		Extra:  extra,
		Home:   requestScheme(r) + "://" + cfg.primaryHost() + "/",
	}
	if err := errorPage.Execute(w, data); err != nil {
		// The status and headers are already sent, so there is nothing useful
		// left to do but log.
		slog.Warn("writing error page failed", "error", err)
	}
}

const errorPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Status}} {{.Title}} · AX</title>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body {
  margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
  background: #0b0e14; color: #e6e6e6; padding: 24px;
  font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
}
main { max-width: 44rem; width: 100%; }
.code {
  margin: 0 0 4px; font: 600 12px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
  letter-spacing: .12em; text-transform: uppercase; color: #f0a35e;
}
h1 { margin: 0 0 12px; font-size: 1.6rem; font-weight: 600; letter-spacing: -.01em; }
p { margin: 0 0 16px; color: #a8b0bd; }
pre {
  margin: 0 0 20px; padding: 12px 14px; overflow-x: auto; border-radius: 8px;
  background: #12161f; border: 1px solid #232a36; color: #8f98a8;
  font: 12.5px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; white-space: pre-wrap;
}
a.home {
  display: inline-block; padding: 8px 14px; border-radius: 8px; text-decoration: none;
  background: #1c2330; border: 1px solid #2c3543; color: #e6e6e6; font-weight: 500;
}
a.home:hover { background: #232c3c; border-color: #3a4658; }
</style>
</head>
<body>
<main>
<p class="code">Error {{.Status}}</p>
<h1>{{.Title}}</h1>
<p>{{.Detail}}</p>
{{if .Extra}}<pre>{{.Extra}}</pre>{{end}}
<a class="home" href="{{.Home}}">&#8592; AX task dashboard</a>
</main>
</body>
</html>
`
