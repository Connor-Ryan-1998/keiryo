package otlp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/hub"
	"houselab/monitoring/internal/store"
)

func TestRustAgentBackupFileThroughDurableOTLPToGoHub(t *testing.T) {
	binary, err := filepath.Abs("../../agent-rust/target/debug/examples/outbox_fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(binary); err != nil {
		if os.Getenv("MONITORING_REQUIRE_RUST_FIXTURE") == "1" {
			t.Fatal(err)
		}
		t.Skip("build Rust outbox fixture first")
	}
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
	c := config.DefaultHub()
	c.AuthMode, c.PublicURL, c.TrustedProxies = "proxy", "https://monitor.example", []string{"127.0.0.1/32"}
	c.Agents = map[string]string{"node": token}
	c.BackupJobs = []config.BackupJob{{Host: "node", Job: "daily", MaxAgeSeconds: 108000}}
	h, err := hub.New(c, db)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(h.AgentHandler())
	defer api.Close()
	statusPath := filepath.Join(dir, "status.json")
	settings, _ := json.Marshal(map[string]any{"host_id": "node", "hub_url": api.URL, "credential_file": token, "queue_database": filepath.Join(dir, "queue.redb"), "docker_socket": "/nonexistent/docker.sock", "backup_status_files": map[string]string{"daily": statusPath}})
	path := filepath.Join(dir, "agent.json")
	if err = os.WriteFile(path, settings, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(commands ...map[string]any) []json.RawMessage {
		t.Helper()
		var input, output, stderr bytes.Buffer
		for _, command := range commands {
			if err := json.NewEncoder(&input).Encode(command); err != nil {
				t.Fatal(err)
			}
		}
		command := exec.Command(binary, path)
		command.Stdin, command.Stdout, command.Stderr = &input, &output, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("Rust fixture: %v %s", err, stderr.String())
		}
		decoder := json.NewDecoder(&output)
		var results []json.RawMessage
		for decoder.More() {
			var result json.RawMessage
			if err := decoder.Decode(&result); err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
		}
		return results
	}
	for _, result := range []string{"success", "failure"} {
		now := time.Now()
		last := now.Unix() - 10
		if result == "failure" {
			last = now.Unix() - 100
		}
		status, _ := json.Marshal(map[string]any{"version": 1, "started_at": now.Unix() - 20, "finished_at": now.Unix() - 10, "last_success_at": last, "result": result})
		if err = os.WriteFile(statusPath, status, 0600); err != nil {
			t.Fatal(err)
		}
		collection := run(map[string]any{"op": "collect", "now_ns": now.UnixNano()})[0]
		run(map[string]any{"op": "enqueue", "collection": collection, "now_ns": now.UnixNano()}, map[string]any{"op": "drain", "now_ns": now.UnixNano()})
		host, err := db.Host(context.Background(), "node")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]float64{}
		for _, metric := range host.Sample.Metrics {
			if metric.Entity == "daily" {
				got[metric.Name] = metric.Value
			}
		}
		want := 1.0
		if result == "failure" {
			want = 2
		}
		if got["backup_status_valid"] != 1 || got["backup_result"] != want || got["backup_last_success"] != float64(last) {
			t.Fatalf("%s: %v", result, got)
		}
		request := httptest.NewRequest("GET", "/?view=conditions", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("X-authentik-username", "test")
		response := httptest.NewRecorder()
		h.UIHandler().ServeHTTP(response, request)
		text := "daily: last successful backup"
		if result == "failure" {
			text = "daily: latest backup failed"
		}
		if response.Code != 200 || !strings.Contains(response.Body.String(), text) {
			t.Fatalf("backup condition not rendered: %d %s", response.Code, text)
		}
	}
}
