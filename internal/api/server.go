// Package api exposes the admin UI and the JSON API used to manage projects.
package api

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"

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

type server struct {
	store    *store.Store
	sup      *supervisor.Supervisor
	user     string
	password string
}

// New returns the admin HTTP handler, protected by basic auth.
func New(st *store.Store, sup *supervisor.Supervisor, user, password string) http.Handler {
	s := &server{store: st, sup: sup, user: user, password: password}
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
	// Any other /api path (or a wrong method on an action) is an API error, not a UI page.
	// Registered per method: a method-less "/api/" would conflict with "GET /".
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		mux.HandleFunc(method+" /api/", unknownAPIRoute)
	}
	return s.basicAuth(mux)
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
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.user)) == 1
		passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.password)) == 1
		if !ok || !userOK || !passwordOK {
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
	if _, err := s.store.Get(p.Name); err == nil {
		writeError(w, http.StatusConflict, errors.New("a project with this name already exists"))
		return
	}
	p.Enabled = false
	if err := s.store.Put(p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.sup.Configure(p)
	writeJSON(w, http.StatusCreated, s.view(p))
}

// updateProject replaces a project's definition and restarts it if it is running,
// so new environment variables take effect immediately.
func (s *server) updateProject(w http.ResponseWriter, r *http.Request) {
	current, ok := s.load(w, r)
	if !ok {
		return
	}
	var p store.Project
	if !decode(w, r, &p) {
		return
	}
	p.Name = current.Name
	p.Enabled = current.Enabled
	if err := s.store.Put(p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.sup.Configure(p)
	if s.sup.Status(p.Name).State != supervisor.StateStopped {
		if err := s.sup.Restart(p); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.view(p))
}

func (s *server) deleteProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.load(w, r)
	if !ok {
		return
	}
	if err := s.sup.Remove(p.Name); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if err := s.store.Delete(p.Name); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) deployProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.setEnabled(w, r, true)
	if !ok {
		return
	}
	if err := s.sup.Deploy(p); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.view(p))
}

func (s *server) startProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.load(w, r)
	if !ok {
		return
	}
	if err := s.sup.Start(p); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if p, ok = s.setEnabled(w, r, true); ok {
		writeJSON(w, http.StatusOK, s.view(p))
	}
}

func (s *server) stopProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.setEnabled(w, r, false)
	if !ok {
		return
	}
	s.sup.Stop(p.Name)
	writeJSON(w, http.StatusOK, s.view(p))
}

func (s *server) restartProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.setEnabled(w, r, true)
	if !ok {
		return
	}
	if err := s.sup.Restart(p); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(p))
}

func (s *server) projectLogs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.load(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.sup.Logs(p.Name))
}

// setEnabled persists whether the project should run, so the choice survives container restarts.
func (s *server) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) (store.Project, bool) {
	p, ok := s.load(w, r)
	if !ok {
		return p, false
	}
	if p.Enabled == enabled {
		return p, true
	}
	p.Enabled = enabled
	if err := s.store.Put(p); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return p, false
	}
	return p, true
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

func statusFor(err error) int {
	switch {
	case errors.Is(err, supervisor.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, supervisor.ErrNotDeployed):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
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
