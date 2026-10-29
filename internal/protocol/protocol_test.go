package protocol

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestContainerExitCodeBoundaryAndLegacySamples(t *testing.T) {
	for _, tc := range []struct {
		data  string
		valid bool
	}{
		{`{"id":"job","state":"exited"}`, true},
		{`{"id":"job","state":"exited","exit_code":0}`, true},
		{`{"id":"job","state":"exited","exit_code":255}`, true},
		{`{"id":"job","state":"exited","exit_code":-1}`, false},
		{`{"id":"job","state":"exited","exit_code":256}`, false},
	} {
		t.Run(tc.data, func(t *testing.T) {
			var c Container
			if err := json.Unmarshal([]byte(tc.data), &c); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			s := Sample{Version: 1, HostID: "node", ID: "sample", At: now, Containers: []Container{c}}
			if err := s.Validate("node", now, time.Hour, time.Minute, 10); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestSampleTrustBoundary(t *testing.T) {
	now := time.Now()
	s := Sample{Version: 1, HostID: "n", ID: "s", At: now, Metrics: []Metric{{Name: "cpu_percent", Value: 50, Weight: 1}}}
	if err := s.Validate("n", now, 24*time.Hour, 5*time.Minute, 10); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Sample){func(s *Sample) { s.HostID = "foreign" }, func(s *Sample) { s.Version = 2 }, func(s *Sample) { s.Metrics[0].Value = math.NaN() }, func(s *Sample) { s.At = now.Add(time.Hour) }, func(s *Sample) { s.Metrics[0].Name = "custom" }, func(s *Sample) { s.Metrics[0].Weight = 0 }} {
		copy := s
		copy.Metrics = append([]Metric(nil), s.Metrics...)
		mutate(&copy)
		if copy.Validate("n", now, 24*time.Hour, 5*time.Minute, 10) == nil {
			t.Fatal("accepted invalid sample")
		}
	}
}

func TestRejectValuesThatOverflowAggregates(t *testing.T) {
	now := time.Now()
	s := Sample{Version: 1, HostID: "n", ID: "s", At: now, Metrics: []Metric{{Name: "memory_used_bytes", Value: 1e308, Weight: 60}}}
	if s.Validate("n", now, time.Hour, time.Minute, 10) == nil {
		t.Fatal("overflowing aggregate accepted")
	}
}
