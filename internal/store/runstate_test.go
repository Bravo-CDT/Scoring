package store

import "testing"

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
