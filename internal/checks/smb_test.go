package checks

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// NOTE: unlike http/tcp_connect/smtp/postgres, this package has no real or
// practical fake SMB2 server to test the success path against: SMB2 is a
// large binary protocol (NTLMv2 session setup, tree connect, SRVSVC-over-
// named-pipe share enumeration, directory queries) that isn't feasible to
// hand-roll here the way the SMTP and Postgres fakes were. These tests cover
// param validation and the connection-level failure paths; the actual
// login/share-listing/file-listing success path needs a manual smoke test
// against a real SMB host before relying on this check live.

func TestSMB_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	_, port := splitHostPortInt(t, addr)
	res := SMB(context.Background(), "127.0.0.1", map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "share": "sambashare",
	})
	if res.Success {
		t.Fatal("expected failure when nothing is listening")
	}
}

func TestSMB_ContextDeadline(t *testing.T) {
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
		<-make(chan struct{}) // accept the TCP connection, then never speak SMB2
	}()

	_, port := splitHostPortInt(t, ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := SMB(ctx, host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "share": "sambashare",
	})
	if res.Success {
		t.Fatal("expected failure against a server that never speaks SMB2")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("SMB took %v to return after a 100ms deadline; it must respect ctx's deadline", elapsed)
	}
}

func TestSMB_RejectsEmptyHost(t *testing.T) {
	res := SMB(context.Background(), "", map[string]any{
		"port": 445, "user": "mrkrabs", "password": "Patty321!", "share": "sambashare",
	})
	if res.Success {
		t.Fatal("expected failure for an empty host")
	}
	if !strings.Contains(res.Detail, "host") {
		t.Errorf("Detail = %q, expected it to mention the host", res.Detail)
	}
}

func TestSMB_InvalidParams(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		mention string
	}{
		{"missing port", map[string]any{"user": "u", "password": "p", "share": "s"}, "port"},
		{"missing user", map[string]any{"port": 445, "password": "p", "share": "s"}, "user"},
		{"empty user", map[string]any{"port": 445, "user": "", "password": "p", "share": "s"}, "user"},
		{"missing password", map[string]any{"port": 445, "user": "u", "share": "s"}, "password"},
		{"missing share", map[string]any{"port": 445, "user": "u", "password": "p"}, "share"},
		{"empty share", map[string]any{"port": 445, "user": "u", "password": "p", "share": ""}, "share"},
		{"unknown param", map[string]any{"port": 445, "user": "u", "password": "p", "share": "s", "shre": "s"}, "shre"},
		{"port out of range", map[string]any{"port": 70000, "user": "u", "password": "p", "share": "s"}, "port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := SMB(context.Background(), "127.0.0.1", tt.params)
			if res.Success {
				t.Fatal("expected failure for invalid params")
			}
			if !strings.Contains(res.Detail, tt.mention) {
				t.Errorf("Detail = %q, expected it to mention %q", res.Detail, tt.mention)
			}
		})
	}
}
