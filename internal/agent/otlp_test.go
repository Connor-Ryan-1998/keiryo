package agent

import (
	"context"
	"errors"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/protocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOTLPExportResponseContract(t *testing.T) {
	for _, tc := range []struct {
		status            int
		body              string
		permanent, failed bool
	}{
		{200, "", false, false}, {200, "bad", true, true}, {200, `{"partialSuccess":{"rejectedDataPoints":"1","errorMessage":"unsupported"}}`, false, false},
		{400, "", true, true}, {401, "", true, true}, {403, "", true, true}, {404, "", true, true}, {413, "", true, true}, {415, "", true, true}, {500, "", true, true},
		{429, "", false, true}, {502, "", false, true}, {503, "", false, true}, {504, "", false, true},
	} {
		t.Run(http.StatusText(tc.status)+tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/metrics" || r.Header.Get("Content-Type") != "application/x-protobuf" {
					t.Error(r.URL, r.Header)
				}
				body, _ := io.ReadAll(r.Body)
				req, err := otlp.Decode(body, "application/x-protobuf")
				if err != nil || len(req.ResourceMetrics) != 1 {
					t.Error(req, err)
				}
				if strings.HasPrefix(tc.body, "{") {
					w.Header().Set("Content-Type", "application/json")
				} else {
					w.Header().Set("Content-Type", "application/x-protobuf")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			a := &Agent{Config: config.Agent{HostID: "node", HubURL: srv.URL}, client: srv.Client()}
			err := a.exportMetrics(context.Background(), protocol.Sample{HostID: "node", At: time.Now(), Metrics: []protocol.Metric{{Name: "uptime_seconds", Value: 100}}})
			if (err != nil) != tc.failed || errors.Is(err, errRejected) != tc.permanent {
				t.Fatalf("err=%v permanent=%v", err, tc.permanent)
			}
		})
	}
}
func TestOTLPExportBoundedResponseAndRetryAfter(t *testing.T) {
	body := ""
	status := 503
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Retry-After", "99999999")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	a := &Agent{Config: config.Agent{HubURL: srv.URL}, client: srv.Client()}
	sample := protocol.Sample{HostID: "node", At: time.Now()}
	err := a.exportMetrics(context.Background(), sample)
	var retry *retryAfterError
	if !errors.As(err, &retry) || retry.after != 5*time.Minute {
		t.Fatal(err)
	}
	status = 200
	body = strings.Repeat("x", (4<<20)+1)
	if err := a.exportMetrics(context.Background(), sample); !errors.Is(err, errRejected) {
		t.Fatal("oversized response retried", err)
	}
}
func TestIndependentMetricsAndStateDelivery(t *testing.T) {
	for _, metricsDown := range []bool{false, true} {
		t.Run(map[bool]string{false: "state down", true: "metrics down"}[metricsDown], func(t *testing.T) {
			var metrics, states atomic.Int32
			metricsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metrics.Add(1)
				w.Header().Set("Content-Type", "application/x-protobuf")
				if metricsDown {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(200)
				}
			}))
			defer metricsServer.Close()
			hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/state" {
					t.Error("unexpected private request", r.URL.Path)
				}
				states.Add(1)
				if metricsDown {
					w.WriteHeader(204)
				} else {
					w.WriteHeader(503)
				}
			}))
			defer hubServer.Close()
			c := config.DefaultAgent()
			c.HubURL = hubServer.URL
			c.OTLPMetricsURL = metricsServer.URL + "/v1/metrics"
			c.HostID = "node"
			c.PollSeconds = 60
			c.IntervalSeconds = 1
			collector := NewCollector(c)
			collector.Docker = nil
			a := &Agent{Config: c, client: hubServer.Client(), collector: collector}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()
			deadline := time.NewTicker(10 * time.Millisecond)
			defer deadline.Stop()
			for metrics.Load() < 2 || states.Load() < 2 {
				select {
				case <-ctx.Done():
					t.Fatalf("delivery blocked: metrics=%d state=%d", metrics.Load(), states.Load())
				case <-deadline.C:
				}
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCollectorPreservesRawCounters(t *testing.T) {
	c := NewCollector(config.DefaultAgent())
	c.Docker = nil
	s := c.Collect(context.Background())
	cpu, network := false, false
	for _, p := range s.Raw {
		if !p.Counter || p.Value < 0 {
			t.Fatal(p)
		}
		if p.Name == "system.cpu.time" {
			cpu = true
		}
		if p.Name == "system.network.io" {
			network = true
		}
	}
	if !cpu || !network {
		t.Fatal("source counters missing", s.Raw, s.Errors)
	}
}
