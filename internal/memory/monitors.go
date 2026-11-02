package memory

import (
	"context"
	"time"
)

// MonitorResult is one uptime/API check outcome.
type MonitorResult struct {
	ID         int64
	Monitor    string
	OK         bool
	StatusCode int
	LatencyMS  int64
	Error      string
	CheckedAt  Time
}

// MonitorSummary aggregates a monitor over a window.
type MonitorSummary struct {
	Monitor      string
	Checks       int
	Failures     int
	UptimePct    float64
	AvgLatencyMS float64
	Last         MonitorResult
}

// AddMonitorResult stores a check result.
func (s *Store) AddMonitorResult(ctx context.Context, r MonitorResult) error {
	ok := 0
	if r.OK {
		ok = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO monitor_results(monitor, ok, status_code, latency_ms, error, checked_at)
		VALUES (?, ?, ?, ?, ?, ?)`, r.Monitor, ok, r.StatusCode, r.LatencyMS, r.Error, ts(s.now()))
	return err
}

// LastMonitorResult returns the previous result for change detection.
func (s *Store) LastMonitorResult(ctx context.Context, monitor string) (*MonitorResult, error) {
	var r MonitorResult
	var ok int
	var c string
	err := s.db.QueryRowContext(ctx, `SELECT id, monitor, ok, status_code, latency_ms, error, checked_at
		FROM monitor_results WHERE monitor = ? ORDER BY id DESC LIMIT 1`, monitor).
		Scan(&r.ID, &r.Monitor, &ok, &r.StatusCode, &r.LatencyMS, &r.Error, &c)
	if err != nil {
		return nil, nil //nolint:nilerr // no previous result is not an error
	}
	r.OK, r.CheckedAt = ok == 1, Time(parseTS(c))
	return &r, nil
}

// MonitorSummaries aggregates every monitor since the given time.
func (s *Store) MonitorSummaries(ctx context.Context, since time.Time) ([]MonitorSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT monitor, count(*), sum(1 - ok), avg(latency_ms)
		FROM monitor_results WHERE checked_at >= ? GROUP BY monitor ORDER BY monitor`, ts(since))
	if err != nil {
		return nil, err
	}
	var out []MonitorSummary
	for rows.Next() {
		var m MonitorSummary
		if err := rows.Scan(&m.Monitor, &m.Checks, &m.Failures, &m.AvgLatencyMS); err != nil {
			rows.Close()
			return nil, err
		}
		if m.Checks > 0 {
			m.UptimePct = 100 * float64(m.Checks-m.Failures) / float64(m.Checks)
		}
		out = append(out, m)
	}
	rows.Close()
	for i := range out {
		if last, _ := s.LastMonitorResult(ctx, out[i].Monitor); last != nil {
			out[i].Last = *last
		}
	}
	return out, nil
}

// PruneMonitorResults deletes results older than before (bounded storage).
func (s *Store) PruneMonitorResults(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM monitor_results WHERE checked_at < ?`, ts(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
