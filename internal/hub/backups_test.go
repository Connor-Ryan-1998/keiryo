package hub

import (
	"context"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExpectedBackupJobsFailClosedAndShareDashboardConditions(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.BackupJobs = []config.BackupJob{{Host: "node", Job: "daily", MaxAgeSeconds: 108000}}
	now := time.Now()
	for _, tc := range []struct {
		name                string
		valid, result, last float64
		observed            time.Time
		want                string
	}{
		{"success", 1, 1, float64(now.Unix() - 3600), now, "healthy"},
		{"failed", 1, 2, float64(now.Unix() - 3600), now, "critical"},
		{"missing", 0, 0, 0, now, "unavailable"},
		{"overdue", 1, 1, float64(now.Unix() - 108001), now, "critical"},
		{"never succeeded", 1, 0, 0, now, "critical"},
		{"running within deadline", 1, 0, float64(now.Unix() - 3600), now, "healthy"},
		{"stale despite fresh host", 1, 1, float64(now.Unix() - 3600), now.Add(-time.Hour), "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := notificationHost(now)
			host.Sample.Incremental = true
			for _, m := range []protocol.Metric{{Name: "backup_status_valid", Value: tc.valid}, {Name: "backup_result", Value: tc.result}, {Name: "backup_last_success", Value: tc.last}} {
				m.Entity, m.ObservedAt = "daily", tc.observed
				host.Sample.Metrics = append(host.Sample.Metrics, m)
			}
			found := false
			for _, signal := range h.fleetRow(host).Signals {
				if signal.Key == "backup/daily" {
					found = true
					if signal.State != tc.want {
						t.Fatalf("%+v want %s", signal, tc.want)
					}
				}
			}
			if !found {
				t.Fatal("expected backup missing from shared signals")
			}
		})
	}
}

func TestHealthyBackupRemainsVisibleOnAlertsPage(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.BackupJobs = []config.BackupJob{{Host: "node", Job: "daily", MaxAgeSeconds: 108000}}
	now := time.Now()
	host := notificationHost(now)
	host.Sample.HostID, host.Sample.ID = "node", "backup-ui"
	for i := range host.Sample.Metrics {
		host.Sample.Metrics[i].Weight = 1
	}
	host.Sample.Metrics = append(host.Sample.Metrics,
		protocol.Metric{Name: "backup_status_valid", Entity: "daily", Value: 1, Weight: 1},
		protocol.Metric{Name: "backup_result", Entity: "daily", Value: 1, Weight: 1},
		protocol.Metric{Name: "backup_last_success", Entity: "daily", Value: float64(now.Unix() - 60), Weight: 1})
	if err := h.store.Ingest(context.Background(), host.Sample, now, 300); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/?view=conditions", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-authentik-username", "user")
	w := httptest.NewRecorder()
	h.UIHandler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "daily: last successful backup") {
		t.Fatal("healthy backup not visible", w.Code)
	}
}
