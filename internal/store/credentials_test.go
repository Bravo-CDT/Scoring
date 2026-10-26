package store

import (
	"testing"
	"time"
)

func TestCredentialOverrides_SetReplaceAndDelete(t *testing.T) {
	st := openTemp(t)
	sess := newSession(t, st, "comp-1")
	other := newSession(t, st, "comp-2")
	at := time.UnixMilli(1_700_000_000_000)

	set := func(sessionID int64, o CredentialOverride) {
		t.Helper()
		if err := st.SetCredentialOverride(sessionID, o); err != nil {
			t.Fatalf("SetCredentialOverride: %v", err)
		}
	}
	set(sess.ID, CredentialOverride{Service: "postgresql", Username: "mrkrabs", Password: "pw1", ChangedAt: at})
	set(sess.ID, CredentialOverride{Service: "postgresql", Username: "patrick", Password: "pw2", ChangedAt: at.Add(time.Minute)})
	set(sess.ID, CredentialOverride{Service: "smb", Username: "sandy", Password: "pw3", ChangedAt: at})
	set(other.ID, CredentialOverride{Service: "postgresql", Username: "gary", Password: "pw4", ChangedAt: at})

	got, err := st.CredentialOverrides(sess.ID)
	if err != nil {
		t.Fatalf("CredentialOverrides: %v", err)
	}
	want := CredentialOverride{Service: "postgresql", Username: "patrick", Password: "pw2", ChangedAt: at.Add(time.Minute)}
	if len(got) != 2 || got["postgresql"] != want {
		t.Fatalf("overrides = %+v, want postgresql replaced by %+v and smb", got, want)
	}

	deleted, err := st.DeleteCredentialOverride(sess.ID, "postgresql")
	if err != nil || !deleted {
		t.Fatalf("DeleteCredentialOverride = %v, %v; want true, nil", deleted, err)
	}
	if deleted, _ := st.DeleteCredentialOverride(sess.ID, "postgresql"); deleted {
		t.Errorf("deleting it again reported a row deleted")
	}
	got, _ = st.CredentialOverrides(sess.ID)
	if _, ok := got["smb"]; len(got) != 1 || !ok {
		t.Errorf("overrides after delete = %+v, want just smb", got)
	}
	if otherGot, _ := st.CredentialOverrides(other.ID); len(otherGot) != 1 {
		t.Errorf("other session's overrides = %+v, want its one untouched", otherGot)
	}
}
