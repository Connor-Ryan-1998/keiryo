package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"houselab/monitoring/internal/config"
)

// healthy reports whether the database answers and no hub subsystem reports a problem.
func (h *Hub) healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.store.Ping(ctx) == nil && h.banner() == ""
}

// healthz is unauthenticated for external watchdogs, so it never reveals detail.
func (h *Hub) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !h.healthy(r.Context()) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "degraded\n")
		return
	}
	io.WriteString(w, "ok\n")
}

// heartbeatURL reads the dead-man's-switch URL. Query strings are allowed because
// push monitors (for example Uptime Kuma) carry status parameters there.
func heartbeatURL(path string) (string, error) {
	secret, err := config.ReadSecret(path)
	if err != nil {
		return "", errors.New("cannot read restricted heartbeat URL file")
	}
	u, err := url.Parse(secret)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("heartbeat requires an HTTPS URL without credentials or fragment")
	}
	return secret, nil
}

// Heartbeat pings an external monitor while the hub is healthy. When the hub
// stops, hangs or degrades, the pings stop and the external monitor alerts.
func (h *Hub) Heartbeat(ctx context.Context) {
	if h.config.HeartbeatURLFile == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(time.Duration(h.config.HeartbeatSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			target, err := heartbeatURL(h.config.HeartbeatURLFile)
			if err == nil {
				err = h.heartbeat(ctx, client, target)
			}
			if err != nil {
				slog.Warn("heartbeat failed", "error", err)
			}
		}
	}
}

func (h *Hub) heartbeat(ctx context.Context, client *http.Client, target string) error {
	if !h.healthy(ctx) {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return errors.New("cannot create heartbeat request")
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("heartbeat request failed")
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("heartbeat endpoint rejected the ping")
	}
	return nil
}
