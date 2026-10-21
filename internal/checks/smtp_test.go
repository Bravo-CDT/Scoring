package checks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// generateSelfSignedCert returns a self-signed TLS certificate for host,
// usable by a test SMTP server's STARTTLS handshake.
func generateSelfSignedCert(t *testing.T, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("building key pair: %v", err)
	}
	return cert
}

// fakeSMTPServer speaks just enough SMTP (greeting, EHLO, STARTTLS, a
// post-TLS EHLO, AUTH PLAIN, QUIT) to exercise the SMTP check against a real
// listener, per this package's convention of real servers over mocks. It
// accepts exactly one connection and checks the AUTH PLAIN credentials
// against wantUser/wantPassword.
func fakeSMTPServer(t *testing.T, wantUser, wantPassword string) (addr, host string) {
	t.Helper()
	host = "127.0.0.1"
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	cert := generateSelfSignedCert(t, host)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tp := textproto.NewConn(conn)
		tp.PrintfLine("220 fake.smtp ESMTP ready")
		for {
			line, err := tp.ReadLine()
			if err != nil {
				return
			}
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO"):
				tp.PrintfLine("250-fake.smtp")
				tp.PrintfLine("250-STARTTLS")
				tp.PrintfLine("250 AUTH PLAIN")
			case strings.HasPrefix(upper, "STARTTLS"):
				tp.PrintfLine("220 ready to start TLS")
				tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				tp = textproto.NewConn(tlsConn)
			case strings.HasPrefix(upper, "AUTH PLAIN"):
				fields := strings.Fields(line)
				if len(fields) != 3 {
					tp.PrintfLine("501 malformed AUTH PLAIN")
					continue
				}
				decoded, err := base64.StdEncoding.DecodeString(fields[2])
				if err != nil {
					tp.PrintfLine("501 bad base64")
					continue
				}
				parts := strings.Split(string(decoded), "\x00")
				if len(parts) != 3 || parts[1] != wantUser || parts[2] != wantPassword {
					tp.PrintfLine("535 authentication failed")
					continue
				}
				tp.PrintfLine("235 authentication successful")
			case strings.HasPrefix(upper, "QUIT"):
				tp.PrintfLine("221 bye")
				return
			default:
				tp.PrintfLine("500 unrecognized command")
			}
		}
	}()
	return ln.Addr().String(), host
}

func TestSMTP_Success(t *testing.T) {
	addr, host := fakeSMTPServer(t, "mrkrabs", "Patty321!")
	_, port := splitHostPortInt(t, addr)
	res := SMTP(context.Background(), host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "insecure_skip_verify": true,
	})
	if !res.Success {
		t.Fatalf("expected success, got failure: %s", res.Detail)
	}
}

func TestSMTP_WrongPassword(t *testing.T) {
	addr, host := fakeSMTPServer(t, "mrkrabs", "Patty321!")
	_, port := splitHostPortInt(t, addr)
	res := SMTP(context.Background(), host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "wrong", "insecure_skip_verify": true,
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

func TestSMTP_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens now

	_, port := splitHostPortInt(t, addr)
	res := SMTP(context.Background(), "127.0.0.1", map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!",
	})
	if res.Success {
		t.Fatal("expected failure when nothing is listening")
	}
}

func TestSMTP_ContextDeadline(t *testing.T) {
	addr, host := fakeSMTPServerThatHangs(t)
	_, port := splitHostPortInt(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := SMTP(ctx, host, map[string]any{
		"port": port, "user": "mrkrabs", "password": "Patty321!", "insecure_skip_verify": true,
	})
	if res.Success {
		t.Fatal("expected failure against a server that never responds")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("SMTP took %v to return after a 50ms deadline; it must respect ctx's deadline", elapsed)
	}
}

// fakeSMTPServerThatHangs accepts a connection, sends the greeting, then
// never responds to anything else -- proving the check honors ctx's deadline
// rather than blocking on net/smtp's deadline-unaware API forever.
func fakeSMTPServerThatHangs(t *testing.T) (addr, host string) {
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
		fmt.Fprintf(conn, "220 fake.smtp ESMTP ready\r\n")
		<-make(chan struct{}) // block forever until the connection is closed by the deadline
	}()
	return ln.Addr().String(), host
}
