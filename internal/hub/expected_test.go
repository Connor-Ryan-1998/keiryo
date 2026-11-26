package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
)

func signal(row fleetHost, key string) *condition {
	for i := range row.Signals {
		if row.Signals[i].Key == key {
			return &row.Signals[i]
		}
	}
	return nil
}

func TestExpectedContainerMissingOnlyWhenStateIsFresh(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.ExpectedContainers = []config.ExpectedContainer{{Host: "node", Name: "traefik", Severity: "critical"}, {Host: "other", Name: "db", Severity: "warning"}}
	now := time.Now()
	host := notificationHost(now)
	host.Sample.Containers = []protocol.Container{{ID: "a", Name: "app", State: "running"}}

	got := signal(h.fleetRow(host), "container/traefik")
	if got == nil || got.State != "critical" || !strings.Contains(got.Detail, "traefik") || !strings.Contains(got.Detail, "missing") {
		t.Fatalf("missing expected container: %+v", got)
	}
	if signal(h.fleetRow(host), "container/db") != nil {
		t.Fatal("expectation for another host applied")
	}
	if row := h.fleetRow(host); row.Health != "critical" {
		t.Fatalf("host health %s", row.Health)
	}

	host.Sample.Containers = append(host.Sample.Containers, protocol.Container{ID: "b", Name: "traefik", State: "running"})
	if got := signal(h.fleetRow(host), "container/traefik"); got == nil || got.State != "healthy" {
		t.Fatalf("present expected container: %+v", got)
	}

	// Stale state proves nothing: no missing-container condition, the host is simply unavailable.
	stale := notificationHost(now.Add(-time.Hour))
	stale.Seen = now
	if got := signal(h.fleetRow(stale), "container/traefik"); got != nil {
		t.Fatalf("stale state reported a missing container: %+v", got)
	}
}

func TestExpectedContainerRecoveryNotifies(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.ExpectedContainers = []config.ExpectedContainer{{Host: "node", Name: "traefik", Severity: "critical"}}
	var messages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		messages = append(messages, string(b[:n]))
		w.WriteHeader(204)
	}))
	defer server.Close()
	now := time.Now()
	missing := notificationHost(now)
	for _, at := range []time.Time{now, now.Add(3 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), missing, at, server.Client(), server.URL); err != nil {
			t.Fatal(err)
		}
	}
	back := notificationHost(now)
	back.Sample.Containers = []protocol.Container{{ID: "new", Name: "traefik", State: "running"}}
	for _, at := range []time.Time{now.Add(4 * time.Minute), now.Add(7 * time.Minute)} {
		if err := h.reconcileNotifications(context.Background(), back, at, server.Client(), server.URL); err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 2 || !strings.Contains(messages[0], "CRITICAL") || !strings.Contains(messages[0], "traefik: missing") || !strings.Contains(messages[1], "RECOVERED") {
		t.Fatalf("messages %v", messages)
	}
}
