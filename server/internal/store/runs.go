package store

import (
	"encoding/json"
	"time"
)

// RunKeep is how many run records are retained. The Activity page is a recent
// history, not an archive — an unbounded log on a Raspberry Pi's SD card is a
// slow-motion disk-full bug.
const RunKeep = 500

// AppendRun records a completed cycle and prunes the log back to RunKeep.
func (s *Store) AppendRun(r Run) (Run, error) {
	if r.Publishes == nil {
		r.Publishes = []PublishResult{}
	}
	blob, err := json.Marshal(r.Publishes)
	if err != nil {
		return Run{}, err
	}

	res, err := s.db.Exec(`
		INSERT INTO runs (started_at, finished_at, trigger, ok_count, error_count, publishes, error)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.StartedAt.UTC().Format(time.RFC3339Nano),
		r.FinishedAt.UTC().Format(time.RFC3339Nano),
		r.Trigger, r.OKCount, r.ErrorCount, string(blob), r.Error)
	if err != nil {
		return Run{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Run{}, err
	}
	r.ID = id

	if _, err := s.db.Exec(`
		DELETE FROM runs WHERE id NOT IN (SELECT id FROM runs ORDER BY id DESC LIMIT ?)`,
		RunKeep); err != nil {
		return r, err
	}
	return r, nil
}

// Runs returns the most recent cycles, newest first.
func (s *Store) Runs(limit int) ([]Run, error) {
	if limit <= 0 || limit > RunKeep {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, started_at, finished_at, trigger, ok_count, error_count, publishes, error
		FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Run{}
	for rows.Next() {
		var (
			r                     Run
			startedAt, finishedAt string
			blob                  string
		)
		if err := rows.Scan(&r.ID, &startedAt, &finishedAt, &r.Trigger,
			&r.OKCount, &r.ErrorCount, &blob, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt = parseTime(startedAt)
		r.FinishedAt = parseTime(finishedAt)
		r.Publishes = []PublishResult{}
		// A record whose publish blob can't be decoded is still worth showing:
		// its counts and its error are the interesting part. Drop the detail,
		// keep the row.
		_ = json.Unmarshal([]byte(blob), &r.Publishes)
		if r.Publishes == nil {
			r.Publishes = []PublishResult{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRun returns the most recent cycle, or ok=false when there hasn't been one.
func (s *Store) LastRun() (Run, bool, error) {
	runs, err := s.Runs(1)
	if err != nil || len(runs) == 0 {
		return Run{}, false, err
	}
	return runs[0], true, nil
}
