package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func dockerRepositoryError(status int, body io.Reader) error {
	// Classify known responses without logging arbitrary registry text or credentials.
	var response struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	reason := "Docker Hub did not provide a recognized reason; check the account's repository-creation access and repository allowance"
	if err := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&response); err == nil {
		message := strings.ToLower(response.Message + " " + response.Detail)
		switch {
		case strings.Contains(message, "quota"), strings.Contains(message, "limit"),
			strings.Contains(message, "subscription"), strings.Contains(message, "upgrade"),
			strings.Contains(message, "plan"):
			reason = "Docker Hub reports a repository allowance or subscription restriction; review repository capacity for this namespace"
		case strings.Contains(message, "permission"), strings.Contains(message, "forbidden"),
			strings.Contains(message, "not allowed"), strings.Contains(message, "access denied"),
			strings.Contains(message, "unauthorized"):
			reason = "Docker Hub reports an authorization restriction; verify the token's repository-management scope and the account's role in the target namespace"
		case strings.Contains(message, "namespace"), strings.Contains(message, "organization"):
			reason = "Docker Hub reports a namespace restriction; verify the target namespace exists and the authenticated account may create repositories there"
		}
	}
	return fmt.Errorf("Docker Hub creation returned HTTP %d: %s. No public repository fallback was attempted", status, reason)
}
