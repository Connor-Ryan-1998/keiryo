package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A busy fleet writes more than one chunk of rows per minute; one pass must catch up.
func TestMaintainDrainsBacklogLargerThanOneChunk(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	if _, err = s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<2500)
 INSERT INTO samples(host,id,at,data) SELECT 'node','s'||i,?,'{}' FROM n`, old.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO points(host,sample,at,name,entity,value,weight) SELECT host,id,at,'cpu_percent','',1,1 FROM samples`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<25000)
 INSERT INTO otlp_observations(host,series,at,groupid,data) SELECT 'node','series'||i,?,'g','{}' FROM n`, old.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err = s.Maintain(ctx, now, 24*time.Hour, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"samples", "points", "otlp_observations"} {
		var n int
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s retained %d expired rows after one maintenance pass", table, n)
		}
	}
}

func TestExtremaLookupUsesSeriesIndex(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT MAX(value) FROM points WHERE host=? AND name=? AND entity=? AND at>=? AND at<?`, "n", "cpu_percent", "", 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if detail := strings.Join(plan, "\n"); !strings.Contains(detail, "point_series (host=? AND name=? AND entity=? AND at>? AND at<?)") {
		t.Fatalf("extrema lookup must seek by series and time: %s", detail)
	}
}
