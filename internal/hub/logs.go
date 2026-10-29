package hub

import (
	"context"
	"encoding/json"
	"errors"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
	"net/http"
	"sync"
	"time"
)

type pendingLog struct {
	request            protocol.LogRequest
	done               chan protocol.LogResult
	claimed, completed bool
}
type logQueue struct {
	mu    sync.Mutex
	items map[string]*pendingLog
}

func (q *logQueue) create(host, container string, c config.Hub) (protocol.LogRequest, <-chan protocol.LogResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.items == nil {
		q.items = map[string]*pendingLog{}
	}
	now := time.Now()
	for id, p := range q.items {
		if now.After(p.request.Expires) {
			delete(q.items, id)
		}
	}
	if len(q.items) >= c.LogQueue {
		return protocol.LogRequest{}, nil, errors.New("log queue full")
	}
	for _, p := range q.items {
		if p.request.HostID == host {
			return protocol.LogRequest{}, nil, errors.New("host already has a log request")
		}
	}
	r := protocol.LogRequest{ID: randomToken(), HostID: host, ContainerID: container, Lines: c.LogLines, Bytes: c.LogBytes, Expires: now.Add(time.Duration(c.LogTimeoutSeconds) * time.Second)}
	p := &pendingLog{request: r, done: make(chan protocol.LogResult, 1)}
	q.items[r.ID] = p
	return r, p.done, nil
}
func (q *logQueue) remove(id string) { q.mu.Lock(); delete(q.items, id); q.mu.Unlock() }
func (q *logQueue) next(host string) *protocol.LogRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range q.items {
		if p.request.HostID == host && !p.claimed && time.Now().Before(p.request.Expires) {
			p.claimed = true
			r := p.request
			return &r
		}
	}
	return nil
}
func (q *logQueue) complete(host string, r protocol.LogResult, maxBytes int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.items[r.ID]
	if !ok || p.request.HostID != host || !p.claimed || p.completed || time.Now().After(p.request.Expires) || len(r.Text) > maxBytes || len(r.Error) > 512 {
		return errors.New("invalid log response")
	}
	p.completed = true
	p.done <- r
	return nil
}
func (h *Hub) nextLog(w http.ResponseWriter, r *http.Request) {
	next := h.logs.next(r.Header.Get("X-Host-ID"))
	if next == nil {
		w.WriteHeader(204)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(next)
}
func (h *Hub) logResult(w http.ResponseWriter, r *http.Request) {
	var out protocol.LogResult
	if decodeJSON(r, &out) != nil || h.logs.complete(r.Header.Get("X-Host-ID"), out, h.config.LogBytes) != nil {
		http.Error(w, "invalid log response", 400)
		return
	}
	w.WriteHeader(204)
}
func (h *Hub) showLogs(w http.ResponseWriter, r *http.Request) {
	id, container := r.PathValue("id"), r.PathValue("container")
	host, err := h.store.Host(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !h.canFetchLogs(host) {
		http.Error(w, "A fresh keiryō agent with log-fetch capability is required.", 409)
		return
	}
	known := false
	for _, c := range host.Sample.Containers {
		if c.ID == container {
			known = true
			break
		}
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	req, done, err := h.logs.create(id, container, h.config)
	if err != nil {
		http.Error(w, err.Error(), 429)
		return
	}
	defer h.logs.remove(req.ID)
	ctx, cancel := context.WithDeadline(r.Context(), req.Expires)
	defer cancel()
	select {
	case result := <-done:
		h.render(w, "logs", map[string]any{"Text": result.Text, "Error": result.Error})
	case <-ctx.Done():
		h.render(w, "logs", map[string]any{"Error": "Agent unavailable or log request timed out."})
	}
}
