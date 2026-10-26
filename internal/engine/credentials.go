package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Bravo-CDT/Scoring/internal/store"
)

// CredentialEntry is one credentialed service as the blue team and admins
// see it: who its check logs in as, and whether that is the config's default
// or a blue team change. Never the password.
type CredentialEntry struct {
	Service  string `json:"service"`
	Host     string `json:"host"`
	HostName string `json:"host_name,omitempty"`
	Username string `json:"username"`
	// ChangedAt is when the blue team last changed it; nil while the check
	// uses the config's credentials.
	ChangedAt *time.Time `json:"changed_at,omitempty"`
}

// Credentials is every credentialed service, by name, and the users the blue
// team may switch one to.
type Credentials struct {
	AllowedUsers []string          `json:"allowed_users"`
	Entries      []CredentialEntry `json:"credentials"`
}

// Credentials lists every active service whose check has credentials, from
// the config last loaded, with any blue team change applied.
func (e *Engine) Credentials() (Credentials, error) {
	l := e.currentLayout()
	out := Credentials{AllowedUsers: l.allowedUsers, Entries: []CredentialEntry{}}
	if out.AllowedUsers == nil {
		out.AllowedUsers = []string{}
	}
	svcs, err := e.store.ListActiveServices(e.session.ID)
	if err != nil {
		return Credentials{}, fmt.Errorf("engine: listing credentials: %w", err)
	}
	overrides, err := e.store.CredentialOverrides(e.session.ID)
	if err != nil {
		return Credentials{}, fmt.Errorf("engine: listing credentials: %w", err)
	}
	for _, svc := range svcs {
		if entry, ok := l.credentialEntry(svc.Name, overrides); ok {
			out.Entries = append(out.Entries, entry)
		}
	}
	slices.SortFunc(out.Entries, func(a, b CredentialEntry) int { return strings.Compare(a.Service, b.Service) })
	return out, nil
}

// SetCredential switches service's check to username, one of the allowed
// users, with password, from the next round on, as the blue team.
func (e *Engine) SetCredential(service, username, password string) (CredentialEntry, error) {
	if _, err := e.lookup(service); err != nil {
		return CredentialEntry{}, err
	}
	l := e.currentLayout()
	if !l.services[service].credentialed {
		return CredentialEntry{}, ErrNotCredentialed
	}
	if !slices.Contains(l.allowedUsers, username) {
		return CredentialEntry{}, ErrUserNotAllowed
	}
	if password == "" || len(password) > maxCredentialPassword {
		return CredentialEntry{}, ErrInvalidPassword
	}
	o := store.CredentialOverride{Service: service, Username: username, Password: password, ChangedAt: time.Now()}
	if err := e.store.SetCredentialOverride(e.session.ID, o); err != nil {
		return CredentialEntry{}, fmt.Errorf("engine: setting credential: %w", err)
	}
	if err := e.logCredentialEvent(ActorBlueTeam, service, EventCredentialChanged,
		fmt.Sprintf("%s: credentials changed, username %s", service, username)); err != nil {
		return CredentialEntry{}, err
	}
	entry, _ := l.credentialEntry(service, map[string]store.CredentialOverride{service: o})
	return entry, nil
}

// ResetCredential drops the blue team's change to service's credentials, so
// its check uses the config's again from the next round on, as an admin.
func (e *Engine) ResetCredential(service string) (CredentialEntry, error) {
	if _, err := e.lookup(service); err != nil {
		return CredentialEntry{}, err
	}
	l := e.currentLayout()
	if !l.services[service].credentialed {
		return CredentialEntry{}, ErrNotCredentialed
	}
	if _, err := e.store.DeleteCredentialOverride(e.session.ID, service); err != nil {
		return CredentialEntry{}, fmt.Errorf("engine: resetting credential: %w", err)
	}
	if err := e.logCredentialEvent(ActorAdmin, service, EventCredentialReset,
		fmt.Sprintf("%s: credentials reset to default", service)); err != nil {
		return CredentialEntry{}, err
	}
	entry, _ := l.credentialEntry(service, nil)
	return entry, nil
}

// logCredentialEvent logs a credential change against service.
func (e *Engine) logCredentialEvent(actor Actor, service, eventType, message string) error {
	svc, err := e.lookup(service)
	if err != nil {
		return err
	}
	return e.logServiceEvent(actor, svc, eventType, message)
}

// currentLayout is the layout from the config last loaded, or an empty one
// before any.
func (e *Engine) currentLayout() *layout {
	if l := e.layout.Load(); l != nil {
		return l
	}
	return &layout{networks: []Network{}, hosts: []Host{}}
}

// credentialEntry is name's entry with its override from overrides applied,
// or false if name's check has no credentials.
func (l *layout) credentialEntry(name string, overrides map[string]store.CredentialOverride) (CredentialEntry, bool) {
	sl, ok := l.services[name]
	if !ok || !sl.credentialed {
		return CredentialEntry{}, false
	}
	entry := CredentialEntry{Service: name, Host: sl.host, Username: sl.defaultUser}
	for _, h := range l.hosts {
		if h.Address == sl.host {
			entry.HostName = h.Name
		}
	}
	if o, ok := overrides[name]; ok {
		entry.Username = o.Username
		changedAt := o.ChangedAt.UTC()
		entry.ChangedAt = &changedAt
	}
	return entry, true
}
