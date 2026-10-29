package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Notifications invalidate views, not a replay log. A full queue becomes a fleet refresh.
func (h *Hub) notify(host string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- host:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- ""
		}
	}
}

func (h *Hub) events(w http.ResponseWriter, r *http.Request) {
	if !h.originOK(r) {
		http.Error(w, "same-origin stream required", 403)
		return
	}
	select {
	case h.eventSlots <- struct{}{}:
		defer func() { <-h.eventSlots }()
	default:
		http.Error(w, "event connection limit reached", 503)
		return
	}
	updates := make(chan string, 1)
	h.mu.Lock()
	h.subscribers[updates] = struct{}{}
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.subscribers, updates); h.mu.Unlock() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(event, host string) bool {
		// Bound blocked writes without changing the deadlines of ordinary UI requests.
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && err != http.ErrNotSupported {
			return false
		}
		data, _ := json.Marshal(struct {
			Host string `json:"host"`
		}{host})
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	valid := func() bool {
		if h.config.AuthMode != "local" {
			return true
		}
		session, _ := r.Context().Value(authKey{}).(browserSession)
		_, err := h.store.Session(r.Context(), session.ID, time.Now())
		return err == nil
	}
	if !send("refresh", "") {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	freshness := time.NewTicker(time.Minute)
	defer freshness.Stop()
	// Reconnect through the authentication proxy periodically to recheck its identity.
	lifetime := time.NewTimer(2 * time.Minute)
	defer lifetime.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-lifetime.C:
			return
		case host := <-updates:
			if !valid() {
				send("auth-expired", "")
				return
			}
			if !send("refresh", host) {
				return
			}
		case <-freshness.C:
			if !valid() {
				send("auth-expired", "")
				return
			}
			if !send("refresh", "") {
				return
			}
		case <-heartbeat.C:
			if !valid() {
				send("auth-expired", "")
				return
			}
			if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil && err != http.ErrNotSupported {
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}
