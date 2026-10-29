package store

import (
	"context"
	"encoding/json"
	"houselab/monitoring/internal/protocol"
	"time"
)

func (s *Store) IngestState(ctx context.Context, state protocol.AgentState, received time.Time, bucket int) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sample, err := loadSample(ctx, tx, state.HostID)
	if err != nil {
		return err
	}
	if !sample.Incremental && !sample.At.IsZero() {
		sample.StateAt = sample.At
		sample.LogFetch = true
		for i := range sample.Metrics {
			sample.Metrics[i].ObservedAt = sample.At
		}
	}
	sample.Incremental = true
	data, err := json.Marshal(Snapshot{At: state.At, Containers: state.Containers, Errors: state.Errors})
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO samples(host,id,at,data) VALUES(?,?,?,?)`, state.HostID, "state-"+state.ID, state.At.Unix(), data)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO state_summaries VALUES(?,?,?,?) ON CONFLICT(host,at) DO UPDATE SET sample_at=excluded.sample_at,data=excluded.data WHERE excluded.sample_at>=state_summaries.sample_at`, state.HostID, state.At.Unix()/int64(bucket)*int64(bucket), state.At.Unix(), data); err != nil {
			return err
		}
		if !state.At.Before(sample.StateAt) {
			sample.StateAt = state.At
			sample.Containers = state.Containers
			sample.Errors = state.Errors
			sample.AgentVersion = state.AgentVersion
			sample.LogFetch = state.LogFetch
			if state.DisplayName != "" {
				sample.DisplayName = state.DisplayName
			}
		}
	}
	if err = saveSample(ctx, tx, state.HostID, sample, received); err != nil {
		return err
	}
	return tx.Commit()
}
