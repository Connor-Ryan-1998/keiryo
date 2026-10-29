// Package otlp implements the supported OTLP metrics boundary and explicit dashboard mapping.
package otlp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"mime"
	"sort"
	"strings"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
)

type Point struct {
	Name, Unit, Entity, ContainerName, DisplayName string
	Series, Group                                  string
	Attributes                                     map[string]string
	At, Start                                      time.Time
	Value                                          float64
	Counter                                        bool
	Delta                                          bool
	Rate                                           *float64 `json:",omitempty"`
}
type Measurement struct {
	Metric protocol.Metric
	At     time.Time
	Source string
}
type IngestResult struct{ Accepted, Rejected int64 }

var ErrHostIdentity = errors.New("resource host.id must match authenticated host")

func Decode(body []byte, contentType string) (*collectorpb.ExportMetricsServiceRequest, error) {
	req := new(collectorpb.ExportMetricsServiceRequest)
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, errors.New("invalid content type")
	}
	switch media {
	case "application/x-protobuf":
		err = (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(body, req)
	case "application/json":
		err = (protojson.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 32}).Unmarshal(body, req)
	default:
		return nil, errors.New("unsupported content type")
	}
	return req, err
}

// attributes retains typed identity separately from the strings used by the mapping.
func attributes(in []*commonpb.KeyValue, limit int) (map[string]string, map[string]string, error) {
	values, identity := map[string]string{}, map[string]string{}
	if len(in) > limit {
		return nil, nil, errors.New("attribute limit exceeded")
	}
	for _, a := range in {
		if a == nil || a.Key == "" || len(a.Key) > 128 || a.Value == nil {
			return nil, nil, errors.New("invalid attribute")
		}
		if _, ok := identity[a.Key]; ok {
			return nil, nil, errors.New("duplicate attribute")
		}
		switch v := a.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			if len(v.StringValue) > 512 {
				return nil, nil, errors.New("attribute value too long")
			}
			values[a.Key] = v.StringValue
		case *commonpb.AnyValue_IntValue, *commonpb.AnyValue_BoolValue:
		case *commonpb.AnyValue_DoubleValue:
			if math.IsNaN(v.DoubleValue) || math.IsInf(v.DoubleValue, 0) {
				return nil, nil, errors.New("invalid attribute number")
			}
		default:
			return nil, nil, errors.New("only scalar attributes supported")
		}
		data, err := protojson.Marshal(a.Value)
		if err != nil {
			return nil, nil, err
		}
		identity[a.Key] = string(data)
	}
	return values, identity, nil
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func Normalize(req *collectorpb.ExportMetricsServiceRequest, host string, now time.Time, age, future time.Duration, max int) ([]Point, int64, error) {
	var out []Point
	var rejected, total int64
	for _, r := range req.ResourceMetrics {
		if r == nil {
			return nil, 0, errors.New("invalid resource")
		}
		rv, ri, err := attributes(r.GetResource().GetAttributes(), 64)
		if err != nil {
			return nil, 0, err
		}
		// Empty envelopes contain no observations and need no host identity.
		hasPoints := false
		for _, s := range r.ScopeMetrics {
			for _, m := range s.GetMetrics() {
				if count(m) > 0 {
					hasPoints = true
				}
			}
		}
		if hasPoints && (rv["host.id"] != host || !config.ValidID(rv["host.id"])) {
			return nil, 0, ErrHostIdentity
		}
		if len(rv["host.name"]) > 256 {
			return nil, 0, errors.New("display name too long")
		}
		for _, s := range r.ScopeMetrics {
			if s == nil || len(s.GetScope().GetName()) > 256 || len(s.GetScope().GetVersion()) > 64 || len(s.SchemaUrl) > 512 || len(r.SchemaUrl) > 512 {
				return nil, 0, errors.New("invalid scope")
			}
			_, si, err := attributes(s.GetScope().GetAttributes(), 16)
			if err != nil {
				return nil, 0, err
			}
			scope := []any{ri, s.GetScope().GetName(), s.GetScope().GetVersion(), si, s.SchemaUrl, r.SchemaUrl}
			for _, m := range s.Metrics {
				n := count(m)
				total += int64(n)
				if total > int64(max) {
					return nil, 0, errors.New("point limit exceeded")
				}
				var data []*metricpb.NumberDataPoint
				counter, delta := false, false
				switch d := m.GetData().(type) {
				case *metricpb.Metric_Gauge:
					data = d.Gauge.GetDataPoints()
				case *metricpb.Metric_Sum:
					data = d.Sum.GetDataPoints()
					counter = d.Sum.IsMonotonic
					delta = d.Sum.AggregationTemporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
					if d.Sum.AggregationTemporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_UNSPECIFIED {
						rejected += int64(n)
						continue
					}
				default:
					rejected += int64(n)
					continue
				}
				for _, d := range data {
					if d == nil {
						rejected++
						continue
					}
					av, ai, err := attributes(d.Attributes, 32)
					if err != nil {
						return nil, 0, err
					}
					p := Point{Name: m.Name, Unit: m.Unit, Attributes: av, Value: d.GetAsDouble(), Counter: counter, Delta: delta, DisplayName: rv["host.name"], ContainerName: rv["container.name"]}
					if v, ok := d.Value.(*metricpb.NumberDataPoint_AsInt); ok {
						p.Value = float64(v.AsInt)
					}
					if d.TimeUnixNano > math.MaxInt64 || d.StartTimeUnixNano > math.MaxInt64 || d.Value == nil || d.Flags != 0 {
						rejected++
						continue
					}
					p.At = time.Unix(0, int64(d.TimeUnixNano)).UTC()
					if d.StartTimeUnixNano != 0 {
						p.Start = time.Unix(0, int64(d.StartTimeUnixNano)).UTC()
					}
					if d.TimeUnixNano == 0 || p.At.Before(now.Add(-age)) || p.At.After(now.Add(future)) || p.Start.After(p.At) || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) || p.Value < 0 || p.Value > 1e30 || !valid(&p, rv) {
						rejected++
						continue
					}
					p.Series = hash([]any{scope, m.Name, m.Unit, counter, delta, ai})
					extra := map[string]string{}
					for k, v := range ai {
						extra[k] = v
					}
					switch m.Name {
					case "system.cpu.time", "system.cpu.utilization":
						delete(extra, "cpu")
						delete(extra, "state")
					case "system.paging.usage":
						delete(extra, "device")
						delete(extra, "state")
					case "system.filesystem.usage":
						delete(extra, "mountpoint")
						delete(extra, "state")
					case "system.disk.io", "system.network.io":
						delete(extra, "device")
						delete(extra, "direction")
					}
					family := m.Name
					if strings.HasPrefix(family, "system.memory.") || family == "system.linux.memory.available" || family == "keiryo.memory.available" {
						family = "memory"
					}
					p.Group = hash([]any{scope, family, extra})
					out = append(out, p)
				}
			}
		}
	}
	return out, rejected, nil
}
func count(m *metricpb.Metric) int {
	switch d := m.GetData().(type) {
	case *metricpb.Metric_Gauge:
		return len(d.Gauge.GetDataPoints())
	case *metricpb.Metric_Sum:
		return len(d.Sum.GetDataPoints())
	case *metricpb.Metric_Histogram:
		return len(d.Histogram.GetDataPoints())
	case *metricpb.Metric_ExponentialHistogram:
		return len(d.ExponentialHistogram.GetDataPoints())
	case *metricpb.Metric_Summary:
		return len(d.Summary.GetDataPoints())
	}
	return 0
}
func valid(p *Point, resource map[string]string) bool {
	unit := ""
	wantCounter := false
	switch p.Name {
	case "system.uptime":
		unit = "s"
	case "system.cpu.load_average.1m":
		unit = "{thread}"
	case "system.memory.limit", "system.linux.memory.available", "keiryo.memory.available":
		unit = "By"
	case "system.paging.usage":
		unit = "By"
		if p.Attributes["state"] != "free" && p.Attributes["state"] != "used" {
			return false
		}
	case "system.filesystem.usage":
		unit = "By"
		p.Entity = p.Attributes["mountpoint"]
		if !strings.HasPrefix(p.Entity, "/") || !oneOf(p.Attributes["state"], "free", "used", "reserved") {
			return false
		}
	case "system.cpu.time", "system.cpu.utilization":
		unit = "s"
		wantCounter = true
		if p.Name == "system.cpu.utilization" {
			unit = "1"
			wantCounter = false
		}
		if p.Attributes["cpu"] == "" {
			p.Attributes["cpu"] = "all"
		}
		if !oneOf(p.Attributes["state"], "idle", "wait", "user", "system", "nice", "interrupt", "softirq", "steal") {
			return false
		}
		if !wantCounter && p.Value > 1 {
			return false
		}
	case "system.disk.io":
		unit = "By"
		wantCounter = true
		p.Entity = p.Attributes["device"]
		if !oneOf(p.Attributes["direction"], "read", "write") {
			return false
		}
	case "system.network.io":
		unit = "By"
		wantCounter = true
		p.Entity = p.Attributes["device"]
		if !oneOf(p.Attributes["direction"], "receive", "transmit") {
			return false
		}
	case "container.cpu.utilization":
		unit = "1"
		p.Entity = resource["container.id"]
	case "container.cpu.usage.total":
		unit = "ns"
		wantCounter = true
		p.Entity = resource["container.id"]
	case "container.memory.usage.total", "container.memory.usage.limit":
		unit = "By"
		p.Entity = resource["container.id"]
	default:
		return false
	}
	if strings.HasPrefix(p.Name, "container.") && !config.ValidID(p.Entity) {
		return false
	}
	if (p.Name == "system.disk.io" || p.Name == "system.network.io") && p.Entity == "" {
		return false
	}
	if len(p.Entity) > 512 || p.Unit != unit || p.Counter != wantCounter {
		return false
	}
	if p.Delta && (!p.Counter || p.Start.IsZero() || !p.At.After(p.Start)) {
		return false
	}
	return true
}
func oneOf(v string, values ...string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}

// Project operates on observations grouped by source and timestamp, never arrival time.
func Project(points []Point) []Measurement {
	type key struct {
		group, entity string
		at            int64
	}
	groups := map[key][]Point{}
	for _, p := range points {
		k := key{p.Group, p.Entity, p.At.UnixNano()}
		groups[k] = append(groups[k], p)
	}
	var out []Measurement
	for k, ps := range groups {
		add := func(name string, value, weight float64) {
			if value >= 0 && value <= 1e30 && !math.IsNaN(value) && !math.IsInf(value, 0) {
				out = append(out, Measurement{protocol.Metric{Name: name, Entity: k.entity, Value: value, Weight: weight}, ps[0].At, k.group})
			}
		}
		values := map[string]float64{}
		states := map[string]float64{}
		total, idle := 0.0, 0.0
		cpuSeen := map[string]bool{}
		cpuIdle := map[string]bool{}
		cpuStates := map[string]map[string]bool{}
		var cpuWindow time.Time
		cpuValid := true
		for _, p := range ps {
			values[p.Name] = p.Value
			switch p.Name {
			case "system.uptime":
				add("uptime_seconds", p.Value, 1)
			case "system.cpu.load_average.1m":
				add("load1", p.Value, 1)
			case "container.cpu.utilization":
				add("container_cpu_percent", p.Value, 1)
			case "container.cpu.usage.total":
				if p.Rate != nil {
					add("container_cpu_percent", *p.Rate/1e9*100, interval(p))
				}
			case "container.memory.usage.total":
				add("container_memory_bytes", p.Value, 1)
			case "container.memory.usage.limit":
				add("container_memory_limit_bytes", p.Value, 1)
			case "system.memory.limit":
				add("memory_total_bytes", p.Value, 1)
			case "system.linux.memory.available", "keiryo.memory.available":
				add("memory_available_bytes", p.Value, 1)
			case "system.filesystem.usage", "system.paging.usage":
				states[p.Attributes["state"]] += p.Value
			case "system.disk.io", "system.network.io":
				if p.Rate != nil {
					name := map[string]string{"read": "disk_read_bytes_s", "write": "disk_write_bytes_s", "receive": "network_rx_bytes_s", "transmit": "network_tx_bytes_s"}[p.Attributes["direction"]]
					add(name, *p.Rate, interval(p))
				}
			case "system.cpu.time", "system.cpu.utilization":
				cpu := p.Attributes["cpu"]
				state := p.Attributes["state"]
				cpuSeen[cpu] = true
				if cpuStates[cpu] == nil {
					cpuStates[cpu] = map[string]bool{}
				}
				if cpuStates[cpu][state] {
					cpuValid = false
				}
				cpuStates[cpu][state] = true
				v := p.Value
				if p.Counter {
					if p.Rate == nil || p.Start.IsZero() {
						cpuValid = false
						continue
					}
					if cpuWindow.IsZero() {
						cpuWindow = p.Start
					} else if !cpuWindow.Equal(p.Start) {
						cpuValid = false
					}
					v = *p.Rate
				}
				total += v
				if p.Attributes["state"] == "idle" || p.Attributes["state"] == "wait" {
					idle += v
				}
				if p.Attributes["state"] == "idle" {
					cpuIdle[p.Attributes["cpu"]] = true
				}
			}
		}
		switch ps[0].Name {
		case "system.cpu.time", "system.cpu.utilization":
			if ps[0].Name == "system.cpu.utilization" {
				total = float64(len(cpuSeen))
			} else {
				// Cumulative totals need the full supported Linux state set, not just idle.
				for _, states := range cpuStates {
					if len(states) != 8 {
						cpuValid = false
					}
				}
			}
			if cpuValid && len(cpuSeen) == len(cpuIdle) && total > 0 {
				add("cpu_percent", 100*(1-idle/total), 1)
			}
		case "system.filesystem.usage":
			free, f := states["free"]
			used, u := states["used"]
			reserved, r := states["reserved"]
			if f && u && r {
				add("filesystem_total_bytes", free+used+reserved, 1)
				add("filesystem_free_bytes", free, 1)
			}
		case "system.paging.usage":
			free, f := states["free"]
			used, u := states["used"]
			if f && u {
				add("swap_used_bytes", used, 1)
				add("swap_total_bytes", free+used, 1)
			}
		}
		if total, ok := values["system.memory.limit"]; ok {
			available, found := values["system.linux.memory.available"]
			if !found {
				available, found = values["keiryo.memory.available"]
			}
			if found && available <= total {
				add("memory_used_bytes", total-available, 1)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			if out[i].Metric.Name == out[j].Metric.Name {
				return out[i].Metric.Entity < out[j].Metric.Entity
			}
			return out[i].Metric.Name < out[j].Metric.Name
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}
func interval(p Point) float64 {
	if !p.Start.IsZero() {
		if v := p.At.Sub(p.Start).Seconds(); v > 0 && v <= 86400 {
			return v
		}
	}
	return 1
}
