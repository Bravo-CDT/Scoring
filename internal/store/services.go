package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/config"
)

// Service is one scored service within a session.
//
// A service's identity is the (SessionID, Name) pair: config.yaml carries no
// service id, so renaming a service there is indistinguishable from removing
// one name and adding another. The old name's row stays (marked inactive, so
// its history is still queryable) and the new name gets a fresh row.
//
// Name and CreatedAt are fixed at creation. Active and ScoringPaused are
// operator state, changed only through SetServiceActive and
// SetServiceScoringPaused:
//
//   - Active = false retires a service: the check routines stop running it and
//     ListActiveServices stops returning it, but its recorded history remains.
//   - ScoringPaused = true keeps the checks running and their results recorded
//     while stopping them from moving points.
type Service struct {
	ID            int64
	SessionID     int64
	Name          string
	Active        bool
	ScoringPaused bool
	CreatedAt     time.Time
}

// serviceColumns is the select list scanService expects, in order.
const serviceColumns = `id, session_id, name, active, scoring_paused, created_at`

// rowScanner is the Scan method *sql.Row and *sql.Rows have in common, so one
// scanService serves both the single-row lookups and the list query.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanService reads one serviceColumns row. Its error is returned unwrapped so
// callers can recognise sql.ErrNoRows.
func scanService(sc rowScanner) (Service, error) {
	var svc Service
	var createdAtMillis int64
	err := sc.Scan(&svc.ID, &svc.SessionID, &svc.Name, &svc.Active, &svc.ScoringPaused, &createdAtMillis)
	if err != nil {
		return Service{}, err
	}
	svc.CreatedAt = time.UnixMilli(createdAtMillis).UTC()
	return svc, nil
}

// GetOrCreateService returns the service called name in session sessionID,
// creating it (active, not scoring-paused) if it does not exist yet.
//
// It is idempotent on (sessionID, name) and never writes to a row that already
// exists: a repeat call returns the current Active and ScoringPaused values
// untouched. The engine calls this for every configured service on every
// startup and config reload, so resetting the defaults here would silently
// undo an operator's decision to pause or retire a service. Reactivating a
// retired service is therefore an explicit SetServiceActive call, never a
// side effect of the service reappearing in config.yaml.
func (s *Store) GetOrCreateService(sessionID int64, name string) (Service, error) {
	svc, err := s.findService(sessionID, name)
	if err == nil {
		return svc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Service{}, err
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.db.Exec(
		`INSERT INTO services (session_id, name, active, scoring_paused, created_at)
		 VALUES (?, ?, TRUE, FALSE, ?)
		 ON CONFLICT(session_id, name) DO NOTHING`,
		sessionID, name, now.UnixMilli(),
	)
	if err != nil {
		return Service{}, fmt.Errorf("store: creating service %q in session %d: %w", name, sessionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Service{}, fmt.Errorf("store: creating service %q in session %d: %w", name, sessionID, err)
	}
	if affected == 0 {
		// A concurrent caller created it between the lookup and the insert;
		// DO NOTHING turns that into zero affected rows rather than a
		// unique-constraint error, so read the winner's row.
		svc, err := s.findService(sessionID, name)
		if errors.Is(err, sql.ErrNoRows) {
			return Service{}, fmt.Errorf("store: service %q in session %d was neither created nor found", name, sessionID)
		}
		return svc, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Service{}, fmt.Errorf("store: reading new service id for %q: %w", name, err)
	}
	return Service{
		ID:        id,
		SessionID: sessionID,
		Name:      name,
		Active:    true,
		CreatedAt: now,
	}, nil
}

// GetServiceByName looks up the service called name in session sessionID,
// reporting found = false (and no error) when there is none. Inactive services
// are returned too, with Active = false.
//
// Unlike GetOrCreateService it never creates a row: the engine resolves
// admin-supplied names with it, and must be able to tell an unknown name
// apart from a valid one rather than conjuring a service from a typo.
func (s *Store) GetServiceByName(sessionID int64, name string) (Service, bool, error) {
	svc, err := s.findService(sessionID, name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Service{}, false, nil
	case err != nil:
		return Service{}, false, err
	default:
		return svc, true, nil
	}
}

// GetService loads the service with the given id. The returned error wraps
// sql.ErrNoRows when there is no such service, so a caller handed an id from
// outside (an admin request, say) can tell "no such service" apart from "the
// query failed".
func (s *Store) GetService(serviceID int64) (Service, error) {
	row := s.db.QueryRow(`SELECT `+serviceColumns+` FROM services WHERE id = ?`, serviceID)
	svc, err := scanService(row)
	if err != nil {
		return Service{}, fmt.Errorf("store: getting service %d: %w", serviceID, err)
	}
	return svc, nil
}

// ListActiveServices returns the session's services that have not been
// retired, ordered by name. Scoring-paused services are included: a pause
// stops points moving, it does not stop the checks running.
func (s *Store) ListActiveServices(sessionID int64) ([]Service, error) {
	rows, err := s.db.Query(
		`SELECT `+serviceColumns+` FROM services
		 WHERE session_id = ? AND active = TRUE
		 ORDER BY name`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: listing active services in session %d: %w", sessionID, err)
	}
	defer rows.Close()

	var out []Service
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning service row: %w", err)
		}
		out = append(out, svc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing active services in session %d: %w", sessionID, err)
	}
	return out, nil
}

// SetServiceActive retires (active = false) or reinstates a service. It leaves
// the service's scoring-paused flag alone.
func (s *Store) SetServiceActive(serviceID int64, active bool) error {
	if err := s.setServiceFlag(`UPDATE services SET active = ? WHERE id = ?`, active, serviceID); err != nil {
		return fmt.Errorf("store: setting service %d active to %t: %w", serviceID, active, err)
	}
	return nil
}

// SetServiceScoringPaused records whether this one service's results stop
// moving points. It leaves the service's active flag alone, and is independent
// of the global pause in SetGlobalScoringPaused — either one suppresses
// scoring. Unlike the global flag, which Open resets to paused on every
// startup, this is durable operator state that survives a restart.
func (s *Store) SetServiceScoringPaused(serviceID int64, paused bool) error {
	if err := s.setServiceFlag(`UPDATE services SET scoring_paused = ? WHERE id = ?`, paused, serviceID); err != nil {
		return fmt.Errorf("store: setting service %d scoring_paused to %t: %w", serviceID, paused, err)
	}
	return nil
}

// setServiceFlag runs query with (value, serviceID), reporting sql.ErrNoRows
// if no row matched. SQLite does not treat an UPDATE that matches nothing as
// an error, so without that check a caller passing an unknown id would be told
// its change succeeded when nothing was written.
func (s *Store) setServiceFlag(query string, value bool, serviceID int64) error {
	res, err := s.db.Exec(query, value, serviceID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// InsertConfigSnapshot appends the config now in force for a service. Every
// call adds a row rather than replacing the last one: a check result scored an
// hour ago has to stay explainable by the configuration that produced it, even
// after config.yaml is edited and reloaded.
func (s *Store) InsertConfigSnapshot(serviceID int64, cfg config.ServiceConfig) error {
	// Checks with no params still get a JSON object, so readers of the column
	// never have to handle the literal "null".
	params := cfg.Params
	if params == nil {
		params = map[string]any{}
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("store: marshaling params for service %d: %w", serviceID, err)
	}
	_, err = s.db.Exec(
		`INSERT INTO service_config_snapshots
		 (service_id, applied_at, host, check_type, points_per_failure, params_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		serviceID, time.Now().UTC().UnixMilli(), cfg.Host, cfg.CheckType,
		cfg.PointsPerFailure, string(paramsJSON),
	)
	if err != nil {
		return fmt.Errorf("store: inserting config snapshot for service %d: %w", serviceID, err)
	}
	return nil
}

// findService loads the service called name in session sessionID. The returned
// error is sql.ErrNoRows (unwrapped) when there is no such service, so callers
// can tell "absent" apart from "query failed".
func (s *Store) findService(sessionID int64, name string) (Service, error) {
	row := s.db.QueryRow(
		`SELECT `+serviceColumns+` FROM services WHERE session_id = ? AND name = ?`,
		sessionID, name,
	)
	switch svc, err := scanService(row); {
	case errors.Is(err, sql.ErrNoRows):
		return Service{}, sql.ErrNoRows
	case err != nil:
		return Service{}, fmt.Errorf("store: looking up service %q in session %d: %w", name, sessionID, err)
	default:
		return svc, nil
	}
}
