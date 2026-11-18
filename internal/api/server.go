// Package api exposes the admin UI and the JSON API used to manage projects.
package api

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"

	"selfhoster/internal/buildinfo"
	"selfhoster/internal/mcp"
	"selfhoster/internal/oauth"
	"selfhoster/internal/projects"
	"selfhoster/internal/settings"
	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
)

// ui/dist is the React admin app built from web/ (git-ignored). ui/placeholder.html
// is served instead when the binary was built without it.
//
//go:embed ui
var uiFiles embed.FS

const maxBodyBytes = 1 << 20

// projectView is a project definition together with its runtime status.
type projectView struct {
	store.Project
	// DataDir is the project's persistent folder, passed to it as DATA_DIR.
	DataDir string            `json:"dataDir"`
	Status  supervisor.Status `json:"status"`
}

// Info describes the deployment, shown read-only on the settings page.
type Info struct {
	DataDir string `json:"dataDir"`
	AppsDir string `json:"appsDir"`
	// Ports is the allowed project port range ("" = any).
	Ports string `json:"ports"`
}

// settingsView is the settings page payload: editable settings plus read-only info.
type settingsView struct {
	settings.View
	Info Info `json:"info"`
	// Version is the running build: "1.42" from CI, a short commit locally, or "dev".
	Version string `json:"version"`
}

type server struct {
	store    *store.Store
	sup      *supervisor.Supervisor
	projects *projects.Service
	settings *settings.Manager
	// mcp is the OAuth provider of the MCP endpoint, nil when it is disabled.
	mcp  *oauth.Provider
	info Info
}

// mcpView is the MCP section of the settings page.
type mcpView struct {
	Enabled bool `json:"enabled"`
	// URL is the public MCP endpoint ("" when disabled).
	URL         string             `json:"url"`
	Connections []oauth.Connection `json:"connections"`
}

// New returns the admin HTTP handler. The admin UI and API use basic auth against the
// settings. When provider is not nil, the same handler also serves the MCP endpoint
// for Claude and its OAuth routes, which use OAuth instead of basic auth.
func New(st *store.Store, sup *supervisor.Supervisor, set *settings.Manager, provider *oauth.Provider, info Info) http.Handler {
	s := &server{store: st, sup: sup, projects: projects.New(st, sup), settings: set, mcp: provider, info: info}
	mux := http.NewServeMux()
	mux.Handle("GET /", uiHandler())
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("GET /api/projects/{name}", s.getProject)
	mux.HandleFunc("PUT /api/projects/{name}", s.updateProject)
	mux.HandleFunc("DELETE /api/projects/{name}", s.deleteProject)
	mux.HandleFunc("POST /api/projects/{name}/deploy", s.deployProject)
	mux.HandleFunc("POST /api/projects/{name}/start", s.startProject)
	mux.HandleFunc("POST /api/projects/{name}/stop", s.stopProject)
	mux.HandleFunc("POST /api/projects/{name}/restart", s.restartProject)
	mux.HandleFunc("GET /api/projects/{name}/logs", s.projectLogs)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.updateSettings)
	mux.HandleFunc("POST /api/settings/password", s.changePassword)
	mux.HandleFunc("GET /api/mcp", s.getMCP)
	mux.HandleFunc("DELETE /api/mcp/connections/{id}", s.revokeMCPConnection)
	// Any other /api path (or a wrong method on an action) is an API error, not a UI page.
	// Registered per method: a method-less "/api/" would conflict with "GET /".
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		mux.HandleFunc(method+" /api/", unknownAPIRoute)
	}
	if provider == nil {
		return s.basicAuth(mux)
	}
	root := http.NewServeMux()
	mcp.Register(root, provider, mcp.New(st, sup, info.Ports))
	root.Handle("/", s.basicAuth(mux))
	return root
}

func unknownAPIRoute(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, errors.New("unknown API route"))
}

// uiHandler serves the built admin app, or a placeholder explaining how to build it.
func uiHandler() http.Handler {
	dist, err := fs.Sub(uiFiles, "ui/dist")
	if err == nil {
		if _, statErr := fs.Stat(dist, "index.html"); statErr == nil {
			return http.FileServerFS(dist)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, uiFiles, "ui/placeholder.html")
	})
}

func (s *server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || !s.settings.Authenticate(user, password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="self-hoster"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) listProjects(w http.ResponseWriter, _ *http.Request) {
	projects := s.store.List()
	views := make([]projectView, 0, len(projects))
	for _, p := range projects {
		views = append(views, s.view(p))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *server) getProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.load(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.view(p))
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if !decode(w, r, &p) {
		return
	}
	p, err := s.projects.Create(p)
	if err != nil {
		writeError(w, statusFor(err, http.StatusBadRequest), err)
		return
	}
	writeJSON(w, http.StatusCreated, s.view(p))
}

// updateProject replaces a project's definition and restarts it if it is running,
// so new environment variables take effect immediately.
func (s *server) updateProject(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if !decode(w, r, &p) {
		return
	}
	p.Name = r.PathValue("name")
	p, err := s.projects.Save(p)
	if err != nil {
		writeError(w, statusFor(err, http.StatusBadRequest), err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(p))
}

func (s *server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.projects.Delete(r.PathValue("name")); err != nil {
		writeError(w, statusFor(err, http.StatusInternalServerError), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) deployProject(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, s.projects.Deploy, http.StatusAccepted)
}

func (s *server) startProject(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, s.projects.Start, http.StatusOK)
}

func (s *server) stopProject(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, s.projects.Stop, http.StatusOK)
}

func (s *server) restartProject(w http.ResponseWriter, r *http.Request) {
	s.action(w, r, s.projects.Restart, http.StatusOK)
}

// action runs a lifecycle operation on the project named in the path.
func (s *server) action(w http.ResponseWriter, r *http.Request, op func(string) (store.Project, error), okStatus int) {
	p, err := op(r.PathValue("name"))
	if err != nil {
		writeError(w, statusFor(err, http.StatusInternalServerError), err)
		return
	}
	writeJSON(w, okStatus, s.view(p))
}

func (s *server) projectLogs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.load(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.sup.Logs(p.Name))
}

func (s *server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, settingsView{View: s.settings.View(), Info: s.info, Version: buildinfo.Version()})
}

// updateSettings saves the editable settings. Changing the username makes the browser
// ask for credentials again on the next request.
func (s *server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var u settings.Update
	if !decode(w, r, &u) {
		return
	}
	if err := s.settings.Apply(u); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsView{View: s.settings.View(), Info: s.info, Version: buildinfo.Version()})
}

type passwordChange struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body passwordChange
	if !decode(w, r, &body) {
		return
	}
	if err := s.settings.ChangePassword(body.Current, body.New); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, settings.ErrWrongPassword) {
			status = http.StatusForbidden
		}
		writeError(w, status, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getMCP(w http.ResponseWriter, _ *http.Request) {
	view := mcpView{Connections: []oauth.Connection{}}
	if s.mcp != nil {
		view.Enabled = true
		view.Connections = s.mcp.Connections()
		if base := s.mcp.PublicURL(); base != "" {
			view.URL = base + oauth.ResourcePath
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// revokeMCPConnection disconnects one MCP client at once (its tokens stop working).
func (s *server) revokeMCPConnection(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		writeError(w, http.StatusNotFound, errors.New("the MCP endpoint is disabled"))
		return
	}
	if err := s.mcp.Revoke(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) load(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	p, err := s.store.Get(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return p, false
	}
	return p, true
}

func (s *server) view(p store.Project) projectView {
	return projectView{Project: p, DataDir: s.sup.AppDir(p.Name), Status: s.sup.Status(p.Name)}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// statusFor maps an operation error to an HTTP status; fallback covers the rest
// (validation errors on edits, I/O errors on actions).
func statusFor(err error, fallback int) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, projects.ErrExists), errors.Is(err, supervisor.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, supervisor.ErrNotDeployed):
		return http.StatusBadRequest
	case errors.Is(err, projects.ErrRestartFailed):
		return http.StatusInternalServerError
	default:
		return fallback
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}
