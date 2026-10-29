// Package store is the hub's only persistent state boundary.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"houselab/monitoring/internal/protocol"
	_ "modernc.org/sqlite"
	"os"
	"strconv"
	"sync"
	"time"
)

type Store struct {
	db              *sql.DB
	writer          sync.Mutex
	maintenanceHost string
}
type Host struct {
	ID, Name string
	Seen     time.Time
	Sample   protocol.Sample
}
type Point struct {
	At                        time.Time
	Name, Entity              string
	Average, Minimum, Maximum float64
	Count                     int
}
type Snapshot struct {
	At         time.Time            `json:"at"`
	Containers []protocol.Container `json:"containers"`
	Errors     []string             `json:"errors"`
}

func Open(path string) (*Store, error) {
	// Create with restrictive permissions before SQLite can create it with defaults.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS schema_version(version INTEGER NOT NULL);
 INSERT INTO schema_version SELECT 1 WHERE NOT EXISTS(SELECT 1 FROM schema_version);
 CREATE TABLE IF NOT EXISTS hosts(id TEXT PRIMARY KEY,name TEXT NOT NULL,seen INTEGER NOT NULL,at INTEGER NOT NULL,data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS samples(host TEXT NOT NULL,id TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(host,id));
 CREATE INDEX IF NOT EXISTS sample_time ON samples(at);
 CREATE TABLE IF NOT EXISTS points(host TEXT NOT NULL,sample TEXT NOT NULL,at INTEGER NOT NULL,name TEXT NOT NULL,entity TEXT NOT NULL,value REAL NOT NULL,weight REAL NOT NULL,FOREIGN KEY(host,sample) REFERENCES samples(host,id) ON DELETE CASCADE);
 CREATE INDEX IF NOT EXISTS point_history ON points(host,at);
 CREATE INDEX IF NOT EXISTS points_sample ON points(host,sample);
 CREATE TABLE IF NOT EXISTS summaries(host TEXT NOT NULL,at INTEGER NOT NULL,name TEXT NOT NULL,entity TEXT NOT NULL,total REAL NOT NULL,weight REAL NOT NULL,minimum REAL NOT NULL,maximum REAL NOT NULL,count INTEGER NOT NULL,PRIMARY KEY(host,at,name,entity));
 CREATE INDEX IF NOT EXISTS summary_time ON summaries(at);
 CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,expires INTEGER NOT NULL,csrf TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS state_summaries(host TEXT NOT NULL,at INTEGER NOT NULL,sample_at INTEGER NOT NULL,data BLOB NOT NULL,PRIMARY KEY(host,at));
 CREATE INDEX IF NOT EXISTS state_summary_time ON state_summaries(at);
 `)
	if err != nil {
		db.Close()
		return nil, err
	}
	var v int
	if err = db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || (v != 1 && v != 2 && v != 3) {
		db.Close()
		return nil, errors.New("unsupported database schema")
	}
	if v == 1 {
		tx, migrateErr := db.Begin()
		if migrateErr == nil {
			_, migrateErr = tx.Exec(`ALTER TABLE samples ADD COLUMN data BLOB NOT NULL DEFAULT '{}'; UPDATE schema_version SET version=2;`)
			if migrateErr == nil {
				migrateErr = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
		if migrateErr != nil {
			db.Close()
			return nil, migrateErr
		}
	}
	if v < 3 {
		tx, migrateErr := db.Begin()
		if migrateErr == nil {
			_, migrateErr = tx.Exec(`
 CREATE TABLE otlp_observations(host TEXT NOT NULL,series TEXT NOT NULL,at INTEGER NOT NULL,groupid TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(host,series,at));
 CREATE INDEX otlp_time ON otlp_observations(at);
 CREATE INDEX otlp_group ON otlp_observations(host,at,groupid);
 CREATE TABLE otlp_series(host TEXT NOT NULL,series TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(host,series));
 CREATE TABLE otlp_owners(host TEXT NOT NULL,name TEXT NOT NULL,entity TEXT NOT NULL,source TEXT NOT NULL,at INTEGER NOT NULL,PRIMARY KEY(host,name,entity));
 ALTER TABLE points ADD COLUMN nanos INTEGER NOT NULL DEFAULT 0;
 UPDATE schema_version SET version=3;`)
			if migrateErr == nil {
				migrateErr = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
		if migrateErr != nil {
			db.Close()
			return nil, migrateErr
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

// Configure prevents incompatible bucket grids being combined after a config edit.
func (s *Store) Configure(ctx context.Context, bucket int) error {
	want := strconv.Itoa(bucket)
	existing, err := s.Setting(ctx, "bucket_seconds")
	if errors.Is(err, sql.ErrNoRows) {
		return s.SetSetting(ctx, "bucket_seconds", want)
	}
	if err != nil {
		return err
	}
	if existing != want {
		return errors.New("bucket_seconds differs from this database; use a new database for a new bucket grid")
	}
	return nil
}

func (s *Store) Ingest(ctx context.Context, x protocol.Sample, received time.Time, bucket int) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := json.Marshal(Snapshot{At: x.At, Containers: x.Containers, Errors: x.Errors})
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO samples(host,id,at,data) VALUES(?,?,?,?)`, x.HostID, x.ID, x.At.Unix(), state)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE hosts SET seen=MAX(seen,?) WHERE id=?`, received.Unix(), x.HostID); err != nil {
			return err
		}
		return tx.Commit()
	}
	data, err := json.Marshal(x)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO hosts VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET seen=MAX(hosts.seen,excluded.seen),name=CASE WHEN excluded.at>=hosts.at THEN excluded.name ELSE hosts.name END,data=CASE WHEN excluded.at>=hosts.at THEN excluded.data ELSE hosts.data END,at=MAX(hosts.at,excluded.at)`, x.HostID, x.DisplayName, received.Unix(), x.At.Unix(), data)
	if err != nil {
		return err
	}
	at := x.At.Unix() / int64(bucket) * int64(bucket)
	if _, err = tx.ExecContext(ctx, `INSERT INTO state_summaries VALUES(?,?,?,?) ON CONFLICT(host,at) DO UPDATE SET sample_at=excluded.sample_at,data=excluded.data WHERE excluded.sample_at>=state_summaries.sample_at`, x.HostID, at, x.At.Unix(), state); err != nil {
		return err
	}
	for _, m := range x.Metrics {
		if _, err = tx.ExecContext(ctx, `INSERT INTO points(host,sample,at,name,entity,value,weight) VALUES(?,?,?,?,?,?,?)`, x.HostID, x.ID, x.At.Unix(), m.Name, m.Entity, m.Value, m.Weight); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO summaries VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(host,at,name,entity) DO UPDATE SET total=total+excluded.total,weight=weight+excluded.weight,minimum=MIN(minimum,excluded.minimum),maximum=MAX(maximum,excluded.maximum),count=count+1`, x.HostID, at, m.Name, m.Entity, m.Value*m.Weight, m.Weight, m.Value, m.Value, 1); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Register(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		data, _ := json.Marshal(protocol.Sample{HostID: id})
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO hosts VALUES(?,?,0,0,?)`, id, id, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Hosts(ctx context.Context) ([]Host, error) { return s.HostsPage(ctx, 0, 100) }
func (s *Store) HostsPage(ctx context.Context, offset, limit int) ([]Host, error) {
	if offset < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid host page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,seen,data FROM hosts ORDER BY id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Host{}
	for rows.Next() {
		var h Host
		var seen int64
		var b []byte
		if err = rows.Scan(&h.ID, &h.Name, &seen, &b); err != nil {
			return nil, err
		}
		if seen != 0 {
			h.Seen = time.Unix(seen, 0)
		}
		if err = json.Unmarshal(b, &h.Sample); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s *Store) Host(ctx context.Context, id string) (Host, error) {
	var h Host
	var seen int64
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT id,name,seen,data FROM hosts WHERE id=?`, id).Scan(&h.ID, &h.Name, &seen, &b)
	if err != nil {
		return h, err
	}
	if seen != 0 {
		h.Seen = time.Unix(seen, 0)
	}
	err = json.Unmarshal(b, &h.Sample)
	return h, err
}

// Integer-second storage must include the current second for subsecond query ends.
func exclusiveSecond(end time.Time) int64 {
	if end.Nanosecond() != 0 {
		return end.Unix() + 1
	}
	return end.Unix()
}

func (s *Store) History(ctx context.Context, host string, start, end time.Time, summary bool, bucket int) ([]Point, error) {
	if !end.After(start) || end.Sub(start) > 366*24*time.Hour {
		return nil, errors.New("invalid history window")
	}
	query := `SELECT CASE WHEN nanos=0 THEN at*1000000000 ELSE nanos END,name,entity,value,value,value,1 FROM points WHERE host=? AND at>=? AND at<? ORDER BY at,nanos,name,entity LIMIT 20000`
	if summary {
		query = `SELECT at,name,entity,total/weight,minimum,maximum,count FROM summaries WHERE host=? AND at>=? AND at<? ORDER BY at,name,entity LIMIT 20000`
	}
	rows, err := s.db.QueryContext(ctx, query, host, start.Unix(), exclusiveSecond(end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		var at int64
		if err = rows.Scan(&at, &p.Name, &p.Entity, &p.Average, &p.Minimum, &p.Maximum, &p.Count); err != nil {
			return nil, err
		}
		if summary {
			p.At = time.Unix(at, 0).UTC()
		} else {
			p.At = time.Unix(0, at).UTC()
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) States(ctx context.Context, host string, start, end time.Time, summary bool) ([]Snapshot, error) {
	if !end.After(start) || end.Sub(start) > 366*24*time.Hour {
		return nil, errors.New("invalid state history window")
	}
	query := `SELECT data FROM samples WHERE host=? AND at>=? AND at<? AND data!='{}' ORDER BY at LIMIT 1000`
	if summary {
		query = `SELECT data FROM state_summaries WHERE host=? AND at>=? AND at<? ORDER BY at LIMIT 1000`
	}
	rows, err := s.db.QueryContext(ctx, query, host, start.Unix(), exclusiveSecond(end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var x Snapshot
		if err = json.Unmarshal(b, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

var ErrStorageBudget = errors.New("retained database pages exceed storage budget after bounded pruning")

func (s *Store) liveBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	for _, p := range []struct {
		q string
		v *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA freelist_count", &free}, {"PRAGMA page_size", &size}} {
		if err := s.db.QueryRowContext(ctx, p.q).Scan(p.v); err != nil {
			return 0, err
		}
	}
	return (pages - free) * size, nil
}

func (s *Store) Maintain(ctx context.Context, now time.Time, raw, summary time.Duration, budget int64) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.pruneLatest(ctx, now.Add(-raw)); err != nil {
		return err
	}
	// Work in bounded chunks. Repeated maintenance catches up without one giant deletion.
	for _, q := range []struct {
		sql string
		cut int64
	}{{`DELETE FROM samples WHERE rowid IN(SELECT rowid FROM samples WHERE at<? ORDER BY at LIMIT 1000)`, now.Add(-raw).Unix()}, {`DELETE FROM summaries WHERE rowid IN(SELECT rowid FROM summaries WHERE at<? ORDER BY at LIMIT 10000)`, now.Add(-summary).Unix()}, {`DELETE FROM state_summaries WHERE rowid IN(SELECT rowid FROM state_summaries WHERE at<? ORDER BY at LIMIT 1000)`, now.Add(-summary).Unix()}, {`DELETE FROM sessions WHERE expires<?`, now.Unix()}, {`DELETE FROM otlp_observations WHERE rowid IN(SELECT rowid FROM otlp_observations WHERE at<? ORDER BY at LIMIT 10000)`, now.Add(-raw).UnixNano()}, {`DELETE FROM otlp_owners WHERE at<?`, now.Add(-raw).UnixNano()}, {`DELETE FROM otlp_series WHERE at<?`, now.Add(-raw).UnixNano()}} {
		if _, err := s.db.ExecContext(ctx, q.sql, q.cut); err != nil {
			return err
		}
	}
	for {
		live, err := s.liveBytes(ctx)
		if err != nil {
			return err
		}
		if live <= budget {
			break
		}
		var cut sql.NullInt64
		if err = s.db.QueryRowContext(ctx, `SELECT MIN(at) FROM (
 SELECT MAX(at) AS at FROM (SELECT at FROM summaries ORDER BY at LIMIT 1000)
 UNION ALL SELECT MAX(at) FROM (SELECT at FROM state_summaries ORDER BY at LIMIT 100)
  UNION ALL SELECT MAX(at) FROM (SELECT at FROM samples WHERE data!='{}' ORDER BY at LIMIT 100)
  UNION ALL SELECT MAX(at) FROM (SELECT at FROM points ORDER BY at LIMIT 100)
  UNION ALL SELECT MAX(at / 1000000000) FROM (SELECT at FROM otlp_observations ORDER BY at LIMIT 1000)
 )`).Scan(&cut); err != nil {
			return err
		}
		if !cut.Valid {
			return ErrStorageBudget
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM points WHERE rowid IN(SELECT rowid FROM points WHERE at<=? ORDER BY at LIMIT 1000)`, `UPDATE samples SET data='{}' WHERE rowid IN(SELECT rowid FROM samples WHERE at<=? AND data!='{}' ORDER BY at LIMIT 100)`, `DELETE FROM summaries WHERE rowid IN(SELECT rowid FROM summaries WHERE at<=? ORDER BY at LIMIT 1000)`, `DELETE FROM state_summaries WHERE rowid IN(SELECT rowid FROM state_summaries WHERE at<=? ORDER BY at LIMIT 100)`, `DELETE FROM otlp_observations WHERE rowid IN(SELECT rowid FROM otlp_observations WHERE at / 1000000000<=? ORDER BY at LIMIT 1000)`} {
			if _, err = tx.ExecContext(ctx, q, cut.Int64); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v, err
}
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
func (s *Store) PutSession(ctx context.Context, id, csrf string, expires time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires<=? OR rowid IN(SELECT rowid FROM sessions ORDER BY rowid DESC LIMIT -1 OFFSET 127)`, time.Now().Unix()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sessions VALUES(?,?,?)`, id, expires.Unix(), csrf); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Session(ctx context.Context, id string, now time.Time) (string, error) {
	var csrf string
	err := s.db.QueryRowContext(ctx, `SELECT csrf FROM sessions WHERE id=? AND expires>?`, id, now.Unix()).Scan(&csrf)
	return csrf, err
}
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	return err
}
func (s *Store) ChangePassword(ctx context.Context, hash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		sql  string
		args []any
	}{{`INSERT INTO settings VALUES('password',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, []any{hash}}, {`INSERT INTO settings VALUES('initialized','yes') ON CONFLICT(key) DO UPDATE SET value='yes'`, nil}, {`DELETE FROM sessions`, nil}} {
		if _, err = tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}
