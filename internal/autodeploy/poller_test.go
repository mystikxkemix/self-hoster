package autodeploy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
	"selfhoster/internal/testutil"
)

type fixture struct {
	t      *testing.T
	repo   string
	store  *store.Store
	sup    *supervisor.Supervisor
	poller *Poller
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "config.json"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(dataDir, filepath.Join(dataDir, "apps"))
	t.Cleanup(sup.Shutdown)
	return &fixture{
		t:      t,
		repo:   testutil.NewRepo(t, map[string]string{"version.txt": "v1"}),
		store:  st,
		sup:    sup,
		poller: New(st, sup, func() time.Duration { return time.Hour }),
	}
}

// register stores a project (built with `buildCmd`) and deploys it once, like a user would.
func (f *fixture) register(name, buildCmd string, autoDeploy, enabled bool) store.Project {
	f.t.Helper()
	p := store.Project{
		Name: name, Type: store.TypeService, Repo: f.repo, BuildCmd: buildCmd,
		StartCmd: "exec sleep 60", Port: 4000 + len(f.store.List()), AutoDeploy: autoDeploy, Enabled: enabled,
	}
	if err := f.store.Put(p); err != nil {
		f.t.Fatal(err)
	}
	if err := f.sup.Deploy(p); err != nil {
		f.t.Fatal(err)
	}
	f.waitDeployed(name)
	return p
}

// push commits a new version to the source repository.
func (f *fixture) push(version string) string {
	f.t.Helper()
	testutil.WriteFiles(f.t, f.repo, map[string]string{"version.txt": version})
	return testutil.Commit(f.t, f.repo, version)
}

// checkAndWait runs one poll and waits for any deploy it started.
func (f *fixture) checkAndWait(names ...string) {
	f.t.Helper()
	f.poller.CheckOnce(context.Background())
	for _, name := range names {
		f.waitDeployed(name)
	}
}

func (f *fixture) waitDeployed(name string) {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for f.sup.Status(name).Deploying {
		if time.Now().After(deadline) {
			f.t.Fatal("timed out waiting for deploy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fixture) logs(name string) string {
	return strings.Join(f.sup.Logs(name), "\n")
}

func TestDeploysNewCommit(t *testing.T) {
	f := newFixture(t)
	f.register("app", "", true, true)
	before := f.sup.Status("app")

	want := f.push("v2")
	f.checkAndWait("app")

	if got := f.sup.DeployedCommit("app"); got != want {
		t.Fatalf("deployed %s, want %s", got, want)
	}
	if after := f.sup.Status("app"); after.PID == before.PID || after.State != supervisor.StateRunning {
		t.Fatalf("not redeployed: before %+v, after %+v", before, after)
	}
	if !strings.Contains(f.logs("app"), "auto-deploying") {
		t.Fatal("auto-deploy not logged")
	}
}

func TestUpToDateProjectIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.register("app", "", true, true)
	before := f.sup.Status("app")
	f.checkAndWait("app")
	if after := f.sup.Status("app"); after.PID != before.PID {
		t.Fatalf("restarted without a new commit: before %+v, after %+v", before, after)
	}
}

func TestSkipsProjectsNotOptedInOrStopped(t *testing.T) {
	f := newFixture(t)
	f.register("manual", "", false, true)  // auto-deploy off
	f.register("stopped", "", true, false) // stopped by the user
	deployed := map[string]string{
		"manual":  f.sup.DeployedCommit("manual"),
		"stopped": f.sup.DeployedCommit("stopped"),
	}

	f.push("v2")
	f.checkAndWait("manual", "stopped")

	for name, commit := range deployed {
		if got := f.sup.DeployedCommit(name); got != commit {
			t.Errorf("%s was auto-deployed but should not be", name)
		}
	}
}

func TestFailedBuildIsNotRetriedUntilNextCommit(t *testing.T) {
	f := newFixture(t)
	// The build fails whenever version.txt says "broken".
	f.register("app", `! grep -q broken version.txt`, true, true)
	running := f.sup.Status("app")

	f.push("broken")
	f.checkAndWait("app")
	if st := f.sup.Status("app"); st.PID != running.PID || !strings.Contains(st.LastError, "build") {
		t.Fatalf("status = %+v, want the previous process kept with a build error", st)
	}
	attempts := strings.Count(f.logs("app"), "auto-deploying")

	f.checkAndWait("app")
	f.checkAndWait("app")
	if got := strings.Count(f.logs("app"), "auto-deploying"); got != attempts {
		t.Fatalf("failed commit retried: %d auto-deploys, want %d", got, attempts)
	}

	fixed := f.push("v3")
	f.checkAndWait("app")
	if got := f.sup.DeployedCommit("app"); got != fixed {
		t.Fatalf("deployed %s, want the fixed commit %s", got, fixed)
	}
	if st := f.sup.Status("app"); st.State != supervisor.StateRunning {
		t.Fatalf("status = %+v", st)
	}
}

func TestBrokenRemoteIsLoggedOnce(t *testing.T) {
	f := newFixture(t)
	p := f.register("app", "", true, true)
	p.Repo = filepath.Join(t.TempDir(), "gone")
	if err := f.store.Put(p); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		f.checkAndWait("app")
	}
	if got := strings.Count(f.logs("app"), "auto-deploy check failed"); got != 1 {
		t.Fatalf("check failure logged %d times, want 1", got)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(f.store, f.sup, func() time.Duration { return 10 * time.Millisecond }).Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
