package checks

import (
	"context"
	"crypto/tls"
	"net"
	"net/smtp"
	"strconv"
)

// SMTP connects to host on params["port"], performs STARTTLS, then
// authenticates with AUTH PLAIN. It never sends a message: a successful
// login is treated as proof the mail service is healthy, the same way
// pg_isready proves a database is healthy without running a query against
// it. Sending real mail every round would flood the account's mailbox for
// the whole competition.
//
// Params:
//   - port (int, required): TCP port, 1-65535.
//   - user (string, required): AUTH PLAIN username.
//   - password (string, required): AUTH PLAIN password.
//   - insecure_skip_verify (bool, optional, default false): skip TLS
//     certificate verification during STARTTLS, for a self-signed cert.
//
// net/smtp's Client has no context-aware methods, so the underlying
// connection's deadline is set from ctx directly: a hung server still makes
// the check return once ctx is done, per CheckFunc's contract.
func SMTP(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseSMTPParams(params)
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
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fail("starting SMTP session with %s: %v", addr, err)
	}
	defer client.Close()

	if err := client.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: p.insecureSkipVerify}); err != nil {
		return fail("starting TLS with %s: %v", addr, err)
	}

	if err := client.Auth(smtp.PlainAuth("", p.user, p.password, host)); err != nil {
		return fail("authenticating as %q: %v", p.user, err)
	}

	client.Quit()
	return Result{Success: true, Detail: "authenticated as " + p.user}
}

// smtpParams is SMTP's parsed params.
type smtpParams struct {
	port               int
	user               string
	password           string
	insecureSkipVerify bool
}

// parseSMTPParams parses and validates SMTP's params. It is the only place
// they are checked, for ValidateParams and the check alike.
func parseSMTPParams(params map[string]any) (smtpParams, error) {
	if err := rejectUnknownParams(params, "port", "user", "password", "insecure_skip_verify"); err != nil {
		return smtpParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return smtpParams{}, err
	}
	user, ok, err := stringParam(params, "user")
	if err != nil {
		return smtpParams{}, err
	}
	if !ok || user == "" {
		return smtpParams{}, errMissingRequired("user")
	}
	password, ok, err := stringParam(params, "password")
	if err != nil {
		return smtpParams{}, err
	}
	if !ok || password == "" {
		return smtpParams{}, errMissingRequired("password")
	}
	insecureSkipVerify, _, err := boolParam(params, "insecure_skip_verify")
	if err != nil {
		return smtpParams{}, err
	}
	return smtpParams{
		port:               port,
		user:               user,
		password:           password,
		insecureSkipVerify: insecureSkipVerify,
	}, nil
}
