package hub

import (
	"context"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"net/http"
	"time"
)

func (h *Hub) ingestState(w http.ResponseWriter, r *http.Request) {
	var state protocol.AgentState
	if err := decodeJSON(r, &state); err != nil {
		http.Error(w, "invalid state JSON", 400)
		return
	}
	sample := protocol.Sample{Version: state.Version, HostID: state.HostID, ID: state.ID, At: state.At, DisplayName: state.DisplayName, AgentVersion: state.AgentVersion, Containers: state.Containers, Errors: state.Errors}
	now := time.Now()
	if err := sample.Validate(r.Header.Get("X-Host-ID"), now, time.Duration(h.config.RawHours)*time.Hour, time.Duration(h.config.FutureSeconds)*time.Second, h.config.MaxEntities); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := h.store.IngestState(ctx, state, now, h.config.BucketSeconds); err != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	h.notify(state.HostID)
	w.WriteHeader(204)
}
func (h *Hub) stateFresh(sample protocol.Sample) bool {
	at := sample.StateAt
	if !sample.Incremental {
		at = sample.At
	}
	return !at.IsZero() && time.Since(at) < time.Duration(h.config.StaleSeconds)*time.Second
}
func (h *Hub) canFetchLogs(host store.Host) bool {
	return h.status(host.Seen) == "connected" && h.stateFresh(host.Sample) && (!host.Sample.Incremental || host.Sample.LogFetch)
}
func metricTime(sample protocol.Sample, m protocol.Metric) time.Time {
	if !m.ObservedAt.IsZero() {
		return m.ObservedAt
	}
	return sample.At
}

// currentReadings filters each measurement, not the timestamp of an unrelated batch.
func (h *Hub) currentReadings(host store.Host) store.Host {
	if !host.Sample.Incremental {
		return host
	}
	original := host.Sample
	host.Sample.Metrics = nil
	for _, m := range original.Metrics {
		at := metricTime(original, m)
		if !at.IsZero() && time.Since(at) < time.Duration(h.config.StaleSeconds)*time.Second {
			host.Sample.Metrics = append(host.Sample.Metrics, m)
		}
	}
	if original.Incremental && !h.stateFresh(original) {
		host.Sample.Errors = nil
		host.Sample.StateAt = time.Time{}
		host.Sample.Containers = nil
		for _, c := range original.Containers {
			c.State = "unknown"
			c.Health = ""
			c.ExitCode = nil
			host.Sample.Containers = append(host.Sample.Containers, c)
		}
	}
	return host
}
