package checks

import (
	"context"
	"net"
	"strconv"
)

// TCPConnect succeeds if a TCP connection to host on params["port"] can be
// established before ctx is done. The connection is closed immediately; no
// data is exchanged.
//
// Params:
//   - port (int, required): TCP port, 1-65535.
func TCPConnect(ctx context.Context, host string, params map[string]any) Result {
	if err := checkHost(host); err != nil {
		return fail("%v", err)
	}
	p, err := parseTCPConnectParams(params)
	if err != nil {
		return fail("%v", err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(p.port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail("connecting to %s: %v", addr, err)
	}
	conn.Close()
	return Result{Success: true, Detail: "connected to " + addr}
}

// tcpConnectParams is TCPConnect's parsed params.
type tcpConnectParams struct {
	port int
}

// parseTCPConnectParams parses and validates TCPConnect's params. It is the
// only place they are checked, for ValidateParams and the check alike.
func parseTCPConnectParams(params map[string]any) (tcpConnectParams, error) {
	if err := rejectUnknownParams(params, "port"); err != nil {
		return tcpConnectParams{}, err
	}
	port, err := paramPort(params)
	if err != nil {
		return tcpConnectParams{}, err
	}
	return tcpConnectParams{port: port}, nil
}
