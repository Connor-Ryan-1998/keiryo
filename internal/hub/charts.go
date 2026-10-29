package hub

import (
	"encoding/json"
	"fmt"
	"houselab/monitoring/internal/store"
	"math"
	"sort"
	"strings"
	"time"
)

type chartSample struct {
	At    int64
	Value float64
}

type chartSeries struct {
	Name    string
	Samples []chartSample
}

type chartView struct {
	Title, Unit, Note, Data, Resource string
	Percent                           bool
}

type statView struct{ Label, Value string }

// chartViews pairs related metrics only at the same timestamp and entity.
// Summary percentages are ratios of bucket averages, not averages of ratios.
func chartViews(points []store.Point, host store.Host) []chartView {
	charts := []chartView{
		{Title: "Host CPU", Unit: "%", Percent: true, Resource: "cpu", Note: "Normalized across all cores"},
		{Title: "Host memory", Unit: "%", Percent: true, Resource: "memory", Note: "Used / total memory"},
		{Title: "Disk usage", Unit: "%", Percent: true, Resource: "disk", Note: "Used capacity per mount"},
		{Title: "Network I/O", Unit: "B/s", Note: "Receive and transmit per interface"},
		{Title: "Container CPU", Unit: "%", Note: "100% equals one core; may exceed 100%"},
		{Title: "Container memory", Unit: "MiB", Note: "Excludes inactive file cache"},
		{Title: "Storage capacity", Unit: "GiB", Note: "Total and free capacity per mount"},
	}
	type key struct {
		at     int64
		entity string
		name   string
	}
	values := make(map[key]float64, len(points))
	for _, p := range points {
		values[key{p.At.UnixMilli(), p.Entity, p.Name}] = p.Average
	}
	names := map[string]string{}
	for _, c := range host.Sample.Containers {
		names[c.ID] = c.Name + " (" + c.ID[:min(12, len(c.ID))] + ")"
	}
	series := make([]map[string][]chartSample, len(charts))
	for i := range series {
		series[i] = map[string][]chartSample{}
	}
	add := func(panel int, label string, at int64, value float64) {
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
			series[panel][label] = append(series[panel][label], chartSample{at, value})
		}
	}
	for _, p := range points {
		at, value := p.At.UnixMilli(), p.Average
		container := names[p.Entity]
		if container == "" {
			container = p.Entity
		}
		switch p.Name {
		case "cpu_percent":
			add(0, "CPU", at, value)
		case "memory_used_bytes":
			if total := values[key{at, p.Entity, "memory_total_bytes"}]; total > 0 {
				add(1, "Memory", at, 100*value/total)
			}
		case "filesystem_free_bytes":
			if total := values[key{at, p.Entity, "filesystem_total_bytes"}]; total > 0 && value <= total {
				add(2, p.Entity, at, 100*(1-value/total))
			}
			add(6, p.Entity+" free", at, value/(1<<30))
		case "filesystem_total_bytes":
			add(6, p.Entity+" total", at, value/(1<<30))
		case "network_rx_bytes_s":
			add(3, p.Entity+" receive", at, value)
		case "network_tx_bytes_s":
			add(3, p.Entity+" transmit", at, value)
		case "container_cpu_percent":
			add(4, container, at, value)
		case "container_memory_bytes":
			add(5, container, at, value/(1<<20))
		}
	}
	for i := range charts {
		out := []chartSeries{}
		for name, samples := range series[i] {
			sort.Slice(samples, func(i, j int) bool { return samples[i].At < samples[j].At })
			out = append(out, chartSeries{name, samples})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		data, _ := json.Marshal(out)
		charts[i].Data = string(data)
	}
	return charts
}

func overview(host store.Host) []statView {
	values := metrics(host)
	cpu := "Unavailable"
	if v, ok := values["cpu_percent\x00"]; ok {
		cpu = fmt.Sprintf("%.1f%%", v)
	}
	memory, disk := "Unavailable", "Unavailable"
	if total := values["memory_total_bytes\x00"]; total > 0 {
		if used, ok := values["memory_used_bytes\x00"]; ok {
			memory = fmt.Sprintf("%.1f%%", 100*used/total)
		}
	}
	if total := values["filesystem_total_bytes\x00/"]; total > 0 {
		if free, ok := values["filesystem_free_bytes\x00/"]; ok && free <= total {
			disk = fmt.Sprintf("%.1f%%", 100*(1-free/total))
		}
	}
	running, other := 0, 0
	for _, c := range host.Sample.Containers {
		if c.State == "running" {
			running++
		} else {
			other++
		}
	}
	containers := "Unavailable"
	if (!host.Sample.Incremental && !host.Sample.At.IsZero()) || !host.Sample.StateAt.IsZero() {
		containers = fmt.Sprintf("%d running / %d other", running, other)
		for _, err := range host.Sample.Errors {
			if strings.HasPrefix(err, "docker") {
				containers += " · collection incomplete"
				break
			}
		}
	}
	return []statView{{"CPU", cpu}, {"Memory", memory}, {"Root disk", disk}, {"Containers", containers}}
}

func chartGap(points []store.Point, summary bool, bucket, stale int) int64 {
	if summary {
		return int64(bucket) * 1500
	}
	// Independent scrapers have independent timestamps and collection cadences.
	times := map[string]map[int64]bool{}
	for _, p := range points {
		key := p.Name + "\x00" + p.Entity
		if times[key] == nil {
			times[key] = map[int64]bool{}
		}
		times[key][p.At.UnixMilli()] = true
	}
	ceiling := int64(stale) * int64(time.Second/time.Millisecond)
	gap := int64(0)
	for _, series := range times {
		ordered := make([]int64, 0, len(series))
		for at := range series {
			ordered = append(ordered, at)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
		cadence := ceiling
		for i := 1; i < len(ordered); i++ {
			cadence = min(cadence, (ordered[i]-ordered[i-1])*3/2)
		}
		if len(ordered) > 1 {
			gap = max(gap, cadence)
		}
	}
	if gap == 0 {
		gap = ceiling
	}
	return max(min(gap, ceiling), 1)
}
