package hub

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"houselab/monitoring/internal/store"
)

func hostCard(body, id string) string {
	return regexp.MustCompile(`(?s)<a class="host-card [a-z]+" href="/hosts/` + id + `">.*?</a>`).FindString(body)
}

func TestDashboardShowsPendingUntilBreachIsSustained(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	ingestCPU(t, h, "node", 95)

	// A fresh breach is pending: not red, and the fleet summary is unchanged.
	body := serveUI(t, h, "/")
	card := hostCard(body, "node")
	if !strings.Contains(card, `host-card healthy`) || !strings.Contains(card, "pending") {
		t.Fatalf("fresh breach card: %s", card)
	}
	if !strings.Contains(body, `<span class="pill pending">`) || !strings.Contains(body, "critical if it lasts 2m") {
		t.Fatal("pending condition not explained")
	}
	if !strings.Contains(body, "within thresholds") {
		t.Fatal("pending breach changed the fleet summary")
	}

	// The tracker saw the breach start three minutes ago and still sees it now.
	h.notificationPass(context.Background(), nil, "", now.Add(-3*time.Minute))
	h.notificationPass(context.Background(), nil, "", now)
	if card := hostCard(serveUI(t, h, "/"), "node"); !strings.Contains(card, `host-card critical`) {
		t.Fatalf("sustained breach card: %s", card)
	}
}

func TestPendingSnapshotShowsElapsedTime(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	ingestCPU(t, h, "node", 95)
	h.notificationPass(context.Background(), nil, "", now.Add(-40*time.Second))
	if body := serveUI(t, h, "/"); !strings.Contains(body, "critical if it lasts 2m (40s so far)") {
		t.Fatal("pending elapsed time missing")
	}
}

func track(t *testing.T, h *Hub, host store.Host, at time.Time) {
	t.Helper()
	if _, err := h.trackConditions(context.Background(), host.ID, h.fleetRow(host).Signals, h.stateFresh(host.Sample), at); err != nil {
		t.Fatal(err)
	}
}

func TestSpikeThatClearsRestartsThePendingPeriod(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	host := notificationHost(now)
	host.Sample.Metrics[0].Value = 95
	track(t, h, host, now.Add(-4*time.Minute))
	host.Sample.Metrics[0].Value = 10
	track(t, h, host, now.Add(-3*time.Minute))
	host.Sample.Metrics[0].Value = 96
	track(t, h, host, now.Add(-time.Minute))
	row := h.displayRow(context.Background(), host, now)
	if row.Health != "healthy" || signal(row, "cpu").State != "pending" {
		t.Fatalf("new spike inherited earlier breach: health=%s cpu=%+v", row.Health, signal(row, "cpu"))
	}
}

func TestEscalationKeepsConfirmedSeverityWhileNewOneIsPending(t *testing.T) {
	h := testHub(t, "proxy")
	now := time.Now()
	host := notificationHost(now)
	host.Sample.Metrics[0].Value = 85
	track(t, h, host, now.Add(-5*time.Minute))
	track(t, h, host, now.Add(-2*time.Minute))
	host.Sample.Metrics[0].Value = 95
	track(t, h, host, now.Add(-30*time.Second))
	row := h.displayRow(context.Background(), host, now)
	cpu := signal(row, "cpu")
	if row.Health != "warning" || cpu.State != "warning" || !strings.Contains(cpu.Detail, "critical if it lasts 2m (30s so far)") {
		t.Fatalf("escalation: health=%s cpu=%+v", row.Health, cpu)
	}
	if row.CPUView.State != "warning" {
		t.Fatalf("CPU meter state %s", row.CPUView.State)
	}
}

func TestZeroPendingShowsBreachImmediately(t *testing.T) {
	h := testHub(t, "proxy")
	h.config.AlertPendingSeconds = 0
	ingestCPU(t, h, "node", 95)
	if card := hostCard(serveUI(t, h, "/"), "node"); !strings.Contains(card, `host-card critical`) {
		t.Fatalf("zero pending card: %s", card)
	}
}

func TestTrackingRunsWithoutDiscord(t *testing.T) {
	h := testHub(t, "proxy")
	ingestCPU(t, h, "node", 95)
	h.notificationPass(context.Background(), nil, "", time.Now())
	states, err := h.store.Notifications(context.Background(), "node")
	if err != nil || states["cpu"].State != "critical" {
		t.Fatalf("tracker state %+v %v", states, err)
	}
}
