package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"selfhoster/internal/store"
)

const (
	stopTimeout   = 10 * time.Second
	deployTimeout = 30 * time.Minute
	// stableRunTime is how long a process must run before its crash backoff resets.
	stableRunTime = time.Minute
	minBackoff    = time.Second
	maxBackoff    = time.Minute
	// internalEnvPrefix marks self-hoster's own settings, which are never passed to projects.
	internalEnvPrefix = "SELFHOSTER_"
)

// process manages the lifecycle of a single project.
type process struct {
	dir      string
	logs     *logBuffer
	shutdown *atomic.Bool

	// opMu serializes start/stop/swap operations. Builds run outside it so the
	// current version keeps serving while the next one builds.
	opMu sync.Mutex

	// mu guards the fields below.
	mu           sync.Mutex
	spec         store.Project
	status       Status
	cmd          *exec.Cmd
	site         *http.Server // set instead of cmd for static projects
	done         chan struct{}
	wantRunning  bool
	backoff      time.Duration
	restartTimer *time.Timer
}

func newProcess(spec store.Project, dir string, shutdown *atomic.Bool) *process {
	return &process{
		dir:      dir,
		logs:     newLogBuffer(spec.Name, os.Stdout),
		shutdown: shutdown,
		spec:     spec,
		status:   Status{State: StateStopped},
	}
}

func (p *process) setSpec(spec store.Project) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spec = spec
}

func (p *process) getSpec() store.Project {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spec
}

// Status returns a copy of the current status.
func (p *process) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// isRunningLocked reports whether a process or static server is active. The caller holds mu.
func (p *process) isRunningLocked() bool {
	return p.cmd != nil || p.site != nil
}

func (p *process) resetBackoff() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backoff = 0
}

func (p *process) setError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.LastError = err.Error()
}

// deploy syncs the repository, builds it, then swaps the running process.
// The previous version keeps running if the sync or the build fails.
func (p *process) deploy(ctx context.Context) {
	defer func() {
		p.mu.Lock()
		p.status.Deploying = false
		p.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()
	spec := p.getSpec()
	p.logs.system("deploy started")
	if err := p.syncRepo(ctx, spec); err != nil {
		p.failDeploy(fmt.Errorf("git: %w", err))
		return
	}
	if strings.TrimSpace(spec.BuildCmd) != "" {
		p.logs.system("building: %s", spec.BuildCmd)
		if err := p.run(ctx, p.dir, environment(spec), "sh", "-c", spec.BuildCmd); err != nil {
			p.failDeploy(fmt.Errorf("build: %w", err))
			return
		}
	}
	if p.shutdown.Load() {
		return
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.stopLocked()
	p.resetBackoff()
	if err := p.startLocked(); err != nil {
		p.failDeploy(fmt.Errorf("start: %w", err))
		return
	}
	p.logs.system("deploy finished")
}

func (p *process) failDeploy(err error) {
	p.setError(err)
	p.logs.system("deploy failed: %v", err)
}

// syncRepo clones the repository on first deploy, then hard-resets it to the remote branch.
// Ignored files (node_modules, build output) are kept to speed up rebuilds.
func (p *process) syncRepo(ctx context.Context, spec store.Project) error {
	if _, err := os.Stat(filepath.Join(p.dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.RemoveAll(p.dir); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p.dir), 0o755); err != nil {
			return err
		}
		// The repo URL may embed a token, so it is never echoed to the logs.
		p.logs.system("cloning repository")
		args := []string{"clone", "--depth", "1"}
		if spec.Branch != "" {
			args = append(args, "--branch", spec.Branch)
		}
		args = append(args, spec.Repo, p.dir)
		return p.run(ctx, "", gitEnvironment(), "git", args...)
	}
	ref := spec.Branch
	if ref == "" {
		ref = "HEAD"
	}
	p.logs.system("fetching %s", ref)
	steps := [][]string{
		{"remote", "set-url", "origin", spec.Repo},
		{"fetch", "--depth", "1", "origin", ref},
		{"reset", "--hard", "FETCH_HEAD"},
		{"clean", "-fd"},
	}
	for _, args := range steps {
		if err := p.run(ctx, p.dir, gitEnvironment(), "git", args...); err != nil {
			return err
		}
	}
	return nil
}

// run executes a command to completion, streaming its output to the project logs.
func (p *process) run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = p.logs
	cmd.Stderr = p.logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// startLocked launches the start command, or the static server for static projects.
// The caller holds opMu.
func (p *process) startLocked() error {
	if _, err := os.Stat(p.dir); err != nil {
		return ErrNotDeployed
	}
	spec := p.getSpec()
	if spec.Type == store.TypeStatic {
		return p.startStaticLocked(spec)
	}
	cmd := exec.Command("sh", "-c", spec.StartCmd)
	cmd.Dir = p.dir
	cmd.Env = environment(spec)
	cmd.Stdout = p.logs
	cmd.Stderr = p.logs
	// Own process group, so stopping also stops children (npm -> node, etc.).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		p.setError(err)
		return err
	}
	started := time.Now()
	done := make(chan struct{})
	commit := p.currentCommit()
	p.mu.Lock()
	p.cmd = cmd
	p.done = done
	p.wantRunning = true
	p.status.State = StateRunning
	p.status.PID = cmd.Process.Pid
	p.status.StartedAt = &started
	p.status.Commit = commit
	p.status.LastError = ""
	p.mu.Unlock()
	p.logs.system("started (pid %d, port %d)", cmd.Process.Pid, spec.Port)
	go p.wait(cmd, done, started)
	return nil
}

// wait reaps the process and schedules a restart when the exit was unexpected.
func (p *process) wait(cmd *exec.Cmd, done chan struct{}, started time.Time) {
	err := cmd.Wait()
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		close(done)
	}()
	p.cmd = nil
	p.status.PID = 0
	p.status.StartedAt = nil
	if !p.wantRunning || p.shutdown.Load() {
		p.status.State = StateStopped
		p.logs.system("stopped")
		return
	}
	if time.Since(started) > stableRunTime {
		p.backoff = 0
	}
	p.backoff = nextBackoff(p.backoff)
	p.status.State = StateCrashed
	p.status.Restarts++
	p.status.LastError = fmt.Sprintf("exited unexpectedly: %v", exitReason(err))
	p.logs.system("%s, restarting in %s", p.status.LastError, p.backoff)
	p.restartTimer = time.AfterFunc(p.backoff, p.restartAfterCrash)
}

func (p *process) restartAfterCrash() {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	p.mu.Lock()
	should := p.wantRunning && !p.isRunningLocked() && !p.shutdown.Load()
	p.mu.Unlock()
	if !should {
		return
	}
	if err := p.startLocked(); err != nil {
		p.logs.system("restart failed: %v", err)
	}
}

// stopLocked sends SIGTERM to the process group, then SIGKILL after stopTimeout.
// The caller holds opMu.
func (p *process) stopLocked() {
	p.mu.Lock()
	p.wantRunning = false
	if p.restartTimer != nil {
		p.restartTimer.Stop()
		p.restartTimer = nil
	}
	cmd, site, done := p.cmd, p.site, p.done
	if site != nil {
		p.mu.Unlock()
		p.stopStatic(site, done)
		return
	}
	if cmd == nil {
		p.status.State = StateStopped
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(stopTimeout):
		p.logs.system("did not stop within %s, killing", stopTimeout)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
}

func (p *process) currentCommit() string {
	out, err := exec.Command("git", "-C", p.dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// environment builds a project's env: the container env (minus self-hoster settings),
// then the project's variables, then PORT. exec keeps the last value of duplicate keys.
func environment(spec store.Project) []string {
	env := baseEnvironment()
	for key, value := range spec.Env {
		env = append(env, key+"="+value)
	}
	return append(env, "PORT="+strconv.Itoa(spec.Port))
}

// gitEnvironment makes git fail fast instead of waiting for credentials on a terminal.
func gitEnvironment() []string {
	return append(baseEnvironment(), "GIT_TERMINAL_PROMPT=0")
}

// baseEnvironment is the container env without self-hoster's own settings (admin password, etc.).
func baseEnvironment() []string {
	all := os.Environ()
	env := make([]string, 0, len(all))
	for _, kv := range all {
		if !strings.HasPrefix(kv, internalEnvPrefix) {
			env = append(env, kv)
		}
	}
	return env
}

func nextBackoff(current time.Duration) time.Duration {
	if current < minBackoff {
		return minBackoff
	}
	return min(current*2, maxBackoff)
}

func exitReason(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}
