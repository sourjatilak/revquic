// SPDX-License-Identifier: GPL-3.0-or-later

package qos

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (cgo-free)
)

// sqliteBackend persists events to a SQLite table, pruning to the most recent maxRows.
type sqliteBackend struct {
	db      *sql.DB
	maxRows int
	inserts int
}

const (
	defaultSQLiteMaxRows = 50000
	pruneEvery           = 256
)

const historySchema = `
CREATE TABLE IF NOT EXISTS qos_events (
  seq            INTEGER PRIMARY KEY AUTOINCREMENT,
  ts             TEXT NOT NULL,
  kind           TEXT NOT NULL,
  session_id     TEXT,
  node_id        TEXT,
  region         TEXT,
  username       TEXT,
  throughput_bps REAL,
  detail         TEXT
);
`

// NewSQLiteHistory opens (creating if needed) a SQLite-backed history store at path.
func NewSQLiteHistory(path string) (HistoryStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writers
	if _, err := db.Exec(historySchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return newAsyncStore(&sqliteBackend{db: db, maxRows: defaultSQLiteMaxRows}, 1024), nil
}

func (s *sqliteBackend) write(ev Event) error {
	_, err := s.db.Exec(
		`INSERT INTO qos_events(ts,kind,session_id,node_id,region,username,throughput_bps,detail) VALUES(?,?,?,?,?,?,?,?)`,
		ev.TS.Format(time.RFC3339Nano), ev.Kind, ev.SessionID, ev.NodeID, ev.Region, ev.Username, ev.ThroughputBps, ev.Detail)
	if err != nil {
		return err
	}
	if s.inserts++; s.inserts%pruneEvery == 0 {
		// Keep only the most recent maxRows.
		_, _ = s.db.Exec(`DELETE FROM qos_events WHERE seq <= (SELECT MAX(seq) FROM qos_events) - ?`, s.maxRows)
	}
	return nil
}

func (s *sqliteBackend) load(limit int) ([]Event, error) {
	q := `SELECT ts,kind,session_id,node_id,region,username,throughput_bps,detail FROM qos_events ORDER BY seq DESC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var ts string
		if err := rows.Scan(&ts, &ev.Kind, &ev.SessionID, &ev.NodeID, &ev.Region, &ev.Username, &ev.ThroughputBps, &ev.Detail); err != nil {
			continue
		}
		ev.TS, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *sqliteBackend) close() error { return s.db.Close() }
