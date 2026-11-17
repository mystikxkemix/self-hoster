// Package projects holds the project operations shared by the admin API and the MCP
// tools: they keep the registry (store) and the running processes (supervisor) in
// step, so both front-ends behave the same.
package projects

import (
	"errors"
	"fmt"

	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
)

var (
	// ErrExists is returned when creating a project whose name is taken.
	ErrExists = errors.New("a project with this name already exists")
	// ErrRestartFailed wraps the error of a restart after a successful save: the new
	// settings are stored but the running process could not be restarted with them.
	ErrRestartFailed = errors.New("saved, but the restart failed")
)

// Service performs project operations. It is safe for concurrent use.
type Service struct {
	store *store.Store
	sup   *supervisor.Supervisor
}

// New returns a Service over the given registry and supervisor.
func New(st *store.Store, sup *supervisor.Supervisor) *Service {
	return &Service{store: st, sup: sup}
}

// Create registers a new project, stopped: it runs once deployed.
func (s *Service) Create(p store.Project) (store.Project, error) {
	if _, err := s.store.Get(p.Name); err == nil {
		return p, ErrExists
	}
	p.Enabled = false
	if err := s.store.Put(p); err != nil {
		return p, err
	}
	return s.configure(p.Name)
}

// Save replaces an existing project's definition and restarts it if it is running, so
// changes (e.g. env vars) apply at once. Whether the project runs (Enabled) is kept:
// only the actions below change it.
func (s *Service) Save(p store.Project) (store.Project, error) {
	current, err := s.store.Get(p.Name)
	if err != nil {
		return p, err
	}
	p.Enabled = current.Enabled
	if err := s.store.Put(p); err != nil {
		return p, err
	}
	if p, err = s.configure(p.Name); err != nil {
		return p, err
	}
	if s.sup.Status(p.Name).State != supervisor.StateStopped {
		if err := s.sup.Restart(p); err != nil {
			return p, fmt.Errorf("%w: %w", ErrRestartFailed, err)
		}
	}
	return p, nil
}

// Delete stops a project and removes it from the registry. Its DATA_DIR is kept.
func (s *Service) Delete(name string) error {
	if _, err := s.store.Get(name); err != nil {
		return err
	}
	if err := s.sup.Remove(name); err != nil {
		return err
	}
	return s.store.Delete(name)
}

// Deploy enables the project, then pulls, builds and (re)starts it in the background.
func (s *Service) Deploy(name string) (store.Project, error) {
	p, err := s.setEnabled(name, true)
	if err != nil {
		return p, err
	}
	return p, s.sup.Deploy(p)
}

// Start starts an already deployed project and, once started, enables it.
func (s *Service) Start(name string) (store.Project, error) {
	p, err := s.store.Get(name)
	if err != nil {
		return p, err
	}
	if err := s.sup.Start(p); err != nil {
		return p, err
	}
	return s.setEnabled(name, true)
}

// Stop disables and stops the project.
func (s *Service) Stop(name string) (store.Project, error) {
	p, err := s.setEnabled(name, false)
	if err != nil {
		return p, err
	}
	s.sup.Stop(p.Name)
	return p, nil
}

// Restart enables the project and restarts its process without rebuilding.
func (s *Service) Restart(name string) (store.Project, error) {
	p, err := s.setEnabled(name, true)
	if err != nil {
		return p, err
	}
	return p, s.sup.Restart(p)
}

// setEnabled persists whether the project should run, so the choice survives
// container restarts.
func (s *Service) setEnabled(name string, enabled bool) (store.Project, error) {
	p, err := s.store.Get(name)
	if err != nil || p.Enabled == enabled {
		return p, err
	}
	p.Enabled = enabled
	return p, s.store.Put(p)
}

// configure hands the stored (normalized) definition to the supervisor.
func (s *Service) configure(name string) (store.Project, error) {
	p, err := s.store.Get(name)
	if err != nil {
		return p, err
	}
	s.sup.Configure(p)
	return p, nil
}
