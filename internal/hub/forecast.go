package hub

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"houselab/monitoring/internal/store"
)

type forecastEntry struct {
	at      time.Time
	signals []condition
}

// forecastSignals fits a least-squares line to each health-selected mount's
// used bytes over the forecast window (summary buckets) and reports when the
// line reaches capacity. At least 24 hours of history are required; shrinking
// or flat usage is healthy. Results are cached briefly because summaries only
// change once per bucket.
func (h *Hub) forecastSignals(host store.Host) []condition {
	f := h.config.AlertPolicy.Resources.Disk.Forecast
	if f.Days == 0 {
		return nil
	}
	h.forecastMu.Lock()
	cached, ok := h.forecasts[host.ID]
	h.forecastMu.Unlock()
	if ok && time.Since(cached.at) < time.Minute {
		return cached.signals
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	points, err := h.store.FilesystemSummaries(ctx, host.ID, time.Now().Add(-time.Duration(f.WindowHours)*time.Hour))
	if err != nil {
		return nil
	}
	type sample struct{ at, used, total float64 }
	type pair struct{ total, free *float64 }
	byMount := map[string]map[int64]*pair{}
	for _, p := range points {
		if len(h.config.DiskHealthMountpoints) > 0 && !slices.Contains(h.config.DiskHealthMountpoints, p.Entity) {
			continue
		}
		if byMount[p.Entity] == nil {
			byMount[p.Entity] = map[int64]*pair{}
		}
		at := p.At.Unix()
		if byMount[p.Entity][at] == nil {
			byMount[p.Entity][at] = &pair{}
		}
		v := p.Average
		if p.Name == "filesystem_total_bytes" {
			byMount[p.Entity][at].total = &v
		} else {
			byMount[p.Entity][at].free = &v
		}
	}
	mounts := make([]string, 0, len(byMount))
	for mount := range byMount {
		mounts = append(mounts, mount)
	}
	sort.Strings(mounts)
	var out []condition
	for _, mount := range mounts {
		var samples []sample
		for at, p := range byMount[mount] {
			if p.total != nil && p.free != nil && *p.total > 0 {
				samples = append(samples, sample{float64(at), *p.total - *p.free, *p.total})
			}
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i].at < samples[j].at })
		if len(samples) < 2 || samples[len(samples)-1].at-samples[0].at < 24*3600 {
			continue
		}
		var meanT, meanU float64
		for _, s := range samples {
			meanT += s.at
			meanU += s.used
		}
		meanT /= float64(len(samples))
		meanU /= float64(len(samples))
		var cov, varT float64
		for _, s := range samples {
			cov += (s.at - meanT) * (s.used - meanU)
			varT += (s.at - meanT) * (s.at - meanT)
		}
		latest := samples[len(samples)-1]
		key, kind := "forecast/"+mount, "Disk forecast"
		slope := cov / varT // bytes per second
		if varT == 0 || slope <= 0 {
			out = append(out, condition{Key: key, Kind: kind, State: "healthy", Detail: mount + " usage is not growing"})
			continue
		}
		days := (latest.total - latest.used) / slope / 86400
		state := "healthy"
		if days < f.Days {
			state = f.Severity
		}
		out = append(out, condition{Key: key, Kind: kind, State: state, Detail: fmt.Sprintf("%s full in about %.1f days at %.1f GiB/day", mount, max(days, 0), slope*86400/(1<<30))})
	}
	h.forecastMu.Lock()
	h.forecasts[host.ID] = forecastEntry{time.Now(), out}
	h.forecastMu.Unlock()
	return out
}
