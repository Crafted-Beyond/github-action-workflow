package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackendCommitRebasesConcurrentApplication(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "no-global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("STATE_REF", "main")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		os.Unsetenv(key)
	}
	run := func(directory string, args ...string) {
		t.Helper()
		if _, err := command(directory, "", "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	remote := filepath.Join(root, "remote.git")
	run(root, "init", "--bare", "--initial-branch=main", remote)
	first := filepath.Join(root, "first")
	run(root, "clone", remote, first)
	run(first, "config", "user.name", "seed")
	run(first, "config", "user.email", "seed@example.invalid")
	if err := os.WriteFile(filepath.Join(first, "seed"), []byte("seed"), 0600); err != nil {
		t.Fatal(err)
	}
	run(first, "add", "seed")
	run(first, "commit", "-m", "seed")
	run(first, "push", "origin", "main")
	second := filepath.Join(root, "second")
	run(root, "clone", remote, second)
	baseline := filepath.Join(root, "baseline")
	run(root, "clone", remote, baseline)
	run(baseline, "config", "user.useConfigOnly", "true")
	if err := os.WriteFile(filepath.Join(baseline, "baseline"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	run(baseline, "add", "baseline")
	run(baseline, "-c", "user.name=github-actions[bot]", "-c", "user.email=bot@example.invalid", "commit", "-m", "Old per-command identity")
	run(first, "config", "--unset", "user.name")
	run(first, "config", "--unset", "user.email")
	run(first, "config", "user.useConfigOnly", "true")
	other := lifecycle{backend: second, repository: "org/ui"}
	if err := writeJSON(filepath.Join(second, "combine/org/ui/pr2/record.json"), map[string]string{"run": "other"}); err != nil {
		t.Fatal(err)
	}
	if err := other.commit("Concurrent UI state"); err != nil {
		t.Fatal(err)
	}
	if _, err := command(baseline, "", "git", "pull", "--rebase", "origin", "main"); err == nil ||
		(!strings.Contains(err.Error(), "identity") && !strings.Contains(err.Error(), "user.email")) {
		t.Fatalf("expected old per-command identity to fail during rebase, got: %v", err)
	}
	current := lifecycle{backend: first, repository: "org/api"}
	if err := writeJSON(filepath.Join(first, "combine/org/api/pr1/record.json"), map[string]string{"run": "current"}); err != nil {
		t.Fatal(err)
	}
	if err := current.commit("API cleanup lock"); err != nil {
		t.Fatalf("push must rebase without global Git identity: %v", err)
	}
	for _, path := range []string{"combine/org/ui/pr2/record.json", "combine/org/api/pr1/record.json"} {
		run(root, "--git-dir="+remote, "cat-file", "-e", "main:"+path)
	}
}
