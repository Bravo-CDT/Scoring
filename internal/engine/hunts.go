package engine

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Bravo-CDT/Scoring/internal/store"
)

// TeamEither is a hunt either team can claim.
const TeamEither = "either"

// MaxHuntDescription bounds a hunt's description, in bytes.
const MaxHuntDescription = 2000

// Errors the hunt operations return. Their messages are shown to the teams
// and the admin as they are.
var (
	ErrHuntNotFound = errors.New("no such hunt")
	ErrHuntClosed   = errors.New("this hunt is not taking codes")
	ErrHuntClaimed  = errors.New("this hunt has already been claimed")
	ErrWrongCode    = errors.New("that code is not right")
	ErrInvalidHunt  = errors.New("a hunt needs a title of 1 to 200 characters, a team (blue, red or either), a code of 1 to 200 characters, points of at least 1, a deadline, and a description of at most 2000 bytes")
)

// HuntView is a hunt as its team and the admin see it.
type HuntView struct {
	ID    int64  `json:"id"`
	Team  string `json:"team"`
	Title string `json:"title"`
	// Description is empty for a team until the hunt is first opened.
	Description string `json:"description"`
	// Code is only shown to the admin.
	Code     string    `json:"code,omitempty"`
	Points   int       `json:"points"`
	Open     bool      `json:"open"`
	Deadline time.Time `json:"deadline"`
	// Claimable is whether a code can be submitted now: open, before the
	// deadline, and unclaimed.
	Claimable bool       `json:"claimable"`
	ClaimedBy string     `json:"claimed_by,omitempty"`
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	// PointsMoved is what the claim actually moved, which can be less than
	// Points.
	PointsMoved *int `json:"points_moved,omitempty"`
}

// HuntInput is a hunt as the admin writes it.
type HuntInput struct {
	Title       string    `json:"title"`
	Team        string    `json:"team"`
	Description string    `json:"description"`
	Code        string    `json:"code"`
	Points      int       `json:"points"`
	Deadline    time.Time `json:"deadline"`
}

// Hunts lists the hunts team can see, or every hunt when team is "" (the
// admin).
func (e *Engine) Hunts(team string) ([]HuntView, error) {
	hs, err := e.store.ListHunts(e.session.ID, team)
	if err != nil {
		return nil, fmt.Errorf("engine: listing hunts: %w", err)
	}
	now := time.Now()
	out := make([]HuntView, 0, len(hs))
	for _, h := range hs {
		out = append(out, huntView(h, team != "", now))
	}
	return out, nil
}

// CreateHunt adds a closed hunt, as the admin.
func (e *Engine) CreateHunt(in HuntInput) (HuntView, error) {
	title, ok := validHunt(in)
	if !ok || (in.Team != TeamBlue && in.Team != TeamRed && in.Team != TeamEither) {
		return HuntView{}, ErrInvalidHunt
	}
	e.huntMu.Lock()
	defer e.huntMu.Unlock()
	h, err := e.store.CreateHunt(e.session.ID, store.Hunt{
		Team: in.Team, Title: title, Description: in.Description, Code: in.Code,
		Points: in.Points, Deadline: in.Deadline.UTC().Truncate(time.Millisecond),
	})
	if err != nil {
		return HuntView{}, fmt.Errorf("engine: creating hunt: %w", err)
	}
	if err := e.logHuntEvent(ActorAdmin, EventHuntCreated, h, "created", ""); err != nil {
		return HuntView{}, err
	}
	return huntView(h, false, time.Now()), nil
}

// UpdateHunt rewrites a hunt's title, description, code, points and deadline
// and sets whether it's open, as the admin. Its team is fixed, so in.Team is
// ignored. A claimed hunt can't be changed.
func (e *Engine) UpdateHunt(id int64, in HuntInput, open bool) (HuntView, error) {
	title, ok := validHunt(in)
	if !ok {
		return HuntView{}, ErrInvalidHunt
	}
	e.huntMu.Lock()
	defer e.huntMu.Unlock()
	h, err := e.getHunt("", id)
	if err != nil {
		return HuntView{}, err
	}
	if h.ClaimedBy != "" {
		return HuntView{}, ErrHuntClaimed
	}
	h.Title, h.Description, h.Code, h.Points = title, in.Description, in.Code, in.Points
	h.Deadline, h.Open = in.Deadline.UTC().Truncate(time.Millisecond), open
	if err := e.store.UpdateHunt(e.session.ID, h); err != nil {
		return HuntView{}, fmt.Errorf("engine: updating hunt: %w", err)
	}
	h.Released = h.Released || open
	state := "closed"
	if open {
		state = "open"
	}
	if err := e.logHuntEvent(ActorAdmin, EventHuntUpdated, h, "updated", state); err != nil {
		return HuntView{}, err
	}
	return huntView(h, false, time.Now()), nil
}

// ClaimHunt checks team's code for the hunt and, if it's right, claims the
// hunt: its points move to blue for a blue claim, from blue to red for a red
// one. Five wrong codes in a row lock the team out for a minute.
func (e *Engine) ClaimHunt(team string, id int64, code string) (HuntView, error) {
	// "" would be read as the admin, who has no side to pay out to.
	if team != TeamBlue && team != TeamRed {
		return HuntView{}, ErrHuntNotFound
	}
	if _, err := e.getHunt(team, id); err != nil {
		return HuntView{}, err
	}
	if left := e.claims.wait(team); left > 0 {
		return HuntView{}, &CooldownError{RetryAfter: left}
	}
	e.huntMu.Lock()
	defer e.huntMu.Unlock()
	h, err := e.getHunt(team, id)
	if err != nil {
		return HuntView{}, err
	}
	if h.ClaimedBy != "" {
		return HuntView{}, ErrHuntClaimed
	}
	if !claimable(h, time.Now()) {
		return HuntView{}, ErrHuntClosed
	}
	// Claims that queued on huntMu all passed the check above before any
	// of them was counted, so look again.
	if left := e.claims.wait(team); left > 0 {
		return HuntView{}, &CooldownError{RetryAfter: left}
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(h.Code)) != 1 {
		e.claims.fail(team)
		return HuntView{}, ErrWrongCode
	}
	from, to := store.SideManual, store.SideBlue
	actor := ActorBlueTeam
	if team == TeamRed {
		from, to = store.SideBlue, store.SideRed
		actor = ActorRedTeam
	}
	reason := fmt.Sprintf("hunt %d: %s", h.ID, h.Title)
	if h, err = e.store.ClaimHunt(e.session.ID, id, team, from, to, reason); err != nil {
		if errors.Is(err, store.ErrHuntClaimed) {
			return HuntView{}, ErrHuntClaimed
		}
		return HuntView{}, fmt.Errorf("engine: claiming hunt: %w", err)
	}
	e.claims.succeed(team)
	if err := e.logHuntEvent(actor, EventHuntClaimed, h, "claimed", ""); err != nil {
		return HuntView{}, err
	}
	return huntView(h, true, time.Now()), nil
}

// DeleteHunt removes the hunt and takes back any points its claim moved, as
// the admin.
func (e *Engine) DeleteHunt(id int64) error {
	e.huntMu.Lock()
	defer e.huntMu.Unlock()
	h, err := e.getHunt("", id)
	if err != nil {
		return err
	}
	if _, err := e.store.VoidLedgerEntry(e.session.ID, store.SourceScavengerHunt, id, "hunt deleted"); err != nil {
		return fmt.Errorf("engine: deleting hunt: %w", err)
	}
	if err := e.store.DeleteHunt(e.session.ID, id); err != nil {
		return fmt.Errorf("engine: deleting hunt: %w", err)
	}
	detail := ""
	if h.PointsMoved != nil {
		detail = fmt.Sprintf("its %d points taken back", *h.PointsMoved)
	}
	return e.logHuntEvent(ActorAdmin, EventHuntDeleted, h, "deleted", detail)
}

// getHunt loads hunt id for team, or for the admin when team is "". A hunt
// team can't see is ErrHuntNotFound, as if it didn't exist.
func (e *Engine) getHunt(team string, id int64) (store.Hunt, error) {
	h, ok, err := e.store.GetHunt(e.session.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Hunt{}, ErrHuntNotFound
		}
		return store.Hunt{}, fmt.Errorf("engine: loading hunt: %w", err)
	}
	if !ok || (team != "" && h.Team != team && h.Team != TeamEither) {
		return store.Hunt{}, ErrHuntNotFound
	}
	return h, nil
}

// huntView is h as the admin sees it, or as a team does when forTeam is set:
// without the code, and without the description until it's first opened.
func huntView(h store.Hunt, forTeam bool, now time.Time) HuntView {
	v := HuntView{
		ID: h.ID, Team: h.Team, Title: h.Title, Description: h.Description, Code: h.Code,
		Points: h.Points, Open: h.Open, Deadline: h.Deadline, Claimable: claimable(h, now),
		ClaimedBy: h.ClaimedBy, ClaimedAt: h.ClaimedAt, PointsMoved: h.PointsMoved,
	}
	if forTeam {
		v.Code = ""
		if !h.Released {
			v.Description = ""
		}
	}
	return v
}

func claimable(h store.Hunt, now time.Time) bool {
	return h.Open && h.ClaimedBy == "" && now.Before(h.Deadline)
}

// validHunt checks the parts of in a create and an update share, returning
// the title trimmed. The team is checked by CreateHunt alone.
func validHunt(in HuntInput) (title string, ok bool) {
	title = strings.TrimSpace(in.Title)
	ok = validTitle(title) && in.Points >= 1 && !in.Deadline.IsZero() &&
		len(in.Description) <= MaxHuntDescription &&
		in.Code != "" && len(in.Code) <= maxInjectTitle && !strings.ContainsFunc(in.Code, unicode.IsControl)
	return title, ok
}

// logHuntEvent logs a hunt event. Events are public, so the message names the
// hunt only by id; its title, team and points go in the detail, which only
// admins see. The code is never logged.
func (e *Engine) logHuntEvent(actor Actor, eventType string, h store.Hunt, verb, extra string) error {
	detail := fmt.Sprintf("%s, for %s, %d points", h.Title, h.Team, h.Points)
	if extra != "" {
		detail += ": " + extra
	}
	return e.logEvent(store.Event{
		Level: "info", Actor: string(actor), Type: eventType,
		Message: fmt.Sprintf("hunt %d %s", h.ID, verb), Detail: detail,
	})
}
