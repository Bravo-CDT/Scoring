package store

import (
	"fmt"
	"time"
)

// CredentialOverride is a blue team change to the username and password one
// service's check logs in with, in place of the config's.
type CredentialOverride struct {
	Service   string
	Username  string
	Password  string
	ChangedAt time.Time
}

// SetCredentialOverride records o for sessionID, replacing any override the
// service already had.
func (s *Store) SetCredentialOverride(sessionID int64, o CredentialOverride) error {
	_, err := s.db.Exec(`
		INSERT INTO credential_overrides (session_id, service, username, password, changed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (session_id, service) DO UPDATE SET
			username = excluded.username, password = excluded.password, changed_at = excluded.changed_at`,
		sessionID, o.Service, o.Username, o.Password, o.ChangedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: setting credential override for %q: %w", o.Service, err)
	}
	return nil
}

// CredentialOverrides returns sessionID's overrides keyed by service name.
func (s *Store) CredentialOverrides(sessionID int64) (map[string]CredentialOverride, error) {
	rows, err := s.db.Query(`
		SELECT service, username, password, changed_at FROM credential_overrides
		WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: listing credential overrides: %w", err)
	}
	defer rows.Close()
	out := make(map[string]CredentialOverride)
	for rows.Next() {
		var o CredentialOverride
		var changedAt int64
		if err := rows.Scan(&o.Service, &o.Username, &o.Password, &changedAt); err != nil {
			return nil, fmt.Errorf("store: reading credential override: %w", err)
		}
		o.ChangedAt = time.UnixMilli(changedAt)
		out[o.Service] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing credential overrides: %w", err)
	}
	return out, nil
}

// DeleteCredentialOverride removes service's override, putting it back on the
// config's credentials, and reports whether it had one.
func (s *Store) DeleteCredentialOverride(sessionID int64, service string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM credential_overrides WHERE session_id = ? AND service = ?`, sessionID, service)
	if err != nil {
		return false, fmt.Errorf("store: deleting credential override for %q: %w", service, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: deleting credential override for %q: %w", service, err)
	}
	return n > 0, nil
}
