// Package gittest builds throwaway Git repositories for tests.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a temporary Git working tree.
type Repo struct {
	Dir string
	t   *testing.T
}

// New initialises an empty repository in t.TempDir() with a fixed identity,
// so commits are reproducible and do not depend on the developer's gitconfig.
func New(t *testing.T) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	r := &Repo{Dir: dir, t: t}
	r.Git("init", "--quiet", "--initial-branch=main")
	r.Git("config", "user.email", "test@example.com")
	r.Git("config", "user.name", "Test")
	r.Git("config", "commit.gpgsign", "false")
	return r
}

// Git runs a git command in the repository and fails the test on error.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.Dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write creates or overwrites a file, creating parent directories.
func (r *Repo) Write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.Dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Remove deletes a file.
func (r *Repo) Remove(path string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.Dir, path)); err != nil {
		r.t.Fatal(err)
	}
}

// Commit stages everything and commits with the given message.
func (r *Repo) Commit(msg string) string {
	r.t.Helper()
	r.Git("add", "-A")
	r.Git("commit", "--quiet", "--allow-empty", "-m", msg)
	return r.Git("rev-parse", "HEAD")
}
