package store

import (
	"context"
	"houselab/monitoring/internal/otlp"
	"testing"
	"time"
)

func TestBackupOTLPSplitReplayAndInvalidStatusDoNotRefreshPreviousSuccess(t *testing.T) {
	s, _ := otlpStore(t)
	now := time.Now().Truncate(time.Second)
	values := []struct {
		name  string
		value float64
	}{{"status_valid", 1}, {"result", 1}, {"started", float64(now.Unix() - 20)}, {"finished", float64(now.Unix() - 10)}, {"last_success", float64(now.Unix() - 10)}}
	var points []otlp.Point
	for _, value := range values {
		points = append(points, otlp.Point{Name: "keiryo.backup." + value.name, Entity: "daily", Series: value.name, Group: "backup", At: now, Value: value.value})
	}
	for _, p := range points {
		ingestPoints(t, s, p)
	}
	if got := reading(t, s, "backup_status_valid").Value; got != 1 {
		t.Fatal(got)
	}
	if got := reading(t, s, "backup_last_success").Value; got != float64(now.Unix()-10) {
		t.Fatal(got)
	}
	ingestPoints(t, s, points...)
	invalid := points[0]
	invalid.At = now.Add(time.Minute)
	invalid.Value = 0
	ingestPoints(t, s, invalid)
	if got := reading(t, s, "backup_status_valid").Value; got != 0 {
		t.Fatal(got)
	}
	h, err := s.Host(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range h.Sample.Metrics {
		if m.Name == "backup_last_success" && !m.ObservedAt.Equal(now) {
			t.Fatal("invalid snapshot refreshed old success", m)
		}
	}
}
