package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"houselab/monitoring/internal/protocol"
)

const gib = float64(1 << 30)

// ingestDiskGrowth stores hourly samples where "/" usage grows by perDay GiB/day.
func ingestDiskGrowth(t *testing.T, h *Hub, hours int, startUsed, perDay float64) {
	t.Helper()
	now := time.Now()
	for i := hours; i >= 0; i-- {
		at := now.Add(-time.Duration(i)*time.Hour - time.Second)
		used := startUsed + perDay*float64(hours-i)/24
		s := protocol.Sample{Version: 1, HostID: "node", ID: fmt.Sprintf("d%d", i), At: at, Metrics: []protocol.Metric{
			{Name: "cpu_percent", Value: 10, Weight: 1},
			{Name: "memory_used_bytes", Value: 20, Weight: 1}, {Name: "memory_total_bytes", Value: 100, Weight: 1},
			{Name: "filesystem_total_bytes", Entity: "/", Value: 100 * gib, Weight: 1}, {Name: "filesystem_free_bytes", Entity: "/", Value: (100 - used) * gib, Weight: 1},
		}}
		if err := h.store.Ingest(context.Background(), s, at, 300); err != nil {
			t.Fatal(err)
		}
	}
}

func forecastSignal(t *testing.T, h *Hub) *condition {
	t.Helper()
	host, err := h.store.Host(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	return signal(h.fleetRow(host), "forecast//")
}

func TestDiskForecastWarnsBeforeFull(t *testing.T) {
	h := testHub(t, "proxy")
	// 50 → 70 GiB over 48h: 10 GiB/day with 30 GiB free is about 3 days.
	ingestDiskGrowth(t, h, 48, 50, 10)
	got := forecastSignal(t, h)
	if got == nil || got.State != "warning" || got.Kind != "Disk forecast" || !strings.Contains(got.Detail, "/ full in about 3.0 days") || !strings.Contains(got.Detail, "10.0 GiB/day") {
		t.Fatalf("forecast: %+v", got)
	}
}

func TestDiskForecastHealthyWhenSlowOrShrinking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		perDay float64
		detail string
	}{{"slow", 1, "full in about"}, {"shrinking", -2, "not growing"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHub(t, "proxy")
			ingestDiskGrowth(t, h, 48, 50, tc.perDay)
			if got := forecastSignal(t, h); got == nil || got.State != "healthy" || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("forecast: %+v", got)
			}
		})
	}
}

func TestDiskForecastNeedsADayOfHistory(t *testing.T) {
	h := testHub(t, "proxy")
	ingestDiskGrowth(t, h, 12, 50, 40)
	if got := forecastSignal(t, h); got != nil {
		t.Fatalf("forecast from 12h of history: %+v", got)
	}
}

func TestDiskForecastRespectsConfig(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.AlertPolicy.Resources.Disk.Forecast.Days = 0
	ingestDiskGrowth(t, h, 48, 50, 10)
	if got := forecastSignal(t, h); got != nil {
		t.Fatalf("disabled forecast reported: %+v", got)
	}
	h2 := testHub(t, "proxy")
	h2.config.DiskHealthMountpoints = []string{"/data"}
	ingestDiskGrowth(t, h2, 48, 50, 10)
	if got := forecastSignal(t, h2); got != nil {
		t.Fatalf("forecast for unselected mount: %+v", got)
	}
}
