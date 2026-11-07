package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"selfhoster/internal/store"
)

// userinfoPattern matches credentials embedded in a URL (https://token@host, https://user:pass@host).
var userinfoPattern = regexp.MustCompile(`://[^/@\s]+@`)

// RemoteCommit returns the commit at the tip of the project's branch (or the remote
// default branch) without fetching it, using the same credentials as the clone.
func (s *Supervisor) RemoteCommit(ctx context.Context, spec store.Project) (string, error) {
	ref := "HEAD"
	if spec.Branch != "" {
		ref = "refs/heads/" + spec.Branch
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", spec.Repo, ref)
	cmd.Env = gitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git ls-remote: %s", redact(strings.TrimSpace(string(exitErr.Stderr)), spec.Repo))
		}
		return "", fmt.Errorf("git ls-remote: %s", redact(err.Error(), spec.Repo))
	}
	sha, _, found := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if !found || sha == "" {
		return "", fmt.Errorf("branch %q not found on the remote", ref)
	}
	return sha, nil
}

// DeployedCommit returns the full hash checked out for the project, or "" if it was never deployed.
func (s *Supervisor) DeployedCommit(name string) string {
	out, err := exec.Command("git", "-C", s.projectDir(name), "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LogSystem appends a self-hoster message to the project's logs.
func (s *Supervisor) LogSystem(spec store.Project, format string, args ...any) {
	s.proc(spec).logs.system(format, args...)
}

// redact removes the repository URL and any URL credentials from git output,
// since the URL may embed an access token.
func redact(text, repo string) string {
	if repo != "" {
		text = strings.ReplaceAll(text, repo, "<repo>")
	}
	return userinfoPattern.ReplaceAllString(text, "://***@")
}
