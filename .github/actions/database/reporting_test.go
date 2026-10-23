package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTerraformReportingPreservesState(t *testing.T) {
	for _, tc := range []struct {
		name, applyCode string
		pr              int
		destroy         bool
	}{
		{"PR direct success", "0", 1, false},
		{"PR partial apply", "1", 1, false},
		{"production saved plan success", "0", 0, false},
		{"production partial apply", "1", 0, false},
		{"cleanup saved plan success", "0", 1, true},
		{"cleanup partial apply", "1", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("GITHUB_WORKSPACE", dir)
			t.Setenv("RUNNER_TEMP", dir)
			t.Setenv("APPLY_CODE", tc.applyCode)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GIT_LOG", filepath.Join(dir, "git.log"))
			t.Setenv("DATABASE_REPORT_SCRIPT", filepath.Join(dir, "lifecycle.sh"))
			write := func(path, value string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(value), mode); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(dir, "git"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GIT_LOG"
if [ "$1" = diff ]; then exit 1; fi
exit 0
`, 0700)
			write(filepath.Join(dir, "lifecycle.sh"), `#!/usr/bin/env bash
set -eu
printf '%s\n' "$1" >> "$GIT_LOG"
if [ "$1" = plan ]; then
  printf 'Plan report\n' > "$3.md"
else
  printf 'new state including partial apply' > terraform.tfstate
  printf 'previous state' > terraform.tfstate.backup
  printf '\nApply report\n' >> "$3.md"
  exit "$APPLY_CODE"
fi
`, 0700)
			l := lifecycle{
				root: filepath.Join(dir, "source"), backend: filepath.Join(dir, "backend"),
				repository: "owner/app", run: "123", pr: tc.pr,
			}
			root := filepath.Join(l.root, "infra", "database")
			write(filepath.Join(root, "main.tf"), "", 0600)
			path := l.statePath("pr1-db")
			if tc.destroy {
				write(filepath.Join(l.backend, path, "terraform.tfstate"), "saved state", 0600)
			}
			err := l.terraform(root, path, tc.destroy)
			if (err != nil) != (tc.applyCode != "0") {
				t.Fatalf("apply code %s: %v", tc.applyCode, err)
			}
			phase := "direct"
			if tc.pr == 0 || tc.destroy {
				phase = "apply"
			}
			if err != nil && !strings.Contains(err.Error(), "Terraform "+phase+" failed") {
				t.Fatal(err)
			}
			for name, want := range map[string]string{
				"terraform.tfstate":        "new state including partial apply",
				"terraform.tfstate.backup": "previous state",
			} {
				data, err := os.ReadFile(filepath.Join(l.backend, path, name))
				if err != nil || string(data) != want {
					t.Fatalf("state not persisted: %s: %s, %v", name, data, err)
				}
			}
			if _, err := os.Stat(filepath.Join(l.backend, path, "deployment-lock.json")); err != nil {
				t.Fatalf("lock must remain through SQL provisioning/recovery: %v", err)
			}
			log, err := os.ReadFile(filepath.Join(dir, "git.log"))
			if err != nil || !strings.Contains(string(log), "Save "+path+" run 123") {
				t.Fatalf("state commit missing: %s, %v", log, err)
			}
			if !strings.Contains(string(log), "\n"+phase+"\n") ||
				strings.Contains(string(log), "\nplan\n") != (phase == "apply") {
				t.Fatalf("incorrect PR/production/cleanup execution phases: %s", log)
			}
		})
	}
}
