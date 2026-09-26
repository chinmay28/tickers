package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Report is a finding an agent wrote up for the person running the app:
// what it tested, what held up, and what it would do next.
type Report struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
}

// Watch is a strategy frozen on a date, to be judged on what came after.
type Watch struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	// Since is the first day the forward test counts, YYYY-MM-DD: the day
	// after the watch was made, so no bar it is judged on existed when
	// the rules were chosen.
	Since     string    `json:"since"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
}

// Bounds on what agents can leave behind.
const (
	MaxReportTitle = 120
	MaxReportBody  = 64 << 10
	MaxReports     = 500
	MaxWatches     = 50
	MaxWatchNote   = 1000
)

// CreateReport saves a report.
func (s *Store) CreateReport(title, body, author string) (Report, error) {
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	switch {
	case title == "":
		return Report{}, errors.New("a report needs a title")
	case len(title) > MaxReportTitle:
		return Report{}, fmt.Errorf("a report title cannot be longer than %d characters", MaxReportTitle)
	case body == "":
		return Report{}, errors.New("a report needs a body")
	case len(body) > MaxReportBody:
		return Report{}, fmt.Errorf("a report cannot be longer than %d KB", MaxReportBody>>10)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM research_reports`).Scan(&n); err != nil {
		return Report{}, err
	}
	if n >= MaxReports {
		return Report{}, fmt.Errorf("there cannot be more than %d reports; delete some in the app first", MaxReports)
	}
	now := nowRFC3339()
	r := Report{ID: newID(), Title: title, Body: body, Author: strings.TrimSpace(author), CreatedAt: parseTime(now)}
	_, err := s.db.Exec(`INSERT INTO research_reports (id, title, body, author, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.Title, r.Body, r.Author, now)
	return r, err
}

// Reports lists reports, newest first.
func (s *Store) Reports() ([]Report, error) {
	rows, err := s.db.Query(`SELECT id, title, body, author, created_at FROM research_reports ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		var r Report
		var created string
		if err := rows.Scan(&r.ID, &r.Title, &r.Body, &r.Author, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteReport removes a report.
func (s *Store) DeleteReport(id string) error { return s.deleteRow(`research_reports`, id) }

// CreateWatch freezes a strategy's rules from since on. Whether they run
// is the strategy package's to say, and the engine asks before this.
func (s *Store) CreateWatch(name string, def json.RawMessage, since, note string) (Watch, error) {
	name, note = strings.TrimSpace(name), strings.TrimSpace(note)
	if err := validStrategy(name, def); err != nil {
		return Watch{}, err
	}
	if _, err := time.Parse(time.DateOnly, since); err != nil {
		return Watch{}, errors.New("a watch starts on a date like 2026-01-31")
	}
	if len(note) > MaxWatchNote {
		return Watch{}, fmt.Errorf("a watch's note cannot be longer than %d characters", MaxWatchNote)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM watches`).Scan(&n); err != nil {
		return Watch{}, err
	}
	if n >= MaxWatches {
		return Watch{}, fmt.Errorf("there cannot be more than %d watched strategies; stop watching some in the app first", MaxWatches)
	}
	now := nowRFC3339()
	w := Watch{ID: newID(), Name: name, Definition: compact(def), Since: since, Note: note, CreatedAt: parseTime(now)}
	_, err := s.db.Exec(`INSERT INTO watches (id, name, definition, since, note, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, string(w.Definition), w.Since, w.Note, now)
	return w, err
}

// Watches lists the watched strategies, oldest first.
func (s *Store) Watches() ([]Watch, error) {
	rows, err := s.db.Query(`SELECT id, name, definition, since, note, created_at FROM watches ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Watch{}
	for rows.Next() {
		var w Watch
		var def, created string
		if err := rows.Scan(&w.ID, &w.Name, &def, &w.Since, &w.Note, &created); err != nil {
			return nil, err
		}
		w.Definition, w.CreatedAt = json.RawMessage(def), parseTime(created)
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWatch stops watching a strategy.
func (s *Store) DeleteWatch(id string) error { return s.deleteRow(`watches`, id) }

func (s *Store) deleteRow(table, id string) error {
	res, err := s.db.Exec(`DELETE FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
