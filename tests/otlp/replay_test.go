package otlp_test

import (
	"context"
	_ "embed"
	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/store"
	"path/filepath"
	"testing"
	"time"
)

//go:embed testdata/stock.json
var stockFixture []byte

func TestStockCollectorFixtureReadingsAndRebatch(t *testing.T) {
	req, err := otlp.Decode(stockFixture, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000020, 0)
	points, n, err := otlp.Normalize(req, "node", now, time.Hour, time.Minute, 100)
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.IngestOTLP(context.Background(), "node", points, now, 300)
	if err != nil || result.Rejected != 0 {
		t.Fatal(result, err)
	}
	host, err := db.Host(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, m := range host.Sample.Metrics {
		values[m.Name] = m.Value
	}
	for name, want := range map[string]float64{"cpu_percent": 25, "memory_total_bytes": 1024, "memory_available_bytes": 256, "memory_used_bytes": 768, "filesystem_total_bytes": 1000, "filesystem_free_bytes": 200, "disk_read_bytes_s": 20, "network_rx_bytes_s": 20, "container_cpu_percent": 150, "container_memory_bytes": 300, "container_memory_limit_bytes": 1000} {
		if values[name] != want {
			t.Fatalf("%s=%v want %v", name, values[name], want)
		}
	}
	if host.Sample.LogFetch || len(host.Sample.Containers) != 1 || host.Sample.Containers[0].State != "unknown" {
		t.Fatal(host)
	}
	for i := len(points) - 1; i >= 0; i-- {
		if _, err = db.IngestOTLP(context.Background(), "node", points[i:i+1], now, 300); err != nil {
			t.Fatal(err)
		}
	}
	history, err := db.History(context.Background(), "node", now.Add(-time.Hour), now, true, 300)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range history {
		if p.Count != 1 {
			t.Fatal("regrouped retry inflated summary", p)
		}
	}
}
