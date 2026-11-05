package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
)

func notificationHost(now time.Time) store.Host {
	return store.Host{ID: "node", Name: "Node", Seen: now, Sample: protocol.Sample{At: now, Metrics: []protocol.Metric{
		{Name: "cpu_percent", Value: 10}, {Name: "memory_used_bytes", Value: 10}, {Name: "memory_total_bytes", Value: 100},
		{Name: "filesystem_total_bytes", Entity: "/", Value: 100}, {Name: "filesystem_free_bytes", Entity: "/", Value: 90},
	}}}
}

func TestNotificationLifecyclePersistsAndUsesAllDashboardConditions(t *testing.T) {
	for _, category := range []string{"CPU", "Memory", "Disk", "Container", "Collection", "Contact / measurements"} {
		t.Run(category, func(t *testing.T) {
			h := testHub(t, "proxy")
			now := time.Now()
			host := notificationHost(now)
			switch category {
			case "CPU":
				host.Sample.Metrics[0].Value = 85
			case "Memory":
				host.Sample.Metrics[1].Value = 85
			case "Disk":
				host.Sample.Metrics[4].Value = 15
			case "Container":
				host.Sample.Containers = []protocol.Container{{ID: "abc", Name: "app", State: "exited"}}
			case "Collection":
				host.Sample.Errors = []string{"Docker unavailable"}
			case "Contact / measurements":
				host.Seen = now.Add(-time.Hour)
				host.Sample.At = host.Seen
			}
			var messages []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Content         string `json:"content"`
					AllowedMentions struct {
						Parse []string `json:"parse"`
					} `json:"allowed_mentions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body.AllowedMentions.Parse) != 0 {
					t.Error("Discord mentions enabled")
				}
				messages = append(messages, body.Content)
				w.WriteHeader(204)
			}))
			defer server.Close()
			client := server.Client()
			for i, at := range []time.Time{now, now.Add(time.Minute), now.Add(3 * time.Minute), now.Add(4 * time.Minute)} {
				if err := h.reconcileNotifications(context.Background(), host, at, client, server.URL); err != nil {
					t.Fatal(err)
				}
				if i < 2 && len(messages) != 0 {
					t.Fatal("notified before pending period")
				}
				if i == 2 {
					// Rebuild the hub against the same database, as after a gateway restart.
					var err error
					h, err = New(h.config, h.store)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(messages) != 1 || !strings.Contains(messages[0], category) {
				t.Fatalf("activation: %v", messages)
			}
			healthy := notificationHost(now)
			if category == "Container" {
				healthy.Sample.Containers = []protocol.Container{{ID: "replacement", Name: "app", State: "running"}}
			}
			for _, at := range []time.Time{now.Add(5 * time.Minute), now.Add(8 * time.Minute)} {
				if err := h.reconcileNotifications(context.Background(), healthy, at, client, server.URL); err != nil {
					t.Fatal(err)
				}
			}
			if len(messages) != 2 || !strings.Contains(messages[1], "RECOVERED") {
				t.Fatalf("recovery: %v", messages)
			}
		})
	}
}

func TestDiscordRetriesRateLimitsWithoutLeakingWebhook(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	host := notificationHost(now)
	host.Sample.Metrics[0].Value = 95
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	for _, at := range []time.Time{now, now.Add(3 * time.Minute), now.Add(4 * time.Minute), now.Add(6 * time.Minute), now.Add(7 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), host, at, server.Client(), server.URL+"/secret-token"); err != nil && strings.Contains(err.Error(), "secret-token") {
			t.Fatal("webhook leaked")
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want one rate limit and one successful retry", calls)
	}
}

func TestDiscordSecretValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord")
	for _, raw := range []string{"http://discord.com/api/webhooks/123/token", "https://evil.test/api/webhooks/123/token", "https://discord.com/not-a-webhook", "https://discord.com/api/webhooks/123/token?wait=true"} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := discordWebhook(path); err == nil || strings.Contains(err.Error(), raw) {
			t.Fatalf("unsafe secret handling: %q, %v", raw, err)
		}
	}
	if err := os.WriteFile(path, []byte("https://discord.com/api/webhooks/123/a-long-placeholder-webhook-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := discordWebhook(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := discordWebhook(path); err == nil {
		t.Fatal("accepted public webhook file")
	}
}

func TestDiscordCooldownAppliesAcrossHostsAndSignals(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	first, second := notificationHost(now), notificationHost(now)
	first.Sample.Metrics[0].Value = 95
	second.ID = "other"
	second.Sample.Metrics[1].Value = 95
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
	}))
	defer server.Close()
	for _, at := range []time.Time{now, now.Add(3 * time.Minute), now.Add(4 * time.Minute)} {
		for _, host := range []store.Host{first, second} {
			_ = h.reconcileNotifications(context.Background(), host, at, server.Client(), server.URL)
		}
	}
	if calls != 1 {
		t.Fatalf("rate-limited webhook received %d calls", calls)
	}
}

func TestNotificationSeverityChangesAndMissingDataNeverRecover(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	host := notificationHost(now)
	host.Sample.Metrics[0].Value = 85
	host.Sample.Containers = []protocol.Container{{ID: "app1", Name: "app", State: "exited"}}
	var messages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Content string }
		json.NewDecoder(r.Body).Decode(&v)
		messages = append(messages, v.Content)
		w.WriteHeader(204)
	}))
	defer server.Close()
	for _, at := range []time.Time{now, now.Add(3 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), host, at, server.Client(), server.URL); err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 2 {
		t.Fatal(messages)
	}
	host.Sample.Metrics[0].Value = 95
	for _, at := range []time.Time{now.Add(4 * time.Minute), now.Add(7 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), host, at, server.Client(), server.URL); err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 3 || !strings.Contains(messages[2], "CRITICAL") {
		t.Fatal(messages)
	}
	host.Sample.Containers = nil
	host.Sample.Metrics = nil
	host.Seen = now.Add(-time.Hour)
	host.Sample.At = host.Seen
	for _, at := range []time.Time{now.Add(8 * time.Minute), now.Add(11 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), host, at, server.Client(), server.URL); err != nil {
			t.Fatal(err)
		}
	}
	for _, message := range messages {
		if strings.Contains(message, "RECOVERED") {
			t.Fatal("unknown data generated recovery", messages)
		}
	}
}

func TestStaleLegacyContainerSnapshotCannotCompleteRecovery(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.AlertPendingSeconds = 300
	now := time.Now()
	host := notificationHost(now)
	host.Sample.Containers = []protocol.Container{{ID: "app", Name: "app", State: "exited"}}
	var messages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct{ Content string }
		json.NewDecoder(r.Body).Decode(&v)
		messages = append(messages, v.Content)
		w.WriteHeader(204)
	}))
	defer server.Close()
	for _, at := range []time.Time{now, now.Add(6 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), host, at, server.Client(), server.URL); err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 1 {
		t.Fatal(messages)
	}
	host.Sample.Containers[0].State = "running"
	if err := h.reconcileNotifications(context.Background(), host, now.Add(7*time.Minute), server.Client(), server.URL); err != nil {
		t.Fatal(err)
	}
	// Expire real observation timestamps, not merely the lifecycle's injected clock.
	host.Seen = now.Add(-time.Hour)
	host.Sample.At = host.Seen
	for _, at := range []time.Time{now.Add(13 * time.Minute), now.Add(20 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), host, at, server.Client(), server.URL); err != nil {
			t.Fatal(err)
		}
	}
	for _, message := range messages {
		if strings.Contains(message, "RECOVERED") {
			t.Fatal("stale legacy state reported recovery", messages)
		}
	}
}
