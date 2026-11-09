package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDeploymentHealthAfterNoOpConfigurationChange(t *testing.T) {
	start := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	fixture := `{
	  "spec":{"source":{"helm":{"values":"virtualService:\n  enabled: true\n"}}},
	  "status":{
	    "reconciledAt":"2026-11-09T01:00:01Z",
	    "health":{"status":"Healthy"},
	    "sync":{"status":"Synced","revision":"abc","comparedTo":{"source":{"helm":{"values":"virtualService:\n  enabled: true\n"}}}},
	    "operationState":{"phase":"Succeeded","syncResult":{"revision":"abc","source":{"helm":{"values":"httpRoute:\n  enabled: false\nvirtualService:\n  enabled: true\n"}}}}
	  }
	}`
	for _, tc := range []struct {
		name string
		edit func(*deploymentApplication)
		want bool
	}{
		{"current healthy comparison with historical no-op values", func(*deploymentApplication) {}, true},
		{"stale comparison", func(a *deploymentApplication) { a.Status.Sync.ComparedTo.Source = nil }, false},
		{"stale refresh", func(a *deploymentApplication) { a.Status.ReconciledAt = start.Add(-time.Second) }, false},
		{"unhealthy", func(a *deploymentApplication) { a.Status.Health.Status = "Degraded" }, false},
		{"out of sync", func(a *deploymentApplication) { a.Status.Sync.Status = "OutOfSync" }, false},
		{"running operation", func(a *deploymentApplication) { a.Status.OperationState.Phase = "Running" }, false},
		{"different revision", func(a *deploymentApplication) { a.Status.OperationState.SyncResult.Revision = "old" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var app deploymentApplication
			if err := json.Unmarshal([]byte(fixture), &app); err != nil {
				t.Fatal(err)
			}
			tc.edit(&app)
			if got := app.readySince(start); got != tc.want {
				t.Fatalf("readySince()=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestVirtualServiceGateways(t *testing.T) {
	for _, tc := range []struct {
		name, gateways, destination string
		want                        [][2]string
		wantError                   bool
	}{
		{"cross namespace", `["ingress/gateway"]`, `{"host":"api.app.svc.cluster.local","port":{"number":80}}`, [][2]string{{"ingress", "gateway"}}, false},
		{"same namespace", `["gateway"]`, `{"host":"api","port":{"number":8080}}`, [][2]string{{"app", "gateway"}}, false},
		{"mesh only", `["mesh"]`, `{"host":"api","port":{"number":80}}`, nil, true},
		{"no gateway", `[]`, `{"host":"api","port":{"number":80}}`, nil, true},
		{"invalid reference", `["a/b/c"]`, `{"host":"api","port":{"number":80}}`, nil, true},
		{"missing backend", `["gateway"]`, `{}`, nil, true},
		{"invalid port", `["gateway"]`, `{"host":"api","port":{"number":65536}}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{"spec":{"hosts":["app.example.com"],"gateways":` + tc.gateways + `,"http":[{"route":[{"destination":` + tc.destination + `}]}]}}`)
			got, err := virtualServiceGateways(data, "app")
			if (err != nil) != tc.wantError || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("gateways=%v err=%v; want=%v wantError=%t", got, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestGatewayPodsReady(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		want       bool
		wantError  bool
	}{
		{"running ready", `{"items":[{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`, true, false},
		{"not ready", `{"items":[{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]}}]}`, false, false},
		{"terminating", `{"items":[{"metadata":{"deletionTimestamp":"2026-11-09T01:00:00Z"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`, false, false},
		{"no pods", `{"items":[]}`, false, false},
		{"invalid response", `{`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gatewayPodsReady([]byte(tc.json))
			if got != tc.want || (err != nil) != tc.wantError {
				t.Fatalf("ready=%t err=%v", got, err)
			}
		})
	}
}
