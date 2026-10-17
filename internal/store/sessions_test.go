package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGetOrCreateSession_CreatesNew(t *testing.T) {
	st := openTemp(t)

	before := time.Now().Add(-time.Second)
	sess, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if sess.Name != "comp-1" {
		t.Errorf("Name = %q, want comp-1", sess.Name)
	}
	if sess.StartingScore != 1000 {
		t.Errorf("StartingScore = %d, want 1000", sess.StartingScore)
	}
	if sess.ID == 0 {
		t.Error("expected non-zero session ID")
	}
	if sess.CreatedAt.Before(before) || sess.CreatedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("CreatedAt = %v, want roughly now", sess.CreatedAt)
	}
}

func TestGetOrCreateSession_ExistingIgnoresNewStartingScore(t *testing.T) {
	st := openTemp(t)

	first, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("first GetOrCreateSession: %v", err)
	}

	// Simulate config.yaml being edited to a different starting_score for an
	// already-existing session name: the persisted score wins, because changing
	// it mid-competition would invalidate every ledger entry scored so far.
	second, err := st.GetOrCreateSession("comp-1", 9999)
	if err != nil {
		t.Fatalf("second GetOrCreateSession: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("expected same session ID, got %d and %d", first.ID, second.ID)
	}
	if second.StartingScore != 1000 {
		t.Errorf("StartingScore = %d, want unchanged 1000", second.StartingScore)
	}

	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	if count != 1 {
		t.Errorf("sessions row count = %d, want 1", count)
	}
}

func TestGetOrCreateSession_DistinctNamesGetDistinctRows(t *testing.T) {
	st := openTemp(t)

	a, err := st.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession(comp-1): %v", err)
	}
	b, err := st.GetOrCreateSession("comp-2", 500)
	if err != nil {
		t.Fatalf("GetOrCreateSession(comp-2): %v", err)
	}
	if a.ID == b.ID {
		t.Errorf("distinct sessions share ID %d", a.ID)
	}
	if b.StartingScore != 500 {
		t.Errorf("comp-2 StartingScore = %d, want 500", b.StartingScore)
	}
}

func TestGetOrCreateSession_ConcurrentCallersShareOneRow(t *testing.T) {
	// Serializing statements on one connection does not make the lookup and the
	// insert atomic: several callers can all find no session and then all try to
	// create it. Racing a fresh database repeatedly makes that window reliably
	// hit; every caller must still come back with the same single row.
	const databases, callers = 10, 8
	for range databases {
		st := openTemp(t)

		start := make(chan struct{})
		results := make(chan Session, callers)
		errs := make(chan error, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				sess, err := st.GetOrCreateSession("comp-1", 1000)
				if err != nil {
					errs <- err
					return
				}
				results <- sess
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		for err := range errs {
			t.Fatalf("concurrent GetOrCreateSession: %v", err)
		}
		var first Session
		for sess := range results {
			if first.ID == 0 {
				first = sess
				continue
			}
			if sess.ID != first.ID || sess.StartingScore != first.StartingScore {
				t.Fatalf("concurrent callers disagree: %+v vs %+v", sess, first)
			}
		}

		var count int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
			t.Fatalf("counting sessions: %v", err)
		}
		if count != 1 {
			t.Fatalf("sessions row count = %d, want 1", count)
		}
	}
}

func TestGetOrCreateSession_SurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoring.db")

	st1 := openAt(t, path)
	created, err := st1.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2 := openAt(t, path)
	reloaded, err := st2.GetOrCreateSession("comp-1", 1000)
	if err != nil {
		t.Fatalf("GetOrCreateSession after reopen: %v", err)
	}
	if reloaded.ID != created.ID || reloaded.Name != created.Name || reloaded.StartingScore != created.StartingScore {
		t.Errorf("reloaded = %+v, want %+v", reloaded, created)
	}
	// created_at is stored as Unix milliseconds, and the Session returned at
	// creation time is truncated to match, so the two must be exactly equal.
	if !reloaded.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", reloaded.CreatedAt, created.CreatedAt)
	}
}
