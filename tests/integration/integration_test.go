//go:build integration

package integration

import (
	"context"
	"houselab/monitoring/internal/agent"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/hub"
	"houselab/monitoring/internal/store"
	"houselab/monitoring/tests/agenttest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLinuxDockerHubAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatal("integration must run on Linux with Docker socket mounted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	token := filepath.Join(dir, "node.token")
	if err := os.WriteFile(token, []byte(strings.Repeat("x", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hc := config.DefaultHub()
	hc.AuthMode = "proxy"
	hc.TrustedProxies = []string{"127.0.0.1/32"}
	hc.PublicURL = "https://monitor.example.com"
	hc.Agents = map[string]string{"node": token}
	h, err := hub.New(hc, db)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(h.AgentHandler())
	defer api.Close()
	ac := config.DefaultAgent()
	ac.HostID = "node"
	ac.CredentialFile = token
	ac.HubURL = api.URL
	ac.IntervalSeconds = 1
	ac.PollSeconds = 1
	done := agenttest.Start(t, ctx, ac)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("no valid Linux/Docker sample received")
		case <-ticker.C:
			hosts, err := db.Hosts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(hosts) == 0 {
				continue
			}
			if len(hosts[0].Sample.Containers) == 0 {
				continue
			}
			foundMemory, foundFS := false, false
			for _, m := range hosts[0].Sample.Metrics {
				if m.Name == "memory_total_bytes" && m.Value > 0 {
					foundMemory = true
				}
				if m.Name == "filesystem_total_bytes" && m.Value > 0 {
					foundFS = true
				}
			}
			if !foundMemory || !foundFS {
				// OTLP metrics and private state are delivered independently.
				continue
			}
			d := agent.NewDocker("/var/run/docker.sock")
			text, err := d.Logs(ctx, hosts[0].Sample.Containers[0].ID, 10, 1024)
			if err != nil {
				t.Fatal(err)
			}
			if len(text) > 1024 {
				t.Fatal("unbounded logs")
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			h.UIHandler().ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal("UI auth bypass")
			}
			r = httptest.NewRequest("POST", "/hosts/node/logs/"+hosts[0].Sample.Containers[0].ID, nil)
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("X-authentik-username", "integration")
			r.Header.Set("Origin", hc.PublicURL)
			w = httptest.NewRecorder()
			h.UIHandler().ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "<pre>") || strings.Contains(w.Body.String(), "timed out") || strings.Contains(w.Body.String(), "unavailable") {
				t.Fatalf("hub/agent log flow failed: %d %s", w.Code, w.Body.String())
			}
			cancel()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("agent did not stop")
			}
			return
		}
	}
}
