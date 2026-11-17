// Package store persists the registered projects in a single JSON file.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"selfhoster/internal/fileutil"
)

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	// ErrNotFound is returned when a project name is not registered.
	ErrNotFound = errors.New("project not found")
)

// Project types.
const (
	// TypeService runs StartCmd as a long-lived process (APIs, SSR apps).
	TypeService = "service"
	// TypeStatic serves the built StaticDir directly (SPAs, static sites). No process is spawned.
	TypeStatic = "static"
)

// Project is the persisted definition of a hosted project.
type Project struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Repo     string            `json:"repo"`
	Branch   string            `json:"branch,omitempty"`
	BuildCmd string            `json:"buildCmd,omitempty"`
	StartCmd string            `json:"startCmd,omitempty"`
	Port     int               `json:"port"`
	Env      map[string]string `json:"env,omitempty"`
	// StaticDir is the folder served by a static project, relative to the repo root (e.g. "dist").
	StaticDir string `json:"staticDir,omitempty"`
	// SPA serves index.html for unknown paths, so client-side routing works on reload.
	SPA bool `json:"spa,omitempty"`
	// URL is where the project is reachable publicly (e.g. through nginx). Optional:
	// the UI falls back to http://<link host>:<port>.
	URL string `json:"url,omitempty"`
	// AutoDeploy redeploys the project when a new commit lands on its branch (polled).
	AutoDeploy bool `json:"autoDeploy,omitempty"`
	// Enabled records whether the project should be running. It is restored when the container boots.
	Enabled bool `json:"enabled"`
}

// Options constrains which ports projects may use.
type Options struct {
	// ReservedPorts are self-hoster's own ports (admin UI, MCP), never assignable to a project.
	ReservedPorts []int
	// MinPort and MaxPort bound the port range published by the container. Zero disables the check.
	MinPort int
	MaxPort int
}

// Store is a concurrency-safe project registry backed by a JSON file.
type Store struct {
	mu       sync.RWMutex
	path     string
	opts     Options
	projects map[string]Project
}

type fileFormat struct {
	Projects []Project `json:"projects"`
}

// Open loads the registry from path, starting empty if the file does not exist yet.
func Open(path string, opts Options) (*Store, error) {
	s := &Store{path: path, opts: opts, projects: map[string]Project{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, p := range f.Projects {
		s.projects[p.Name] = p
	}
	return s, nil
}

// List returns all projects sorted by name.
func (s *Store) List() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the project with the given name.
func (s *Store) Get(name string) (Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[name]
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}

// Put validates and creates or replaces a project, then persists the registry.
func (s *Store) Put(p Project) error {
	p.Repo = strings.TrimSpace(p.Repo)
	p.Branch = strings.TrimSpace(p.Branch)
	p.StaticDir = strings.TrimSpace(p.StaticDir)
	p.URL = strings.TrimSpace(p.URL)
	if p.Type == "" {
		p.Type = TypeService
	}
	if err := s.validate(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.projects {
		if other.Name != p.Name && other.Port == p.Port {
			return fmt.Errorf("port %d is already used by %q", p.Port, other.Name)
		}
	}
	previous, existed := s.projects[p.Name]
	s.projects[p.Name] = p
	if err := s.saveLocked(); err != nil {
		if existed {
			s.projects[p.Name] = previous
		} else {
			delete(s.projects, p.Name)
		}
		return err
	}
	return nil
}

// Delete removes a project and persists the registry.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, ok := s.projects[name]
	if !ok {
		return ErrNotFound
	}
	delete(s.projects, name)
	if err := s.saveLocked(); err != nil {
		s.projects[name] = previous
		return err
	}
	return nil
}

func (s *Store) validate(p Project) error {
	if !namePattern.MatchString(p.Name) {
		return errors.New("name must be lowercase letters, digits and dashes (max 63 chars)")
	}
	if p.Repo == "" {
		return errors.New("repo is required")
	}
	switch p.Type {
	case TypeService:
		if strings.TrimSpace(p.StartCmd) == "" {
			return errors.New("start command is required")
		}
	case TypeStatic:
		if p.StaticDir == "" {
			return errors.New("static folder is required (e.g. dist)")
		}
		if !filepath.IsLocal(p.StaticDir) {
			return errors.New("static folder must be a relative path inside the repository")
		}
	default:
		return fmt.Errorf("type must be %q or %q", TypeService, TypeStatic)
	}
	if p.Port < 1 || p.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if slices.Contains(s.opts.ReservedPorts, p.Port) {
		return fmt.Errorf("port %d is reserved for self-hoster (admin UI or MCP)", p.Port)
	}
	if s.opts.MinPort > 0 && (p.Port < s.opts.MinPort || p.Port > s.opts.MaxPort) {
		return fmt.Errorf("port must be in the published range %d-%d", s.opts.MinPort, s.opts.MaxPort)
	}
	if p.URL != "" {
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("public URL must be an http(s) URL, e.g. https://app.example.com")
		}
	}
	for key := range p.Env {
		if !envKeyPattern.MatchString(key) {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
		if key == "PORT" {
			return errors.New("PORT is set from the project port, remove it from the environment")
		}
	}
	return nil
}

// saveLocked writes the registry atomically. The file holds secrets, so it is private to the owner.
func (s *Store) saveLocked() error {
	f := fileFormat{Projects: make([]Project, 0, len(s.projects))}
	for _, p := range s.projects {
		f.Projects = append(f.Projects, p)
	}
	sort.Slice(f.Projects, func(i, j int) bool { return f.Projects[i].Name < f.Projects[j].Name })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(s.path, data)
}
