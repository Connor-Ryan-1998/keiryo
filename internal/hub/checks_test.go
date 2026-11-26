package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"houselab/monitoring/internal/config"
)

func check(name, url string) config.HTTPCheck {
	return config.HTTPCheck{Name: name, URL: url, IntervalSeconds: 60, TimeoutSeconds: 2, TLSWarnDays: 14, TLSCriticalDays: 3, Severity: "critical"}
}

func TestProbeStatusRedirectsAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/down":
			w.WriteHeader(503)
		case "/moved":
			http.Redirect(w, r, "/down", 302)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		}
	}))
	defer server.Close()
	for _, tc := range []struct {
		name   string
		check  config.HTTPCheck
		ok     bool
		status int
		detail string
	}{
		{"ok", check("ok", server.URL+"/ok"), true, 200, ""},
		{"down", check("down", server.URL+"/down"), false, 503, "unexpected status 503"},
		{"redirect kept", check("moved", server.URL+"/moved"), true, 302, ""},
		{"redirect followed", func() config.HTTPCheck { c := check("moved", server.URL+"/moved"); c.FollowRedirects = true; return c }(), false, 503, "unexpected status 503"},
		{"expect list", func() config.HTTPCheck { c := check("down", server.URL+"/down"); c.ExpectStatus = []int{503}; return c }(), true, 503, ""},
		{"timeout", func() config.HTTPCheck { c := check("slow", server.URL+"/slow"); c.TimeoutSeconds = 0; return c }(), false, 0, "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := checkClient(tc.check, server.Client().Transport)
			if tc.check.TimeoutSeconds == 0 {
				client.Timeout = 100 * time.Millisecond
			}
			got := probe(context.Background(), client, tc.check)
			if got.OK != tc.ok || got.Status != tc.status || !strings.Contains(got.Error, tc.detail) || got.At.IsZero() {
				t.Fatalf("%+v", got)
			}
			if strings.Contains(got.Error, server.URL) {
				t.Fatal("probe error leaked URL")
			}
		})
	}
}

func TestProbeRecordsCertificateExpiry(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	c := check("tls", server.URL)
	got := probe(context.Background(), checkClient(c, server.Client().Transport), c)
	if !got.OK || got.CertExpiry.IsZero() {
		t.Fatalf("%+v", got)
	}
	// An untrusted certificate is a failed check, not a silent pass.
	got = probe(context.Background(), checkClient(c, nil), c)
	if got.OK || !strings.Contains(got.Error, "TLS") {
		t.Fatalf("untrusted certificate: %+v", got)
	}
}

func TestCheckSignals(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	noTLS := check("plain", "https://plain.example")
	noTLS.TLSWarnDays, noTLS.TLSCriticalDays = 0, 0
	h.config.HTTPChecks = []config.HTTPCheck{check("up", "https://up.example"), check("down", "https://down.example"), check("soon", "https://soon.example"), check("later", "https://later.example"), noTLS, check("never", "https://never.example")}
	h.recordCheck("up", checkResult{At: now, OK: true, Status: 200, Latency: 120 * time.Millisecond, CertExpiry: now.Add(60 * 24 * time.Hour)})
	h.recordCheck("down", checkResult{At: now, Status: 503, Error: "unexpected status 503", Latency: 40 * time.Millisecond})
	h.recordCheck("soon", checkResult{At: now, OK: true, Status: 200, CertExpiry: now.Add(2 * 24 * time.Hour)})
	h.recordCheck("later", checkResult{At: now, OK: true, Status: 200, CertExpiry: now.Add(10 * 24 * time.Hour)})
	h.recordCheck("plain", checkResult{At: now, OK: true, Status: 200, CertExpiry: now.Add(24 * time.Hour)})
	signals := map[string]condition{}
	for _, s := range h.checkSignals() {
		signals[s.Key] = s
	}
	for key, want := range map[string]string{"check/up": "healthy", "cert/up": "healthy", "check/down": "critical", "cert/soon": "critical", "cert/later": "warning"} {
		if signals[key].State != want {
			t.Fatalf("%s: %+v want %s", key, signals[key], want)
		}
	}
	if d := signals["check/down"].Detail; d != "down: unexpected status 503 (40 ms)" {
		t.Fatalf("failure detail %q", d)
	}
	if d := signals["cert/later"].Detail; d != "later certificate expires in 10.0 days" {
		t.Fatalf("certificate detail %q", d)
	}
	for _, key := range []string{"cert/down", "cert/plain", "check/never", "cert/never"} {
		if _, ok := signals[key]; ok {
			t.Fatalf("unexpected signal %s", key)
		}
	}
}

func TestFailingCheckOnDashboardAndDiscord(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.AlertPendingSeconds = 0
	ingestCPU(t, h, "node", 10)
	h.config.HTTPChecks = []config.HTTPCheck{check("traefik", "https://traefik.example")}
	h.recordCheck("traefik", checkResult{At: time.Now(), Status: 502, Error: "unexpected status 502", Latency: 10 * time.Millisecond})
	body := serveUI(t, h, "/")
	if strings.Contains(body, "within thresholds") || !strings.Contains(body, "1 check failing") {
		t.Fatal("failing check left the fleet summary all clear")
	}
	if !strings.Contains(body, "traefik: unexpected status 502") || !strings.Contains(body, `href="/?view=conditions#checks"`) {
		t.Fatal("failing check missing from Needs attention")
	}
	if body := serveUI(t, h, "/?view=conditions"); !strings.Contains(body, `id="checks"`) || !strings.Contains(body, "traefik") {
		t.Fatal("checks section missing")
	}
	var messages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Content string }
		json.NewDecoder(r.Body).Decode(&v)
		messages = append(messages, v.Content)
		w.WriteHeader(204)
	}))
	defer server.Close()
	h.notificationPass(context.Background(), server.Client(), server.URL, time.Now())
	if len(messages) != 1 || !strings.Contains(messages[0], "[CRITICAL] HTTP checks · HTTP check") || !strings.Contains(messages[0], "traefik: unexpected status 502") {
		t.Fatalf("messages %v", messages)
	}
}
