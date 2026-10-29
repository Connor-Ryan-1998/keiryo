package hub

import (
	"context"
	"encoding/json"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFleetThresholdsAndMissingData(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	for _, tc := range []struct {
		cpu  float64
		want string
	}{{0, "healthy"}, {79.9, "healthy"}, {80, "warning"}, {90, "critical"}} {
		host := store.Host{ID: "node", Seen: now, Sample: protocol.Sample{At: now, Metrics: []protocol.Metric{
			{Name: "cpu_percent", Value: tc.cpu}, {Name: "memory_used_bytes", Value: 20}, {Name: "memory_total_bytes", Value: 100},
			{Name: "filesystem_total_bytes", Entity: "/", Value: 100}, {Name: "filesystem_free_bytes", Entity: "/", Value: 80},
		}}}
		row := h.fleetRow(host)
		if row.Health != tc.want {
			t.Fatalf("cpu %v: %s", tc.cpu, row.Health)
		}
		host.Sample.Metrics = append(host.Sample.Metrics, protocol.Metric{Name: "filesystem_total_bytes", Entity: "/data", Value: 100}, protocol.Metric{Name: "filesystem_free_bytes", Entity: "/data", Value: 5})
		if h.fleetRow(host).Health != "critical" {
			t.Fatal("worst mount ignored")
		}
		host.Sample.At = now.Add(-time.Hour)
		if h.fleetRow(host).Health != "unavailable" {
			t.Fatal("old measurement presented as current health")
		}
	}
	if row := h.fleetRow(store.Host{ID: "empty"}); row.Health != "unavailable" || row.CPU != nil {
		t.Fatalf("missing becomes zero/healthy: %+v", row)
	}
	partial := store.Host{ID: "partial", Seen: now, Sample: protocol.Sample{At: now, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 95}}}}
	if row := h.fleetRow(partial); row.Health != "critical" {
		t.Fatalf("missing metric masks known critical reading: %+v", row)
	}
	partial.Seen = now.Add(-time.Hour)
	if row := h.fleetRow(partial); row.Health != "unavailable" {
		t.Fatal("offline host reported current thresholds")
	}
}

func TestFleetDiskHealthMountpoints(t *testing.T) {
	h := testHub(t, "proxy")
	defaults := h.config
	now := time.Now()
	for _, tc := range []struct {
		name, config, health string
		rootFree             float64
		omitRoot             bool
		disk                 float64
	}{
		{"default all mounts", `{}`, "critical", 80, false, 95},
		{"empty all mounts", `{"disk_health_mountpoints":[]}`, "critical", 80, false, 95},
		{"root only", `{"disk_health_mountpoints":["/"]}`, "healthy", 80, false, 20},
		{"root warning", `{"disk_health_mountpoints":["/"]}`, "warning", 20, false, 80},
		{"root critical", `{"disk_health_mountpoints":["/"]}`, "critical", 5, false, 95},
		{"multiple mounts", `{"disk_health_mountpoints":["/","/data"]}`, "critical", 80, false, 95},
		{"exact match", `{"disk_health_mountpoints":["/dat"]}`, "unavailable", 80, false, -1},
		{"missing root", `{"disk_health_mountpoints":["/"]}`, "unavailable", 80, true, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.config = defaults
			if err := json.Unmarshal([]byte(tc.config), &h.config); err != nil {
				t.Fatal(err)
			}
			host := store.Host{ID: "node", Seen: now, Sample: protocol.Sample{At: now, Metrics: []protocol.Metric{
				{Name: "cpu_percent", Value: 20},
				{Name: "memory_used_bytes", Value: 20}, {Name: "memory_total_bytes", Value: 100},
				{Name: "filesystem_total_bytes", Entity: "/data", Value: 100},
				{Name: "filesystem_free_bytes", Entity: "/data", Value: 5},
			}}}
			if !tc.omitRoot {
				host.Sample.Metrics = append(host.Sample.Metrics,
					protocol.Metric{Name: "filesystem_total_bytes", Entity: "/", Value: 100},
					protocol.Metric{Name: "filesystem_free_bytes", Entity: "/", Value: tc.rootFree})
			}
			row := h.fleetRow(host)
			if row.Health != tc.health || (tc.disk < 0 && row.Disk != nil) || (tc.disk >= 0 && (row.Disk == nil || *row.Disk != tc.disk)) {
				t.Fatalf("health=%s disk=%v; want health=%s disk=%v", row.Health, row.Disk, tc.health, tc.disk)
			}
		})
	}
}

func TestFleetStorageFilterPreservesHostCharts(t *testing.T) {
	h := testHub(t, "proxy")
	if err := json.Unmarshal([]byte(`{"disk_health_mountpoints":["/"]}`), &h.config); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Second)
	s := protocol.Sample{Version: protocol.Version, HostID: "node", ID: "mounts", At: now, Metrics: []protocol.Metric{
		{Name: "filesystem_total_bytes", Entity: "/", Value: 100, Weight: 1},
		{Name: "filesystem_free_bytes", Entity: "/", Value: 80, Weight: 1},
		{Name: "filesystem_total_bytes", Entity: "/data", Value: 100, Weight: 1},
		{Name: "filesystem_free_bytes", Entity: "/data", Value: 5, Weight: 1},
	}}
	if err := h.store.Ingest(context.Background(), s, now, 300); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/", "/hosts/node"} {
		r := httptest.NewRequest("GET", url, nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-authentik-username", "test")
		w := httptest.NewRecorder()
		h.UIHandler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", url, w.Code, w.Body.String())
		}
		if got := strings.Contains(w.Body.String(), "/data"); got != (url != "/") {
			t.Fatalf("%s: unexpected /data visibility: %v", url, got)
		}
	}
}

func TestDuplicateDisplayNamesStaySeparateInFleetCharts(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now().Add(-time.Second)
	for _, id := range []string{"node", "other"} {
		s := protocol.Sample{Version: 1, HostID: id, ID: "one", At: now, DisplayName: "server", Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 25, Weight: 1}}}
		if err := h.store.Ingest(context.Background(), s, now, 300); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest("GET", "/?view=resources", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-authentik-username", "user")
	w := httptest.NewRecorder()
	h.UIHandler().ServeHTTP(w, r)
	match := regexp.MustCompile(`data-kind="heatmap" data-series="([^"]*)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("heatmap missing")
	}
	var cells []struct{ Host string }
	if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &cells); err != nil {
		t.Fatal(err)
	}
	if len(cells) != 2 || cells[0].Host == cells[1].Host {
		t.Fatalf("distinct hosts collapsed: %+v", cells)
	}
}

func TestFleetViewsFiltersRangesAndEscaping(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now().Add(-time.Second)
	s := protocol.Sample{Version: 1, HostID: "node", ID: "fleet", At: now, DisplayName: `<img src=x onerror=alert(1)>`, Containers: []protocol.Container{{ID: "c", Name: "database", State: "exited"}}, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 95, Weight: 1}}}
	if err := h.store.Ingest(context.Background(), s, now, 300); err != nil {
		t.Fatal(err)
	}
	for _, window := range []string{"1h", "24h", "7d", "30d"} {
		for _, view := range []string{"", "overview", "resources", "metrics", "conditions"} {
			r := httptest.NewRequest("GET", "/?view="+view+"&range="+window+"&q=node", nil)
			r.RemoteAddr = "10.0.0.1:1234"
			r.Header.Set("X-authentik-username", "test")
			w := httptest.NewRecorder()
			h.UIHandler().ServeHTTP(w, r)
			for _, want := range []string{"Fleet Overview", "Resource Views", "Fleet Metrics", "Alerts &amp; Hosts"} {
				if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
					t.Fatalf("%s missing %s: %d %s", window, want, w.Code, w.Body.String())
				}
			}
			selected := view
			if selected == "" {
				selected = "overview"
			}
			for _, section := range []string{"overview", "resources", "metrics", "conditions"} {
				if got := strings.Contains(w.Body.String(), `<section id="`+section+`"`); got != (section == selected) {
					t.Fatalf("view %q: section %s present=%v", view, section, got)
				}
			}
			content := map[string]string{"overview": "critical", "resources": `data-kind="heatmap"`, "metrics": "Filesystems", "conditions": "database"}
			if !strings.Contains(w.Body.String(), content[selected]) {
				t.Fatalf("view %s missing content %s", selected, content[selected])
			}
			if !strings.Contains(w.Body.String(), `<input type="hidden" name="view" value="`+selected+`">`) {
				t.Fatalf("view %s lost on Apply", selected)
			}
			if strings.Contains(w.Body.String(), "<img src=x") {
				t.Fatal("unescaped fleet name")
			}
		}
	}
	for _, query := range []string{"range=invalid", "view=invalid"} {
		r := httptest.NewRequest("GET", "/?"+query, nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-authentik-username", "test")
		w := httptest.NewRecorder()
		h.UIHandler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid query accepted: %s", query)
		}
	}
}

func TestFleetNavigationPreservesSelection(t *testing.T) {
	h := testHub(t, "proxy")
	r := httptest.NewRequest("GET", "/?view=metrics&range=7d&offset=100&q=node+%26+other", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-authentik-username", "test")
	w := httptest.NewRecorder()
	h.UIHandler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	links := regexp.MustCompile(`<a href="([^"]*)"([^>]*)>(Fleet Overview|Resource Views|Fleet Metrics|Alerts &amp; Hosts|Previous)</a>`).FindAllStringSubmatch(w.Body.String(), -1)
	if len(links) != 5 {
		t.Fatalf("expected four views and previous page, got %d", len(links))
	}
	for i, link := range links {
		u, err := url.Parse(html.UnescapeString(link[1]))
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		wantView := []string{"overview", "resources", "metrics", "conditions", "metrics"}[i]
		wantOffset := "100"
		if i == 4 {
			wantOffset = "0"
		}
		if q.Get("view") != wantView || q.Get("range") != "7d" || q.Get("q") != "node & other" || q.Get("offset") != wantOffset {
			t.Fatalf("navigation lost selection: %s", u)
		}
		if strings.Contains(link[2], `aria-current="page"`) != (i == 2) {
			t.Fatalf("incorrect active view: %s", link[0])
		}
	}
}
