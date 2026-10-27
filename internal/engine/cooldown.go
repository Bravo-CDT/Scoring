package engine

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Wrong codes in a row that lock a team out of claiming, and for how long.
const (
	claimFailLimit = 5
	claimLockout   = 60 * time.Second
)

// ErrCooldown is what a CooldownError matches with errors.Is.
var ErrCooldown = errors.New("too many wrong codes")

// CooldownError is a claim refused because the team guessed wrong too many
// times in a row. RetryAfter is how long the lock has left.
type CooldownError struct{ RetryAfter time.Duration }

func (e *CooldownError) Error() string {
	secs := int(math.Ceil(e.RetryAfter.Seconds()))
	return fmt.Sprintf("too many wrong codes; try again in %d seconds", secs)
}

func (e *CooldownError) Is(target error) bool { return target == ErrCooldown }

type claimState struct {
	fails  int
	locked time.Time // the lock ends then; zero when not locked
}

// claimLimiter locks a team out of claiming after a run of wrong codes. It is
// a type of its own so tests can give it a fake clock.
type claimLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*claimState
}

func newClaimLimiter(now func() time.Time) *claimLimiter {
	return &claimLimiter{now: now, state: make(map[string]*claimState)}
}

// wait is how long team must wait before claiming again, 0 when it needn't.
// An expired lock is cleared, and its count with it.
func (l *claimLimiter) wait(team string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[team]
	if st == nil || st.locked.IsZero() {
		return 0
	}
	if left := st.locked.Sub(l.now()); left > 0 {
		return left
	}
	delete(l.state, team)
	return 0
}

// fail counts a wrong code. The fifth in a row locks the team and zeroes the
// count.
func (l *claimLimiter) fail(team string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[team]
	if st == nil {
		st = &claimState{}
		l.state[team] = st
	}
	if st.fails++; st.fails >= claimFailLimit {
		st.fails = 0
		st.locked = l.now().Add(claimLockout)
	}
}

// succeed zeroes the team's count.
func (l *claimLimiter) succeed(team string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.state[team]; st != nil {
		st.fails = 0
	}
}
