package main

import (
	"encoding/json"
	"testing"
)

func TestDatabaseSourcesEqual(t *testing.T) {
	for _, tc := range []struct {
		name, desired, compared string
		equal                   bool
	}{
		{"omitted empty files", `{"path":"k3s","helm":{"values":"same","valueFiles":[]}}`, `{"path":"k3s","helm":{"values":"same"}}`, true},
		{"different values", `{"helm":{"values":"new","valueFiles":[]}}`, `{"helm":{"values":"old"}}`, false},
		{"nonempty files", `{"helm":{"valueFiles":["prod.yaml"]}}`, `{"helm":{}}`, false},
		{"different revision", `{"targetRevision":"new"}`, `{"targetRevision":"old"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var desired, compared map[string]any
			if err := json.Unmarshal([]byte(tc.desired), &desired); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.compared), &compared); err != nil {
				t.Fatal(err)
			}
			if got := databaseSourcesEqual(desired, compared); got != tc.equal {
				t.Fatalf("equal=%t, want %t", got, tc.equal)
			}
		})
	}
}
