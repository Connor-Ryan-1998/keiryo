package otlp

import (
	"houselab/monitoring/internal/protocol"
	"testing"
	"time"
)

func TestEncodeSampleResourcesAndRawCounters(t *testing.T) {
	at := time.Now().UTC()
	s := protocol.Sample{HostID: "node", DisplayName: "Node", At: at, Containers: []protocol.Container{{ID: "abc", Name: "web"}}, Metrics: []protocol.Metric{{Name: "memory_total_bytes", Value: 1000}, {Name: "memory_available_bytes", Value: 400}, {Name: "container_cpu_percent", Entity: "abc", Value: 150}}, Raw: []protocol.RawMetric{{Name: "system.network.io", Unit: "By", Value: 12345, Counter: true, Attributes: map[string]string{"device": "eth0", "direction": "receive"}, Start: at.Add(-time.Hour)}}}
	req, err := EncodeSample(s)
	if err != nil {
		t.Fatal(err)
	}
	points, n, err := normalize(t, req)
	if err != nil || n != 0 || len(points) != 4 {
		t.Fatal(points, n, err)
	}
	found := false
	for _, p := range points {
		if p.Name == "system.network.io" {
			found = true
			if !p.Counter || p.Value != 12345 || p.Entity != "eth0" || !p.At.Equal(at) {
				t.Fatal(p)
			}
		}
		if p.Name == "container.cpu.utilization" && (p.Entity != "abc" || p.ContainerName != "web" || p.Value != 150) {
			t.Fatal(p)
		}
	}
	if !found {
		t.Fatal("raw network counter missing")
	}
}
