package supervisor

import (
	"context"
	"strings"
	"testing"

	"selfhoster/internal/store"
	"selfhoster/internal/testutil"
)

func TestRemoteCommit(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "main"})
	mainSHA := testutil.Git(t, repo, "rev-parse", "HEAD")
	defaultBranch := testutil.Git(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	testutil.Git(t, repo, "checkout", "-q", "-b", "feature")
	testutil.WriteFiles(t, repo, map[string]string{"README": "feature"})
	featureSHA := testutil.Commit(t, repo, "feature")
	testutil.Git(t, repo, "checkout", "-q", defaultBranch)

	s := New(t.TempDir())
	tests := []struct {
		branch string
		want   string
	}{
		{"", mainSHA},            // remote default branch (HEAD)
		{defaultBranch, mainSHA}, // explicit branch
		{"feature", featureSHA},
	}
	for _, tc := range tests {
		got, err := s.RemoteCommit(context.Background(), store.Project{Repo: repo, Branch: tc.branch})
		if err != nil {
			t.Fatalf("branch %q: %v", tc.branch, err)
		}
		if got != tc.want {
			t.Fatalf("branch %q: got %s, want %s", tc.branch, got, tc.want)
		}
	}

	if _, err := s.RemoteCommit(context.Background(), store.Project{Repo: repo, Branch: "missing"}); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing branch error = %v", err)
	}
}

func TestRemoteCommitErrorDoesNotLeakToken(t *testing.T) {
	s := New(t.TempDir())
	// A token in an https URL, pointing at a closed local port: fails fast, no network needed.
	repo := "https://ghp_secretToken123@127.0.0.1:1/me/repo.git"
	_, err := s.RemoteCommit(context.Background(), store.Project{Repo: repo})
	if err == nil {
		t.Fatal("expected an error for a missing repository")
	}
	if strings.Contains(err.Error(), "ghp_secretToken123") {
		t.Fatalf("error leaks the repo URL: %v", err)
	}
}

func TestDeployedCommit(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"README": "x"})
	want := testutil.Git(t, repo, "rev-parse", "HEAD")
	s := newSupervisor(t)
	if got := s.DeployedCommit("app"); got != "" {
		t.Fatalf("before deploy: %q, want empty", got)
	}
	if err := s.Deploy(service("app", repo, "", "exec sleep 60")); err != nil {
		t.Fatal(err)
	}
	waitDeployed(t, s, "app")
	if got := s.DeployedCommit("app"); got != want {
		t.Fatalf("DeployedCommit = %q, want %q", got, want)
	}
}

func TestRedact(t *testing.T) {
	tests := []struct{ in, repo, want string }{
		{"fatal: https://ghp_abc@github.com/me/r.git not found", "", "fatal: https://***@github.com/me/r.git not found"},
		{"fatal: https://user:pass@host/r.git", "", "fatal: https://***@host/r.git"},
		{"cannot read git@github.com:me/r.git", "git@github.com:me/r.git", "cannot read <repo>"},
		{"no credentials here", "", "no credentials here"},
	}
	for _, tc := range tests {
		if got := redact(tc.in, tc.repo); got != tc.want {
			t.Errorf("redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
