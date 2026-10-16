package store

import (
	"fmt"
	"time"
)

// Event is one entry in the session's audit log: what happened, who caused it
// and when. Events are the operator-facing record of everything the engine and
// the admins did, so they are written but never updated or deleted.
//
// ServiceID is nil for engine-level events (started, stopped, config reloaded)
// and set for anything attributable to one service.
//
// Level is "info", "warning" or "error"; Actor is "admin" or "system"; Type is
// the machine-readable event name the dashboard filters on, and Message its
// human-readable form. Detail holds any longer text and, like a check result's,
// is capped and scrubbed on the way in (see sanitizeDetail).
type Event struct {
	ID        int64
	SessionID int64
	ServiceID *int64
	Level     string
	Actor     string
	Type      string
	Message   string
	Detail    string
	Timestamp time.Time
}

// eventColumns is the select list scanEvent expects, in order.
const eventColumns = `id, session_id, service_id, level, actor, type, message, detail, timestamp`

// scanEvent reads one eventColumns row. Its error is returned unwrapped so
// callers can recognise sql.ErrNoRows.
func scanEvent(sc rowScanner) (Event, error) {
	var e Event
	var timestampMillis int64
	err := sc.Scan(&e.ID, &e.SessionID, &e.ServiceID, &e.Level, &e.Actor,
		&e.Type, &e.Message, &e.Detail, &timestampMillis)
	if err != nil {
		return Event{}, err
	}
	e.Timestamp = time.UnixMilli(timestampMillis).UTC()
	return e, nil
}

// InsertEvent appends one event to the session's log. A zero Timestamp means
// now, which is the usual case: callers describe what happened and let the
// store date it.
func (s *Store) InsertEvent(e Event) error {
	_, err := s.db.Exec(
		`INSERT INTO events (session_id, service_id, level, actor, type, message, detail, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.SessionID, e.ServiceID, e.Level, e.Actor, e.Type, e.Message,
		sanitizeDetail(e.Detail), timestampMillis(e.Timestamp),
	)
	if err != nil {
		return fmt.Errorf("store: inserting %q event in session %d: %w", e.Type, e.SessionID, err)
	}
	return nil
}

// ListEvents returns up to limit of the session's events, newest first,
// engine-level and service-level together. beforeID pages exactly as it does
// in ListCheckResults: 0 for the first page, then the last id of the page just
// read.
func (s *Store) ListEvents(sessionID int64, limit int, beforeID int64) ([]Event, error) {
	where, args, err := s.pageBefore("events", beforeID)
	if err != nil {
		return nil, fmt.Errorf("store: listing events in session %d: %w", sessionID, err)
	}
	rows, err := s.db.Query(
		`SELECT `+eventColumns+` FROM events
		 WHERE session_id = ?`+where+` `+newestFirst+` LIMIT ?`,
		append(append([]any{sessionID}, args...), listLimit(limit))...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: listing events in session %d: %w", sessionID, err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning event row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing events in session %d: %w", sessionID, err)
	}
	return out, nil
}
