package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrInjectApproved reports that an inject has already been approved, so its
// points have already moved.
var ErrInjectApproved = errors.New("store: inject already approved")

// Inject is a written task for one team. Grade is set while it's being graded;
// Points and ApprovedAt once it's approved.
type Inject struct {
	ID         int64
	Team       string
	Title      string
	PDFName    string // the instructions' original file name; "" when none uploaded
	Open       bool
	Deadline   time.Time
	CreatedAt  time.Time
	Grade      *int
	Points     *int
	ApprovedAt *time.Time
}

// Submission is a team's latest upload for an inject. The file is on disk
// under StoredName; FileName is the name it was uploaded with.
type Submission struct {
	InjectID    int64
	Note        string
	FileName    string
	StoredName  string
	Size        int64
	SubmittedAt time.Time
}

const injectColumns = `id, team, title, pdf_name, open, deadline, created_at, grade, points, approved_at`

func scanInject(row interface{ Scan(...any) error }) (Inject, error) {
	var in Inject
	var deadline, created int64
	var grade, points, approved sql.NullInt64
	if err := row.Scan(&in.ID, &in.Team, &in.Title, &in.PDFName, &in.Open, &deadline, &created, &grade, &points, &approved); err != nil {
		return Inject{}, err
	}
	in.Deadline = time.UnixMilli(deadline).UTC()
	in.CreatedAt = time.UnixMilli(created).UTC()
	if grade.Valid {
		g := int(grade.Int64)
		in.Grade = &g
	}
	if points.Valid {
		p := int(points.Int64)
		in.Points = &p
	}
	if approved.Valid {
		a := time.UnixMilli(approved.Int64).UTC()
		in.ApprovedAt = &a
	}
	return in, nil
}

// CreateInject adds a closed, ungraded inject for team.
func (s *Store) CreateInject(sessionID int64, team, title string, deadline time.Time) (Inject, error) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.db.Exec(
		`INSERT INTO injects (session_id, team, title, deadline, created_at) VALUES (?, ?, ?, ?, ?)`,
		sessionID, team, title, deadline.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Inject{}, fmt.Errorf("store: creating inject %q: %w", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Inject{}, fmt.Errorf("store: reading new inject id: %w", err)
	}
	return Inject{ID: id, Team: team, Title: title, Deadline: deadline.UTC().Truncate(time.Millisecond), CreatedAt: now}, nil
}

// GetInject returns the inject with id, and whether there is one.
func (s *Store) GetInject(sessionID, id int64) (Inject, bool, error) {
	in, err := scanInject(s.db.QueryRow(
		`SELECT `+injectColumns+` FROM injects WHERE session_id = ? AND id = ?`, sessionID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Inject{}, false, nil
	}
	if err != nil {
		return Inject{}, false, fmt.Errorf("store: loading inject %d: %w", id, err)
	}
	return in, true, nil
}

// ListInjects returns team's injects, or every inject when team is "", by
// deadline and then id.
func (s *Store) ListInjects(sessionID int64, team string) ([]Inject, error) {
	rows, err := s.db.Query(
		`SELECT `+injectColumns+` FROM injects
		 WHERE session_id = ? AND (? = '' OR team = ?)
		 ORDER BY deadline, id`, sessionID, team, team)
	if err != nil {
		return nil, fmt.Errorf("store: listing injects: %w", err)
	}
	defer rows.Close()
	var out []Inject
	for rows.Next() {
		in, err := scanInject(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading inject: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing injects: %w", err)
	}
	return out, nil
}

// UpdateInject saves in's title, open state, deadline, PDF name and grade.
// Approval is ApproveInject's alone.
func (s *Store) UpdateInject(sessionID int64, in Inject) error {
	var grade sql.NullInt64
	if in.Grade != nil {
		grade = sql.NullInt64{Int64: int64(*in.Grade), Valid: true}
	}
	_, err := s.db.Exec(
		`UPDATE injects SET title = ?, open = ?, deadline = ?, pdf_name = ?, grade = ?
		 WHERE session_id = ? AND id = ?`,
		in.Title, in.Open, in.Deadline.UnixMilli(), in.PDFName, grade, sessionID, in.ID)
	if err != nil {
		return fmt.Errorf("store: updating inject %d: %w", in.ID, err)
	}
	return nil
}

// DeleteInject removes the inject and its submission. Its ledger entry, if
// any, is VoidLedgerEntry's to undo.
func (s *Store) DeleteInject(sessionID, id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: beginning inject delete: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM inject_submissions WHERE session_id = ? AND inject_id = ?`, sessionID, id); err != nil {
		return fmt.Errorf("store: deleting the submission of inject %d: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM injects WHERE session_id = ? AND id = ?`, sessionID, id); err != nil {
		return fmt.Errorf("store: deleting inject %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing inject delete: %w", err)
	}
	return nil
}

// PutSubmission records sub as its inject's submission, replacing any earlier
// one, which it returns so the caller can remove the earlier file.
func (s *Store) PutSubmission(sessionID int64, sub Submission) (previous *Submission, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: beginning submission: %w", err)
	}
	defer tx.Rollback()
	prev, found, err := getSubmission(tx, sessionID, sub.InjectID)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`
		INSERT INTO inject_submissions (inject_id, session_id, note, file_name, stored_name, size, submitted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (inject_id) DO UPDATE SET
			note = excluded.note, file_name = excluded.file_name, stored_name = excluded.stored_name,
			size = excluded.size, submitted_at = excluded.submitted_at`,
		sub.InjectID, sessionID, sub.Note, sub.FileName, sub.StoredName, sub.Size, sub.SubmittedAt.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("store: saving the submission for inject %d: %w", sub.InjectID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: committing the submission for inject %d: %w", sub.InjectID, err)
	}
	if !found {
		return nil, nil
	}
	return &prev, nil
}

// GetSubmission returns the inject's submission, and whether it has one.
func (s *Store) GetSubmission(sessionID, injectID int64) (Submission, bool, error) {
	return getSubmission(s.db, sessionID, injectID)
}

func getSubmission(q rowQuerier, sessionID, injectID int64) (Submission, bool, error) {
	var sub Submission
	var at int64
	err := q.QueryRow(`
		SELECT inject_id, note, file_name, stored_name, size, submitted_at FROM inject_submissions
		WHERE session_id = ? AND inject_id = ?`, sessionID, injectID).
		Scan(&sub.InjectID, &sub.Note, &sub.FileName, &sub.StoredName, &sub.Size, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Submission{}, false, nil
	}
	if err != nil {
		return Submission{}, false, fmt.Errorf("store: loading the submission for inject %d: %w", injectID, err)
	}
	sub.SubmittedAt = time.UnixMilli(at).UTC()
	return sub, true, nil
}

// ApproveInject records the inject's grade, points and approval time, and the
// ledger entry moving points from one side to the other, in one transaction.
// An inject already approved is refused with ErrInjectApproved; a missing one
// with an error wrapping sql.ErrNoRows.
func (s *Store) ApproveInject(sessionID, id int64, from, to string, grade, points int, reason string) (Inject, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Inject{}, fmt.Errorf("store: beginning inject approval: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Truncate(time.Millisecond)
	res, err := tx.Exec(
		`UPDATE injects SET grade = ?, points = ?, approved_at = ?
		 WHERE session_id = ? AND id = ? AND approved_at IS NULL`,
		grade, points, now.UnixMilli(), sessionID, id)
	if err != nil {
		return Inject{}, fmt.Errorf("store: approving inject %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return Inject{}, fmt.Errorf("store: approving inject %d: %w", id, err)
	} else if n == 0 {
		var approved sql.NullInt64
		err := tx.QueryRow(`SELECT approved_at FROM injects WHERE session_id = ? AND id = ?`, sessionID, id).Scan(&approved)
		if err != nil {
			return Inject{}, fmt.Errorf("store: approving inject %d: %w", id, err)
		}
		return Inject{}, fmt.Errorf("%w: inject %d", ErrInjectApproved, id)
	}
	if _, err := recordLedgerEntryTx(tx, sessionID, SourceInject, id, from, to, points, reason); err != nil {
		return Inject{}, err
	}
	in, err := scanInject(tx.QueryRow(`SELECT `+injectColumns+` FROM injects WHERE session_id = ? AND id = ?`, sessionID, id))
	if err != nil {
		return Inject{}, fmt.Errorf("store: loading approved inject %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Inject{}, fmt.Errorf("store: committing inject approval: %w", err)
	}
	return in, nil
}
