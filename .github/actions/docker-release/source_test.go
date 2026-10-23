package main

import (
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
