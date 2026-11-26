package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func healthz(h *Hub) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	// No session, no proxy identity: external watchdogs cannot authenticate.
	h.UIHandler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	return w
}

func TestHealthzReportsHubProblemsWithoutDetail(t *testing.T) {
	for _, mode := range []string{"local", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			h := testHub(t, mode)
			if w := healthz(h); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "ok" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("healthy hub: %d %q", w.Code, w.Body.String())
			}
			h.setStatus(statusNotify, "Alert delivery failed; check Discord configuration, connectivity and database health.")
			if w := healthz(h); w.Code != 503 || strings.Contains(w.Body.String(), "Discord") {
				t.Fatalf("degraded hub: %d %q", w.Code, w.Body.String())
			}
			h.setStatus(statusNotify, "")
			if w := healthz(h); w.Code != 200 {
				t.Fatalf("recovered hub: %d", w.Code)
			}
			h.store.Close()
			if w := healthz(h); w.Code != 503 {
				t.Fatalf("unavailable database: %d", w.Code)
			}
		})
	}
}

func TestHeartbeatPingsOnlyWhileHealthy(t *testing.T) {
	h := testHub(t, "proxy")
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer server.Close()
	if err := h.heartbeat(context.Background(), server.Client(), server.URL+"/ping"); err != nil || hits.Load() != 1 {
		t.Fatalf("healthy heartbeat: err=%v hits=%d", err, hits.Load())
	}
	h.setStatus(statusOTLP, "OTLP metrics could not be committed; check database and disk capacity.")
	if err := h.heartbeat(context.Background(), server.Client(), server.URL+"/ping"); err != nil || hits.Load() != 1 {
		t.Fatalf("degraded hub pinged: err=%v hits=%d", err, hits.Load())
	}
}

func TestHeartbeatFailureDoesNotLeakURL(t *testing.T) {
	h := testHub(t, "proxy")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	err := h.heartbeat(context.Background(), server.Client(), server.URL+"/secret-ping-token")
	if err == nil || strings.Contains(err.Error(), "secret-ping-token") {
		t.Fatalf("heartbeat error %v", err)
	}
}

func TestHeartbeatSecretValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	for _, raw := range []string{"http://hc-ping.com/0f4b9c2e-placeholder-uuid-value", "https://user:pw@hc-ping.com/0f4b9c2e-placeholder-uuid", "https://hc-ping.com/0f4b9c2e-placeholder-uuid#frag", "ftp://hc-ping.com/0f4b9c2e-placeholder-uuid-value"} {
		os.WriteFile(path, []byte(raw), 0600)
		if _, err := heartbeatURL(path); err == nil || strings.Contains(err.Error(), raw) {
			t.Fatalf("unsafe heartbeat secret accepted: %q %v", raw, err)
		}
	}
	for _, raw := range []string{"https://hc-ping.com/0f4b9c2e-placeholder-uuid-value", "https://kuma.example/api/push/placeholder-push-token?status=up&msg=OK"} {
		os.WriteFile(path, []byte(raw), 0600)
		if _, err := heartbeatURL(path); err != nil {
			t.Fatalf("valid heartbeat %q rejected: %v", raw, err)
		}
	}
}
