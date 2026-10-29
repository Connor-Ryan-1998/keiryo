package store

import (
	"context"
	"database/sql"
	"fmt"
	"houselab/monitoring/internal/protocol"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngestDedupRollupAndRetention(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for i, v := range []float64{20, 80} {
		x := protocol.Sample{Version: 1, HostID: "node", ID: []string{"a", "b"}[i], At: now.Add(time.Duration(i) * time.Minute), Metrics: []protocol.Metric{{Name: "cpu_percent", Value: v, Weight: 1}}}
		if err = s.Ingest(ctx, x, now, 300); err != nil {
			t.Fatal(err)
		}
		if err = s.Ingest(ctx, x, now, 300); err != nil {
			t.Fatal(err)
		}
	}
	points, err := s.History(ctx, "node", now, now.Add(time.Hour), true, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Average != 50 || points[0].Maximum != 80 || points[0].Count != 2 {
		t.Fatalf("bad rollup: %+v", points)
	}
	if err = s.Maintain(ctx, now.Add(25*time.Hour), 24*time.Hour, 30*24*time.Hour, 1<<30); err != nil {
		t.Fatal(err)
	}
	raw, _ := s.History(ctx, "node", now, now.Add(time.Hour), false, 300)
	if len(raw) != 0 {
		t.Fatal("raw data not pruned")
	}
	points, _ = s.History(ctx, "node", now, now.Add(time.Hour), true, 300)
	if len(points) != 1 {
		t.Fatal("summary lost with raw")
	}
	hosts, err := s.Hosts(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts %v %v", hosts, err)
	}
	if err = s.Maintain(ctx, now.Add(31*24*time.Hour), 24*time.Hour, 30*24*time.Hour, 1<<30); err != nil {
		t.Fatal(err)
	}
	points, _ = s.History(ctx, "node", now, now.Add(time.Hour), true, 300)
	if len(points) != 0 {
		t.Fatal("summary not pruned")
	}
}

func TestMigratesVersionOneAndPreservesMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(1); CREATE TABLE samples(host TEXT NOT NULL,id TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(host,id)); INSERT INTO samples VALUES('n','old',1);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count int
	s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version)
	s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE id='old'`).Scan(&count)
	if version != 3 || count != 1 {
		t.Fatal("migration lost data or version")
	}
	if err = s.Ingest(context.Background(), protocol.Sample{HostID: "n", ID: "new", At: time.Now()}, time.Now(), 300); err != nil {
		t.Fatal(err)
	}
}

func TestStoreReopenAndWeightedRates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(5 * time.Minute)
	for i, m := range []protocol.Metric{{Name: "network_rx_bytes_s", Entity: "eth0", Value: 10, Weight: 60}, {Name: "network_rx_bytes_s", Entity: "eth0", Value: 30, Weight: 120}} {
		if err = s.Ingest(ctx, protocol.Sample{HostID: "n", ID: []string{"1", "2"}[i], At: now, Metrics: []protocol.Metric{m}}, now, 300); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.History(ctx, "n", now, now.Add(time.Minute), true, 300)
	if err != nil || len(p) != 1 || p[0].Average < 23.33 || p[0].Average > 23.34 {
		t.Fatalf("weighted rate %+v %v", p, err)
	}
}

func BenchmarkIngest(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		x := protocol.Sample{HostID: "n", ID: time.Unix(int64(i), 0).String(), At: now, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 10, Weight: 1}}}
		if err = s.Ingest(context.Background(), x, now, 300); err != nil {
			b.Fatal(err)
		}
	}
}

func TestFailedIngestRollsBack(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	s.db.Exec(`CREATE TRIGGER reject_points BEFORE INSERT ON points BEGIN SELECT RAISE(ABORT,'simulated storage failure'); END`)
	err = s.Ingest(ctx, protocol.Sample{HostID: "n", ID: "one", At: time.Now(), Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 1, Weight: 1}}}, time.Now(), 300)
	if err == nil {
		t.Fatal("failed write acknowledged")
	}
	hosts, _ := s.Hosts(ctx)
	if len(hosts) != 0 {
		t.Fatal("partially committed host")
	}
}

func TestRecentDuplicateUpdatesContactWithoutDuplicating(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Second)
	x := protocol.Sample{HostID: "n", ID: "one", At: now}
	s.Ingest(context.Background(), x, now, 300)
	s.Ingest(context.Background(), x, now.Add(time.Minute), 300)
	hosts, _ := s.Hosts(context.Background())
	if !hosts[0].Seen.Equal(now.Add(time.Minute)) {
		t.Fatal("accepted retry did not update contact freshness")
	}
}

func TestSessionStorageBounded(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 140; i++ {
		if err = s.PutSession(context.Background(), fmt.Sprint(i), "csrf", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count)
	if count > 128 {
		t.Fatalf("unbounded sessions: %d", count)
	}
}

func TestSummaryBucketConfigurationCannotSilentlyMix(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Configure(context.Background(), 300); err != nil {
		t.Fatal(err)
	}
	if err = s.Configure(context.Background(), 600); err == nil {
		t.Fatal("mixed incompatible summary buckets")
	}
}

func TestHostsPaginationIncludesNeverSeen(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err = s.Register(ctx, []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.HostsPage(ctx, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != "b" || !page[0].Seen.IsZero() {
		t.Fatalf("page %+v %v", page, err)
	}
}

func TestRetainsCategoricalHistoryAndIndexesCascade(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().Truncate(5 * time.Minute)
	for i, state := range []string{"exited", "running"} {
		x := protocol.Sample{HostID: "n", ID: fmt.Sprint(i), At: now.Add(time.Duration(i) * time.Minute), Containers: []protocol.Container{{ID: "abc", State: state}}, Errors: []string{state}}
		if err = s.Ingest(ctx, x, now, 300); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := s.States(ctx, "n", now, now.Add(time.Hour), false)
	if err != nil || len(raw) != 2 || raw[0].Containers[0].State != "exited" {
		t.Fatalf("lost state history: %+v %v", raw, err)
	}
	summary, err := s.States(ctx, "n", now, now.Add(time.Hour), true)
	if err != nil || len(summary) != 1 || summary[0].Containers[0].State != "running" {
		t.Fatalf("bad categorical summary: %+v %v", summary, err)
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT rowid FROM points WHERE host='n' AND sample='0'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var a, b, c int
	var detail string
	for rows.Next() {
		rows.Scan(&a, &b, &c, &detail)
	}
	if !strings.Contains(detail, "points_sample") {
		t.Fatalf("unindexed retention lookup: %s", detail)
	}
}

func TestStorageBudgetReportsFailureToConverge(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	x := protocol.Sample{HostID: "n", ID: "one", At: now, DisplayName: strings.Repeat("x", 4096)}
	if err = s.Ingest(ctx, x, now, 300); err != nil {
		t.Fatal(err)
	}
	if err = s.Maintain(ctx, now, 24*time.Hour, 30*24*time.Hour, 1); err == nil {
		t.Fatal("budget pressure not surfaced")
	}
}

func TestBudgetPrunesMoreThanOneFixedBatch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6000; i++ {
		_, err = tx.Exec(`INSERT INTO summaries VALUES('n',?,'cpu_percent',?,1,1,1,1,1)`, now.Unix()-int64(i)*300, fmt.Sprint(i)+strings.Repeat("x", 500))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.Maintain(context.Background(), now, 24*time.Hour, 30*24*time.Hour, 1<<20); err != nil {
		t.Fatal(err)
	}
	live, err := s.liveBytes(context.Background())
	if err != nil || live > 1<<20 {
		t.Fatalf("budget did not converge: %d %v", live, err)
	}
}
