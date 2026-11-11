package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidatePreviewSource(t *testing.T) {
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, expected, actual string
		preview, wantError     bool
	}{
		{"matching captured preview", sha, sha, true, false},
		{"head changed", sha, other, true, true},
		{"invalid SHA", "main", sha, true, true},
		{"not a preview", sha, sha, false, true},
		{"image only without prerequisite", "", sha, false, false},
		{"release without prerequisite", "", other, false, false},
		{"preview without prerequisite", "", sha, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePreviewSource(tc.expected, tc.actual, tc.preview); (err != nil) != tc.wantError {
				t.Fatalf("validatePreviewSource() = %v; wantError=%t", err, tc.wantError)
			}
		})
	}
}

func TestRecoveryTarget(t *testing.T) {
	for _, test := range []struct {
		command string
		want    int
		found   bool
	}{
		{"/recover-release-pr1", 1, true},
		{"/recover-release-pr42", 42, true},
		{"/recover-release-pr0", 0, false},
		{"/recover-release-prx", 0, false},
		{"/recover-release-pr1 extra", 0, false},
		{"/minor", 0, false},
	} {
		t.Run(test.command, func(t *testing.T) {
			got, found, err := recoveryTarget(test.command)
			if err != nil || got != test.want || found != test.found {
				t.Fatalf("recoveryTarget(%q) = %d, %t, %v; want %d, %t", test.command, got, found, err, test.want, test.found)
			}
		})
	}
}

func TestFailedRunOnlyAllowsCompletedFailures(t *testing.T) {
	for _, test := range []struct {
		name, status, conclusion string
		wantFailed, wantError    bool
	}{
		{"failed", "completed", "failure", true, false},
		{"cancelled", "completed", "cancelled", true, false},
		{"successful", "completed", "success", false, false},
		{"active", "in_progress", "", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/owner/repo/actions/runs/123" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":%q,"conclusion":%q}`, test.status, test.conclusion)
			}))
			defer server.Close()
			c := &controller{api: server.URL, token: "test", client: server.Client()}
			c.event.Repository.FullName = "owner/repo"
			got, err := c.failedRun("123")
			if got != test.wantFailed || (err != nil) != test.wantError {
				t.Fatalf("failedRun() = %t, %v; want %t, error=%t", got, err, test.wantFailed, test.wantError)
			}
		})
	}
}
