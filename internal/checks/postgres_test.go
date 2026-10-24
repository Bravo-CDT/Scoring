package checks

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakePostgresCreds is the username/password/dbname a fakePostgresServer
// accepts; any other combination gets a FATAL ErrorResponse, matching a real
// server's "password authentication failed" behavior closely enough for this
// check's purposes.
type fakePostgresCreds struct {
	user, password, dbname string
}

// fakePostgresServer speaks just enough of the PostgreSQL wire protocol
// (SSLRequest negotiation, cleartext password auth, the simple query
// protocol) to exercise the Postgres check against a real listener, per this
// package's convention of real servers over mocks. It accepts exactly one
// connection.
func fakePostgresServer(t *testing.T, want fakePostgresCreds) (addr, host string) {
	t.Helper()
	host = "127.0.0.1"
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		backend := pgproto3.NewBackend(conn, conn)

		// sslmode=disable means the client never sends an SSLRequest; the
		// first packet is always the real StartupMessage.
		msg, err := backend.ReceiveStartupMessage()
		if err != nil {
			return
		}
		startup, ok := msg.(*pgproto3.StartupMessage)
		if !ok {
			return
		}

		backend.Send(&pgproto3.AuthenticationCleartextPassword{})
		if err := backend.Flush(); err != nil {
			return
		}
		if err := backend.SetAuthType(pgproto3.AuthTypeCleartextPassword); err != nil {
			return
		}
		msg, err = backend.Receive()
		if err != nil {
			return
		}
		pw, ok := msg.(*pgproto3.PasswordMessage)
		if !ok {
			return
		}

		if pw.Password != want.password || startup.Parameters["user"] != want.user || startup.Parameters["database"] != want.dbname {
			backend.Send(&pgproto3.ErrorResponse{
				Severity: "FATAL", Code: "28P01",
				Message: "password authentication failed for user \"" + startup.Parameters["user"] + "\"",
			})
			backend.Flush()
			return
		}

		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "15.0"})
		backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
		backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			return
		}

		for {
			msg, err := backend.Receive()
			if err != nil {
				return
			}
			switch msg.(type) {
			case *pgproto3.Query:
				backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("?column?"), DataTypeOID: 23, DataTypeSize: 4, Format: 0},
				}})
				backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if err := backend.Flush(); err != nil {
					return
				}
			case *pgproto3.Terminate:
				return
			default:
				return
			}
		}
	}()
	return ln.Addr().String(), host
}

func TestPostgres_Success(t *testing.T) {
	addr, host := fakePostgresServer(t, fakePostgresCreds{"mrkrabs", "Patty321!", "KrustySafe"})
	_, port := splitHostPortInt(t, addr)
	res := Postgres(context.Background(), host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "dbname": "KrustySafe",
	})
	if !res.Success {
		t.Fatalf("expected success, got failure: %s", res.Detail)
	}
}

func TestPostgres_WrongPassword(t *testing.T) {
	addr, host := fakePostgresServer(t, fakePostgresCreds{"mrkrabs", "Patty321!", "KrustySafe"})
	_, port := splitHostPortInt(t, addr)
	res := Postgres(context.Background(), host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "wrong", "dbname": "KrustySafe",
	})
	if res.Success {
		t.Fatal("expected failure for a wrong password")
	}
	if !strings.Contains(res.Detail, "mrkrabs") {
		t.Errorf("Detail = %q, expected it to mention the username", res.Detail)
	}
	if strings.Contains(res.Detail, "wrong") {
		t.Errorf("Detail = %q, must not echo the attempted password", res.Detail)
	}
}

func TestPostgres_WrongDatabase(t *testing.T) {
	addr, host := fakePostgresServer(t, fakePostgresCreds{"mrkrabs", "Patty321!", "KrustySafe"})
	_, port := splitHostPortInt(t, addr)
	res := Postgres(context.Background(), host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "dbname": "WrongDB",
	})
	if res.Success {
		t.Fatal("expected failure for the wrong database")
	}
}

func TestPostgres_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	_, port := splitHostPortInt(t, addr)
	res := Postgres(context.Background(), "127.0.0.1", map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "dbname": "KrustySafe",
	})
	if res.Success {
		t.Fatal("expected failure when nothing is listening")
	}
}

func TestPostgres_ContextDeadline(t *testing.T) {
	host := "127.0.0.1"
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-make(chan struct{}) // never respond
	}()

	_, port := splitHostPortInt(t, ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := Postgres(ctx, host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "dbname": "KrustySafe",
	})
	if res.Success {
		t.Fatal("expected failure against a server that never responds")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Postgres took %v to return after a 100ms deadline; it must respect ctx's deadline", elapsed)
	}
}
