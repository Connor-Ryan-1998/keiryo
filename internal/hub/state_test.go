package hub

import (
	"context"
	"encoding/json"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPrivateStatePreservesMetricsAndCapabilities(t *testing.T) {
	h := testHub(t, "local")
	body, _ := json.Marshal(protocol.AgentState{Version: 1, HostID: "node", ID: "state", At: time.Now().UTC(), LogFetch: true, Containers: []protocol.Container{{ID: "abc", Name: "job", State: "exited", ExitCode: new(int)}}})
	r := httptest.NewRequest("POST", "/v1/state", strings.NewReader(string(body)))
	r.Header.Set("X-Host-ID", "node")
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	w := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	host, _ := h.store.Host(context.Background(), "node")
	if !host.Sample.At.IsZero() || !host.Sample.LogFetch || !h.canFetchLogs(host) {
		t.Fatal(host)
	}
	if err := h.store.IngestState(context.Background(), protocol.AgentState{Version: 1, HostID: "node", ID: "older", At: time.Now().Add(-time.Hour), Containers: []protocol.Container{{ID: "old", State: "running"}}}, time.Now(), 300); err != nil {
		t.Fatal(err)
	}
	host, _ = h.store.Host(context.Background(), "node")
	if host.Sample.Containers[0].ID != "abc" || host.Sample.Containers[0].ExitCode == nil {
		t.Fatal(host)
	}
}
func TestFreshCPUOrStateDoesNotFreshenOldMemory(t *testing.T) {
	h := testHub(t, "local")
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	host := store.Host{ID: "node", Seen: now, Sample: protocol.Sample{Incremental: true, At: now, StateAt: now, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 20, Weight: 1, ObservedAt: now}, {Name: "memory_used_bytes", Value: 90, Weight: 1, ObservedAt: old}, {Name: "memory_total_bytes", Value: 100, Weight: 1, ObservedAt: old}}}}
	row := h.fleetRow(host)
	if row.CPU == nil || row.Memory != nil || row.Health != "unavailable" {
		t.Fatal(row)
	}
	host.Sample.Metrics[1].ObservedAt = now
	row = h.fleetRow(host)
	if row.Memory != nil {
		t.Fatal("paired unmatched timestamps", row.Memory)
	}
}
func TestExternalContainerCannotFetchLogsAndIsUnknown(t *testing.T) {
	h := testHub(t, "local")
	now := time.Now()
	s := protocol.Sample{HostID: "node", At: now, ID: "external", Incremental: true, Containers: []protocol.Container{{ID: "abc", Name: "web", State: "unknown"}}}
	if err := h.store.Ingest(context.Background(), s, now, 300); err != nil {
		t.Fatal(err)
	}
	host, _ := h.store.Host(context.Background(), "node")
	if h.canFetchLogs(host) {
		t.Fatal("external container advertised private capability")
	}
	if got := h.containerState("node", s.Containers[0]); got != "unavailable" {
		t.Fatal(got)
	}
	r := httptest.NewRequest("POST", "/hosts/node/logs/abc", nil)
	r.SetPathValue("id", "node")
	r.SetPathValue("container", "abc")
	w := httptest.NewRecorder()
	h.showLogs(w, r)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
}
