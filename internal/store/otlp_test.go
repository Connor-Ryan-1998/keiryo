package store

import (
	"context"
	"encoding/json"
	"fmt"
	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/protocol"
	"path/filepath"
	"testing"
	"time"
)

func otlpStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}
func ingestPoints(t *testing.T, s *Store, points ...otlp.Point) otlp.IngestResult {
	t.Helper()
	r, err := s.IngestOTLP(context.Background(), "node", points, time.Now(), 300)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func gauge(name, series string, at time.Time, value float64) otlp.Point {
	return otlp.Point{Name: name, Series: series, Group: "source", At: at, Value: value}
}
func counter(at time.Time, value float64) otlp.Point {
	return otlp.Point{Name: "system.network.io", Series: "rx", Group: "net", Entity: "eth0", At: at, Value: value, Counter: true, Attributes: map[string]string{"direction": "receive"}}
}
func reading(t *testing.T, s *Store, name string) protocol.Metric {
	t.Helper()
	h, err := s.Host(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range h.Sample.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("missing %s: %+v", name, h.Sample)
	return protocol.Metric{}
}
func TestOTLPRetryAndRebatchDedup(t *testing.T) {
	s, _ := otlpStore(t)
	at := time.Now().UTC()
	a := gauge("system.uptime", "a", at, 10)
	b := gauge("system.memory.limit", "b", at, 1000)
	ingestPoints(t, s, a, b)
	ingestPoints(t, s, b)
	ingestPoints(t, s, a)
	points, err := s.History(context.Background(), "node", at.Add(-10*time.Minute), at.Add(time.Minute), true, 300)
	if err != nil || len(points) != 2 {
		t.Fatal(points, err)
	}
	for _, p := range points {
		if p.Count != 1 {
			t.Fatal("retry counted twice", p)
		}
	}
	a.Value = 20
	if r := ingestPoints(t, s, a); r.Rejected != 1 {
		t.Fatal("conflicting duplicate not rejected", r)
	}
	if got := reading(t, s, "uptime_seconds").Value; got != 10 {
		t.Fatal(got)
	}
}
func TestOTLPSplitUpdatesMerge(t *testing.T) {
	s, _ := otlpStore(t)
	at := time.Now().UTC()
	ingestPoints(t, s, gauge("system.memory.limit", "total", at, 1000))
	ingestPoints(t, s, gauge("system.linux.memory.available", "available", at, 400))
	if m := reading(t, s, "memory_used_bytes"); m.Value != 600 || !m.ObservedAt.Equal(at) {
		t.Fatal(m)
	}
	ingestPoints(t, s, gauge("system.uptime", "up", at.Add(time.Minute), 120))
	if m := reading(t, s, "memory_used_bytes"); m.Value != 600 || !m.ObservedAt.Equal(at) {
		t.Fatal("other update changed freshness", m)
	}
}
func TestOTLPCounterResetAndRestart(t *testing.T) {
	s, path := otlpStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	ingestPoints(t, s, counter(at, 100))
	h, _ := s.Host(context.Background(), "node")
	if len(h.Sample.Metrics) != 0 {
		t.Fatal("first counter became zero", h)
	}
	s.Close()
	var err error
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ingestPoints(t, s, counter(at.Add(10*time.Second), 300))
	if got := reading(t, s, "network_rx_bytes_s").Value; got != 20 {
		t.Fatal(got)
	}
	ingestPoints(t, s, counter(at.Add(20*time.Second), 1))
	m := reading(t, s, "network_rx_bytes_s")
	if !m.ObservedAt.Equal(at.Add(10 * time.Second)) {
		t.Fatal("reset emitted zero", m)
	}
	ingestPoints(t, s, counter(at.Add(30*time.Second), 51))
	if got := reading(t, s, "network_rx_bytes_s").Value; got != 5 {
		t.Fatal(got)
	}
	p := counter(at.Add(40*time.Second), 151)
	p.Start = at.Add(35 * time.Second)
	ingestPoints(t, s, p)
	if m := reading(t, s, "network_rx_bytes_s"); !m.ObservedAt.Equal(at.Add(30 * time.Second)) {
		t.Fatal("new epoch used old baseline", m)
	}
}
func TestOTLPDelayedCounterDoesNotRewind(t *testing.T) {
	s, _ := otlpStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	ingestPoints(t, s, counter(at, 100))
	ingestPoints(t, s, counter(at.Add(20*time.Second), 300))
	ingestPoints(t, s, counter(at.Add(10*time.Second), 150))
	if got := reading(t, s, "network_rx_bytes_s"); got.Value != 15 || got.Weight != 10 {
		t.Fatal("successor interval was not corrected", got)
	}
	ingestPoints(t, s, counter(at.Add(30*time.Second), 500))
	if got := reading(t, s, "network_rx_bytes_s").Value; got != 20 {
		t.Fatal(got)
	}
}

func TestOTLPDelayedCounterCorrectsSummaryAndReset(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "late intermediate", true: "late reset"}[reset], func(t *testing.T) {
			s, _ := otlpStore(t)
			at := time.Now().UTC().Truncate(5 * time.Minute).Add(-4 * time.Minute)
			ingestPoints(t, s, counter(at, 100))
			ingestPoints(t, s, counter(at.Add(20*time.Second), 300))
			value := 150.0
			if reset {
				value = 400
			}
			ingestPoints(t, s, counter(at.Add(10*time.Second), value))
			p, err := s.History(context.Background(), "node", at.Add(-time.Minute), at.Add(time.Minute), true, 300)
			if err != nil || len(p) != 1 {
				t.Fatal(p, err)
			}
			want, count := 10.0, 2
			if reset {
				want, count = 30, 1
			}
			if p[0].Average != want || p[0].Count != count {
				t.Fatal("overlapping or reset interval remained in summary", p)
			}
			latest := reading(t, s, "network_rx_bytes_s")
			wantAt := at.Add(20 * time.Second)
			wantRate := 15.0
			if reset {
				wantAt = at.Add(10 * time.Second)
				wantRate = 30
			}
			if !latest.ObservedAt.Equal(wantAt) || latest.Value != wantRate {
				t.Fatal(latest)
			}
		})
	}
	s, _ := otlpStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	ingestPoints(t, s, counter(at, 100))
	ingestPoints(t, s, counter(at.Add(20*time.Second), 300))
	late := counter(at.Add(10*time.Second), 1)
	late.Start = at.Add(5 * time.Second)
	ingestPoints(t, s, late)
	h, _ := s.Host(context.Background(), "node")
	if len(h.Sample.Metrics) != 0 {
		t.Fatal("invalidated successor remained live", h.Sample.Metrics)
	}
}
func TestOTLPDeltaIntervals(t *testing.T) {
	s, _ := otlpStore(t)
	at := time.Now().UTC()
	p := counter(at, 200)
	p.Delta = true
	p.Start = at.Add(-10 * time.Second)
	ingestPoints(t, s, p)
	if m := reading(t, s, "network_rx_bytes_s"); m.Value != 20 || m.Weight != 10 {
		t.Fatal(m)
	}
}
func TestOTLPMigrationPreservesHistory(t *testing.T) {
	s, path := otlpStore(t)
	at := time.Now().UTC()
	legacy := protocol.Sample{Version: 1, HostID: "node", ID: "old", At: at, Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 25, Weight: 1}}}
	if err := s.Ingest(context.Background(), legacy, at, 300); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var err error
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ingestPoints(t, s, gauge("system.uptime", "new", at, 12))
	p, err := s.History(context.Background(), "node", at.Add(-time.Minute), at.Add(time.Minute), false, 300)
	if err != nil || len(p) != 2 {
		t.Fatal(p, err)
	}
}
func TestOTLPAmbiguousStreamsAndCleanup(t *testing.T) {
	s, _ := otlpStore(t)
	at := time.Now().UTC().Add(-time.Hour)
	a := gauge("system.uptime", "a", at, 10)
	ingestPoints(t, s, a)
	b := a
	b.Series = "b"
	b.Group = "different scope"
	if r := ingestPoints(t, s, b); r.Rejected != 1 {
		t.Fatal("ambiguous stream accepted", r)
	}
	if err := s.Maintain(context.Background(), time.Now(), time.Minute, 24*time.Hour, 1<<30); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM otlp_observations`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestOTLPNanosecondHistoryAndPartialGroupCorrection(t *testing.T) {
	s, _ := otlpStore(t)
	at := time.Now().UTC().Truncate(time.Second).Add(100 * time.Millisecond)
	ingestPoints(t, s, gauge("system.uptime", "up", at, 1), gauge("system.uptime", "up", at.Add(100*time.Millisecond), 2))
	p, err := s.History(context.Background(), "node", at.Add(-time.Second), at.Add(time.Second), false, 300)
	if err != nil || len(p) != 2 || !p[0].At.Equal(at) || !p[1].At.Equal(at.Add(100*time.Millisecond)) {
		t.Fatal(p, err)
	}
	// A split CPU group initially has idle/user, then receives another busy state.
	for i, point := range []otlp.Point{
		{Name: "system.cpu.utilization", Group: "cpu", At: at, Value: .5, Attributes: map[string]string{"cpu": "0", "state": "idle"}},
		{Name: "system.cpu.utilization", Group: "cpu", At: at, Value: .25, Attributes: map[string]string{"cpu": "0", "state": "user"}},
		{Name: "system.cpu.utilization", Group: "cpu", At: at, Value: .25, Attributes: map[string]string{"cpu": "0", "state": "system"}},
	} {
		point.Series = []string{"idle", "user", "system"}[i]
		ingestPoints(t, s, point)
	}
	if m := reading(t, s, "cpu_percent"); m.Value != 50 {
		t.Fatal(m)
	}
	summaries, err := s.History(context.Background(), "node", at.Add(-10*time.Minute), at.Add(time.Minute), true, 300)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range summaries {
		if p.Name == "cpu_percent" && (p.Count != 1 || p.Average != 50 || p.Minimum != 50 || p.Maximum != 50) {
			t.Fatal("partial CPU counted multiple times", p)
		}
	}
}

func cpuCounters(at, epoch time.Time, idle, user float64) []otlp.Point {
	var points []otlp.Point
	for _, state := range []string{"idle", "wait", "user", "system", "nice", "interrupt", "softirq", "steal"} {
		value := 0.0
		if state == "idle" {
			value = idle
		}
		if state == "user" {
			value = user
		}
		points = append(points, otlp.Point{Name: "system.cpu.time", Series: state, Group: "cpu", At: at, Start: epoch, Value: value, Counter: true, Attributes: map[string]string{"cpu": "all", "state": state}})
	}
	return points
}
func TestOTLPCumulativeCPUPartialBatchesRemainUnavailable(t *testing.T) {
	s, _ := otlpStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	epoch := at.Add(-time.Hour)
	ingestPoints(t, s, cpuCounters(at, epoch, 100, 100)...)
	ingestPoints(t, s, cpuCounters(at.Add(10*time.Second), epoch, 108, 102)[:1]...)
	host, _ := s.Host(context.Background(), "node")
	if len(host.Sample.Metrics) != 0 {
		t.Fatal("idle-only counter batch became CPU 0", host.Sample.Metrics)
	}
	ingestPoints(t, s, cpuCounters(at.Add(20*time.Second), epoch, 110, 110)...)
	host, _ = s.Host(context.Background(), "node")
	if len(host.Sample.Metrics) != 0 {
		t.Fatal("different baseline windows became CPU", host.Sample.Metrics)
	}
	ingestPoints(t, s, cpuCounters(at.Add(30*time.Second), epoch, 118, 112)...)
	if m := reading(t, s, "cpu_percent"); m.Value < 19.999 || m.Value > 20.001 {
		t.Fatal(m)
	}
	ingestPoints(t, s, cpuCounters(at.Add(10*time.Second), epoch, 108, 102)[1:]...)
	history, err := s.History(context.Background(), "node", at, at.Add(time.Minute), false, 300)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range history {
		if p.Name == "cpu_percent" && p.At.Equal(at.Add(20*time.Second)) {
			found = true
			if p.Average < 79.999 || p.Average > 80.001 {
				t.Fatal(p)
			}
		}
	}
	if !found {
		t.Fatal("late complete batch did not repair successor", history)
	}
}

func TestOTLPLatestRetentionPreservesPrivateState(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprint(private), func(t *testing.T) {
			s, _ := otlpStore(t)
			now := time.Now().UTC()
			old := now.Add(-2 * time.Hour)
			ingestPoints(t, s, otlp.Point{Name: "container.memory.usage.total", Series: "old-container", Group: "docker", Entity: "old", ContainerName: "old", At: old, Value: 10})
			ingestPoints(t, s, gauge("system.uptime", "up", now, 100))
			if private {
				if err := s.IngestState(context.Background(), protocol.AgentState{HostID: "node", ID: "private", At: old, Containers: []protocol.Container{{ID: "job", Name: "job", State: "exited", ExitCode: new(int)}}}, now, 300); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Maintain(context.Background(), now, time.Hour, 24*time.Hour, 1<<30); err != nil {
				t.Fatal(err)
			}
			h, _ := s.Host(context.Background(), "node")
			if len(h.Sample.Metrics) != 1 || h.Sample.Metrics[0].Name != "uptime_seconds" {
				t.Fatal("expired latest metrics retained", h.Sample.Metrics)
			}
			if private {
				if len(h.Sample.Containers) != 1 || h.Sample.Containers[0].ID != "job" || h.Sample.Containers[0].ExitCode == nil {
					t.Fatal("private state pruned", h.Sample)
				}
			} else if len(h.Sample.Containers) != 0 {
				t.Fatal("expired external container retained", h.Sample.Containers)
			}
		})
	}
}
func TestOTLPLatestPersistentBoundsAndContainerChurn(t *testing.T) {
	s, _ := otlpStore(t)
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	sample := protocol.Sample{HostID: "node", At: old, Incremental: true}
	for i := 0; i < 10001; i++ {
		sample.Metrics = append(sample.Metrics, protocol.Metric{Name: "container_memory_bytes", Entity: fmt.Sprintf("old-%d", i), Value: 1, Weight: 1, ObservedAt: old})
	}
	for i := 0; i < 2048; i++ {
		sample.Containers = append(sample.Containers, protocol.Container{ID: fmt.Sprintf("old-%d", i), Name: "old", State: "unknown"})
	}
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO hosts VALUES(?,?,?,?,?)`, "node", "node", now.Unix(), old.Unix(), data); err != nil {
		t.Fatal(err)
	}
	ingestPoints(t, s, otlp.Point{Name: "container.memory.usage.total", Series: "new-container", Group: "docker", Entity: "new", ContainerName: "new", At: now, Value: 10})
	h, _ := s.Host(context.Background(), "node")
	if len(h.Sample.Metrics) > 10000 || len(h.Sample.Containers) > 2048 {
		t.Fatalf("latest state remains unbounded: metrics=%d containers=%d", len(h.Sample.Metrics), len(h.Sample.Containers))
	}
	found := false
	for _, c := range h.Sample.Containers {
		if c.ID == "new" {
			found = true
		}
	}
	if !found {
		t.Fatal("obsolete container names permanently crowded out replacement")
	}
	if err = s.Maintain(context.Background(), now, time.Hour, 24*time.Hour, 1<<30); err != nil {
		t.Fatal(err)
	}
	h, _ = s.Host(context.Background(), "node")
	if len(h.Sample.Metrics) != 1 || len(h.Sample.Containers) != 1 || h.Sample.Containers[0].ID != "new" {
		t.Fatalf("obsolete latest values did not expire: metrics=%d containers=%d", len(h.Sample.Metrics), len(h.Sample.Containers))
	}
}
