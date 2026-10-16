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
	port, err := paramPort(params)
	if err != nil {
		return fail("%v", err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail("connecting to %s: %v", addr, err)
	}
	conn.Close()
	return Result{Success: true, Detail: "connected to " + addr}
}
