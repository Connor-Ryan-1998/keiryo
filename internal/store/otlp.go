package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/protocol"
)

func targets(p otlp.Point) []string {
	switch p.Name {
	case "system.uptime":
		return []string{"uptime_seconds"}
	case "system.cpu.load_average.1m":
		return []string{"load1"}
	case "system.cpu.time", "system.cpu.utilization":
		return []string{"cpu_percent"}
	case "system.memory.limit", "system.linux.memory.available", "keiryo.memory.available":
		return []string{"memory_total_bytes", "memory_available_bytes", "memory_used_bytes"}
	case "system.paging.usage":
		return []string{"swap_total_bytes", "swap_used_bytes"}
	case "system.filesystem.usage":
		return []string{"filesystem_total_bytes", "filesystem_free_bytes"}
	case "system.disk.io":
		if p.Attributes["direction"] == "read" {
			return []string{"disk_read_bytes_s"}
		}
		return []string{"disk_write_bytes_s"}
	case "system.network.io":
		if p.Attributes["direction"] == "receive" {
			return []string{"network_rx_bytes_s"}
		}
		return []string{"network_tx_bytes_s"}
	case "container.cpu.utilization", "container.cpu.usage.total":
		return []string{"container_cpu_percent"}
	case "container.memory.usage.total":
		return []string{"container_memory_bytes"}
	case "container.memory.usage.limit":
		return []string{"container_memory_limit_bytes"}
	}
	return nil
}

// IngestOTLP commits source deduplication, counter state, history and latest values together.
func (s *Store) IngestOTLP(ctx context.Context, host string, points []otlp.Point, received time.Time, bucket int) (otlp.IngestResult, error) {
	result := otlp.IngestResult{}
	if len(points) == 0 {
		return result, nil
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	latest, err := loadSample(ctx, tx, host)
	if err != nil {
		return result, err
	}
	if !latest.Incremental && !latest.At.IsZero() {
		latest.StateAt = latest.At
		latest.LogFetch = true
		for i := range latest.Metrics {
			latest.Metrics[i].ObservedAt = latest.At
		}
	}
	latest.Incremental = true
	latest.HostID = host
	times := map[int64]bool{}
	var streams int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM otlp_series WHERE host=?`, host).Scan(&streams); err != nil {
		return result, err
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
	for _, p := range points {
		data, err := json.Marshal(p)
		if err != nil {
			return result, err
		}
		var old []byte
		err = tx.QueryRowContext(ctx, `SELECT data FROM otlp_observations WHERE host=? AND series=? AND at=?`, host, p.Series, p.At.UnixNano()).Scan(&old)
		if err == nil {
			if bytes.Equal(old, data) {
				result.Accepted++
			} else {
				result.Rejected++
			}
			continue
		}
		if err != sql.ErrNoRows {
			return result, err
		}
		allowed := true
		for _, name := range targets(p) {
			var source string
			err = tx.QueryRowContext(ctx, `SELECT source FROM otlp_owners WHERE host=? AND name=? AND entity=?`, host, name, p.Entity).Scan(&source)
			if err != nil && err != sql.ErrNoRows {
				return result, err
			}
			if err == nil && source != p.Group {
				allowed = false
				break
			}
		}
		var known int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM otlp_series WHERE host=? AND series=?`, host, p.Series).Scan(&known)
		if err != nil && err != sql.ErrNoRows {
			return result, err
		}
		if !allowed || (streams >= 10000 && known == 0) {
			result.Rejected++
			continue
		}
		if known == 0 {
			streams++
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO otlp_series VALUES(?,?,?) ON CONFLICT(host,series) DO UPDATE SET at=MAX(at,excluded.at)`, host, p.Series, p.At.UnixNano()); err != nil {
			return result, err
		}
		for _, name := range targets(p) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO otlp_owners VALUES(?,?,?,?,?) ON CONFLICT(host,name,entity) DO UPDATE SET at=MAX(at,excluded.at)`, host, name, p.Entity, p.Group, p.At.UnixNano()); err != nil {
				return result, err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO otlp_observations VALUES(?,?,?,?,?)`, host, p.Series, p.At.UnixNano(), p.Group, data); err != nil {
			return result, err
		}
		result.Accepted++
		times[p.At.UnixNano()] = true
		if p.Counter && !p.Delta {
			var successor sql.NullInt64
			if err = tx.QueryRowContext(ctx, `SELECT MIN(at) FROM otlp_observations WHERE host=? AND series=? AND at>?`, host, p.Series, p.At.UnixNano()).Scan(&successor); err != nil {
				return result, err
			}
			if successor.Valid {
				times[successor.Int64] = true
			}
		}
		if p.At.After(latest.At) {
			latest.At = p.At
			if p.DisplayName != "" {
				latest.DisplayName = p.DisplayName
			}
		}
		if strings.HasPrefix(p.Name, "container.") && p.Entity != "" && latest.StateAt.IsZero() {
			name := p.ContainerName
			if name == "" {
				name = p.Entity
			}
			found := false
			for i := range latest.Containers {
				if latest.Containers[i].ID == p.Entity {
					if !p.At.Before(latest.Containers[i].ObservedAt) {
						latest.Containers[i].Name = name
						latest.Containers[i].ObservedAt = p.At
					}
					found = true
				}
			}
			if !found {
				latest.Containers = append(latest.Containers, protocol.Container{ID: p.Entity, Name: name, State: "unknown", ObservedAt: p.At})
			}
		}
	}
	ordered := make([]int64, 0, len(times))
	for at := range times {
		ordered = append(ordered, at)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, at := range ordered {
		ps, err := sourcePoints(ctx, tx, host, at)
		if err != nil {
			return result, err
		}
		for i := range ps {
			p := &ps[i]
			if !p.Counter {
				continue
			}
			if p.Delta {
				elapsed := p.At.Sub(p.Start).Seconds()
				if elapsed > 0 && elapsed <= 86400 {
					v := p.Value / elapsed
					p.Rate = &v
				}
				continue
			}
			var before []byte
			err = tx.QueryRowContext(ctx, `SELECT data FROM otlp_observations WHERE host=? AND series=? AND at<? ORDER BY at DESC LIMIT 1`, host, p.Series, at).Scan(&before)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return result, err
			}
			var old otlp.Point
			if err = json.Unmarshal(before, &old); err != nil {
				return result, err
			}
			elapsed := p.At.Sub(old.At).Seconds()
			if elapsed > 0 && elapsed <= 86400 && p.Value >= old.Value && p.Start.Equal(old.Start) {
				v := (p.Value - old.Value) / elapsed
				p.Rate = &v
				p.Start = old.At
			}
		}
		projected := otlp.Project(ps)
		type key struct{ name, entity string }
		missing := map[key]bool{}
		for _, p := range ps {
			for _, name := range targets(p) {
				missing[key{name, p.Entity}] = true
			}
		}
		for _, m := range projected {
			delete(missing, key{m.Metric.Name, m.Metric.Entity})
		}
		for k := range missing {
			removed, err := deleteMeasurement(ctx, tx, host, k.name, k.entity, time.Unix(0, at), bucket)
			if err != nil {
				return result, err
			}
			if removed {
				if err = refreshLatest(ctx, tx, host, &latest, k.name, k.entity, time.Unix(0, at)); err != nil {
					return result, err
				}
			}
		}
		for _, m := range projected {
			if err = writeMeasurement(ctx, tx, host, m, bucket); err != nil {
				return result, err
			}
			m.Metric.ObservedAt = m.At
			found := false
			for i, old := range latest.Metrics {
				if old.Name == m.Metric.Name && old.Entity == m.Metric.Entity {
					found = true
					oldAt := old.ObservedAt
					if oldAt.IsZero() {
						oldAt = latest.At
					}
					if !m.At.Before(oldAt) {
						latest.Metrics[i] = m.Metric
					}
					break
				}
			}
			if !found {
				latest.Metrics = append(latest.Metrics, m.Metric)
			}
		}
	}
	if result.Accepted > 0 {
		if err = saveSample(ctx, tx, host, latest, received); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}

func loadSample(ctx context.Context, tx *sql.Tx, host string) (protocol.Sample, error) {
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM hosts WHERE id=?`, host).Scan(&data)
	if err == sql.ErrNoRows {
		return protocol.Sample{HostID: host}, nil
	}
	if err != nil {
		return protocol.Sample{}, err
	}
	var sample protocol.Sample
	err = json.Unmarshal(data, &sample)
	return sample, err
}
func saveSample(ctx context.Context, tx *sql.Tx, host string, sample protocol.Sample, received time.Time) error {
	boundLatest(&sample)
	data, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO hosts VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET seen=MAX(hosts.seen,excluded.seen),name=excluded.name,at=excluded.at,data=excluded.data`, host, sample.DisplayName, received.Unix(), sample.At.Unix(), data)
	return err
}
func sourcePoints(ctx context.Context, tx *sql.Tx, host string, at int64) ([]otlp.Point, error) {
	rows, err := tx.QueryContext(ctx, `SELECT data FROM otlp_observations WHERE host=? AND at=? ORDER BY series LIMIT 10001`, host, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []otlp.Point
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var p otlp.Point
		if err = json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) > 10000 {
		return nil, errors.New("host source point limit exceeded")
	}
	return out, rows.Err()
}
func writeMeasurement(ctx context.Context, tx *sql.Tx, host string, m otlp.Measurement, bucket int) error {
	id := measurementID(host, m.Metric.Name, m.Metric.Entity, m.At)
	var oldValue, oldWeight float64
	count := int64(1)
	err := tx.QueryRowContext(ctx, `SELECT value,weight FROM points WHERE host=? AND sample=?`, host, id).Scan(&oldValue, &oldWeight)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		count = 0
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO samples(host,id,at,data) VALUES(?,?,?,'{}')`, host, id, m.At.Unix()); err != nil {
		return err
	}
	if count == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE points SET value=?,weight=? WHERE host=? AND sample=?`, m.Metric.Value, m.Metric.Weight, host, id)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO points VALUES(?,?,?,?,?,?,?,?)`, host, id, m.At.Unix(), m.Metric.Name, m.Metric.Entity, m.Metric.Value, m.Metric.Weight, m.At.UnixNano())
	}
	if err != nil {
		return err
	}
	at := m.At.Unix() / int64(bucket) * int64(bucket)
	// Adjust partial-group projections without counting their earlier intermediate value twice.
	_, err = tx.ExecContext(ctx, `INSERT INTO summaries VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(host,at,name,entity) DO UPDATE SET total=total+excluded.total,weight=weight+excluded.weight,count=count+excluded.count`, host, at, m.Metric.Name, m.Metric.Entity, m.Metric.Value*m.Metric.Weight-oldValue*oldWeight, m.Metric.Weight-oldWeight, m.Metric.Value, m.Metric.Value, count)
	if err != nil {
		return err
	}
	return updateExtrema(ctx, tx, host, m.Metric.Name, m.Metric.Entity, at, bucket)
}

func measurementID(host, name, entity string, at time.Time) string {
	data, _ := json.Marshal([]any{host, name, entity, at.UnixNano()})
	sum := sha256.Sum256(data)
	return "otlp-" + hex.EncodeToString(sum[:])
}
func updateExtrema(ctx context.Context, tx *sql.Tx, host, name, entity string, at int64, bucket int) error {
	_, err := tx.ExecContext(ctx, `UPDATE summaries SET minimum=COALESCE((SELECT MIN(value) FROM points WHERE host=? AND name=? AND entity=? AND at>=? AND at<?),minimum),maximum=COALESCE((SELECT MAX(value) FROM points WHERE host=? AND name=? AND entity=? AND at>=? AND at<?),maximum) WHERE host=? AND at=? AND name=? AND entity=?`, host, name, entity, at, at+int64(bucket), host, name, entity, at, at+int64(bucket), host, at, name, entity)
	return err
}
func deleteMeasurement(ctx context.Context, tx *sql.Tx, host, name, entity string, at time.Time, bucket int) (bool, error) {
	id := measurementID(host, name, entity, at)
	var value, weight float64
	err := tx.QueryRowContext(ctx, `SELECT value,weight FROM points WHERE host=? AND sample=?`, host, id).Scan(&value, &weight)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM samples WHERE host=? AND id=?`, host, id); err != nil {
		return false, err
	}
	start := at.Unix() / int64(bucket) * int64(bucket)
	if _, err = tx.ExecContext(ctx, `UPDATE summaries SET total=total-?,weight=weight-?,count=count-1 WHERE host=? AND at=? AND name=? AND entity=?`, value*weight, weight, host, start, name, entity); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM summaries WHERE host=? AND at=? AND name=? AND entity=? AND count<=0`, host, start, name, entity); err != nil {
		return false, err
	}
	return true, updateExtrema(ctx, tx, host, name, entity, start, bucket)
}
func refreshLatest(ctx context.Context, tx *sql.Tx, host string, sample *protocol.Sample, name, entity string, removedAt time.Time) error {
	for i, m := range sample.Metrics {
		if m.Name != name || m.Entity != entity || !m.ObservedAt.Equal(removedAt) {
			continue
		}
		var at int64
		err := tx.QueryRowContext(ctx, `SELECT value,weight,CASE WHEN nanos=0 THEN at*1000000000 ELSE nanos END FROM points WHERE host=? AND name=? AND entity=? ORDER BY at DESC,nanos DESC LIMIT 1`, host, name, entity).Scan(&m.Value, &m.Weight, &at)
		if err == sql.ErrNoRows {
			sample.Metrics = append(sample.Metrics[:i], sample.Metrics[i+1:]...)
			return nil
		}
		if err != nil {
			return err
		}
		m.ObservedAt = time.Unix(0, at).UTC()
		sample.Metrics[i] = m
		return nil
	}
	return nil
}
