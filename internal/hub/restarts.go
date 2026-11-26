package hub

import (
	"context"
	"fmt"
	"time"

	"houselab/monitoring/internal/store"
)

// restartSignals compares each container's current Docker restart count with
// the earliest stored snapshot inside its window. Containers created inside the
// window count from zero; recreated containers (new IDs) start over. Only a
// fresh snapshot is judged, and a rule with restarts 0 is disabled.
func (h *Hub) restartSignals(host store.Host, add func(key, kind, state, detail string)) {
	if !h.stateFresh(host.Sample) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	baselines := map[int]map[string]int{}
	for _, c := range host.Sample.Containers {
		if c.Name == "" {
			continue
		}
		rule := h.config.AlertPolicy.RestartLoopFor(host.ID, c.Name)
		if rule.Restarts == 0 {
			continue
		}
		baseline, cached := baselines[rule.WindowSeconds]
		if !cached {
			found, ok, err := h.store.RestartBaseline(ctx, host.ID, time.Now().Add(-time.Duration(rule.WindowSeconds)*time.Second))
			if err != nil || !ok {
				found = nil
			}
			baselines[rule.WindowSeconds], baseline = found, found
		}
		if baseline == nil {
			continue
		}
		restarts := max(0, c.Restarts-baseline[c.ID])
		state := "healthy"
		if restarts >= rule.Restarts {
			state = rule.Severity
		}
		add("restarts/"+c.Name, "Container", state, fmt.Sprintf("%s restarted %d× in %s", c.Name, restarts, duration(int64(rule.WindowSeconds))))
	}
}
