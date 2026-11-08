package supervisor

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"selfhoster/internal/store"
	"selfhoster/internal/testutil"
)

// newSupervisor returns a supervisor whose projects are all stopped at the end of the test.
func newSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	s := New(t.TempDir(), t.TempDir())
	t.Cleanup(s.Shutdown)
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitDeployed waits for a deploy to finish and returns the resulting status.
func waitDeployed(t *testing.T, s *Supervisor, name string) Status {
	t.Helper()
	waitFor(t, "deploy to finish", func() bool { return !s.Status(name).Deploying })
	return s.Status(name)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func service(name, repo, build, start string) store.Project {
	return store.Project{Name: name, Type: store.TypeService, Repo: repo, BuildCmd: build, StartCmd: start, Port: 4000}
}

func TestDeployBuildsAndRunsService(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"app.sh": "echo running\nexec sleep 60\n"})
	s := newSupervisor(t)
	spec := service("app", repo, "echo built > built.txt", "sh app.sh")

	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	status := waitDeployed(t, s, "app")

	if status.State != StateRunning || status.PID == 0 || status.StartedAt == nil {
		t.Fatalf("status = %+v, want running with a pid", status)
	}
	if status.Commit == "" {
		t.Fatal("commit not recorded")
	}
	if !s.IsDeployed("app") {
		t.Fatal("IsDeployed = false after deploy")
	}
	if _, err := os.Stat(filepath.Join(s.projectDir("app"), "built.txt")); err != nil {
		t.Fatalf("build command did not run: %v", err)
	}
	waitFor(t, "app output in logs", func() bool {
		return strings.Contains(strings.Join(s.Logs("app"), "\n"), "running")
	})

	s.Stop("app")
	status = s.Status("app")
	if status.State != StateStopped || processAlive(status.PID) && status.PID != 0 {
		t.Fatalf("status after Stop = %+v", status)
	}
}

func TestEnvironmentIsInjectedAndSelfHosterSettingsAreHidden(t *testing.T) {
	t.Setenv("SELFHOSTER_ADMIN_PASSWORD", "top-secret")
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	spec := service("env", repo, "env > build-env.txt", "env > run-env.txt; exec sleep 60")
	spec.Port = 4321
	spec.Env = map[string]string{"GREETING": "hello", "MONGO_URL": "mongodb://db/app"}

	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	waitDeployed(t, s, "env")
	runEnv := filepath.Join(s.projectDir("env"), "run-env.txt")
	waitFor(t, "run env file", func() bool {
		data, _ := os.ReadFile(runEnv)
		return strings.Contains(string(data), "PORT=")
	})

	for _, file := range []string{"build-env.txt", "run-env.txt"} {
		data, err := os.ReadFile(filepath.Join(s.projectDir("env"), file))
		if err != nil {
			t.Fatal(err)
		}
		env := string(data)
		for _, want := range []string{"GREETING=hello", "MONGO_URL=mongodb://db/app", "PORT=4321", "PATH=", "DATA_DIR=" + s.AppDir("env")} {
			if !strings.Contains(env, want) {
				t.Errorf("%s: missing %q", file, want)
			}
		}
		if strings.Contains(env, "top-secret") || strings.Contains(env, "SELFHOSTER_") {
			t.Errorf("%s: self-hoster settings leaked to the project", file)
		}
	}
}

func TestCrashedServiceIsRestarted(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	// Crashes on the first run only: the marker file makes the second run stay up.
	start := "if [ -f started ]; then exec sleep 60; fi; touch started; exit 3"
	if err := s.Deploy(service("crashy", repo, "", start)); err != nil {
		t.Fatal(err)
	}
	waitDeployed(t, s, "crashy")
	waitFor(t, "restart after crash", func() bool {
		st := s.Status("crashy")
		return st.Restarts == 1 && st.State == StateRunning
	})
	logs := strings.Join(s.Logs("crashy"), "\n")
	if !strings.Contains(logs, "exited unexpectedly: exit status 3") {
		t.Fatalf("crash not logged:\n%s", logs)
	}
}

func TestStopCancelsPendingRestart(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	if err := s.Deploy(service("dies", repo, "", "exit 1")); err != nil {
		t.Fatal(err)
	}
	waitDeployed(t, s, "dies")
	waitFor(t, "crash", func() bool { return s.Status("dies").State == StateCrashed })
	s.Stop("dies")
	time.Sleep(1500 * time.Millisecond) // longer than the first backoff
	if st := s.Status("dies"); st.State != StateStopped || st.Restarts != 1 {
		t.Fatalf("status = %+v, want stopped with no further restart", st)
	}
}

func TestStopKillsTheWholeProcessGroup(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	// The shell starts a grandchild, like `npm start` spawning node.
	start := "sleep 60 & echo $! > child.pid; wait"
	if err := s.Deploy(service("tree", repo, "", start)); err != nil {
		t.Fatal(err)
	}
	waitDeployed(t, s, "tree")
	pidFile := filepath.Join(s.projectDir("tree"), "child.pid")
	var child int
	waitFor(t, "child pid", func() bool {
		data, err := os.ReadFile(pidFile)
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && child > 0
	})

	s.Stop("tree")
	waitFor(t, "grandchild to exit", func() bool { return !processAlive(child) || isZombie(child) })
}

// isZombie reports whether pid has exited but not been reaped (reaping is tini's job in the container).
func isZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestFailedBuildKeepsPreviousVersionRunning(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "v1"})
	s := newSupervisor(t)
	spec := service("app", repo, "true", "exec sleep 60")
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	before := waitDeployed(t, s, "app")

	spec.BuildCmd = "exit 7"
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	after := waitDeployed(t, s, "app")

	if after.State != StateRunning || after.PID != before.PID {
		t.Fatalf("previous version not kept: before %+v, after %+v", before, after)
	}
	if !strings.Contains(after.LastError, "build") {
		t.Fatalf("LastError = %q, want a build error", after.LastError)
	}
}

func TestRedeployPicksUpNewCommits(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"version.txt": "v1"})
	s := newSupervisor(t)
	spec := service("app", repo, "", "exec sleep 60")
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	first := waitDeployed(t, s, "app")

	if err := os.WriteFile(filepath.Join(repo, "version.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, repo, "v2")
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	second := waitDeployed(t, s, "app")

	data, _ := os.ReadFile(filepath.Join(s.projectDir("app"), "version.txt"))
	if string(data) != "v2" {
		t.Fatalf("checkout has %q, want v2", data)
	}
	if second.Commit == first.Commit || second.PID == first.PID {
		t.Fatalf("not redeployed: first %+v, second %+v", first, second)
	}
}

func TestConcurrentDeployIsRejected(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	spec := service("slow", repo, "sleep 1", "exec sleep 60")
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	if err := s.Deploy(spec); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Deploy = %v, want ErrBusy", err)
	}
	if err := s.Remove("slow"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Remove during deploy = %v, want ErrBusy", err)
	}
	waitDeployed(t, s, "slow")
}

func TestStartBeforeDeployFails(t *testing.T) {
	s := newSupervisor(t)
	err := s.Start(service("never", "/nowhere", "", "true"))
	if !errors.Is(err, ErrNotDeployed) {
		t.Fatalf("Start = %v, want ErrNotDeployed", err)
	}
}

func TestRemoveDeletesCheckout(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	if err := s.Deploy(service("gone", repo, "", "exec sleep 60")); err != nil {
		t.Fatal(err)
	}
	waitDeployed(t, s, "gone")
	if err := s.Remove("gone"); err != nil {
		t.Fatal(err)
	}
	if s.IsDeployed("gone") {
		t.Fatal("checkout still present")
	}
	if st := s.Status("gone"); st.State != StateStopped {
		t.Fatalf("status = %+v", st)
	}
}

func TestStaticProjectIsServed(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"src/index.html": "<h1>site</h1>"})
	s := newSupervisor(t)
	port := freePort(t)
	spec := store.Project{
		Name: "site", Type: store.TypeStatic, Repo: repo, Port: port,
		BuildCmd: "mkdir -p dist && cp src/index.html dist/", StaticDir: "dist", SPA: true,
	}
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	status := waitDeployed(t, s, "site")
	if status.State != StateRunning {
		t.Fatalf("status = %+v, want running", status)
	}

	body := httpGet(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/client/route")
	if !strings.Contains(body, "site") {
		t.Fatalf("body = %q, want the SPA index", body)
	}

	s.Stop("site")
	if s.Status("site").State != StateStopped {
		t.Fatal("static project not stopped")
	}
	if _, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port)); err == nil {
		t.Fatal("port still served after Stop")
	}
}

func TestStaticProjectWithoutBuildOutputFails(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	spec := store.Project{Name: "site", Type: store.TypeStatic, Repo: repo, Port: freePort(t), StaticDir: "dist"}
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	status := waitDeployed(t, s, "site")
	if status.State != StateStopped || !strings.Contains(status.LastError, "not found") {
		t.Fatalf("status = %+v, want stopped with a missing-folder error", status)
	}
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return string(body)
}

func TestNextBackoff(t *testing.T) {
	steps := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 32 * time.Second, time.Minute}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, time.Minute, time.Minute}
	for i, current := range steps {
		if got := nextBackoff(current); got != want[i] {
			t.Errorf("nextBackoff(%s) = %s, want %s", current, got, want[i])
		}
	}
}

func TestDataDirIsPersistentAndKeptOnRemove(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	// The app writes into its data folder on every start, like a SQLite database would.
	start := `echo run >> "$DATA_DIR/runs.log"; exec sleep 60`
	spec := service("keep", repo, `echo build >> "$DATA_DIR/runs.log"`, start)

	for range 2 {
		if err := s.Deploy(spec); err != nil {
			t.Fatal(err)
		}
		waitDeployed(t, s, "keep")
	}
	logFile := filepath.Join(s.AppDir("keep"), "runs.log")
	waitFor(t, "second start to write", func() bool {
		data, _ := os.ReadFile(logFile)
		return strings.Count(string(data), "run") == 2
	})

	if err := s.Remove("keep"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("data folder removed with the project: %v", err)
	}
	if got := string(data); got != "build\nrun\nbuild\nrun\n" {
		t.Fatalf("runs.log = %q, want both deploys recorded", got)
	}
}

func TestDataDirCanBeOverridden(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	s := newSupervisor(t)
	custom := t.TempDir()
	spec := service("custom", repo, "", `echo "$DATA_DIR" > dir.txt; exec sleep 60`)
	spec.Env = map[string]string{"DATA_DIR": custom}
	if err := s.Deploy(spec); err != nil {
		t.Fatal(err)
	}
	waitDeployed(t, s, "custom")
	out := filepath.Join(s.projectDir("custom"), "dir.txt")
	waitFor(t, "dir.txt", func() bool {
		data, _ := os.ReadFile(out)
		return strings.TrimSpace(string(data)) == custom
	})
}
