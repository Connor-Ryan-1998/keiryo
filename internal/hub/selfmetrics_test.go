package hub

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/store"
)

const metricsToken = "metrics-token-placeholder-0123456789abcdef"

func metricsHub(t *testing.T) *Hub {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	agent, metrics := filepath.Join(dir, "token"), filepath.Join(dir, "metrics")
	os.WriteFile(agent, []byte(strings.Repeat("s", 32)), 0600)
	os.WriteFile(metrics, []byte(metricsToken), 0600)
	c := config.DefaultHub()
	c.AuthMode = "proxy"
	c.TrustedProxies = []string{"10.0.0.1/32"}
	c.PublicURL = "https://monitor.example.com"
	c.Database = filepath.Join(dir, "test.db")
	c.Agents = map[string]string{"node": agent}
	c.MetricsTokenFile = metrics
	h, err := New(c, s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func scrape(h *Hub, token, hostID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/metrics", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if hostID != "" {
		r.Header.Set("X-Host-ID", hostID)
	}
	w := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(w, r)
	return w
}

func TestSelfMetricsRequireDedicatedToken(t *testing.T) {
	h := metricsHub(t)
	for _, tc := range []struct{ token, host string }{{"", ""}, {"wrong-token-placeholder-0123456789abcdef", ""}, {strings.Repeat("s", 32), "node"}} {
		if w := scrape(h, tc.token, tc.host); w.Code != 401 {
			t.Fatalf("token %q host %q: %d", tc.token, tc.host, w.Code)
		}
	}
	// The metrics token must not authorize agent ingestion.
	r := httptest.NewRequest("POST", "/v1/samples", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+metricsToken)
	r.Header.Set("X-Host-ID", "node")
	w := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("metrics token ingested: %d", w.Code)
	}
	disabled := testHub(t, "proxy")
	if w := scrape(disabled, metricsToken, ""); w.Code != 404 {
		t.Fatalf("disabled metrics endpoint: %d", w.Code)
	}
}

func TestSelfMetricsReportHubActivity(t *testing.T) {
	h := metricsHub(t)
	failed, cancel := context.WithCancel(context.Background())
	cancel()
	postSample(h, failed, "b", time.Now().Add(-time.Second))
	if w := scrape(h, metricsToken, ""); !strings.Contains(w.Body.String(), "keiryo_up 0\n") {
		t.Fatal("failed commit did not mark the hub down")
	}
	if code := postSample(h, context.Background(), "a", time.Now().Add(-time.Second)); code != 204 {
		t.Fatalf("ingest %d", code)
	}
	scrape(h, "wrong-token-placeholder-0123456789abcdef", "")
	h.maintain(context.Background(), time.Now())
	w := scrape(h, metricsToken, "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("scrape %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, want := range []string{
		"keiryo_up 1\n",
		`keiryo_ingest_total{endpoint="samples",result="accepted"} 1` + "\n",
		`keiryo_ingest_total{endpoint="samples",result="error"} 1` + "\n",
		`keiryo_agent_rejected_total{reason="unauthorized"} 1` + "\n",
		"keiryo_otlp_points_rejected_total 0\n",
		`keiryo_notifications_total{result="sent"} 0` + "\n",
		`keiryo_hosts{health="healthy"} 1` + "\n",
		`keiryo_hosts{health="critical"} 0` + "\n",
		"keiryo_retention_backlog_seconds 0\n",
		"# TYPE keiryo_db_size_bytes gauge\n",
		"keiryo_maintenance_duration_seconds ",
		"keiryo_maintenance_last_success_timestamp_seconds ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, metricsToken) {
		t.Fatal("scrape leaked token")
	}
}

func TestMetricsTokenMustDifferFromAgentTokens(t *testing.T) {
	h := metricsHub(t)
	c := h.config
	c.MetricsTokenFile = c.Agents["node"]
	if _, err := New(c, h.store); err == nil {
		t.Fatal("metrics token equal to an agent token accepted")
	}
}
