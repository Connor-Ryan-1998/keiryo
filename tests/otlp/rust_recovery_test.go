package otlp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/shirou/gopsutil/v4/disk"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/hub"
	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
)

func TestRustOutboxMigrationRecoveryAndLostAcknowledgement(t *testing.T) {
	binary, err := filepath.Abs("../../agent-rust/target/debug/examples/outbox_fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binary); err != nil {
		if os.Getenv("MONITORING_REQUIRE_RUST_FIXTURE") == "1" {
			t.Fatal(err)
		}
		t.Skip("run task monitoring:test SUITE=recovery to build the Rust test fixture")
	}
	ctx := context.Background()
	dir := t.TempDir()
	token := filepath.Join(dir, "node.token")
	if err = os.WriteFile(token, []byte(strings.Repeat("a", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hc := config.DefaultHub()
	hc.Agents = map[string]string{"node": token}
	h, err := hub.New(hc, db)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	loseAck := true
	requests := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		h.AgentHandler().ServeHTTP(recorder, r)
		response := new(collectorpb.ExportMetricsServiceResponse)
		if recorder.Code != 200 || proto.Unmarshal(recorder.Body.Bytes(), response) != nil || response.PartialSuccess != nil {
			t.Errorf("incompatible Rust export: %d %s", recorder.Code, recorder.Body.Bytes())
		}
		mu.Lock()
		defer mu.Unlock()
		requests++
		if loseAck {
			loseAck = false
			w.WriteHeader(503)
			return
		}
		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer api.Close()
	path := filepath.Join(dir, "agent.json")
	settings := map[string]any{"host_id": "node", "display_name": "Node", "hub_url": api.URL, "credential_file": token, "queue_database": filepath.Join(dir, "agent.redb")}
	data, _ := json.Marshal(settings)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Hour)
	boot := start.Add(-time.Hour)
	legacy, err := otlp.EncodeSample(protocol.Sample{HostID: "node", DisplayName: "Node", At: start.Add(-time.Minute), Metrics: []protocol.Metric{{Name: "uptime_seconds", Value: 100}}, Raw: []protocol.RawMetric{{Name: "system.network.io", Unit: "By", Value: 100, Counter: true, Start: boot, Attributes: map[string]string{"device": "eth0", "direction": "receive"}}}})
	if err != nil {
		t.Fatal(err)
	}
	legacyBody, _ := proto.Marshal(legacy)
	req := httptest.NewRequest("POST", "/v1/metrics", bytes.NewReader(legacyBody))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Host-ID", "node")
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	out := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal(out.Code)
	}
	runFixture := func(commands []map[string]any) []map[string]any {
		t.Helper()
		var input bytes.Buffer
		for _, command := range commands {
			if err := json.NewEncoder(&input).Encode(command); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(binary, path)
		cmd.Stdin = &input
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("fixture: %v %s", err, stderr.String())
		}
		dec := json.NewDecoder(bytes.NewReader(output))
		var results []map[string]any
		for {
			var result map[string]any
			err := dec.Decode(&result)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if result["error"] != nil {
				t.Fatal(result)
			}
			results = append(results, result)
		}
		return results
	}
	var commands []map[string]any
	for i := 0; i < 60; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		state := map[string]any{"version": 1, "host_id": "node", "id": fmt.Sprintf("sample-%d", i), "at": at, "display_name": "Node", "agent_version": "0.1.0", "containers": []any{}, "errors": []string{}, "log_fetch": false}
		collection := map[string]any{"at_ns": at.UnixNano(), "state": state, "observations": []any{
			map[string]any{"name": "system.uptime", "unit": "s", "value": 160 + i*60}, map[string]any{"name": "system.network.io", "unit": "By", "value": 160 + i*60, "counter": true, "start_ns": boot.UnixNano(), "attributes": map[string]string{"device": "eth0", "direction": "receive"}}}}
		commands = append(commands, map[string]any{"op": "enqueue", "collection": collection, "now_ns": at.UnixNano()})
	}
	commands = append(commands, map[string]any{"op": "stats", "now_ns": now.UnixNano()})
	results := runFixture(commands)
	if len(results) != 61 || results[60]["depth"] != float64(60) {
		t.Fatalf("one-hour backlog missing: %v", results)
	}
	// First delivery commits at the hub but loses its acknowledgement; process exits with the queue intact.
	results = runFixture([]map[string]any{{"op": "drain", "now_ns": now.UnixNano()}, {"op": "stats", "now_ns": now.UnixNano()}})
	if results[1]["depth"] != float64(60) {
		t.Fatalf("ambiguous acknowledgement discarded data: %v", results)
	}
	commands = nil
	for i := 0; i < 60; i++ {
		commands = append(commands, map[string]any{"op": "drain", "now_ns": now.UnixNano()})
	}
	commands = append(commands, map[string]any{"op": "stats", "now_ns": now.UnixNano()})
	results = runFixture(commands)
	if results[60]["depth"] != float64(0) {
		t.Fatal("queue did not drain")
	}
	points, err := db.History(ctx, "node", start.Add(-2*time.Minute), now.Add(time.Minute), false, 1)
	if err != nil {
		t.Fatal(err)
	}
	uptime, rates := 0, 0
	for _, point := range points {
		switch point.Name {
		case "uptime_seconds":
			uptime++
		case "network_rx_bytes_s":
			rates++
			if point.Average != 1 {
				t.Fatalf("replay rate %v", point)
			}
		}
	}
	if uptime != 61 || rates != 60 {
		t.Fatalf("duplicate or lost history: uptime=%d rates=%d", uptime, rates)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 61 {
		t.Fatalf("expected one replay, got %d requests", requests)
	}
}

func TestRustMacDiskDeviceParityWithGoCollector(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS collection check")
	}
	binary, err := filepath.Abs("../../agent-rust/target/debug/examples/outbox_fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binary); err != nil {
		t.Skip("build Rust fixture first")
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err = os.WriteFile(token, []byte(strings.Repeat("a", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.json")
	data, _ := json.Marshal(map[string]any{"host_id": "node", "hub_url": "http://127.0.0.1:9", "credential_file": token, "queue_database": filepath.Join(dir, "q.redb"), "exclude_filesystems": []string{"apfs"}, "docker_socket": "/nonexistent/docker.sock"})
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := disk.IOCountersWithContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, path)
	cmd.Stdin = strings.NewReader("{\"op\":\"collect\",\"now_ns\":1}\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture collection: %v %s", err, output)
	}
	after, err := disk.IOCountersWithContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var collection struct {
		Observations []struct {
			Name       string
			Value      float64
			Attributes map[string]string
		}
	}
	if err = json.Unmarshal(output, &collection); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range collection.Observations {
		if m.Name != "system.disk.io" {
			continue
		}
		device := m.Attributes["device"]
		a, exists := before[device]
		b, existsAfter := after[device]
		if !exists || !existsAfter {
			t.Fatalf("device identity changed: %s", device)
		}
		low, high := a.ReadBytes, b.ReadBytes
		if m.Attributes["direction"] == "write" {
			low, high = a.WriteBytes, b.WriteBytes
		}
		if m.Value < float64(low) || m.Value > float64(high) {
			t.Fatalf("counter mismatch for %s: %v not in [%d,%d]", device, m.Value, low, high)
		}
		seen[device] = true
	}
	if len(seen) == 0 {
		t.Fatal("no device counters despite Go collector success")
	}
}

func TestRustAgentKeepsCurrentStateDuringMetricsOutageAndReplaysAfterKill(t *testing.T) {
	binary, err := filepath.Abs("../../bin/monitoring-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binary); err != nil {
		t.Skip("build native Rust agent first")
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "node.token")
	if err = os.WriteFile(token, []byte(strings.Repeat("a", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	var online atomic.Bool
	var failed atomic.Int32
	var states atomic.Int32
	var replayed atomic.Bool
	var mu sync.Mutex
	var first []byte
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/state":
			states.Add(1)
			w.WriteHeader(204)
		case "/v1/metrics":
			mu.Lock()
			if first == nil {
				first = append([]byte{}, body...)
			}
			same := bytes.Equal(first, body)
			mu.Unlock()
			if !online.Load() {
				failed.Add(1)
				w.WriteHeader(503)
				return
			}
			if same {
				replayed.Store(true)
			}
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(200)
		default:
			w.WriteHeader(204)
		}
	}))
	defer api.Close()
	path := filepath.Join(dir, "agent.json")
	data, _ := json.Marshal(map[string]any{"host_id": "node", "hub_url": api.URL, "credential_file": token, "queue_database": filepath.Join(dir, "agent.redb"), "interval_seconds": 1, "docker_socket": "/nonexistent/docker.sock"})
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	start := func() (*exec.Cmd, <-chan error) {
		t.Helper()
		cmd := exec.Command(binary, "agent", "-config", path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		finished := make(chan struct{})
		go func() { done <- cmd.Wait(); close(finished) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-finished })
		return cmd, done
	}
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("Rust agent did not reach expected state")
	}
	cmd, done := start()
	wait(func() bool { return failed.Load() >= 1 && states.Load() >= 2 })
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	oldStates := states.Load()
	online.Store(true)
	_, _ = start()
	wait(func() bool { return replayed.Load() && states.Load() > oldStates })
}

func TestRustAgentPersistenceFailureIsVisibleWithoutBypassingQueue(t *testing.T) {
	binary, err := filepath.Abs("../../bin/monitoring-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binary); err != nil {
		t.Skip("build Rust agent first")
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err = os.WriteFile(token, []byte(strings.Repeat("a", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	states := make(chan []string, 4)
	var metrics atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			metrics.Add(1)
		}
		if r.URL.Path == "/v1/state" {
			var state protocol.AgentState
			if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
				t.Error(err)
			}
			select {
			case states <- state.Errors:
			default:
			}
		}
		w.WriteHeader(204)
	}))
	defer api.Close()
	path := filepath.Join(dir, "agent.json")
	data, _ := json.Marshal(map[string]any{"host_id": "node", "hub_url": api.URL, "credential_file": token, "queue_database": filepath.Join(dir, "q.redb"), "queue_max_bytes": 1, "interval_seconds": 1, "docker_socket": "/nonexistent/docker.sock"})
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	cmd := exec.Command(binary, "agent", "-config", path)
	cmd.Stderr = logs
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	select {
	case errors := <-states:
		if !strings.Contains(strings.Join(errors, " "), "persistence failed") {
			t.Fatalf("missing state health error: %v", errors)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("no state during persistence failure")
	}
	if metrics.Load() != 0 {
		t.Fatal("metrics bypassed durable storage")
	}
	data, err = os.ReadFile(logs.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"persistence_failures":1`)) {
		t.Fatalf("missing failure count: %s", data)
	}
}
