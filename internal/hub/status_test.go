package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"houselab/monitoring/internal/protocol"
)

func postSample(h *Hub, ctx context.Context, id string, at time.Time) int {
	body, _ := json.Marshal(protocol.Sample{Version: 1, HostID: "node", ID: id, At: at, Metrics: []protocol.Metric{
		{Name: "cpu_percent", Value: 20, Weight: 1},
		{Name: "memory_used_bytes", Value: 20, Weight: 1}, {Name: "memory_total_bytes", Value: 100, Weight: 1},
		{Name: "filesystem_total_bytes", Entity: "/", Value: 100, Weight: 1}, {Name: "filesystem_free_bytes", Entity: "/", Value: 80, Weight: 1},
	}})
	r := httptest.NewRequest("POST", "/v1/samples", bytes.NewReader(body)).WithContext(ctx)
	r.Header.Set("X-Host-ID", "node")
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	w := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(w, r)
	return w.Code
}

func TestIngestBannerClearsAfterSuccessfulCommit(t *testing.T) {
	h := testHub(t, "proxy")
	failed, cancel := context.WithCancel(context.Background())
	cancel()
	if code := postSample(h, failed, "a", time.Now().Add(-time.Second)); code != 503 {
		t.Fatalf("cancelled commit status %d", code)
	}
	if body := serveUI(t, h, "/"); !strings.Contains(body, `role="alert">Metrics could not be committed`) {
		t.Fatal("ingest failure not surfaced")
	}
	if code := postSample(h, context.Background(), "b", time.Now().Add(-time.Second)); code != 204 {
		t.Fatalf("ingest status %d", code)
	}
	if body := serveUI(t, h, "/"); strings.Contains(body, `role="alert"`) || !strings.Contains(body, "within thresholds") {
		t.Fatal("ingest banner persisted after a successful commit")
	}
}

func TestNotificationBannerClearsAfterSuccessfulDelivery(t *testing.T) {
	h := testHub(t, "proxy")
	ingestCPU(t, h, "node", 95)
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	now := time.Now()
	h.notificationPass(context.Background(), server.Client(), server.URL, now)
	h.notificationPass(context.Background(), server.Client(), server.URL, now.Add(3*time.Minute))
	if body := serveUI(t, h, "/"); !strings.Contains(body, `role="alert">Alert delivery failed`) {
		t.Fatal("delivery failure not surfaced")
	}
	// Still inside the retry cooldown: nothing was delivered, so the banner stays.
	h.notificationPass(context.Background(), server.Client(), server.URL, now.Add(3*time.Minute+10*time.Second))
	if body := serveUI(t, h, "/"); !strings.Contains(body, `role="alert">Alert delivery failed`) {
		t.Fatal("banner cleared while delivery is still backing off")
	}
	fail.Store(false)
	h.notificationPass(context.Background(), server.Client(), server.URL, now.Add(10*time.Minute))
	if body := serveUI(t, h, "/"); strings.Contains(body, `role="alert">Alert delivery failed`) {
		t.Fatal("delivery banner persisted after successful delivery")
	}
}

func TestBannerShowsRemainingProblemWhenOneClears(t *testing.T) {
	h := testHub(t, "proxy")
	h.setStatus(statusNotify, "Alert delivery failed; check Discord configuration, connectivity and database health.")
	h.setStatus(statusSamples, "Metrics could not be committed; check database and disk capacity.")
	h.setStatus(statusSamples, "")
	if body := serveUI(t, h, "/"); !strings.Contains(body, `role="alert">Alert delivery failed`) {
		t.Fatal("clearing one subsystem hid another subsystem's problem")
	}
}
