package hub

import (
	"encoding/json"
	"fmt"
	"houselab/monitoring/internal/store"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type condition struct{ Host, Name, Kind, State, Detail string }
type volumeView struct {
	Host, Name, Mount string
	Used, Total       float64
	Percent           float64
	State             string
}
type fleetHost struct {
	store.Host
	Status, Health             string
	CPU, Memory, Disk, Network *float64
	MemoryTotal                float64
	Fresh                      bool
	Uptime                     string
	Conditions                 []condition
}
type fleetChart struct{ Title, Kind, Unit, Data, Resource string }

func reading(v *float64) string {
	if v == nil {
		return "Unavailable"
	}
	return fmt.Sprintf("%.1f", *v)
}
func byteSize(v float64) string { return fmt.Sprintf("%.1f GiB", v/(1<<30)) }
func percent(used, total float64) *float64 {
	if total <= 0 || used < 0 || used > total {
		return nil
	}
	v := 100 * used / total
	return &v
}
func severity(s string) int {
	switch s {
	case "unavailable":
		return 4
	case "critical":
		return 3
	case "warning":
		return 2
	default:
		return 0
	}
}
func hostName(host store.Host) string {
	if host.Name != "" {
		return host.Name
	}
	return host.ID
}
func chartHostName(host store.Host) string {
	if host.Name != "" && host.Name != host.ID {
		return fmt.Sprintf("%s [%s]", host.Name, host.ID)
	}
	return host.ID
}
func metrics(host store.Host) map[string]float64 {
	m := map[string]float64{}
	times := map[string]time.Time{}
	for _, p := range host.Sample.Metrics {
		m[p.Name+"\x00"+p.Entity] = p.Value
		times[p.Name+"\x00"+p.Entity] = metricTime(host.Sample, p)
	}
	for _, p := range host.Sample.Metrics {
		paired := ""
		switch p.Name {
		case "memory_used_bytes":
			paired = "memory_total_bytes"
		case "filesystem_free_bytes":
			paired = "filesystem_total_bytes"
		}
		if paired != "" && !times[p.Name+"\x00"+p.Entity].Equal(times[paired+"\x00"+p.Entity]) {
			delete(m, p.Name+"\x00"+p.Entity)
		}
	}
	return m
}
func (h *Hub) volumes(host store.Host) []volumeView {
	host = h.currentReadings(host)
	m := metrics(host)
	out := []volumeView{}
	for _, p := range host.Sample.Metrics {
		if p.Name != "filesystem_total_bytes" {
			continue
		}
		if len(h.config.DiskHealthMountpoints) > 0 && !slices.Contains(h.config.DiskHealthMountpoints, p.Entity) {
			continue
		}
		free, ok := m["filesystem_free_bytes\x00"+p.Entity]
		if !ok {
			continue
		}
		v := percent(p.Value-free, p.Value)
		if v == nil {
			continue
		}
		out = append(out, volumeView{host.ID, hostName(host), p.Entity, p.Value - free, p.Value, *v, h.config.AlertPolicy.Resources.Disk.State(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}
func (h *Hub) fleetRow(host store.Host) fleetHost {
	host = h.currentReadings(host)
	row := fleetHost{Host: host, Status: h.status(host.Seen), Health: "healthy", Uptime: "Unavailable"}
	row.Fresh = row.Status == "connected" && !host.Sample.At.IsZero() && time.Since(host.Sample.At) < time.Duration(h.config.StaleSeconds)*time.Second
	missing := false
	m := metrics(host)
	if v, ok := m["cpu_percent\x00"]; ok {
		row.CPU = &v
	}
	if used, ok := m["memory_used_bytes\x00"]; ok {
		row.Memory = percent(used, m["memory_total_bytes\x00"])
	}
	row.MemoryTotal = m["memory_total_bytes\x00"]
	for _, v := range h.volumes(host) {
		if row.Disk == nil || v.Percent > *row.Disk {
			p := v.Percent
			row.Disk = &p
		}
	}
	for _, p := range host.Sample.Metrics {
		if p.Name == "network_rx_bytes_s" || p.Name == "network_tx_bytes_s" {
			if row.Network == nil {
				v := 0.0
				row.Network = &v
			}
			*row.Network += p.Value
		}
	}
	if v, ok := m["uptime_seconds\x00"]; ok {
		row.Uptime = fmt.Sprintf("%.1f days", v/86400)
	}
	add := func(kind, state, detail string) {
		if state == "unavailable" {
			missing = true
		}
		row.Conditions = append(row.Conditions, condition{host.ID, hostName(host), kind, state, detail})
		if state != "unavailable" && severity(state) > severity(row.Health) {
			row.Health = state
		}
	}
	if !row.Fresh {
		add("Contact / measurements", "unavailable", row.Status+"; latest readings may be old")
	} else {
		for _, reading := range []struct {
			name, resource string
			v              *float64
		}{{"CPU", "cpu", row.CPU}, {"Memory", "memory", row.Memory}, {"Disk", "disk", row.Disk}} {
			if reading.v == nil {
				missing = true
				add(reading.name, "unavailable", "No valid reading")
			} else if state := h.metricReading(reading.resource, reading.v).State; state != "healthy" {
				add(reading.name, state, fmt.Sprintf("%.1f%% used", *reading.v))
			}
		}
	}
	for _, c := range host.Sample.Containers {
		if state := h.containerState(host.ID, c); state != "healthy" {
			add("Container", state, containerDetail(c))
		}
	}
	for _, err := range host.Sample.Errors {
		add("Collection", "warning", err)
	}
	if !row.Fresh || (missing && row.Health == "healthy") {
		row.Health = "unavailable"
	}
	return row
}

func historyHours(window string) (int, bool) {
	switch window {
	case "1h":
		return 1, true
	case "24h":
		return 24, true
	case "7d":
		return 168, true
	case "30d":
		return 720, true
	}
	return 0, false
}

func (h *Hub) fleet(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if view == "" {
		view = "overview"
	}
	views := []struct{ ID, Title string }{
		{"overview", "Fleet Overview"}, {"resources", "Resource Views"},
		{"metrics", "Fleet Metrics"}, {"conditions", "Alerts & Hosts"},
	}
	title := ""
	for _, v := range views {
		if v.ID == view {
			title = v.Title
		}
	}
	if title == "" {
		http.Error(w, "unsupported fleet view", 400)
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > int(^uint(0)>>1)-100 {
			http.Error(w, "invalid page", 400)
			return
		}
		offset = n
	}
	window := r.URL.Query().Get("range")
	if window == "" {
		window = "24h"
	}
	hours, ok := historyHours(window)
	if !ok {
		http.Error(w, "unsupported history range", 400)
		return
	}
	hosts, err := h.store.HostsPage(r.Context(), offset, 100)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	now := time.Now()
	start := now.Add(-time.Duration(hours) * time.Hour)
	// Align with stored buckets; large ranges stay bounded to about 48 cells per host.
	step := max(h.config.BucketSeconds, ((hours*3600/48+h.config.BucketSeconds-1)/h.config.BucketSeconds)*h.config.BucketSeconds)
	cpu, err := h.store.FleetCPU(r.Context(), start, now, step, offset, 100)
	if err != nil {
		http.Error(w, "fleet history unavailable", 503)
		return
	}
	query := r.URL.Query().Get("q")
	if len(query) > 256 {
		http.Error(w, "filter too long", 400)
		return
	}
	rows := []fleetHost{}
	conditions := []condition{}
	storage := []volumeView{}
	counts := map[string]int{"healthy": 0, "warning": 0, "critical": 0, "unavailable": 0}
	names := map[string]string{}
	lines := map[string][]chartSample{}
	sparks := map[string]string{}
	type heatCell struct {
		Host        string
		At          int64
		Value, Peak float64
	}
	cells := []heatCell{}
	type memoryCell struct {
		Name        string
		Value, Used float64
	}
	memory := []memoryCell{}
	for _, host := range hosts {
		if query != "" && !strings.Contains(strings.ToLower(host.ID+" "+host.Name), strings.ToLower(query)) {
			continue
		}
		row := h.fleetRow(host)
		rows = append(rows, row)
		counts[row.Health]++
		conditions = append(conditions, row.Conditions...)
		storage = append(storage, h.volumes(host)...)
		names[host.ID] = chartHostName(host)
		if row.Memory != nil && row.Fresh {
			memory = append(memory, memoryCell{chartHostName(host), row.MemoryTotal, *row.Memory})
		}
	}
	for _, p := range cpu {
		if name, ok := names[p.Host]; ok {
			lines[p.Host] = append(lines[p.Host], chartSample{p.At.UnixMilli(), p.Average})
			cells = append(cells, heatCell{name, p.At.UnixMilli(), p.Average, p.Maximum})
		}
	}
	series := []chartSeries{}
	for _, row := range rows {
		if samples := lines[row.ID]; len(samples) > 0 {
			series = append(series, chartSeries{names[row.ID], samples})
		}
		b, _ := json.Marshal([]chartSeries{{Name: names[row.ID], Samples: lines[row.ID]}})
		sparks[row.ID] = string(b)
	}
	charts := []fleetChart{}
	for _, c := range []struct {
		title, kind, unit, resource string
		data                        any
	}{{"CPU heatmap", "heatmap", "%", "cpu", cells}, {"Memory by host", "treemap", "bytes", "memory", memory}, {"Host CPU comparison", "line", "%", "cpu", series}} {
		b, _ := json.Marshal(c.data)
		charts = append(charts, fleetChart{c.title, c.kind, c.unit, string(b), c.resource})
	}
	sort.SliceStable(rows, func(i, j int) bool { return severity(rows[i].Health) > severity(rows[j].Health) })
	sort.SliceStable(storage, func(i, j int) bool { return storage[i].Percent > storage[j].Percent })
	ranks := map[string][]fleetHost{}
	for _, key := range []string{"CPU", "Memory", "Network"} {
		list := []fleetHost{}
		value := func(row fleetHost) *float64 {
			switch key {
			case "CPU":
				return row.CPU
			case "Memory":
				return row.Memory
			default:
				return row.Network
			}
		}
		for _, row := range rows {
			if value(row) != nil && row.Fresh {
				list = append(list, row)
			}
		}
		sort.SliceStable(list, func(i, j int) bool { return *value(list[i]) > *value(list[j]) })
		ranks[key] = list[:min(5, len(list))]
	}
	h.mu.Lock()
	degraded := h.degraded
	h.mu.Unlock()
	h.render(w, "fleet", map[string]any{"View": view, "Title": title, "Views": views, "Hosts": rows, "Counts": counts, "Conditions": conditions, "Volumes": storage, "Ranks": ranks, "Charts": charts, "Sparks": sparks, "Start": start.UnixMilli(), "End": now.UnixMilli(), "Gap": step * 1500, "Step": step * 1000, "Range": window, "Query": query, "Offset": offset, "Previous": max(offset-100, 0), "Next": offset + 100, "More": len(hosts) == 100, "Degraded": degraded, "CSRF": h.csrf(r), "Local": h.config.AuthMode == "local"})
}
