package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeploymentPlanCheckNeverWrites(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(fmt.Sprint(locked), func(t *testing.T) {
			t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
			t.Setenv("DEPLOY_ACTION", "plan")
			t.Setenv("STATE_REPOSITORY", "owner/backend")
			t.Setenv("STATE_REF", "main")
			t.Setenv("STATE_PATH", "combine/owner/app/prod")
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet {
					t.Errorf("plan attempted mutation: %s", r.Method)
				}
				if locked && strings.Contains(r.URL.Path, "/contents/") {
					content := base64.StdEncoding.EncodeToString([]byte(`{"run":"123","mode":"deployment"}`))
					fmt.Fprintf(w, `{"sha":"lock-sha","content":%q}`, content)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			c := &controller{api: server.URL, client: server.Client(), image: "owner/app", registry: "docker.io"}
			c.event.Repository.FullName = "owner/app"
			if err := c.deploymentPlanCheck(); (err != nil) != locked || requests == 0 {
				t.Fatalf("locked=%t, error=%v, requests=%d", locked, err, requests)
			}
			t.Setenv("DEPLOY_ACTION", "apply")
			before := requests
			if err := c.deploymentPlanCheck(); err == nil || requests != before {
				t.Fatal("read-only check must reject non-plan requests before API access")
			}
		})
	}
}
