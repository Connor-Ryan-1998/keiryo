package hub

import (
	"context"
	"encoding/json"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResourcePolicyControlsConditionsAndStorage(t *testing.T) {
	h := testHub(t, "proxy")
	if err := json.Unmarshal([]byte(`{"alert_policy":{"resources":{"cpu":{"warning":70,"critical":95},"memory":{"warning":60,"critical":85},"disk":{"warning":50,"critical":75}}}}`), &h.config); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	host := store.Host{ID: "node", Seen: now, Sample: protocol.Sample{At: now, Metrics: []protocol.Metric{
		{Name: "cpu_percent", Value: 90},
		{Name: "memory_used_bytes", Value: 65}, {Name: "memory_total_bytes", Value: 100},
		{Name: "filesystem_total_bytes", Entity: "/", Value: 100}, {Name: "filesystem_free_bytes", Entity: "/", Value: 20},
	}}}
	row := h.fleetRow(host)
	for _, want := range []struct{ kind, state string }{{"CPU", "warning"}, {"Memory", "warning"}, {"Disk", "critical"}} {
		found := false
		for _, c := range row.Conditions {
			found = found || c.Kind == want.kind && c.State == want.state
		}
		if !found {
			t.Errorf("missing %s %s: %+v", want.kind, want.state, row.Conditions)
		}
	}
	if row.Health != "critical" || h.volumes(host)[0].State != "critical" {
		t.Fatal("disk policy not reflected in aggregate health/storage")
	}
}

func TestContainerPolicyScopesSuccessfulOneShots(t *testing.T) {
	h := testHub(t, "proxy")
	defaults := h.config
	for _, tc := range []struct {
		name, policy, host, container, want string
	}{
		{"default exited", `{}`, "node", `{"name":"init","state":"exited","exit_code":0}`, "warning"},
		{"successful job", `{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot","failure":"critical"}]}`, "node", `{"name":"init","state":"exited","exit_code":0}`, "healthy"},
		{"failed job", `{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot","failure":"critical"}]}`, "node", `{"name":"init","state":"exited","exit_code":1}`, "critical"},
		{"old agent unknown exit", `{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot","failure":"critical"}]}`, "node", `{"name":"init","state":"exited"}`, "warning"},
		{"created is not completed", `{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot"}]}`, "node", `{"name":"init","state":"created","exit_code":0}`, "warning"},
		{"other host", `{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot"}]}`, "other", `{"name":"init","state":"exited","exit_code":0}`, "warning"},
		{"other container", `{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot"}]}`, "node", `{"name":"app","state":"exited","exit_code":0}`, "warning"},
		{"unhealthy", `{"containers":{"unhealthy":"critical"}}`, "node", `{"name":"app","state":"running","health":"unhealthy"}`, "critical"},
		{"starting", `{"containers":{"starting":"critical"}}`, "node", `{"name":"app","state":"running","health":"starting"}`, "critical"},
		{"stopped", `{"containers":{"stopped":"critical"}}`, "node", `{"name":"app","state":"dead"}`, "critical"},
		{"job still unhealthy", `{"containers":{"unhealthy":"critical"},"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot"}]}`, "node", `{"name":"init","state":"running","health":"unhealthy","exit_code":0}`, "critical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.config = defaults
			if err := json.Unmarshal([]byte(`{"alert_policy":`+tc.policy+`}`), &h.config); err != nil {
				t.Fatal(err)
			}
			var c protocol.Container
			if err := json.Unmarshal([]byte(tc.container), &c); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			host := store.Host{ID: tc.host, Seen: now, Sample: protocol.Sample{At: now, Containers: []protocol.Container{c}, Metrics: []protocol.Metric{
				{Name: "cpu_percent", Value: 10}, {Name: "memory_used_bytes", Value: 10}, {Name: "memory_total_bytes", Value: 100},
				{Name: "filesystem_total_bytes", Entity: "/", Value: 100}, {Name: "filesystem_free_bytes", Entity: "/", Value: 90},
			}}}
			if got := h.fleetRow(host).Health; got != tc.want {
				t.Fatalf("health=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestPolicyRenderingMatchesConditions(t *testing.T) {
	h := testHub(t, "proxy")
	if err := json.Unmarshal([]byte(`{"alert_policy":{"resources":{"cpu":{"warning":95,"critical":99}},"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot"}]}}`), &h.config); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Second)
	var c protocol.Container
	json.Unmarshal([]byte(`{"id":"job","name":"init","state":"exited","exit_code":0}`), &c)
	s := protocol.Sample{Version: 1, HostID: "node", ID: "policy", At: now, Containers: []protocol.Container{c}, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 90, Weight: 1}}}
	if err := h.store.Ingest(context.Background(), s, now, 300); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ url, want string }{
		{"/", `<meter class="healthy" min="0" max="100" value="90">`},
		{"/?view=conditions", `container-chip healthy`},
		{"/?view=resources", `data-resource="cpu"`},
		{"/hosts/node", `data-resource="cpu"`},
	} {
		r := httptest.NewRequest("GET", tc.url, nil)
		r.RemoteAddr = "10.0.0.1:1"
		r.Header.Set("X-authentik-username", "user")
		w := httptest.NewRecorder()
		h.UIHandler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: expected %q, got %d %s", tc.url, tc.want, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "warning ≥80%") {
			t.Error("UI still advertises hard-coded thresholds")
		}
	}
}
