// Package autodeploy redeploys projects when new commits land on their branch.
// It polls the git remotes, so GitHub never needs to reach the box (no webhook, no runner).
package autodeploy

import (
	"context"
	"log"
	"time"

	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
)

// checkTimeout bounds a single git ls-remote call.
const checkTimeout = 30 * time.Second

// Poller periodically compares each auto-deploy project's remote branch with its checkout.
type Poller struct {
	store    *store.Store
	sup      *supervisor.Supervisor
	interval time.Duration

	// lastTried is the remote commit last deployed per project. A failed build is not
	// retried on every tick: the poller waits for the next commit.
	lastTried map[string]string
	// lastError is the last reported check error per project, so a broken remote is
	// logged once instead of on every tick.
	lastError map[string]string
}

// New creates a poller that checks every interval.
func New(st *store.Store, sup *supervisor.Supervisor, interval time.Duration) *Poller {
	return &Poller{store: st, sup: sup, interval: interval, lastTried: map[string]string{}, lastError: map[string]string{}}
}

// Run checks immediately, then on every interval until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		p.CheckOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// CheckOnce checks every project once and starts deploys for those behind their remote.
// Only enabled projects are checked: a stopped project stays stopped.
func (p *Poller) CheckOnce(ctx context.Context) {
	for _, project := range p.store.List() {
		if ctx.Err() != nil {
			return
		}
		if !project.AutoDeploy || !project.Enabled || p.sup.Status(project.Name).Deploying {
			continue
		}
		p.check(ctx, project)
	}
}

func (p *Poller) check(ctx context.Context, project store.Project) {
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	remote, err := p.sup.RemoteCommit(checkCtx, project)
	if err != nil {
		if ctx.Err() == nil && p.lastError[project.Name] != err.Error() {
			p.lastError[project.Name] = err.Error()
			p.sup.LogSystem(project, "auto-deploy check failed: %v", err)
		}
		return
	}
	delete(p.lastError, project.Name)

	if remote == p.sup.DeployedCommit(project.Name) || remote == p.lastTried[project.Name] {
		return
	}
	p.lastTried[project.Name] = remote
	p.sup.LogSystem(project, "new commit %s, auto-deploying", shortSHA(remote))
	if err := p.sup.Deploy(project); err != nil {
		log.Printf("auto-deploy %s: %v", project.Name, err)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
