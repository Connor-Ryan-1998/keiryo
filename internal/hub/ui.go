package hub

import (
	"bytes"
	"fmt"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"net/http"
	"time"
)

func number(v float64) string { return fmt.Sprintf("%.2f", v) }
func (h *Hub) render(w http.ResponseWriter, name string, data any) {
	var b bytes.Buffer
	if err := h.templates.ExecuteTemplate(&b, name, data); err != nil {
		http.Error(w, "page rendering failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}

type hostView struct {
	store.Host
	Status         string
	Stats          []statView
	CanFetchLogs   bool
	LiveContainers []protocol.Container
}

func (h *Hub) status(seen time.Time) string {
	if seen.IsZero() {
		return "never seen"
	}
	age := time.Since(seen)
	if age >= time.Duration(h.config.OfflineSeconds)*time.Second {
		return "offline"
	}
	if age >= time.Duration(h.config.StaleSeconds)*time.Second {
		return "stale"
	}
	return "connected"
}
func (h *Hub) host(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	host, err := h.store.Host(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	window := r.URL.Query().Get("range")
	hours := 1
	switch window {
	case "24h":
		hours = 24
	case "7d":
		hours = 168
	case "30d":
		hours = 720
	case "", "1h":
	default:
		http.Error(w, "unsupported history range", 400)
		return
	}
	end := time.Now()
	summary := hours > h.config.RawHours
	points, err := h.store.History(r.Context(), id, end.Add(-time.Duration(hours)*time.Hour), end, summary, h.config.BucketSeconds)
	if err != nil {
		http.Error(w, "history unavailable", 503)
		return
	}
	states, err := h.store.States(r.Context(), id, end.Add(-time.Duration(hours)*time.Hour), end, summary)
	if err != nil {
		http.Error(w, "state history unavailable", 503)
		return
	}
	nodes, err := h.store.HostsPage(r.Context(), 0, 100)
	if err != nil {
		http.Error(w, "nodes unavailable", 503)
		return
	}
	h.render(w, "host", map[string]any{
		"Host":  hostView{Host: host, Status: h.status(host.Seen), Stats: overview(h.currentReadings(host)), CanFetchLogs: h.canFetchLogs(host), LiveContainers: h.currentReadings(host).Sample.Containers},
		"Nodes": nodes, "Range": window, "Charts": chartViews(points, host),
		"Start": end.Add(-time.Duration(hours) * time.Hour).UnixMilli(), "End": end.UnixMilli(),
		"Gap":    chartGap(points, summary, h.config.BucketSeconds, h.config.StaleSeconds),
		"Points": points, "States": states, "Summary": summary,
		"Truncated": len(points) == 20000 || len(states) == 1000,
		"CSRF":      h.csrf(r), "Local": h.config.AuthMode == "local",
	})
}
