// Package supervisor clones, builds and runs projects as child processes,
// restarting them when they crash.
package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"selfhoster/internal/store"
)

var (
	// ErrBusy is returned when an operation conflicts with a deploy in progress.
	ErrBusy = errors.New("a deploy is already in progress")
	// ErrNotDeployed is returned when starting a project that was never cloned.
	ErrNotDeployed = errors.New("project has not been deployed yet")
)

// State is the lifecycle state of a project's process.
type State string

const (
	StateStopped State = "stopped"
	StateRunning State = "running"
	// StateCrashed means the process exited unexpectedly and a restart is scheduled.
	StateCrashed State = "crashed"
)

// Status is the runtime view of a project.
type Status struct {
	State     State      `json:"state"`
	PID       int        `json:"pid,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	Restarts  int        `json:"restarts"`
	Deploying bool       `json:"deploying"`
	Commit    string     `json:"commit,omitempty"`
	LastError string     `json:"lastError,omitempty"`
}

// Supervisor owns one process per registered project.
type Supervisor struct {
	dataDir  string
	mu       sync.Mutex
	procs    map[string]*process
	shutdown atomic.Bool
}

// New creates a supervisor that keeps project checkouts under dataDir/projects.
func New(dataDir string) *Supervisor {
	return &Supervisor{dataDir: dataDir, procs: map[string]*process{}}
}

// Configure registers or updates a project definition without starting it.
// Changes apply on the next start.
func (s *Supervisor) Configure(spec store.Project) {
	s.proc(spec).setSpec(spec)
}

// IsDeployed reports whether the project's repository has been cloned.
func (s *Supervisor) IsDeployed(name string) bool {
	_, err := os.Stat(filepath.Join(s.projectDir(name), ".git"))
	return err == nil
}

// Deploy pulls, builds and (re)starts the project in the background.
func (s *Supervisor) Deploy(spec store.Project) error {
	p := s.proc(spec)
	p.setSpec(spec)
	p.mu.Lock()
	if p.status.Deploying {
		p.mu.Unlock()
		return ErrBusy
	}
	p.status.Deploying = true
	p.mu.Unlock()
	go p.deploy(context.Background())
	return nil
}

// Start launches the project's start command.
func (s *Supervisor) Start(spec store.Project) error {
	p := s.proc(spec)
	p.setSpec(spec)
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.mu.Lock()
	running := p.isRunningLocked()
	p.mu.Unlock()
	if running {
		return nil
	}
	p.resetBackoff()
	return p.startLocked()
}

// Restart stops then starts the project, applying the latest definition.
func (s *Supervisor) Restart(spec store.Project) error {
	p := s.proc(spec)
	p.setSpec(spec)
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.stopLocked()
	p.resetBackoff()
	return p.startLocked()
}

// Stop terminates the project and cancels pending crash restarts.
func (s *Supervisor) Stop(name string) {
	p := s.lookup(name)
	if p == nil {
		return
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.stopLocked()
}

// Remove stops the project and deletes its checkout.
func (s *Supervisor) Remove(name string) error {
	p := s.lookup(name)
	if p != nil {
		if p.Status().Deploying {
			return ErrBusy
		}
		p.opMu.Lock()
		p.stopLocked()
		p.opMu.Unlock()
		s.mu.Lock()
		delete(s.procs, name)
		s.mu.Unlock()
	}
	return os.RemoveAll(s.projectDir(name))
}

// Status returns the runtime status of a project.
func (s *Supervisor) Status(name string) Status {
	p := s.lookup(name)
	if p == nil {
		return Status{State: StateStopped}
	}
	return p.Status()
}

// Logs returns the buffered build and runtime output of a project.
func (s *Supervisor) Logs(name string) []string {
	p := s.lookup(name)
	if p == nil {
		return []string{}
	}
	return p.logs.Lines()
}

// Shutdown stops every project in parallel. It does not change their Enabled flag,
// so they come back on the next boot.
func (s *Supervisor) Shutdown() {
	s.shutdown.Store(true)
	s.mu.Lock()
	procs := make([]*process, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Add(1)
		go func(p *process) {
			defer wg.Done()
			p.opMu.Lock()
			defer p.opMu.Unlock()
			p.stopLocked()
		}(p)
	}
	wg.Wait()
}

func (s *Supervisor) projectDir(name string) string {
	return filepath.Join(s.dataDir, "projects", name)
}

func (s *Supervisor) lookup(name string) *process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[name]
}

func (s *Supervisor) proc(spec store.Project) *process {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[spec.Name]
	if !ok {
		p = newProcess(spec, s.projectDir(spec.Name), &s.shutdown)
		s.procs[spec.Name] = p
	}
	return p
}
