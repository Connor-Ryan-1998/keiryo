package hub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// counters is a minimal labelled counter set for the Prometheus text format.
type counters struct {
	mu     sync.Mutex
	values map[string]uint64
}

func (c *counters) add(series string, n uint64) {
	c.mu.Lock()
	if c.values == nil {
		c.values = map[string]uint64{}
	}
	c.values[series] += n
	c.mu.Unlock()
}
func (c *counters) get(series string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[series]
}

func (h *Hub) countIngest(endpoint, result string) {
	h.counters.add(fmt.Sprintf(`keiryo_ingest_total{endpoint=%q,result=%q}`, endpoint, result), 1)
}

func (h *Hub) metricsAuthorized(r *http.Request) bool {
	actual := sha256.Sum256([]byte(bearer(r)))
	return subtle.ConstantTimeCompare(actual[:], h.metricsToken[:]) == 1
}

// selfMetrics exposes hub health in the Prometheus text format. It is served on the
// private agent listener and authenticated with its own token, never an agent's.
func (h *Hub) selfMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var b strings.Builder
	metric := func(name, kind, help string, lines ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
	}
	up := 0
	if h.healthy(ctx) {
		up = 1
	}
	metric("keiryo_up", "gauge", "1 when the database answers and no hub subsystem reports a problem.", fmt.Sprintf("keiryo_up %d", up))

	var ingest []string
	for _, endpoint := range []string{"samples", "otlp", "state"} {
		for _, result := range []string{"accepted", "invalid", "error"} {
			series := fmt.Sprintf(`keiryo_ingest_total{endpoint=%q,result=%q}`, endpoint, result)
			ingest = append(ingest, fmt.Sprintf("%s %d", series, h.counters.get(series)))
		}
	}
	metric("keiryo_ingest_total", "counter", "Agent ingest requests by endpoint and result.", ingest...)
	var rejected []string
	for _, reason := range []string{"unauthorized", "busy"} {
		series := fmt.Sprintf(`keiryo_agent_rejected_total{reason=%q}`, reason)
		rejected = append(rejected, fmt.Sprintf("%s %d", series, h.counters.get(series)))
	}
	metric("keiryo_agent_rejected_total", "counter", "Agent listener requests rejected before handling.", rejected...)
	metric("keiryo_otlp_points_rejected_total", "counter", "OTLP data points rejected as unsupported, invalid or conflicting.", fmt.Sprintf("keiryo_otlp_points_rejected_total %d", h.counters.get("keiryo_otlp_points_rejected_total")))
	var notifications []string
	for _, result := range []string{"sent", "failed"} {
		series := fmt.Sprintf(`keiryo_notifications_total{result=%q}`, result)
		notifications = append(notifications, fmt.Sprintf("%s %d", series, h.counters.get(series)))
	}
	metric("keiryo_notifications_total", "counter", "Discord notification attempts by result.", notifications...)

	var size int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(h.config.Database + suffix); err == nil {
			size += info.Size()
		}
	}
	metric("keiryo_db_size_bytes", "gauge", "SQLite database plus WAL file size.", fmt.Sprintf("keiryo_db_size_bytes %d", size))
	if backlog, err := h.store.RetentionBacklog(ctx, time.Now(), time.Duration(h.config.RawHours)*time.Hour); err == nil {
		metric("keiryo_retention_backlog_seconds", "gauge", "Age beyond raw retention of the oldest raw row; 0 when cleanup keeps up.", fmt.Sprintf("keiryo_retention_backlog_seconds %d", int64(backlog.Seconds())))
	}

	h.mu.Lock()
	duration, last := h.maintenanceDuration, h.maintenanceSuccess
	h.mu.Unlock()
	metric("keiryo_maintenance_duration_seconds", "gauge", "Duration of the latest retention pass.", fmt.Sprintf("keiryo_maintenance_duration_seconds %g", duration.Seconds()))
	lastSeconds := int64(0)
	if !last.IsZero() {
		lastSeconds = last.Unix()
	}
	metric("keiryo_maintenance_last_success_timestamp_seconds", "gauge", "Unix time of the latest successful retention pass; 0 before the first.", fmt.Sprintf("keiryo_maintenance_last_success_timestamp_seconds %d", lastSeconds))

	if hosts, err := h.store.HostsPage(ctx, 0, 1000); err == nil {
		counts := map[string]int{"healthy": 0, "warning": 0, "critical": 0, "unavailable": 0}
		for _, host := range hosts {
			counts[h.displayRow(ctx, host, time.Now()).Health]++
		}
		states := make([]string, 0, len(counts))
		for state := range counts {
			states = append(states, state)
		}
		sort.Strings(states)
		lines := make([]string, 0, len(states))
		for _, state := range states {
			lines = append(lines, fmt.Sprintf(`keiryo_hosts{health=%q} %d`, state, counts[state]))
		}
		metric("keiryo_hosts", "gauge", "Configured hosts by dashboard health (first 1000).", lines...)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	io.WriteString(w, b.String())
}
