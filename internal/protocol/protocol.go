package protocol

import (
	"errors"
	"houselab/monitoring/internal/config"
	"math"
	"time"
)

const Version = 1
const AgentVersion = "0.1.0"

type Metric struct {
	Name       string    `json:"name"`
	Entity     string    `json:"entity,omitempty"`
	Value      float64   `json:"value"`
	Weight     float64   `json:"weight"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}
type Container struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Health     string    `json:"health,omitempty"`
	Restarts   int       `json:"restarts"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
}
type Sample struct {
	Version      int         `json:"version"`
	AgentVersion string      `json:"agent_version"`
	HostID       string      `json:"host_id"`
	DisplayName  string      `json:"display_name"`
	ID           string      `json:"id"`
	At           time.Time   `json:"at"`
	Metrics      []Metric    `json:"metrics"`
	Containers   []Container `json:"containers"`
	Errors       []string    `json:"errors"`
	Raw          []RawMetric `json:"-"`
	Incremental  bool        `json:"incremental,omitempty"`
	StateAt      time.Time   `json:"state_at,omitzero"`
	LogFetch     bool        `json:"log_fetch,omitempty"`
}

// RawMetric retains source counters for OTLP export, never the legacy JSON wire format.
type RawMetric struct {
	Name, Unit string
	Value      float64
	Attributes map[string]string
	Counter    bool
	Start      time.Time
}

// AgentState is the private control-plane snapshot; numeric metrics use OTLP.
type AgentState struct {
	Version      int         `json:"version"`
	HostID       string      `json:"host_id"`
	ID           string      `json:"id"`
	At           time.Time   `json:"at"`
	DisplayName  string      `json:"display_name,omitempty"`
	AgentVersion string      `json:"agent_version"`
	Containers   []Container `json:"containers"`
	Errors       []string    `json:"errors"`
	LogFetch     bool        `json:"log_fetch"`
}

func (s Sample) State() AgentState {
	return AgentState{Version: s.Version, HostID: s.HostID, ID: s.ID, At: s.At, DisplayName: s.DisplayName, AgentVersion: s.AgentVersion, Containers: s.Containers, Errors: s.Errors, LogFetch: true}
}

var Names = map[string]bool{"cpu_percent": true, "load1": true, "memory_used_bytes": true, "memory_available_bytes": true, "memory_total_bytes": true, "swap_used_bytes": true, "swap_total_bytes": true, "uptime_seconds": true, "filesystem_total_bytes": true, "filesystem_free_bytes": true, "disk_read_bytes_s": true, "disk_write_bytes_s": true, "network_rx_bytes_s": true, "network_tx_bytes_s": true, "container_cpu_percent": true, "container_memory_bytes": true, "container_memory_limit_bytes": true}

func (s Sample) Validate(host string, now time.Time, rawAge, future time.Duration, max int) error {
	if s.Version != Version || s.HostID != host || !config.ValidID(s.ID) || len(s.DisplayName) > 256 || len(s.AgentVersion) > 64 || s.At.IsZero() || s.At.Before(now.Add(-rawAge)) || s.At.After(now.Add(future)) {
		return errors.New("invalid identity, version, or sample time")
	}
	if len(s.Metrics) > max || len(s.Containers) > max || len(s.Errors) > 64 {
		return errors.New("sample entity limit exceeded")
	}
	seen := map[string]bool{}
	for _, m := range s.Metrics {
		key := m.Name + "\x00" + m.Entity
		if !Names[m.Name] || len(m.Entity) > 512 || seen[key] || math.IsNaN(m.Value) || math.IsInf(m.Value, 0) || m.Value < 0 || m.Value > 1e30 || m.Weight <= 0 || m.Weight > 86400 || math.IsNaN(m.Weight) || math.IsInf(m.Weight, 0) {
			return errors.New("invalid or duplicate measurement")
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, c := range s.Containers {
		if !config.ValidID(c.ID) || seen[c.ID] || len(c.Name) > 256 || len(c.State) > 32 || len(c.Health) > 32 || c.Restarts < 0 || (c.ExitCode != nil && (*c.ExitCode < 0 || *c.ExitCode > 255)) {
			return errors.New("invalid container")
		}
		seen[c.ID] = true
	}
	for _, e := range s.Errors {
		if len(e) > 512 {
			return errors.New("collection error too long")
		}
	}
	return nil
}

type LogRequest struct {
	ID          string    `json:"id"`
	HostID      string    `json:"host_id"`
	ContainerID string    `json:"container_id"`
	Lines       int       `json:"lines"`
	Bytes       int       `json:"bytes"`
	Expires     time.Time `json:"expires"`
}
type LogResult struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}
