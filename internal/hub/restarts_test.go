package hub

import (
	"context"
	"testing"
	"time"

	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
)

func ingestContainers(t *testing.T, h *Hub, id string, at time.Time, containers ...protocol.Container) {
	t.Helper()
	s := protocol.Sample{Version: 1, HostID: "node", ID: id, At: at, Containers: containers, Metrics: []protocol.Metric{
		{Name: "cpu_percent", Value: 10, Weight: 1},
		{Name: "memory_used_bytes", Value: 20, Weight: 1}, {Name: "memory_total_bytes", Value: 100, Weight: 1},
		{Name: "filesystem_total_bytes", Entity: "/", Value: 100, Weight: 1}, {Name: "filesystem_free_bytes", Entity: "/", Value: 80, Weight: 1},
	}}
	if err := h.store.Ingest(context.Background(), s, at, 300); err != nil {
		t.Fatal(err)
	}
}

func restartRow(t *testing.T, h *Hub) fleetHost {
	t.Helper()
	host, err := h.store.Host(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	return h.fleetRow(host)
}

func TestRestartLoopUsesRestartsWithinWindow(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	ingestContainers(t, h, "old", now.Add(-20*time.Minute), protocol.Container{ID: "w1", Name: "worker", State: "running", Restarts: 0})
	ingestContainers(t, h, "start", now.Add(-9*time.Minute), protocol.Container{ID: "w1", Name: "worker", State: "running", Restarts: 1}, protocol.Container{ID: "a1", Name: "app", State: "running", Restarts: 7})
	ingestContainers(t, h, "now", now.Add(-time.Second), protocol.Container{ID: "w1", Name: "worker", State: "running", Restarts: 4}, protocol.Container{ID: "a1", Name: "app", State: "running", Restarts: 7}, protocol.Container{ID: "n1", Name: "fresh", State: "running", Restarts: 3})

	row := restartRow(t, h)
	if got := signal(row, "restarts/worker"); got == nil || got.State != "warning" || got.Detail != "worker restarted 3× in 10m" {
		t.Fatalf("looping container: %+v", got)
	}
	if got := signal(row, "restarts/app"); got == nil || got.State != "healthy" {
		t.Fatalf("stable container with old restarts: %+v", got)
	}
	// A container created inside the window counts from zero.
	if got := signal(row, "restarts/fresh"); got == nil || got.State != "warning" {
		t.Fatalf("new looping container: %+v", got)
	}
}

func TestRestartLoopOverridesAndDisable(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	ingestContainers(t, h, "start", now.Add(-9*time.Minute), protocol.Container{ID: "w1", Name: "worker", State: "running", Restarts: 0}, protocol.Container{ID: "c1", Name: "cron", State: "running", Restarts: 0})
	ingestContainers(t, h, "now", now.Add(-time.Second), protocol.Container{ID: "w1", Name: "worker", State: "running", Restarts: 5}, protocol.Container{ID: "c1", Name: "cron", State: "running", Restarts: 5})
	h.config.AlertPolicy.ContainerOverrides = []config.ContainerOverride{
		{Host: "node", Name: "worker", RestartLoop: &config.RestartLoop{Restarts: 0}},
		{Host: "node", Name: "cron", RestartLoop: &config.RestartLoop{Restarts: 2, Severity: "critical"}},
	}
	row := restartRow(t, h)
	if got := signal(row, "restarts/worker"); got != nil {
		t.Fatalf("disabled restart loop reported: %+v", got)
	}
	if got := signal(row, "restarts/cron"); got == nil || got.State != "critical" || got.Detail != "cron restarted 5× in 10m" {
		t.Fatalf("override: %+v", got)
	}
	h.config.AlertPolicy.ContainerOverrides = nil
	h.config.AlertPolicy.Containers.RestartLoop.Restarts = 0
	if got := signal(restartRow(t, h), "restarts/cron"); got != nil {
		t.Fatalf("globally disabled restart loop reported: %+v", got)
	}
}

func TestRestartLoopNeedsFreshState(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	ingestContainers(t, h, "start", now.Add(-50*time.Minute), protocol.Container{ID: "w1", Name: "worker", State: "running", Restarts: 0})
	ingestContainers(t, h, "now", now.Add(-45*time.Minute), protocol.Container{ID: "w1", Name: "worker", State: "running", Restarts: 9})
	if got := signal(restartRow(t, h), "restarts/worker"); got != nil {
		t.Fatalf("stale state reported restarts: %+v", got)
	}
}
