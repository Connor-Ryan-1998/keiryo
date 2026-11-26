package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestRetentionBacklogMeasuresOldestExpiredRow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if got, err := s.RetentionBacklog(ctx, now, 24*time.Hour); err != nil || got != 0 {
		t.Fatalf("empty backlog %v %v", got, err)
	}
	s.db.Exec(`INSERT INTO samples(host,id,at,data) VALUES('n','a',?,'{}')`, now.Add(-25*time.Hour).Unix())
	s.db.Exec(`INSERT INTO otlp_observations VALUES('n','s',?,'g','{}')`, now.Add(-27*time.Hour).UnixNano())
	if got, err := s.RetentionBacklog(ctx, now, 24*time.Hour); err != nil || got != 3*time.Hour {
		t.Fatalf("backlog %v %v", got, err)
	}
}

func TestRestartBaselineUsesEarliestSnapshotInWindow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for i, restarts := range []int{1, 4, 6} {
		at := now.Add(time.Duration(i*5-12) * time.Minute)
		if _, err = s.db.Exec(`INSERT INTO samples(host,id,at,data) VALUES('n',?,?,?)`, fmt.Sprint(i), at.Unix(), fmt.Sprintf(`{"at":"%s","containers":[{"id":"w1","name":"worker","state":"running","restarts":%d}],"errors":null}`, at.Format(time.RFC3339), restarts)); err != nil {
			t.Fatal(err)
		}
	}
	baseline, ok, err := s.RestartBaseline(ctx, "n", now.Add(-10*time.Minute))
	if err != nil || !ok || baseline["w1"] != 4 {
		t.Fatalf("baseline %v %v %v", baseline, ok, err)
	}
	if _, ok, err = s.RestartBaseline(ctx, "other", now.Add(-10*time.Minute)); err != nil || ok {
		t.Fatalf("unknown host baseline ok=%v err=%v", ok, err)
	}
}
