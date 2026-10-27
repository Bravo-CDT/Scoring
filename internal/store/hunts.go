package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrHuntClaimed reports that a hunt has already been claimed, so its points
// have already moved.
var ErrHuntClaimed = errors.New("store: hunt already claimed")

// Hunt is a code for one team, or either, to find and submit for points.
// ClaimedBy, ClaimedAt and PointsMoved are set once it's claimed.
type Hunt struct {
	ID          int64
	Team        string // blue, red or either
	Title       string
	Description string
	Code        string
	Points      int
	Open        bool
	// Released is whether the hunt has ever been opened: its team sees the
	// description from then on.
	Released  bool
	Deadline  time.Time
	CreatedAt time.Time
	ClaimedBy string // "" until claimed; else SideBlue or SideRed
	ClaimedAt *time.Time
	// PointsMoved is what the ledger entry actually moved, which can be less
	// than Points.
	PointsMoved *int
}

const huntColumns = `id, team, title, description, code, points, open, released, deadline, created_at, claimed_by, claimed_at, points_moved`

func scanHunt(row interface{ Scan(...any) error }) (Hunt, error) {
	var h Hunt
	var deadline, created int64
	var claimedBy sql.NullString
	var claimedAt, moved sql.NullInt64
	if err := row.Scan(&h.ID, &h.Team, &h.Title, &h.Description, &h.Code, &h.Points, &h.Open, &h.Released, &deadline, &created, &claimedBy, &claimedAt, &moved); err != nil {
		return Hunt{}, err
	}
	h.Deadline = time.UnixMilli(deadline).UTC()
	h.CreatedAt = time.UnixMilli(created).UTC()
	h.ClaimedBy = claimedBy.String
	if claimedAt.Valid {
		a := time.UnixMilli(claimedAt.Int64).UTC()
		h.ClaimedAt = &a
	}
	if moved.Valid {
		m := int(moved.Int64)
		h.PointsMoved = &m
	}
	return h, nil
}

// CreateHunt adds a closed, unclaimed hunt from h's team, title, description,
// code, points and deadline.
func (s *Store) CreateHunt(sessionID int64, h Hunt) (Hunt, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.db.Exec(
		`INSERT INTO hunts (session_id, team, title, description, code, points, deadline, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, h.Team, h.Title, h.Description, h.Code, h.Points, h.Deadline.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Hunt{}, fmt.Errorf("store: creating hunt %q: %w", h.Title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Hunt{}, fmt.Errorf("store: reading new hunt id: %w", err)
	}
	return Hunt{
		ID: id, Team: h.Team, Title: h.Title, Description: h.Description, Code: h.Code, Points: h.Points,
		Deadline: h.Deadline.UTC().Truncate(time.Millisecond), CreatedAt: now,
	}, nil
}

// GetHunt returns the hunt with id, and whether there is one.
func (s *Store) GetHunt(sessionID, id int64) (Hunt, bool, error) {
	h, err := scanHunt(s.db.QueryRow(
		`SELECT `+huntColumns+` FROM hunts WHERE session_id = ? AND id = ?`, sessionID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Hunt{}, false, nil
	}
	if err != nil {
		return Hunt{}, false, fmt.Errorf("store: loading hunt %d: %w", id, err)
	}
	return h, true, nil
}

// ListHunts returns team's hunts and those for either team, or every hunt
// when team is "", by deadline and then id.
func (s *Store) ListHunts(sessionID int64, team string) ([]Hunt, error) {
	rows, err := s.db.Query(
		`SELECT `+huntColumns+` FROM hunts
		 WHERE session_id = ? AND (? = '' OR team = ? OR team = 'either')
		 ORDER BY deadline, id`, sessionID, team, team)
	if err != nil {
		return nil, fmt.Errorf("store: listing hunts: %w", err)
	}
	defer rows.Close()
	var out []Hunt
	for rows.Next() {
		h, err := scanHunt(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading hunt: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing hunts: %w", err)
	}
	return out, nil
}

// UpdateHunt saves h's title, description, code, points, open state and
// deadline, and marks it released once it's open. Claiming is ClaimHunt's
// alone.
func (s *Store) UpdateHunt(sessionID int64, h Hunt) error {
	_, err := s.db.Exec(
		`UPDATE hunts SET title = ?, description = ?, code = ?, points = ?, open = ?, released = released OR ?, deadline = ?
		 WHERE session_id = ? AND id = ?`,
		h.Title, h.Description, h.Code, h.Points, h.Open, h.Open, h.Deadline.UnixMilli(), sessionID, h.ID)
	if err != nil {
		return fmt.Errorf("store: updating hunt %d: %w", h.ID, err)
	}
	return nil
}

// DeleteHunt removes the hunt. Its ledger entry, if any, is VoidLedgerEntry's
// to undo.
func (s *Store) DeleteHunt(sessionID, id int64) error {
	if _, err := s.db.Exec(`DELETE FROM hunts WHERE session_id = ? AND id = ?`, sessionID, id); err != nil {
		return fmt.Errorf("store: deleting hunt %d: %w", id, err)
	}
	return nil
}

// ClaimHunt records team as the hunt's claimant and the ledger entry moving
// the hunt's points from one side to the other, in one transaction. A hunt
// already claimed is refused with ErrHuntClaimed; a missing one with an error
// wrapping sql.ErrNoRows.
func (s *Store) ClaimHunt(sessionID, id int64, team, from, to, reason string) (Hunt, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Hunt{}, fmt.Errorf("store: beginning hunt claim: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := tx.Exec(
		`UPDATE hunts SET claimed_by = ?, claimed_at = ?
		 WHERE session_id = ? AND id = ? AND claimed_by IS NULL`,
		team, now.UnixMilli(), sessionID, id)
	if err != nil {
		return Hunt{}, fmt.Errorf("store: claiming hunt %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return Hunt{}, fmt.Errorf("store: claiming hunt %d: %w", id, err)
	} else if n == 0 {
		var claimed sql.NullString
		err := tx.QueryRow(`SELECT claimed_by FROM hunts WHERE session_id = ? AND id = ?`, sessionID, id).Scan(&claimed)
		if err != nil {
			return Hunt{}, fmt.Errorf("store: claiming hunt %d: %w", id, err)
		}
		return Hunt{}, fmt.Errorf("%w: hunt %d", ErrHuntClaimed, id)
	}
	var points int
	if err := tx.QueryRow(`SELECT points FROM hunts WHERE session_id = ? AND id = ?`, sessionID, id).Scan(&points); err != nil {
		return Hunt{}, fmt.Errorf("store: claiming hunt %d: %w", id, err)
	}
	entry, err := recordLedgerEntryTx(tx, sessionID, SourceScavengerHunt, id, from, to, points, reason)
	if err != nil {
		return Hunt{}, err
	}
	if _, err := tx.Exec(`UPDATE hunts SET points_moved = ? WHERE session_id = ? AND id = ?`, entry.Points, sessionID, id); err != nil {
		return Hunt{}, fmt.Errorf("store: recording the points hunt %d moved: %w", id, err)
	}
	h, err := scanHunt(tx.QueryRow(`SELECT `+huntColumns+` FROM hunts WHERE session_id = ? AND id = ?`, sessionID, id))
	if err != nil {
		return Hunt{}, fmt.Errorf("store: loading claimed hunt %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Hunt{}, fmt.Errorf("store: committing hunt claim: %w", err)
	}
	return h, nil
}
