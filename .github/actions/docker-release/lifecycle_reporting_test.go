package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLifecycleReporting(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "..", "tofu-plan", "lifecycle.sh")
	for _, tc := range []struct {
		name, planCode, applyCode, initCode string
		wantPlanFailure, wantApplyFailure   bool
	}{
		{"changes", "2", "0", "0", false, false},
		{"no changes", "0", "0", "0", false, false},
		{"failed plan", "1", "0", "0", true, false},
		{"failed init with code two", "2", "0", "2", true, false},
		{"partial apply failure", "2", "1", "0", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fake := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$CALLS"
case "$1" in
init) exit "$INIT_CODE";;
validate) exit 0;;
plan) echo "Plan: 2 to add, 3 to change, 0 to destroy."; touch lifecycle.tfplan; exit "$PLAN_CODE";;
show)
  if [ "$2" = "-json" ]; then
    if [ "$PLAN_CODE" = 0 ]; then echo '{"resource_changes":[]}'; else
      echo '{"resource_changes":[{"address":"test.one","change":{"actions":["create"]}},{"address":"test.two","change":{"actions":["create"]}},{"address":"test.three","change":{"actions":["update"]}},{"address":"test.four","change":{"actions":["update"]}},{"address":"test.five","change":{"actions":["update"]}}]}'
    fi
  else echo "Full plan details"; fi;;
apply) echo "Saved-plan apply output"; exit "$APPLY_CODE";;
*) exit 99;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "tofu"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}

			prefix := filepath.Join(dir, "database-plan")
			calls := filepath.Join(dir, "calls")
			env := append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"), "GITHUB_SHA="+strings.Repeat("a", 40),
				"TOFU_SOURCE_SHA="+strings.Repeat("b", 40),
				"PLAN_CODE="+tc.planCode, "APPLY_CODE="+tc.applyCode, "INIT_CODE="+tc.initCode, "CALLS="+calls)
			run := func(phase string) ([]byte, error) {
				cmd := exec.Command("bash", script, phase, "combine/test/app/pr1-db", prefix, "environments/prod.tfvars", "true")
				cmd.Dir, cmd.Env = dir, env
				return cmd.CombinedOutput()
			}
			out, err := run("plan")
			if (err != nil) != tc.wantPlanFailure {
				t.Fatalf("plan error=%v\n%s", err, out)
			}
			md, err := os.ReadFile(prefix + ".md")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(md), "Commit `bbbbbbb`") {
				t.Fatalf("report must identify the selected source, not the caller SHA: %s", md)
			}
			if tc.wantPlanFailure {
				if !strings.Contains(string(md), "Plan failed") {
					t.Fatalf("missing failure report: %s", md)
				}
				return
			}
			if tc.planCode == "2" {
				for _, text := range []string{"Create (2)", "Update (3)", "| Create  |     2 |", "| Update  |     3 |", "::group::Details"} {
					if !strings.Contains(string(out), text) {
						t.Fatalf("missing %q in output: %s", text, out)
					}
				}
			} else if !strings.Contains(string(md), "No changes") {
				t.Fatalf("missing no-change summary: %s", md)
			}
			out, err = run("apply")
			if (err != nil) != tc.wantApplyFailure {
				t.Fatalf("apply error=%v\n%s", err, out)
			}
			md, err = os.ReadFile(prefix + ".md")
			if err != nil {
				t.Fatal(err)
			}
			expected := "Apply succeeded"
			if tc.wantApplyFailure {
				expected = "Apply failed"
			}
			if !strings.Contains(string(md), expected) || !strings.Contains(string(md), "OpenTofu plan") {
				t.Fatalf("missing combined plan/apply report: %s", md)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			for _, flag := range []string{"-destroy", "-var-file=environments/prod.tfvars", "apply -input=false -no-color lifecycle.tfplan"} {
				if !strings.Contains(string(commands), flag) {
					t.Fatalf("missing saved-plan argument %q: %s", flag, commands)
				}
			}
			for _, suffix := range []string{".json", ".txt", "-plan.log", "-apply.log"} {
				if _, err := os.Stat(prefix + suffix); !os.IsNotExist(err) {
					t.Fatalf("temporary report input remains: %s", suffix)
				}
			}

		})
	}
}

func TestDirectLifecycleApply(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "..", "tofu-plan", "lifecycle.sh")
	for _, code := range []string{"0", "1"} {
		t.Run("exit "+code, func(t *testing.T) {
			dir := t.TempDir()
			fake := "#!/usr/bin/env bash\nset -eu\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\ncase \"$1\" in\ninit|validate) exit 0;;\napply) echo 'Direct apply output'; exit \"$APPLY_CODE\";;\n*) exit 99;;\nesac\n"
			if err := os.WriteFile(filepath.Join(dir, "tofu"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			prefix := filepath.Join(dir, "report")
			cmd := exec.Command("bash", script, "direct", "preview", prefix, "environments/prod.tfvars", "false")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CALLS="+calls, "APPLY_CODE="+code, "GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))
			out, err := cmd.CombinedOutput()
			if (err != nil) != (code != "0") {
				t.Fatalf("direct apply error=%v: %s", err, out)
			}
			commands, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			want := "init -input=false -no-color\nvalidate -no-color\napply -input=false -no-color -auto-approve -var-file=environments/prod.tfvars\n"
			if string(commands) != want {
				t.Fatalf("must apply directly without plan/show: %s", commands)
			}
			md, err := os.ReadFile(prefix + ".md")
			if err != nil {
				t.Fatal(err)
			}
			result := "Apply succeeded"
			if code != "0" {
				result = "Apply failed"
			}
			if !strings.Contains(string(md), result) || !strings.Contains(string(md), "Direct apply output") {
				t.Fatalf("missing direct apply result: %s", md)
			}
		})
	}
}
