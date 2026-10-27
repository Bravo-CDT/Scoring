package engine

import (
	"errors"
	"testing"
	"time"
)

func TestClaimLimiter(t *testing.T) {
	now := time.Now()
	l := newClaimLimiter(func() time.Time { return now })
	fails := func(n int) {
		for i := 0; i < n; i++ {
			l.fail("blue")
		}
	}

	fails(4)
	if w := l.wait("blue"); w != 0 {
		t.Fatalf("wait after 4 fails = %v, want 0", w)
	}
	fails(1)
	if w := l.wait("blue"); w != time.Minute {
		t.Fatalf("wait after 5 fails = %v, want 60s", w)
	}
	if w := l.wait("red"); w != 0 {
		t.Errorf("red wait = %v, want 0", w)
	}

	now = now.Add(61 * time.Second)
	if w := l.wait("blue"); w != 0 {
		t.Fatalf("wait after the lock expired = %v, want 0", w)
	}
	fails(1)
	if w := l.wait("blue"); w != 0 {
		t.Errorf("one fail after expiry locked: wait = %v", w)
	}

	l.succeed("blue")
	fails(4)
	if w := l.wait("blue"); w != 0 {
		t.Errorf("succeed didn't reset the count: wait = %v", w)
	}
}

func TestCooldownError(t *testing.T) {
	err := error(&CooldownError{RetryAfter: 1500 * time.Millisecond})
	if !errors.Is(err, ErrCooldown) {
		t.Error("CooldownError is not ErrCooldown")
	}
	if got, want := err.Error(), "too many wrong codes; try again in 2 seconds"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
