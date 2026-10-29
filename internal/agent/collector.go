package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func rate(before, after float64, elapsed time.Duration) (float64, bool) {
	if elapsed <= 0 || after < before {
		return 0, false
	}
	return (after - before) / elapsed.Seconds(), true
}

type Collector struct {
	Config   config.Agent
	Docker   *Docker
	at       time.Time
	counters map[string]float64
	boot     uint64
	started  time.Time
}

func NewCollector(c config.Agent) *Collector {
	return &Collector{Config: c, Docker: NewDocker(c.DockerSocket)}
}
func (c *Collector) Collect(ctx context.Context) protocol.Sample {
	now := time.Now()
	var id [16]byte
	rand.Read(id[:])
	x := protocol.Sample{Version: protocol.Version, AgentVersion: protocol.AgentVersion, HostID: c.Config.HostID, DisplayName: c.Config.DisplayName, ID: hex.EncodeToString(id[:]), At: now.UTC()}
	counters := map[string]float64{}
	elapsed := now.Sub(c.at)
	add := func(name, entity string, value, weight float64) {
		x.Metrics = append(x.Metrics, protocol.Metric{Name: name, Entity: entity, Value: value, Weight: weight})
	}
	failure := func(err error, what string) {
		if err != nil {
			x.Errors = append(x.Errors, what+" collection failed")
		}
	}
	uptime, err := host.UptimeWithContext(ctx)
	failure(err, "uptime")
	if err == nil {
		add("uptime_seconds", "", float64(uptime), 1)
		if c.started.IsZero() || uptime < c.boot {
			c.started = now.Add(-time.Duration(uptime) * time.Second).Truncate(time.Second)
		}
		if uptime < c.boot {
			c.counters = nil
		}
		c.boot = uptime
	}
	times, err := cpu.TimesWithContext(ctx, false)
	failure(err, "cpu")
	if err == nil && len(times) > 0 {
		t := times[0]
		for _, state := range []struct {
			name  string
			value float64
		}{{"user", t.User}, {"system", t.System}, {"nice", t.Nice}, {"idle", t.Idle}, {"wait", t.Iowait}, {"interrupt", t.Irq}, {"softirq", t.Softirq}, {"steal", t.Steal}} {
			x.Raw = append(x.Raw, protocol.RawMetric{Name: "system.cpu.time", Unit: "s", Value: state.value, Counter: true, Start: c.started, Attributes: map[string]string{"cpu": "all", "state": state.name}})
		}
		total := t.User + t.System + t.Nice + t.Idle + t.Iowait + t.Irq + t.Softirq + t.Steal
		counters["cpu_total"] = total
		counters["cpu_idle"] = t.Idle + t.Iowait
		if old, ok := c.counters["cpu_total"]; ok && total > old && counters["cpu_idle"] >= c.counters["cpu_idle"] {
			v := 100 * (1 - (counters["cpu_idle"]-c.counters["cpu_idle"])/(total-old))
			if v >= 0 && v <= 100 {
				add("cpu_percent", "", v, 1)
			}
		}
	}
	memory, err := mem.VirtualMemoryWithContext(ctx)
	failure(err, "memory")
	if err == nil {
		add("memory_total_bytes", "", float64(memory.Total), 1)
		add("memory_available_bytes", "", float64(memory.Available), 1)
		add("memory_used_bytes", "", float64(memory.Total-memory.Available), 1)
	}
	swap, err := mem.SwapMemoryWithContext(ctx)
	failure(err, "swap")
	if err == nil {
		add("swap_used_bytes", "", float64(swap.Used), 1)
		add("swap_total_bytes", "", float64(swap.Total), 1)
	}
	loads, err := load.AvgWithContext(ctx)
	failure(err, "load")
	if err == nil {
		add("load1", "", loads.Load1, 1)
	}
	parts, err := disk.PartitionsWithContext(ctx, false)
	failure(err, "filesystem")
	for _, p := range parts {
		skip := false
		for _, f := range c.Config.ExcludeFilesystems {
			if f == p.Mountpoint || f == p.Fstype {
				skip = true
			}
		}
		if skip {
			continue
		}
		path := p.Mountpoint
		if root := os.Getenv("HOST_ROOT"); root != "" {
			path = filepath.Join(root, p.Mountpoint)
		}
		usage, e := disk.UsageWithContext(ctx, path)
		if e != nil {
			failure(e, "filesystem")
			continue
		}
		add("filesystem_total_bytes", p.Mountpoint, float64(usage.Total), 1)
		add("filesystem_free_bytes", p.Mountpoint, float64(usage.Free), 1)
	}
	deltas := func(name, entity string, value float64) {
		rawName, direction := "system.disk.io", "read"
		switch name {
		case "disk_write_bytes_s":
			direction = "write"
		case "network_rx_bytes_s":
			rawName = "system.network.io"
			direction = "receive"
		case "network_tx_bytes_s":
			rawName = "system.network.io"
			direction = "transmit"
		}
		x.Raw = append(x.Raw, protocol.RawMetric{Name: rawName, Unit: "By", Value: value, Counter: true, Start: c.started, Attributes: map[string]string{"device": entity, "direction": direction}})
		key := name + "/" + entity
		counters[key] = value
		if before, ok := c.counters[key]; ok {
			if v, valid := rate(before, value, elapsed); valid {
				add(name, entity, v, elapsed.Seconds())
			}
		}
	}
	disks, err := disk.IOCountersWithContext(ctx)
	failure(err, "disk I/O")
	for name, d := range disks {
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		deltas("disk_read_bytes_s", name, float64(d.ReadBytes))
		deltas("disk_write_bytes_s", name, float64(d.WriteBytes))
	}
	nets, err := networkCounters(ctx)
	failure(err, "network")
	for _, n := range nets {
		skip := false
		for _, name := range c.Config.ExcludeInterfaces {
			if n.Name == name {
				skip = true
			}
		}
		if skip {
			continue
		}
		deltas("network_rx_bytes_s", n.Name, float64(n.BytesRecv))
		deltas("network_tx_bytes_s", n.Name, float64(n.BytesSent))
	}
	if c.Docker != nil {
		containers, metrics, errs := c.Docker.Collect(ctx)
		x.Containers = containers
		x.Metrics = append(x.Metrics, metrics...)
		x.Errors = append(x.Errors, errs...)
	}
	c.at = now
	c.counters = counters
	if len(x.Errors) > 64 {
		x.Errors = x.Errors[:64]
	}
	return x
}
