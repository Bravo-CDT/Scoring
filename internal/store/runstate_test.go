package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestGetRunState_NoCountdownOnFreshDatabase(t *testing.T) {
	st := openTemp(t)

	rs, err := st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.CountdownTarget != nil {
		t.Errorf("CountdownTarget = %v on a fresh database, want nil", rs.CountdownTarget)
	}
}

func TestSetCountdownTarget_PersistsAndClears(t *testing.T) {
	st := openTemp(t)

	// Given in a non-UTC zone: the stored instant must survive, and it is read
	// back in UTC like every other timestamp.
	target := time.Date(2026, 10, 17, 13, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
	if err := st.SetCountdownTarget(&target); err != nil {
		t.Fatalf("SetCountdownTarget: %v", err)
	}
	rs, err := st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.CountdownTarget == nil || !rs.CountdownTarget.Equal(target) {
		t.Fatalf("CountdownTarget = %v, want %v", rs.CountdownTarget, target)
	}
	if rs.CountdownTarget.Location() != time.UTC {
		t.Errorf("CountdownTarget location = %v, want UTC", rs.CountdownTarget.Location())
	}

	if err := st.SetCountdownTarget(nil); err != nil {
		t.Fatalf("SetCountdownTarget(nil): %v", err)
	}
	rs, err = st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.CountdownTarget != nil {
		t.Errorf("CountdownTarget = %v after clearing, want nil", rs.CountdownTarget)
	}
}

func TestOpen_PreservesCountdownTargetAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoring.db")

	st1 := openAt(t, path)
	target := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	if err := st1.SetCountdownTarget(&target); err != nil {
		t.Fatalf("SetCountdownTarget: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The restart's safe-default reset stops the engine and pauses scoring,
	// but the countdown is an admin's schedule, not run state to distrust.
	st2 := openAt(t, path)
	rs, err := st2.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.CountdownTarget == nil || !rs.CountdownTarget.Equal(target) {
		t.Errorf("CountdownTarget = %v after reopen, want %v", rs.CountdownTarget, target)
	}
	if rs.EngineState != EngineStopped || !rs.ScoringPaused {
		t.Errorf("run state after reopen = %+v, want stopped and paused", rs)
	}
}

func TestGetRunState_SafeDefaultsOnFreshDatabase(t *testing.T) {
	st := openTemp(t)

	rs, err := st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.EngineState != "stopped" {
		t.Errorf("EngineState = %q, want stopped", rs.EngineState)
	}
	if !rs.ScoringPaused {
		t.Error("ScoringPaused = false, want true")
	}
}

func TestSetEngineState_And_SetGlobalScoringPaused(t *testing.T) {
	st := openTemp(t)

	if err := st.SetEngineState("running"); err != nil {
		t.Fatalf("SetEngineState: %v", err)
	}
	if err := st.SetGlobalScoringPaused(false); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	rs, err := st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.EngineState != "running" || rs.ScoringPaused {
		t.Errorf("rs = %+v, want running/false", rs)
	}

	// Setting one flag must not disturb the other.
	if err := st.SetGlobalScoringPaused(true); err != nil {
		t.Fatalf("SetGlobalScoringPaused: %v", err)
	}
	rs, err = st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.EngineState != "running" {
		t.Errorf("EngineState changed unexpectedly to %q", rs.EngineState)
	}
	if !rs.ScoringPaused {
		t.Error("ScoringPaused = false, want true")
	}

	if err := st.SetEngineState("stopped"); err != nil {
		t.Fatalf("SetEngineState(stopped): %v", err)
	}
	rs, err = st.GetRunState()
	if err != nil {
		t.Fatalf("GetRunState: %v", err)
	}
	if rs.EngineState != "stopped" {
		t.Errorf("EngineState = %q, want stopped", rs.EngineState)
	}
	if !rs.ScoringPaused {
		t.Error("ScoringPaused = false after engine-state change, want true")
	}
}

func TestRunState_SingleRowOnly(t *testing.T) {
	st := openTemp(t)

	// The run state is a singleton; the id = 1 CHECK keeps a second row out, so
	// GetRunState never has to pick between competing rows.
	if _, err := st.db.Exec(`INSERT INTO engine_run_state (id, engine_state, scoring_paused) VALUES (2, 'running', 0)`); err == nil {
		t.Error("inserting a second engine_run_state row succeeded, want CHECK violation")
	}
}
