package checks

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNS sends one A query for params["name"] to the DNS server at host on
// params["port"] (the scored DNS server is the host itself) and succeeds if
// the reply carries at least one A record and, when params["expect_ip"] is
// set, that IP is among them.
//
// Params:
//   - port (int, required): UDP port, 1-65535.
//   - name (string, required): the name to look up, e.g. krusty.krab.
//   - expect_ip (string, optional): an IPv4 address the answer must include.
//
// The query is built and sent by hand rather than through net.Resolver,
// which consults /etc/hosts and appends search domains: either could make the
// check pass without the scored server ever answering.
func DNS(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseDNSParams(params)
	if err != nil {
		return fail("%v", err)
	}

	// The query name is made absolute so the server never sees it as
	// relative to some search domain.
	qname, err := dnsmessage.NewName(strings.TrimSuffix(p.name, ".") + ".")
	if err != nil {
		return fail("building query for %s: %v", p.name, err)
	}
	id := uint16(rand.Uint32())
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: qname, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	query, err := b.Finish()
	if err != nil {
		return fail("building query for %s: %v", p.name, err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(p.port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return fail("connecting to %s: %v", addr, err)
	}
	defer conn.Close()
	// Reads on a UDP conn ignore ctx, so its deadline is set from ctx, and
	// pulled in to now should ctx be cancelled before it.
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	if _, err := conn.Write(query); err != nil {
		return fail("querying %s: %v", addr, err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return fail("no reply from %s: %v", addr, err)
	}

	var parser dnsmessage.Parser
	hdr, err := parser.Start(buf[:n])
	if err != nil {
		return fail("malformed reply from %s: %v", addr, err)
	}
	if !hdr.Response || hdr.ID != id {
		return fail("reply from %s does not answer our query", addr)
	}
	switch hdr.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return fail("%s: no such name (NXDOMAIN) at %s", p.name, addr)
	default:
		return fail("%s: server %s returned %v", p.name, addr, hdr.RCode)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return fail("malformed reply from %s: %v", addr, err)
	}

	var ips []net.IP
	for {
		rh, err := parser.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return fail("malformed reply from %s: %v", addr, err)
		}
		if rh.Type != dnsmessage.TypeA {
			if err := parser.SkipAnswer(); err != nil {
				return fail("malformed reply from %s: %v", addr, err)
			}
			continue
		}
		a, err := parser.AResource()
		if err != nil {
			return fail("malformed reply from %s: %v", addr, err)
		}
		ips = append(ips, net.IP(a.A[:]))
	}
	if len(ips) == 0 {
		return fail("no A records for %s", p.name)
	}

	got := make([]string, len(ips))
	for i, ip := range ips {
		got[i] = ip.String()
	}
	resolved := fmt.Sprintf("%s resolved to %s", p.name, strings.Join(got, ", "))
	if p.expectIP != nil {
		found := false
		for _, ip := range ips {
			if ip.Equal(p.expectIP) {
				found = true
				break
			}
		}
		if !found {
			return fail("%s, want %s", resolved, p.expectIP)
		}
	}
	return Result{Success: true, Detail: resolved}
}

// dnsParams is DNS's parsed params.
type dnsParams struct {
	port     int
	name     string
	expectIP net.IP // nil when not set
}

// parseDNSParams parses and validates DNS's params. It is the only place
// they are checked, for ValidateParams and the check alike.
func parseDNSParams(params map[string]any) (dnsParams, error) {
	if err := rejectUnknownParams(params, "port", "name", "expect_ip"); err != nil {
		return dnsParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return dnsParams{}, err
	}
	name, ok, err := stringParam(params, "name")
	if err != nil {
		return dnsParams{}, err
	}
	if !ok || name == "" {
		return dnsParams{}, errMissingRequired("name")
	}
	// dnsmessage only checks the total length; a bad label would build an
	// empty query the server answers with a misleading FORMERR.
	if len(name) > 253 {
		return dnsParams{}, fmt.Errorf("param %q is longer than 253 bytes", "name")
	}
	for label := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		if label == "" || len(label) > 63 {
			return dnsParams{}, fmt.Errorf("param %q must be dot-separated labels of 1 to 63 bytes, got %q", "name", name)
		}
	}
	var expectIP net.IP
	if s, ok, err := stringParam(params, "expect_ip"); err != nil {
		return dnsParams{}, err
	} else if ok {
		// The check only asks for A records, so an IPv6 expect_ip could
		// never match: reject it rather than run a check that cannot pass.
		expectIP = net.ParseIP(s).To4()
		if expectIP == nil {
			return dnsParams{}, fmt.Errorf("param %q must be an IPv4 address, got %q", "expect_ip", s)
		}
	}
	return dnsParams{port: port, name: name, expectIP: expectIP}, nil
}
