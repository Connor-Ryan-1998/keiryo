package hub

import (
	"context"
	"fmt"
	"time"

	"houselab/monitoring/internal/store"
)

// displayRow is the dashboard view of a host: warning/critical conditions only
// count once they have lasted alert_pending_seconds, as tracked by the
// background condition tracker. Until then they show as pending, or keep the
// last confirmed problem severity when a condition is escalating.
func (h *Hub) displayRow(ctx context.Context, host store.Host, now time.Time) fleetHost {
	row := h.fleetRow(host)
	if h.config.AlertPendingSeconds == 0 {
		return row
	}
	states, err := h.store.Notifications(ctx, host.ID)
	if err != nil {
		// Without tracking state, show raw readings rather than hide a problem.
		return row
	}
	for i, signal := range row.Signals {
		tracked, exists := states[signal.Key]
		row.Signals[i] = h.pendingSignal(signal, tracked, exists, now)
	}
	row.settle()
	return row
}

func (h *Hub) pendingSignal(signal condition, tracked store.Notification, exists bool, now time.Time) condition {
	if signal.State != "warning" && signal.State != "critical" {
		return signal
	}
	pending := int64(h.config.AlertPendingSeconds)
	elapsed := int64(-1)
	if exists && tracked.State == signal.State {
		elapsed = max(0, now.Unix()-tracked.Since)
		if elapsed >= pending {
			return signal
		}
	}
	note := fmt.Sprintf("%s if it lasts %s", signal.State, duration(pending))
	if elapsed >= 0 {
		note += fmt.Sprintf(" (%s so far)", duration(elapsed))
	}
	signal.Detail += " · " + note
	// An escalating or easing condition keeps the milder of the confirmed and current severities.
	if exists && (tracked.Confirmed == "warning" || tracked.Confirmed == "critical") {
		if severity(tracked.Confirmed) < severity(signal.State) {
			signal.State = tracked.Confirmed
		}
		return signal
	}
	signal.State = "pending"
	return signal
}

// duration renders whole seconds compactly: 40s, 2m, 2m30s, 1h5m.
func duration(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", seconds)
	case d < time.Hour:
		if d%time.Minute == 0 {
			return fmt.Sprintf("%dm", seconds/60)
		}
		return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%dh%dm", seconds/3600, seconds%3600/60)
	}
}
