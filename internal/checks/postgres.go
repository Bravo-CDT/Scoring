package checks

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Postgres performs a full login against a PostgreSQL server on
// params["port"] and runs a trivial query, the same way the operator's
// own manual check does ("psql -h ... -U ... -d ... # full login"): a
// server that merely accepts TCP connections but can't authenticate or
// serve a query is not actually up.
//
// Params:
//   - port (int, required): TCP port, 1-65535.
//   - user (string, required): login role.
//   - password (string, required): login password.
//   - dbname (string, required): database to connect to.
//
// The connection is plain (sslmode "disable"), matching the operator's own
// manual psql command, which passes no TLS flags.
func Postgres(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parsePostgresParams(params)
	if err != nil {
		return fail("%v", err)
	}

	connString := postgresConnString(host, p)
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return fail("connecting as %q to database %q: %v", p.user, p.dbname, err)
	}
	defer conn.Close(ctx)

	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fail("querying as %q: %v", p.user, err)
	}
	return Result{Success: true, Detail: fmt.Sprintf("connected and queried as %q", p.user)}
}

// postgresConnString builds a libpq-style connection string. Building it
// with net/url rather than string concatenation keeps the password and
// database name correctly escaped regardless of what characters they
// contain.
func postgresConnString(host string, p postgresParams) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(p.user, p.password),
		Host:   net.JoinHostPort(host, strconv.Itoa(p.port)),
		Path:   "/" + p.dbname,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	// The simple query protocol is all this check needs and is universally
	// supported, so it's used for every connection, not just test doubles.
	q.Set("default_query_exec_mode", "simple_protocol")
	u.RawQuery = q.Encode()
	return u.String()
}

// postgresParams is Postgres's parsed params.
type postgresParams struct {
	port     int
	user     string
	password string
	dbname   string
}

// parsePostgresParams parses and validates Postgres's params. It is the only
// place they are checked, for ValidateParams and the check alike.
func parsePostgresParams(params map[string]any) (postgresParams, error) {
	if err := rejectUnknownParams(params, "port", "user", "password", "dbname"); err != nil {
		return postgresParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return postgresParams{}, err
	}
	user, ok, err := stringParam(params, "user")
	if err != nil {
		return postgresParams{}, err
	}
	if !ok || user == "" {
		return postgresParams{}, errMissingRequired("user")
	}
	password, ok, err := stringParam(params, "password")
	if err != nil {
		return postgresParams{}, err
	}
	if !ok || password == "" {
		return postgresParams{}, errMissingRequired("password")
	}
	dbname, ok, err := stringParam(params, "dbname")
	if err != nil {
		return postgresParams{}, err
	}
	if !ok || dbname == "" {
		return postgresParams{}, errMissingRequired("dbname")
	}
	return postgresParams{port: port, user: user, password: password, dbname: dbname}, nil
}
