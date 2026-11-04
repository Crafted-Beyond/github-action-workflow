package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKubernetesFailureHints(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   string
	}{
		{"dial tcp: i/o timeout", "timed out"},
		{"context deadline exceeded", "timed out"},
		{"Error from server (Forbidden)", "forbidden"},
		{"Error from server (Unauthorized)", "authentication failed"},
		{"the server has asked for the client to provide credentials", "authentication failed"},
		{"x509: certificate signed by unknown authority", "TLS verification failed"},
		{"dial tcp: connection refused", "connection failed"},
		{"dial tcp: no route to host", "connection failed"},
		{"lookup server: no such host", "connection failed"},
		{"some unknown error with password=private-value", "stderr suppressed"},
	} {
		hint := kubernetesFailureHint(context.Background(), tc.stderr)
		if !strings.Contains(hint, tc.want) || strings.Contains(hint, "private-value") {
			t.Errorf("hint for %q = %q; want %q without sensitive input", tc.stderr, hint, tc.want)
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	defer cancel()
	if hint := kubernetesFailureHint(ctx, ""); !strings.Contains(hint, "timed out") {
		t.Fatalf("deadline hint = %q", hint)
	}
}

func TestGetReportsResourceWithoutLeakingSecretOutput(t *testing.T) {
	dir := t.TempDir()
	shim := `#!/bin/sh
printf '%s\n' "$@" > "$TEST_KUBECTL_ARGS"
echo '{"data":{"password":"private-value"}}'
echo 'Error from server (Forbidden): private-value' >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(dir, "args")
	t.Setenv("TEST_KUBECTL_ARGS", argsPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	value, err := get("secret", "argocd", "wireguard-api-pr8-database")
	if value != nil || err == nil {
		t.Fatalf("get returned %v, %v", value, err)
	}
	for _, expected := range []string{"secret", "argocd", "wireguard-api-pr8-database", "forbidden"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q missing %q", err, expected)
		}
	}
	if strings.Contains(err.Error(), "private-value") {
		t.Fatal("secret output leaked in error")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--request-timeout=15s\n") {
		t.Fatalf("request timeout missing from arguments: %s", args)
	}
}
