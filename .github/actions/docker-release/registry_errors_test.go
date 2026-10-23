package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type registryCreationTransport func(*http.Request) (*http.Response, error)

func (f registryCreationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDockerRepositoryErrorRedaction(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{
		{`{"message":"private repository quota exceeded secret-token"}`, "allowance or subscription"},
		{`{"detail":"insufficient permissions secret-token"}`, "authorization restriction"},
		{`{"message":"namespace unavailable secret-token"}`, "namespace restriction"},
		{`{"message":"secret-token"}`, "did not provide a recognized reason"},
		{`<html>secret-token</html>`, "did not provide a recognized reason"},
		{`{"detail":{"token":"secret-token"}}`, "did not provide a recognized reason"},
	} {
		err := dockerRepositoryError(403, strings.NewReader(tc.body))
		if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("unsafe or incorrect diagnostic: %v", err)
		}
	}
}

func TestAutomaticPrivateRepositoryCreation(t *testing.T) {
	t.Setenv("REPOSITORY_VISIBILITY", "")
	for _, image := range []string{"john24rel/wireguard-ui", "john24rel/future-application"} {
		t.Run(image, func(t *testing.T) {
			created := false
			client := &http.Client{Transport: registryCreationTransport(func(r *http.Request) (*http.Response, error) {
				status := http.StatusOK
				if r.Method == http.MethodGet && !created {
					status = http.StatusNotFound
				}
				if r.Method == http.MethodPost {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(body, []byte(`"is_private":true`)) {
						t.Fatalf("creation must stay private: %s", body)
					}
					created = true
					status = http.StatusCreated
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
			})}
			registry := &registryClient{controller: &controller{image: image, client: client}, token: "test-token"}
			if err := registry.ensureRepository(); err != nil || !created {
				t.Fatalf("automatic creation failed: %v", err)
			}
		})
	}
}

func TestRepositoryVisibility(t *testing.T) {
	for _, visibility := range []string{"private", "public", "invalid"} {
		t.Run(visibility, func(t *testing.T) {
			t.Setenv("REPOSITORY_VISIBILITY", visibility)
			created := false
			requests := 0
			client := &http.Client{Transport: registryCreationTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				status := http.StatusOK
				if r.Method == http.MethodGet && !created {
					status = http.StatusNotFound
				}
				if r.Method == http.MethodPost {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					want := `"is_private":false`
					if visibility == "private" {
						want = `"is_private":true`
					}
					if !bytes.Contains(data, []byte(want)) {
						t.Fatalf("visibility %s sent wrong creation payload: %s", visibility, data)
					}
					created = true
					status = http.StatusCreated
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
			})}
			registry := &registryClient{controller: &controller{image: "org/new-app", client: client}}
			err := registry.ensureRepository()
			if visibility == "invalid" {
				if err == nil || requests != 0 {
					t.Fatalf("invalid visibility must fail before requests: %v, %d", err, requests)
				}
			} else if err != nil || !created || requests != 3 {
				t.Fatalf("creation/verification failed: %v, created=%t, requests=%d", err, created, requests)
			}
		})
	}
}

func TestExistingRepositoryVisibilityUnchanged(t *testing.T) {
	t.Setenv("REPOSITORY_VISIBILITY", "public")
	client := &http.Client{Transport: registryCreationTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("existing repository visibility must not be mutated")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})}
	registry := &registryClient{controller: &controller{image: "org/existing-app", client: client}}
	if err := registry.ensureRepository(); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryCreationFailureDiagnostic(t *testing.T) {
	t.Setenv("REPOSITORY_VISIBILITY", "")
	requests := 0
	client := &http.Client{Transport: registryCreationTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		status, body := http.StatusNotFound, "{}"
		if r.Method == http.MethodPost {
			status, body = http.StatusForbidden, `{"message":"insufficient permissions secret-token"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	registry := &registryClient{controller: &controller{image: "john24rel/future-app", client: client}, token: "secret-token"}
	err := registry.ensureRepository()
	if err == nil || !strings.Contains(err.Error(), "authorization restriction") ||
		strings.Contains(err.Error(), "secret-token") || requests != 2 {
		t.Fatalf("unexpected error or public fallback: requests=%d, error=%v", requests, err)
	}
}
