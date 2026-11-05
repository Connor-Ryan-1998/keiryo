package otlp

import (
	"testing"
	"time"
)

func TestBackupGaugesNormalizeAndProjectWithJobIdentity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	var points []Point
	for _, metric := range []struct {
		name, unit string
		value      float64
	}{
		{"status_valid", "1", 1}, {"result", "1", 1}, {"started", "s", float64(now.Unix() - 20)},
		{"finished", "s", float64(now.Unix() - 10)}, {"last_success", "s", float64(now.Unix() - 10)},
	} {
		req := export("keiryo.backup."+metric.name, metric.unit, metric.value, attr("backup.job", "daily"))
		req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0].TimeUnixNano = uint64(now.UnixNano())
		p, rejected, err := normalize(t, req)
		if err != nil || rejected != 0 || len(p) != 1 {
			t.Fatalf("%s: %v rejected=%d err=%v", metric.name, p, rejected, err)
		}
		points = append(points, p...)
	}
	measurements := Project(points)
	if len(measurements) != 5 {
		t.Fatalf("projected %v", measurements)
	}
	for _, m := range measurements {
		if m.Metric.Entity != "daily" {
			t.Fatalf("job identity lost: %v", m)
		}
	}
	if got := Project(points[:1]); len(got) != 1 || got[0].Metric.Name != "backup_status_valid" || got[0].Metric.Value != 0 {
		t.Fatal("partial success must not project healthy backup status", got)
	}
	for _, value := range []float64{3, 1.5, float64(now.Unix() + 1000)} {
		name, unit := "keiryo.backup.result", "1"
		if value > 3 {
			name, unit = "keiryo.backup.last_success", "s"
		}
		if p, rejected, err := normalize(t, export(name, unit, value, attr("backup.job", "daily"))); err != nil || rejected != 1 || len(p) != 0 {
			t.Fatal("invalid backup metric accepted", p, rejected, err)
		}
	}
}
