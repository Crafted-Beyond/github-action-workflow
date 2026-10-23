package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewCleanupAuthorization(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, state, repository, base, head string
		wantError                           bool
	}{
		{"closed same repository", "closed", "org/ui", "main", sha, false},
		{"open PR", "open", "org/ui", "main", sha, true},
		{"fork", "closed", "other/ui", "main", sha, true},
		{"wrong base", "closed", "org/ui", "dev", sha, true},
		{"changed head", "closed", "org/ui", "main", strings.Repeat("b", 40), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_EVENT_NAME", "pull_request")
			t.Setenv("RELEASE_BRANCH", "main")
			t.Setenv("STATE_REPOSITORY", "org/private-state")
			t.Setenv("INFRASTRUCTURE_DIRECTORY", "infra/ui")
			t.Setenv("RUNNER_TEMP", t.TempDir())
			t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
			if err := os.WriteFile(os.Getenv("GITHUB_OUTPUT"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/org/private-state" {
					w.Write([]byte(`{"private":true}`))
					return
				}
				json.NewEncoder(w).Encode(map[string]any{
					"state": tc.state,
					"head":  map[string]any{"sha": tc.head, "repo": map[string]string{"full_name": tc.repository}},
					"base":  map[string]string{"ref": tc.base},
				})
			}))
			defer server.Close()
			c := &controller{api: server.URL, client: server.Client(), registry: "docker.io", image: "org/ui"}
			c.event.Repository.FullName = "org/ui"
			c.event.Action = "closed"
			c.event.PullRequest.Number = 7
			c.event.PullRequest.Head.SHA = sha
			if err := c.previewCleanupPrepare(); (err != nil) != tc.wantError {
				t.Fatalf("cleanup authorization: %v; wantError=%t", err, tc.wantError)
			}
			if !tc.wantError {
				data, err := os.ReadFile(filepath.Join(os.Getenv("RUNNER_TEMP"), "combine.auto.tfvars.json"))
				if err != nil {
					t.Fatal(err)
				}
				var config struct {
					Runtime struct {
						Preview     bool   `json:"preview"`
						Environment string `json:"environment"`
						PR          int    `json:"pr_number"`
					} `json:"combine_runtime"`
				}
				if err := json.Unmarshal(data, &config); err != nil || !config.Runtime.Preview || config.Runtime.PR != 7 || config.Runtime.Environment != "pr7" {
					t.Fatalf("cleanup must target only this preview: %s (%v)", data, err)
				}
			}
		})
	}
}

func TestVerifyPreviewCleanupState(t *testing.T) {
	owned := `{"outputs":{"combine_deployment":{"value":{"name":"ui-pr7","namespace":"ui-pr7"}}},
	"resources":[{"mode":"managed","type":"kubernetes_namespace_v1","name":"preview","instances":[
	{"attributes":{"metadata":[{"name":"ui-pr7","labels":{"app.kubernetes.io/managed-by":"combine-workflow","craftedbeyond.com/environment":"pr7"}}]}}]}]}`
	for _, tc := range []struct {
		name, data string
		wantError  bool
	}{
		{"owned preview", owned, false},
		{"already destroyed", `{"resources":[]}`, false},
		{"wrong PR", strings.ReplaceAll(owned, "ui-pr7", "ui-pr8"), true},
		{"permanent namespace", strings.ReplaceAll(owned, `"name":"preview"`, `"name":"application"`), true},
		{"wrong environment", strings.ReplaceAll(owned, `"pr7"`, `"prod"`), true},
		{"missing ownership", strings.ReplaceAll(owned, "combine-workflow", "other"), true},
		{"partial state missing outputs", strings.ReplaceAll(owned, `"combine_deployment"`, `"missing"`), true},
		{"invalid state", `{invalid`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyPreviewState([]byte(tc.data), "ui-pr7"); (err != nil) != tc.wantError {
				t.Fatalf("state ownership: %v; wantError=%t", err, tc.wantError)
			}
		})
	}
}
