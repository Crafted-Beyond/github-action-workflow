package main

import (
	"strings"
	"testing"
)

func TestValidateDatabaseSource(t *testing.T) {
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, expected, checkout, head string
		wantError                      bool
	}{
		{"same immutable commit", sha, sha, sha, false},
		{"checkout is main instead", sha, other, sha, true},
		{"PR advanced", sha, sha, other, true},
		{"missing captured commit", "", sha, sha, true},
		{"branch instead of commit", "main", "main", "main", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateDatabaseSource(tc.expected, tc.checkout, tc.head); (err != nil) != tc.wantError {
				t.Fatalf("validateDatabaseSource() = %v; wantError=%t", err, tc.wantError)
			}
		})
	}
}
