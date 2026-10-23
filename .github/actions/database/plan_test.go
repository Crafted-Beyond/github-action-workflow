package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabasePlanDoesNotMutate(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		existing, locked, fail bool
	}{
		{name: "new database"},
		{name: "existing database", existing: true},
		{name: "held backend lock", existing: true, locked: true},
		{name: "failed plan", existing: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("RUNNER_TEMP", root)
			t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(root, "summary"))
			t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
			t.Setenv("GITHUB_REF", "refs/heads/main")
			t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
			t.Setenv("RELEASE_BRANCH", "main")
			t.Setenv("DATABASE_PASSWORD", strings.Repeat("p", 24))
			t.Setenv("DATABASE_VAR_FILE", "")
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CALLS", filepath.Join(root, "calls"))
			script := filepath.Join(root, "lifecycle.sh")
			t.Setenv("DATABASE_REPORT_SCRIPT", script)
			write := func(path, data string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(data), mode); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "git"), "#!/bin/sh\necho 'Unexpected Git mutation' >&2\nexit 99\n", 0700)
			write(filepath.Join(root, "kubectl"), `#!/bin/sh
printf '%s\n' "$*" >> "$CALLS"
case " $* " in
  *" get "*) exit 0;;
  *) echo 'Unexpected Kubernetes mutation' >&2; exit 99;;
esac
`, 0700)
			code := "0"
			if tc.fail {
				code = "1"
			}
			write(script, `#!/bin/sh
printf '%s\n' "$1" >> "$CALLS"
test "$1" = plan || exit 99
printf 'Plan only\n' > "$3.md"
exit `+code+"\n", 0700)
			l := lifecycle{root: filepath.Join(root, "source"), backend: filepath.Join(root, "backend"), repository: "owner/app", name: "app"}
			path := l.statePath("db")
			write(filepath.Join(l.databaseDirectory(), "main.tf"), "", 0600)
			if tc.existing {
				write(filepath.Join(l.backend, path, "terraform.tfstate"), `{"version":4,"resources":[]}`, 0600)
			}
			if tc.locked {
				write(filepath.Join(l.backend, path, "deployment-lock.json"), `{"run":"other"}`, 0600)
			}
			err := l.plan()
			if (err != nil) != (tc.locked || tc.fail) {
				t.Fatalf("plan error: %v", err)
			}
			if tc.existing {
				data, err := os.ReadFile(filepath.Join(l.backend, path, "terraform.tfstate"))
				if err != nil || string(data) != `{"version":4,"resources":[]}` {
					t.Fatal("plan modified saved state")
				}
			} else if _, err := os.Stat(filepath.Join(l.backend, path)); !os.IsNotExist(err) {
				t.Fatal("plan must not create a backend directory or lock")
			}
			if !tc.locked {
				if _, err := os.Stat(filepath.Join(l.backend, path, "deployment-lock.json")); !os.IsNotExist(err) {
					t.Fatal("plan must not acquire a backend lock")
				}
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), "\napply\n") || strings.Contains(string(calls), " exec ") {
				t.Fatalf("unexpected write during plan: %s", calls)
			}
			if !tc.locked && !strings.Contains(string(calls), "\nplan\n") {
				t.Fatalf("plan not executed: %s", calls)
			}
		})
	}
}

func TestDatabasePlanRejectsPRAndNonReleaseDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, event, ref string
		pr               int
	}{
		{"PR", "issue_comment", "refs/heads/main", 1},
		{"feature branch", "workflow_dispatch", "refs/heads/feature", 0},
		{"push", "push", "refs/heads/main", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_EVENT_NAME", tc.event)
			t.Setenv("GITHUB_REF", tc.ref)
			t.Setenv("RELEASE_BRANCH", "main")
			l := lifecycle{pr: tc.pr}
			if err := l.plan(); err == nil {
				t.Fatal("plan must require a manual run on the release branch")
			}
		})
	}
}
