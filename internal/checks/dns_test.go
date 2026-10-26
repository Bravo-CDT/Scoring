package checks

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNSServer answers A queries over UDP on 127.0.0.1: names in answers
// (written without the trailing dot) get A records for their IPs, and any
// other name gets NXDOMAIN. It serves until the test ends.
func fakeDNSServer(t *testing.T, answers map[string][]string) (host string, port int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		pc.Close()
		<-done
	})

	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			ips, known := answers[strings.TrimSuffix(q.Name.String(), ".")]
			rcode := dnsmessage.RCodeSuccess
			if !known {
				rcode = dnsmessage.RCodeNameError
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, RCode: rcode})
			b.StartQuestions()
			b.Question(q)
			b.StartAnswers()
			for _, ip := range ips {
				a := netip.MustParseAddr(ip).As4()
				b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: a})
			}
			msg, err := b.Finish()
			if err != nil {
				continue
			}
			pc.WriteTo(msg, addr)
		}
	}()

	return "127.0.0.1", pc.LocalAddr().(*net.UDPAddr).Port
}

func dnsCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestDNS_ResolvesName(t *testing.T) {
	host, port := fakeDNSServer(t, map[string][]string{"krusty.krab": {"10.111.2.1"}})
	res := DNS(dnsCtx(t), host, map[string]any{"port": port, "name": "krusty.krab"})
	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	if !strings.Contains(res.Detail, "10.111.2.1") {
		t.Errorf("Detail %q should contain the resolved IP", res.Detail)
	}
}

func TestDNS_ExpectIPMismatch(t *testing.T) {
	host, port := fakeDNSServer(t, map[string][]string{"krusty.krab": {"10.111.2.1"}})
	res := DNS(dnsCtx(t), host, map[string]any{"port": port, "name": "krusty.krab", "expect_ip": "10.0.0.9"})
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if !strings.Contains(res.Detail, "10.111.2.1") || !strings.Contains(res.Detail, "10.0.0.9") {
		t.Errorf("Detail %q should name both the got and wanted IPs", res.Detail)
	}
}

func TestDNS_NXDOMAIN(t *testing.T) {
	host, port := fakeDNSServer(t, map[string][]string{"krusty.krab": {"10.111.2.1"}})
	res := DNS(dnsCtx(t), host, map[string]any{"port": port, "name": "chum.bucket"})
	if res.Success {
		t.Fatalf("expected failure, got %+v", res)
	}
	if !strings.Contains(res.Detail, "no such name") {
		t.Errorf("Detail %q should say no such name", res.Detail)
	}
}

func TestDNS_Params(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"missing name", map[string]any{"port": 53}, `"name"`},
		{"empty name", map[string]any{"port": 53, "name": ""}, `"name"`},
		{"label over 63 bytes", map[string]any{"port": 53, "name": strings.Repeat("k", 64) + ".krab"}, `"name"`},
		{"empty label", map[string]any{"port": 53, "name": "krusty..krab"}, `"name"`},
		{"bad expect_ip", map[string]any{"port": 53, "name": "krusty.krab", "expect_ip": "nope"}, `"expect_ip"`},
		{"unknown param", map[string]any{"port": 53, "name": "krusty.krab", "bogus": 1}, `"bogus"`},
		{"missing port", map[string]any{"name": "krusty.krab"}, `"port"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateParams("dns", tt.params)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should mention %s", err, tt.want)
			}
		})
	}
	if err := ValidateParams("dns", map[string]any{"port": 53, "name": "krusty.krab", "expect_ip": "10.111.2.1"}); err != nil {
		t.Errorf("valid params rejected: %v", err)
	}
}
