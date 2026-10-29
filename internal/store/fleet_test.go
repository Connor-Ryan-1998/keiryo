package store

import (
	"context"
	"houselab/monitoring/internal/protocol"
	"path/filepath"
	"testing"
	"time"
)

func TestFleetCPUUsesWeightedBucketsAndPageBounds(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	start := time.Now().Truncate(time.Hour)
	for _, host := range []string{"a", "b"} {
		for i, v := range []float64{10, 30} {
			sample := protocol.Sample{HostID: host, ID: []string{"one", "two"}[i], At: start.Add(time.Duration(i) * time.Minute), Metrics: []protocol.Metric{{Name: "cpu_percent", Value: v, Weight: float64(i*2 + 1)}, {Name: "load1", Value: 999, Weight: 1}}}
			if err := s.Ingest(ctx, sample, start, 300); err != nil {
				t.Fatal(err)
			}
		}
	}
	points, err := s.FleetCPU(ctx, start, start.Add(time.Hour), 300, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Host != "b" || points[0].Average != 25 || points[0].Maximum != 30 {
		t.Fatalf("wrong fleet history %+v", points)
	}
	if _, err := s.FleetCPU(ctx, start, start.Add(time.Hour), 0, 0, 100); err == nil {
		t.Fatal("zero bucket accepted")
	}
}

func TestHistoryIncludesJustCommittedCurrentSecond(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "current.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	at := time.Now().Truncate(time.Second)
	sample := protocol.Sample{HostID: "node", ID: "new", At: at, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 40, Weight: 1}}}
	if err := s.Ingest(ctx, sample, at, 300); err != nil {
		t.Fatal(err)
	}
	end := at.Add(500 * time.Millisecond)
	points, err := s.History(ctx, "node", at.Add(-time.Hour), end, false, 300)
	if err != nil || len(points) != 1 {
		t.Fatalf("current-second sample omitted: %v %+v", err, points)
	}
	states, err := s.States(ctx, "node", at.Add(-time.Hour), end, false)
	if err != nil || len(states) != 1 {
		t.Fatalf("current-second state omitted: %v %+v", err, states)
	}
	points, err = s.History(ctx, "node", at.Add(-time.Hour), at, false, 300)
	if err != nil || len(points) != 0 {
		t.Fatal("exact exclusive end boundary changed")
	}
}
