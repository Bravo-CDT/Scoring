#!/usr/bin/env bash
#
# ca_rpc.sh <host> -- score a Windows CA by asking its RPC endpoint mapper
# whether the ICertPassage certificate-request interface is registered.
#
# Run by the engine's script check as `ca_rpc.sh <host>`; host is the only
# argument. Environment is only PATH and HOME plus the configured CHECK_*
# vars. Exit 0 = up, non-zero = down; the last line printed becomes the
# public scoreboard Detail, so it must never leak anything sensitive.
#
# Env: CHECK_PORT (optional, default 135 -- the endpoint mapper port).
set -u

host="${1:-}"
port="${CHECK_PORT:-135}"

if [ -z "$host" ]; then
	echo "no host given"
	exit 1
fi

# -Protocol:/UUID lines are what rpcdump prints per registered endpoint;
# capture both streams so a failure message can't reach the Detail line,
# and so success/failure is decided from the output, not just the status.
out=$(impacket-rpcdump "$host" -port "$port" 2>&1)
status=$?

if [ "$status" -ne 0 ] || ! printf '%s\n' "$out" | grep -qiE 'Protocol:|UUID'; then
	echo "could not query the endpoint mapper on $host:$port"
	exit 1
fi

# ICertPassage (the CA's certificate-request RPC interface) UUID.
if printf '%s\n' "$out" | grep -qi '91ae6020-9e3c-11cf-8d7c-00aa00c091be'; then
	echo "CA RPC interface (ICertPassage) registered"
	exit 0
fi

echo "endpoint mapper answered but the CA RPC interface (ICertPassage) is not registered"
exit 1
