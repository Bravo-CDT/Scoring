package checks

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// LDAP connects over plain LDAP to host on params["port"], performs a simple
// bind as params["bind_user"], and succeeds if a subtree search under
// params["base_dn"] finds the account whose sAMAccountName is
// params["search_user"]. It mirrors
// `ldapsearch -x -H ldap://host -D bind_user -w password -b base_dn "(sAMAccountName=search_user)" dn`.
//
// Params:
//   - port (int, required): TCP port, 1-65535 (389 for Active Directory).
//   - bind_user (string, required): the bind name, passed to the server as
//     is. Active Directory accepts a UPN, e.g. spongebob@krusty.krab.
//   - password (string, required): the bind password.
//   - base_dn (string, required): where to search, e.g. dc=krusty,dc=krab.
//   - search_user (string, required): the sAMAccountName to find. It is
//     escaped before going into the filter.
func LDAP(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseLDAPParams(params)
	if err != nil {
		return fail("%v", err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(p.port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail("connecting to %s: %v", addr, err)
	}
	// The ldap client ignores ctx, so the conn's deadline is set from ctx,
	// and pulled in to now should ctx be cancelled before it. Every request
	// then fails once ctx is done.
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	l := ldap.NewConn(conn, false)
	l.Start()
	defer l.Close()

	if err := l.Bind(p.bindUser, p.password); err != nil {
		return fail("binding as %q: %v", p.bindUser, err)
	}

	req := ldap.NewSearchRequest(
		p.baseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		1, 0, false,
		fmt.Sprintf("(sAMAccountName=%s)", ldap.EscapeFilter(p.searchUser)),
		[]string{"dn"}, nil,
	)
	res, err := l.Search(req)
	// A server with more than one match returns the first entry and then
	// "size limit exceeded"; the user was still found.
	if err != nil && !(ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) && res != nil && len(res.Entries) > 0) {
		return fail("searching for %q under %s: %v", p.searchUser, p.baseDN, err)
	}
	if len(res.Entries) == 0 {
		return fail("user %q not found under %s", p.searchUser, p.baseDN)
	}
	return Result{Success: true, Detail: fmt.Sprintf("bound as %q; found %q", p.bindUser, p.searchUser)}
}

// ldapParams is LDAP's parsed params.
type ldapParams struct {
	port       int
	bindUser   string
	password   string
	baseDN     string
	searchUser string
}

// parseLDAPParams parses and validates LDAP's params. It is the only place
// they are checked, for ValidateParams and the check alike.
func parseLDAPParams(params map[string]any) (ldapParams, error) {
	if err := rejectUnknownParams(params, "port", "bind_user", "password", "base_dn", "search_user"); err != nil {
		return ldapParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return ldapParams{}, err
	}
	p := ldapParams{port: port}
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"bind_user", &p.bindUser},
		{"password", &p.password},
		{"base_dn", &p.baseDN},
		{"search_user", &p.searchUser},
	} {
		s, ok, err := stringParam(params, f.key)
		if err != nil {
			return ldapParams{}, err
		}
		if !ok || s == "" {
			return ldapParams{}, errMissingRequired(f.key)
		}
		*f.dst = s
	}
	return p, nil
}
