// Package testutil holds helpers shared by tests across packages.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NewRepo creates a local git repository with the given files committed, and returns its path.
// The test is skipped when git is not installed.
func NewRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	WriteFiles(t, dir, files)
	Git(t, dir, "init", "-q")
	Commit(t, dir, "init")
	return dir
}

// WriteFiles writes files (relative path -> content) under dir, creating folders as needed.
func WriteFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Commit stages everything in dir and commits it, returning the new commit hash.
func Commit(t *testing.T, dir, message string) string {
	t.Helper()
	Git(t, dir, "add", "-A")
	Git(t, dir, "-c", "user.email=test@test", "-c", "user.name=test", "commit", "-q", "-m", message)
	return Git(t, dir, "rev-parse", "HEAD")
}

// Git runs a git command in dir and returns its trimmed output, failing the test on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
