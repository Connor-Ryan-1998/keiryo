package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"houselab/monitoring/internal/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventStreamAuthAndCommittedSample(t *testing.T) {
	h := testHub(t, "local")
	if w := request(h.UIHandler(), "GET", "/events", "", nil); w.Code != 303 {
		t.Fatal("anonymous events allowed")
	}
	ctx := context.Background()
	token := strings.Repeat("a", 64)
	if err := h.store.SetSetting(ctx, "initialized", "yes"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutSession(ctx, hashToken(token), "csrf", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.UIHandler())
	defer srv.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("not SSE: %d", res.StatusCode)
	}
	reader := bufio.NewReader(res.Body)
	readEvent := func() string {
		t.Helper()
		var b strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(line)
			if line == "\n" {
				return b.String()
			}
		}
	}
	if first := readEvent(); !strings.Contains(first, "event: refresh") {
		t.Fatalf("no reconnect snapshot: %s", first)
	}
	sample := protocol.Sample{Version: 1, HostID: "node", ID: "stream", At: time.Now(), Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 40, Weight: 1}}}
	data, _ := json.Marshal(sample)
	upload := httptest.NewRequest("POST", "/v1/samples", bytes.NewReader(data))
	upload.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	upload.Header.Set("X-Host-ID", "node")
	w := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(w, upload)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if event := readEvent(); !strings.Contains(event, `data: {"host":"node"}`) {
		t.Fatalf("missing committed update: %s", event)
	}
	if host, err := h.store.Host(ctx, "node"); err != nil || host.Sample.ID != "stream" {
		t.Fatal("not committed before notification")
	}
	// Logout/revocation is enforced before sending further sample notifications.
	if err := h.store.DeleteSession(ctx, hashToken(token)); err != nil {
		t.Fatal(err)
	}
	h.notify("node")
	if event := readEvent(); !strings.Contains(event, "event: auth-expired") {
		t.Fatalf("revoked session still streams: %s", event)
	}
}

func TestEventStreamOriginAndIndependentLimits(t *testing.T) {
	h := testHub(t, "proxy")
	r := httptest.NewRequest("GET", "/events", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-authentik-username", "user")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.UIHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-origin stream: %d", w.Code)
	}
	for i := 0; i < cap(h.eventSlots); i++ {
		h.eventSlots <- struct{}{}
	}
	r.Header.Del("Origin")
	w = httptest.NewRecorder()
	h.UIHandler().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("unbounded streams: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-authentik-username", "user")
	w = httptest.NewRecorder()
	h.UIHandler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("stream slots starved UI")
	}
	for len(h.eventSlots) > 0 {
		<-h.eventSlots
	}
}

func TestRejectedSampleDoesNotNotifyAndSlowSubscriberCoalesces(t *testing.T) {
	h := testHub(t, "proxy")
	updates := make(chan string, 1)
	h.subscribers[updates] = struct{}{}
	r := httptest.NewRequest("POST", "/v1/samples", strings.NewReader(`{"version":999}`))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 32))
	r.Header.Set("X-Host-ID", "node")
	w := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("invalid sample accepted")
	}
	select {
	case <-updates:
		t.Fatal("rejected sample notified browser")
	default:
	}
	h.notify("one")
	h.notify("two")
	if host := <-updates; host != "" {
		t.Fatalf("coalesced update must refresh all hosts, got %q", host)
	}
}
