package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"houselab/monitoring/internal/protocol"
)

const maxLatestMetrics = 10000
const maxLatestContainers = 2048

func observedAt(sample protocol.Sample, m protocol.Metric) time.Time {
	if !m.ObservedAt.IsZero() {
		return m.ObservedAt
	}
	return sample.At
}

// Bound persisted snapshots as well as source-series tables, including between cleanups.
func boundLatest(sample *protocol.Sample) {
	if !sample.Incremental {
		return
	}
	if len(sample.Metrics) > maxLatestMetrics {
		sort.SliceStable(sample.Metrics, func(i, j int) bool {
			return observedAt(*sample, sample.Metrics[i]).After(observedAt(*sample, sample.Metrics[j]))
		})
		sample.Metrics = sample.Metrics[:maxLatestMetrics]
	}
	if sample.StateAt.IsZero() && len(sample.Containers) > maxLatestContainers {
		sort.SliceStable(sample.Containers, func(i, j int) bool { return sample.Containers[i].ObservedAt.After(sample.Containers[j].ObservedAt) })
		sample.Containers = sample.Containers[:maxLatestContainers]
	}
}
func expireLatest(sample *protocol.Sample, cutoff time.Time) {
	if !sample.Incremental {
		return
	}
	entities := map[string]time.Time{}
	metrics := sample.Metrics[:0]
	for _, m := range sample.Metrics {
		at := observedAt(*sample, m)
		if at.After(entities[m.Entity]) {
			entities[m.Entity] = at
		}
		if !at.Before(cutoff) {
			metrics = append(metrics, m)
		}
	}
	sample.Metrics = metrics
	// Private state is a bounded complete snapshot and has independent freshness.
	if sample.StateAt.IsZero() {
		containers := sample.Containers[:0]
		for _, c := range sample.Containers {
			at := c.ObservedAt
			if at.IsZero() {
				at = entities[c.ID]
			}
			if !at.IsZero() && !at.Before(cutoff) {
				containers = append(containers, c)
			}
		}
		sample.Containers = containers
	}
	boundLatest(sample)
}

// Visit bounded pages; each committed host advances the cursor even if cleanup times out.
func (s *Store) pruneLatest(ctx context.Context, cutoff time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM hosts WHERE id>? ORDER BY id LIMIT 100`, s.maintenanceHost)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		sample, err := loadSample(ctx, tx, id)
		if err != nil {
			tx.Rollback()
			return err
		}
		oldMetrics, oldContainers := len(sample.Metrics), len(sample.Containers)
		expireLatest(&sample, cutoff)
		if oldMetrics != len(sample.Metrics) || oldContainers != len(sample.Containers) {
			data, e := json.Marshal(sample)
			if e != nil {
				tx.Rollback()
				return e
			}
			if _, err = tx.ExecContext(ctx, `UPDATE hosts SET data=? WHERE id=?`, data, id); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		s.maintenanceHost = id
	}
	if len(ids) < 100 {
		s.maintenanceHost = ""
	}
	return nil
}
