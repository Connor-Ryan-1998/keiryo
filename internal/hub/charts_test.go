package hub

import (
	"context"
	"encoding/json"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"html"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDashboardMetricsAndRendering(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	host := store.Host{ID: "node", Sample: protocol.Sample{
		At:         now,
		Containers: []protocol.Container{{ID: "container", Name: `<img src=x onerror=alert(1)>`, State: "running"}},
		Metrics: []protocol.Metric{
			{Name: "cpu_percent", Value: 25},
			{Name: "memory_used_bytes", Value: 30},
			{Name: "memory_total_bytes", Value: 100},
			{Name: "filesystem_free_bytes", Entity: "/", Value: 25},
			{Name: "filesystem_total_bytes", Entity: "/", Value: 100},
			{Name: "container_cpu_percent", Entity: "container", Value: 250},
			{Name: "container_memory_bytes", Entity: "container", Value: 2 << 20},
			{Name: "network_rx_bytes_s", Entity: "eth0", Value: 1024},
		},
	}}
	points := []store.Point{}
	for _, m := range host.Sample.Metrics {
		points = append(points, store.Point{At: now, Name: m.Name, Entity: m.Entity, Average: m.Value})
	}
	// Missing denominators, zero totals, and different timestamps must not produce percentages.
	points = append(points,
		store.Point{At: now.Add(-time.Minute), Name: "memory_used_bytes", Average: 50},
		store.Point{At: now, Name: "filesystem_free_bytes", Entity: "/missing", Average: 5},
		store.Point{At: now, Name: "filesystem_free_bytes", Entity: "/zero", Average: 0},
		store.Point{At: now, Name: "filesystem_total_bytes", Entity: "/zero", Average: 0},
	)
	charts := chartViews(points, host)
	for i, want := range map[int]float64{0: 25, 1: 30, 2: 75, 3: 1024, 4: 250, 5: 2} {
		var series []chartSeries
		if err := json.Unmarshal([]byte(charts[i].Data), &series); err != nil {
			t.Fatal(err)
		}
		if len(series) != 1 || len(series[0].Samples) != 1 || series[0].Samples[0].Value != want {
			t.Fatalf("panel %d: %s, want %v", i, charts[i].Data, want)
		}
	}
	h := testHub(t, "proxy")
	host.Seen = now
	stats := h.hostStats(host)
	want := []statView{{"CPU", "25.0%", "healthy"}, {"Memory", "30.0%", "healthy"}, {"Disk /", "75.0%", "healthy"}, {"Containers", "1 running", ""}}
	if !reflect.DeepEqual(stats, want) {
		t.Fatalf("incorrect overview: %+v", stats)
	}
	if h.hostStats(store.Host{})[0].Value != "Unavailable" {
		t.Fatal("missing reading rendered as zero")
	}
	if gap := chartGap([]store.Point{{At: now}, {At: now.Add(time.Minute)}, {At: now.Add(3 * time.Minute)}}, false, 300, 180); gap != 90000 {
		t.Fatalf("raw gap %d", gap)
	}
	if gap := chartGap(nil, true, 300, 180); gap != 450000 {
		t.Fatalf("summary gap %d", gap)
	}

	sample := host.Sample
	sample.HostID, sample.ID, sample.Version = "node", "chart-sample", protocol.Version
	for i := range sample.Metrics {
		sample.Metrics[i].Weight = 1
	}
	if err := h.store.Ingest(context.Background(), sample, now, 300); err != nil {
		t.Fatal(err)
	}
	ui := h.UIHandler()
	if w := request(ui, "GET", "/hosts/node", "", nil); w.Code != 403 {
		t.Fatal("dashboard auth bypass")
	}
	for _, window := range []string{"1h", "24h", "7d", "30d"} {
		r := httptest.NewRequest("GET", "/hosts/node?range="+window, nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-authentik-username", "tester")
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "Host CPU") || strings.Contains(body, "<img src=x") {
			t.Fatalf("dashboard rendering %s: %d %s", window, w.Code, body)
		}
		matches := regexp.MustCompile(`data-series="([^"]*)"`).FindAllStringSubmatch(body, -1)
		if len(matches) != 7 {
			t.Fatalf("expected seven panels, got %d", len(matches))
		}
		for _, match := range matches {
			var series []chartSeries
			if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &series); err != nil {
				t.Fatalf("invalid embedded chart data: %v", err)
			}
		}
	}
	if w := request(ui, "GET", "/static/dashboard.js", "", nil); w.Code != 200 || w.Header().Get("Content-Type") != "text/javascript" || len(w.Result().Cookies()) != 0 {
		t.Fatal("chart asset unavailable before login or changes cookies")
	}
}

func TestChartGapUsesPerSeriesCadence(t *testing.T) {
	at := time.Now().UTC()
	points := []store.Point{
		{Name: "cpu_percent", At: at},
		{Name: "memory_total_bytes", At: at.Add(time.Millisecond)},
		{Name: "cpu_percent", At: at.Add(time.Minute)},
		{Name: "memory_total_bytes", At: at.Add(time.Minute + time.Millisecond)},
	}
	if got := chartGap(points, false, 300, 180); got != 90000 {
		t.Fatalf("unrelated timestamps shrank gap: %d", got)
	}
	points = append(points, store.Point{Name: "network_rx_bytes_s", Entity: "eth0", At: at}, store.Point{Name: "network_rx_bytes_s", Entity: "eth0", At: at.Add(time.Second)})
	if got := chartGap(points, false, 300, 180); got != 90000 {
		t.Fatalf("faster series broke slower series: %d", got)
	}
}

func TestHostStatsWorstMount(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	host := store.Host{ID: "node", Seen: now, Sample: protocol.Sample{At: now, Containers: []protocol.Container{{ID: "a", Name: "a", State: "running"}, {ID: "b", Name: "b", State: "exited"}, {ID: "c", Name: "c", State: "exited"}}, Metrics: []protocol.Metric{
		{Name: "cpu_percent", Value: 20}, {Name: "memory_used_bytes", Value: 20}, {Name: "memory_total_bytes", Value: 100},
		{Name: "filesystem_total_bytes", Entity: "/", Value: 100}, {Name: "filesystem_free_bytes", Entity: "/", Value: 80},
		{Name: "filesystem_total_bytes", Entity: "/data", Value: 100}, {Name: "filesystem_free_bytes", Entity: "/data", Value: 5},
	}}}
	stats := h.hostStats(host)
	if stats[2] != (statView{"Disk /data", "95.0%", "critical"}) || stats[3].Value != "1 running · 2 other" {
		t.Fatalf("stats %+v", stats)
	}
	host.Sample.At, host.Seen = now.Add(-time.Hour), now.Add(-time.Hour)
	if s := h.hostStats(host); s[0].State != "" || s[2].State != "" {
		t.Fatalf("stale readings coloured as current: %+v", s)
	}
}

func TestProblemChartFirst(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		state bool
	}{{"fresh", time.Second, true}, {"stale", time.Hour, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHub(t, "proxy")
			at := time.Now().Add(-tc.age)
			s := protocol.Sample{Version: 1, HostID: "node", ID: "p", At: at, Metrics: []protocol.Metric{
				{Name: "cpu_percent", Value: 95, Weight: 1}, {Name: "memory_used_bytes", Value: 20, Weight: 1}, {Name: "memory_total_bytes", Value: 100, Weight: 1},
			}}
			if err := h.store.Ingest(context.Background(), s, at, 300); err != nil {
				t.Fatal(err)
			}
			body := serveUI(t, h, "/hosts/node")
			first := regexp.MustCompile(`<figure class="chart-panel"[^>]*>`).FindString(body)
			if got := strings.Contains(body, "data-state="); got != tc.state {
				t.Fatalf("data-state present=%v", got)
			}
			if tc.state && (!strings.Contains(first, `data-resource="cpu"`) || !strings.Contains(first, `data-state="critical"`) || !strings.Contains(first, `data-entity="CPU"`)) {
				t.Fatalf("problem chart not first: %s", first)
			}
		})
	}
}
