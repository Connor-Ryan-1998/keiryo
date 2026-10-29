//go:build integration

package otlp_test

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/hub"
	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/store"
	"houselab/monitoring/tests/agenttest"
)

//go:embed collector.yaml
var collectorConfig []byte

func TestStockCollectorBothDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hc := config.DefaultHub()
	hc.Agents = map[string]string{}
	for _, host := range []string{"stock", "agent"} {
		token := filepath.Join(dir, host+".token")
		if err = os.WriteFile(token, []byte(strings.Repeat(host, 16)), 0600); err != nil {
			t.Fatal(err)
		}
		hc.Agents[host] = token
	}
	h, err := hub.New(hc, db)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var failures []string
	captured := map[string]bool{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		h.AgentHandler().ServeHTTP(recorder, r)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/metrics" {
			response := new(collectorpb.ExportMetricsServiceResponse)
			if err := proto.Unmarshal(recorder.Body.Bytes(), response); recorder.Code != 200 || err != nil || response.GetPartialSuccess() != nil {
				failures = append(failures, fmt.Sprintf("%s: HTTP %d response %q", r.Header.Get("X-Host-ID"), recorder.Code, recorder.Body.Bytes()))
			}
			if r.Header.Get("X-Host-ID") == "stock" {
				decoded := body
				if r.Header.Get("Content-Encoding") == "gzip" {
					gz, e := gzip.NewReader(bytes.NewReader(body))
					if e == nil {
						decoded, _ = io.ReadAll(gz)
						gz.Close()
					}
				}
				req, e := otlp.Decode(decoded, r.Header.Get("Content-Type"))
				if e == nil {
					for _, rm := range req.ResourceMetrics {
						for _, sm := range rm.ScopeMetrics {
							for _, m := range sm.Metrics {
								if !captured[m.Name] {
									captured[m.Name] = true
									data, _ := protojson.Marshal(m)
									t.Logf("stock schema %s", data)
								}
							}
						}
					}
				}
			}
		}
		for k, values := range recorder.Header() {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(recorder.Code)
		w.Write(recorder.Body.Bytes())
	}))
	defer api.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := listener.Addr().String()
	listener.Close()
	path := filepath.Join(dir, "collector.yaml")
	if err = os.WriteFile(path, collectorConfig, 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "collector.log")
	logs, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	image := os.Getenv("COLLECTOR_IMAGE")
	cmd := exec.CommandContext(ctx, "/otelcol-contrib", "--config="+path)
	cmd.Env = append(os.Environ(), "OTLP_HUB_URL="+api.URL, "AGENT_OTLP_LISTEN="+listen, "STOCK_TOKEN="+strings.Repeat("stock", 16), "AGENT_TOKEN="+strings.Repeat("agent", 16), "COLLECTOR_IMAGE="+image)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { cancel(); <-exited }()
	ac := config.DefaultAgent()
	ac.HostID = "agent"
	ac.HubURL = api.URL
	ac.OTLPMetricsURL = "http://" + listen + "/v1/metrics"
	ac.CredentialFile = hc.Agents["agent"]
	ac.IntervalSeconds = 2
	ac.PollSeconds = 60
	agentDone := agenttest.Start(t, ctx, ac)
	defer func() { cancel(); <-agentDone }()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			data, _ := os.ReadFile(logPath)
			mu.Lock()
			t.Errorf("Collector interoperability timed out; failures=%v, log=%s", failures, data)
			mu.Unlock()
			return
		case <-ticker.C:
			mu.Lock()
			failed := len(failures) > 0
			mu.Unlock()
			if failed {
				data, _ := os.ReadFile(logPath)
				mu.Lock()
				t.Errorf("OTLP failures: %v\nCollector: %s", failures, data)
				mu.Unlock()
				return
			}
			ready := true
			for _, host := range []string{"stock", "agent"} {
				snapshot, e := db.Host(ctx, host)
				if e != nil {
					t.Fatal(e)
				}
				found := map[string]bool{}
				for _, m := range snapshot.Sample.Metrics {
					found[m.Name] = true
				}
				for _, name := range []string{"cpu_percent", "memory_total_bytes", "memory_used_bytes", "filesystem_total_bytes", "network_rx_bytes_s", "container_memory_bytes"} {
					if !found[name] {
						ready = false
					}
				}
				if host == "stock" && snapshot.Sample.LogFetch {
					t.Fatal("stock Collector advertised private logs")
				}
				if host == "agent" && !snapshot.Sample.LogFetch {
					ready = false
				}
			}
			if ready {
				t.Log("stock hostmetrics/docker_stats → hub and agent → stock OTLP receiver → hub verified")
				return
			}
		}
	}
}
