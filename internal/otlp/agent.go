package otlp

import (
	"errors"
	"houselab/monitoring/internal/protocol"
	"runtime"
	"sort"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func keyValues(values map[string]string) []*commonpb.KeyValue {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*commonpb.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: values[k]}}})
	}
	return out
}

func EncodeSample(s protocol.Sample) (*collectorpb.ExportMetricsServiceRequest, error) {
	if s.At.IsZero() || s.At.UnixNano() < 0 {
		return nil, errors.New("invalid observation time")
	}
	req := new(collectorpb.ExportMetricsServiceRequest)
	names := map[string]string{}
	for _, c := range s.Containers {
		names[c.ID] = c.Name
	}
	resources := map[string]*metricpb.ScopeMetrics{}
	add := func(entity, name, unit string, value float64, attrs map[string]string, counter bool, start uint64) {
		scope := resources[entity]
		if scope == nil {
			r := map[string]string{"host.id": s.HostID, "host.name": s.DisplayName}
			if entity != "" {
				r["container.id"] = entity
				r["container.name"] = names[entity]
			}
			scope = &metricpb.ScopeMetrics{Scope: &commonpb.InstrumentationScope{Name: "keiryo.agent", Version: protocol.AgentVersion}}
			resources[entity] = scope
			req.ResourceMetrics = append(req.ResourceMetrics, &metricpb.ResourceMetrics{Resource: &resourcepb.Resource{Attributes: keyValues(r)}, ScopeMetrics: []*metricpb.ScopeMetrics{scope}})
		}
		point := &metricpb.NumberDataPoint{Attributes: keyValues(attrs), TimeUnixNano: uint64(s.At.UnixNano()), StartTimeUnixNano: start, Value: &metricpb.NumberDataPoint_AsDouble{AsDouble: value}}
		m := &metricpb.Metric{Name: name, Unit: unit}
		if counter {
			m.Data = &metricpb.Metric_Sum{Sum: &metricpb.Sum{IsMonotonic: true, AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metricpb.NumberDataPoint{point}}}
		} else {
			m.Data = &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{point}}}
		}
		// One metric per name/resource; append dimensional points to its existing descriptor.
		for _, old := range scope.Metrics {
			if old.Name == name {
				if counter {
					old.GetSum().DataPoints = append(old.GetSum().DataPoints, point)
				} else {
					old.GetGauge().DataPoints = append(old.GetGauge().DataPoints, point)
				}
				return
			}
		}
		scope.Metrics = append(scope.Metrics, m)
	}
	totalFS, freeFS := map[string]float64{}, map[string]float64{}
	swapTotal, swapUsed := 0.0, 0.0
	hasSwap := false
	for _, m := range s.Metrics {
		switch m.Name {
		case "uptime_seconds":
			add("", "system.uptime", "s", m.Value, nil, false, 0)
		case "load1":
			add("", "system.cpu.load_average.1m", "{thread}", m.Value, nil, false, 0)
		case "memory_total_bytes":
			add("", "system.memory.limit", "By", m.Value, nil, false, 0)
		case "memory_available_bytes":
			name := "keiryo.memory.available"
			if runtime.GOOS == "linux" {
				name = "system.linux.memory.available"
			}
			add("", name, "By", m.Value, nil, false, 0)
		case "swap_total_bytes":
			swapTotal = m.Value
			hasSwap = true
		case "swap_used_bytes":
			swapUsed = m.Value
		case "filesystem_total_bytes":
			totalFS[m.Entity] = m.Value
		case "filesystem_free_bytes":
			freeFS[m.Entity] = m.Value
		case "container_cpu_percent":
			add(m.Entity, "container.cpu.utilization", "1", m.Value, nil, false, 0)
		case "container_memory_bytes":
			add(m.Entity, "container.memory.usage.total", "By", m.Value, nil, false, 0)
		case "container_memory_limit_bytes":
			add(m.Entity, "container.memory.usage.limit", "By", m.Value, nil, false, 0)
		}
	}
	if hasSwap && swapUsed <= swapTotal {
		add("", "system.paging.usage", "By", swapUsed, map[string]string{"state": "used"}, false, 0)
		add("", "system.paging.usage", "By", swapTotal-swapUsed, map[string]string{"state": "free"}, false, 0)
	}
	mounts := make([]string, 0, len(totalFS))
	for mount := range totalFS {
		mounts = append(mounts, mount)
	}
	sort.Strings(mounts)
	for _, mount := range mounts {
		total := totalFS[mount]
		free, ok := freeFS[mount]
		if !ok || free > total {
			continue
		}
		for _, state := range []string{"used", "free", "reserved"} {
			value := 0.0
			if state == "used" {
				value = total - free
			}
			if state == "free" {
				value = free
			}
			add("", "system.filesystem.usage", "By", value, map[string]string{"mountpoint": mount, "state": state}, false, 0)
		}
	}
	for _, m := range s.Raw {
		var start uint64
		if !m.Start.IsZero() && m.Start.UnixNano() > 0 {
			start = uint64(m.Start.UnixNano())
		}
		add("", m.Name, m.Unit, m.Value, m.Attributes, m.Counter, start)
	}
	return req, nil
}
