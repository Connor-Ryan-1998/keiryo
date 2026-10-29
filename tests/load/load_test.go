package load

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/hub"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
)

// BenchmarkFleet includes authentication, JSON validation, SQLite, history and UI reads.
// It measures an in-process synthetic workload, not a production fleet capacity.
func BenchmarkFleet(b *testing.B) {
	for _, hosts := range []int{4, 16, 64} {
		b.Run(fmt.Sprintf("hosts-%d", hosts), func(b *testing.B) {
			dir := b.TempDir()
			path := filepath.Join(dir, "hub.db")
			db, err := store.Open(path)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			c := config.DefaultHub()
			c.AuthMode = "proxy"
			c.TrustedProxies = []string{"127.0.0.1/32"}
			c.PublicURL = "https://monitor.example.com"
			for i := 0; i < hosts; i++ {
				id := fmt.Sprintf("node-%d", i)
				token := filepath.Join(dir, id+".token")
				if err = os.WriteFile(token, []byte(fmt.Sprintf("%032d", i)), 0600); err != nil {
					b.Fatal(err)
				}
				c.Agents[id] = token
			}
			h, err := hub.New(c, db)
			if err != nil {
				b.Fatal(err)
			}
			api, ui := h.AgentHandler(), h.UIHandler()
			p, _ := process.NewProcess(int32(os.Getpid()))
			before, _ := p.Times()
			now := time.Now().UTC()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				host := i % hosts
				id := fmt.Sprintf("node-%d", host)
				s := protocol.Sample{Version: 1, HostID: id, ID: fmt.Sprintf("sample-%d", i), At: now, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 25, Weight: 1}, {Name: "memory_used_bytes", Value: 1 << 30, Weight: 1}}}
				for j := 0; j < 8; j++ {
					container := fmt.Sprintf("container-%d", j)
					s.Containers = append(s.Containers, protocol.Container{ID: container, Name: container, State: "running"})
					s.Metrics = append(s.Metrics, protocol.Metric{Name: "container_cpu_percent", Entity: container, Value: 5, Weight: 1}, protocol.Metric{Name: "container_memory_bytes", Entity: container, Value: 64 << 20, Weight: 1})
				}
				data, _ := json.Marshal(s)
				r := httptest.NewRequest("POST", "/v1/samples", bytes.NewReader(data))
				r.Header.Set("Authorization", "Bearer "+fmt.Sprintf("%032d", host))
				r.Header.Set("X-Host-ID", id)
				w := httptest.NewRecorder()
				api.ServeHTTP(w, r)
				if w.Code != 204 {
					b.Fatalf("ingestion %d %s", w.Code, w.Body.String())
				}
				if i%60 == 0 {
					r = httptest.NewRequest("GET", "/", nil)
					r.RemoteAddr = "127.0.0.1:1234"
					r.Header.Set("X-authentik-username", "bench")
					w = httptest.NewRecorder()
					ui.ServeHTTP(w, r)
					if w.Code != 200 || !strings.Contains(w.Body.String(), "Fleet") {
						b.Fatal("UI read failed")
					}
					if _, err = db.History(context.Background(), id, now.Add(-time.Hour), now.Add(time.Minute), true, 300); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			after, _ := p.Times()
			if before != nil && after != nil {
				b.ReportMetric(((after.User+after.System)-(before.User+before.System))*1e9/float64(b.N), "cpu-ns/upload")
			}
			if memory, err := p.MemoryInfo(); err == nil {
				b.ReportMetric(float64(memory.RSS)/(1<<20), "process-RSS-MiB")
			}
			if info, err := os.Stat(path); err == nil {
				b.ReportMetric(float64(info.Size())/float64(b.N), "main-db-bytes/upload")
			}
		})
	}
}
