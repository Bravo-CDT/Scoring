package checks

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"

	smb2 "github.com/hirochachacha/go-smb2"
)

// SMB logs into an SMB/CIFS server on params["port"] via NTLM, optionally
// confirms a share is listed, then mounts params["share"] and lists its
// root, optionally confirming a specific file is present -- mirroring
// "smbclient -L ... ; smbclient //host/share -c ls".
//
// Params:
//   - port (int, required): TCP port, 1-65535 (445 for SMB).
//   - user (string, required): login username.
//   - password (string, required): login password.
//   - domain (string, optional, default ""): NTLM domain/workgroup.
//   - share (string, required): share to mount and list.
//   - expect_share (string, optional): if non-empty, must appear in the
//     server's share listing.
//   - expect_file (string, optional): if non-empty, must appear (by exact
//     name) in share's root directory listing.
func SMB(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseSMBParams(params)
	if err != nil {
		return fail("%v", err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(p.port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail("connecting to %s: %v", addr, err)
	}
	defer conn.Close()

	dialer := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: p.user, Password: p.password, Domain: p.domain}}
	session, err := dialer.DialContext(ctx, conn)
	if err != nil {
		return fail("authenticating as %q: %v", p.user, err)
	}
	defer session.Logoff()
	session = session.WithContext(ctx)

	if p.expectShare != "" {
		shares, err := session.ListSharenames()
		if err != nil {
			return fail("listing shares: %v", err)
		}
		if !slices.Contains(shares, p.expectShare) {
			return fail("share %q not found", p.expectShare)
		}
	}

	share, err := session.Mount(p.share)
	if err != nil {
		return fail("mounting share %q: %v", p.share, err)
	}
	defer share.Umount()
	share = share.WithContext(ctx)

	entries, err := share.ReadDir(".")
	if err != nil {
		return fail("listing share %q: %v", p.share, err)
	}
	if p.expectFile != "" {
		found := false
		for _, e := range entries {
			if e.Name() == p.expectFile {
				found = true
				break
			}
		}
		if !found {
			return fail("file %q not found in share %q", p.expectFile, p.share)
		}
	}

	return Result{Success: true, Detail: fmt.Sprintf("connected as %q; share %q listed", p.user, p.share)}
}

// smbParams is SMB's parsed params.
type smbParams struct {
	port        int
	user        string
	password    string
	domain      string
	share       string
	expectShare string
	expectFile  string
}

// parseSMBParams parses and validates SMB's params. It is the only place
// they are checked, for ValidateParams and the check alike.
func parseSMBParams(params map[string]any) (smbParams, error) {
	if err := rejectUnknownParams(params, "port", "user", "password", "domain", "share", "expect_share", "expect_file"); err != nil {
		return smbParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return smbParams{}, err
	}
	user, ok, err := stringParam(params, "user")
	if err != nil {
		return smbParams{}, err
	}
	if !ok || user == "" {
		return smbParams{}, errMissingRequired("user")
	}
	password, ok, err := stringParam(params, "password")
	if err != nil {
		return smbParams{}, err
	}
	if !ok || password == "" {
		return smbParams{}, errMissingRequired("password")
	}
	domain, _, err := stringParam(params, "domain")
	if err != nil {
		return smbParams{}, err
	}
	share, ok, err := stringParam(params, "share")
	if err != nil {
		return smbParams{}, err
	}
	if !ok || share == "" {
		return smbParams{}, errMissingRequired("share")
	}
	expectShare, _, err := stringParam(params, "expect_share")
	if err != nil {
		return smbParams{}, err
	}
	expectFile, _, err := stringParam(params, "expect_file")
	if err != nil {
		return smbParams{}, err
	}
	return smbParams{
		port:        port,
		user:        user,
		password:    password,
		domain:      domain,
		share:       share,
		expectShare: expectShare,
		expectFile:  expectFile,
	}, nil
}
