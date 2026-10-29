package store

import (
	"context"
	"errors"
	"time"
)

type CPUPoint struct {
	Host             string
	At               time.Time
	Average, Maximum float64
}

// FleetCPU only reads CPU summaries for the displayed host page.
func (s *Store) FleetCPU(ctx context.Context, start, end time.Time, step, offset, limit int) ([]CPUPoint, error) {
	if !end.After(start) || end.Sub(start) > 31*24*time.Hour || step < 1 || offset < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid fleet history window or page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT host,(at / ?) * ?,SUM(total)/SUM(weight),MAX(maximum) FROM summaries
 WHERE name='cpu_percent' AND entity='' AND at>=? AND at<? AND host IN (SELECT id FROM hosts ORDER BY id LIMIT ? OFFSET ?)
 GROUP BY host,(at / ?) ORDER BY host,(at / ?) LIMIT 20000`, step, step, start.Unix(), exclusiveSecond(end), limit, offset, step, step)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CPUPoint{}
	for rows.Next() {
		var p CPUPoint
		var at int64
		if err = rows.Scan(&p.Host, &at, &p.Average, &p.Maximum); err != nil {
			return nil, err
		}
		p.At = time.Unix(at, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}
