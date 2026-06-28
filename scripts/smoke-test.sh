#!/usr/bin/env bash
# Revquic Phase 0/1 loopback smoke test (Linux + root).
#
# Brings up broker + exit + client on one host and verifies:
#   1. the exit registers and appears in the admin API,
#   2. an IP packet round-trips A -> B -> C -> TUN (ping the exit gateway).
#
# This is a control-path + relay-path smoke test. Full internet egress (-full on the
# client) is intentionally NOT enabled here to avoid changing the host's default route.
set -euo pipefail

BIN=${BIN:-bin}
REGION=${REGION:-us-west}
UPLINK=${UPLINK:-$(ip route show default 2>/dev/null | awk '{print $5; exit}')}
ADMIN_TOKEN=${ADMIN_TOKEN:-admin-secret}
NODE_TOKEN=${NODE_TOKEN:-node-secret}
CLIENT_TOKEN=${CLIENT_TOKEN:-alice-token}

if [[ $EUID -ne 0 ]]; then echo "must run as root (TUN + iptables)"; exit 1; fi
for b in revquic-broker revquic-exit revquic-client; do
  [[ -x "$BIN/$b" ]] || { echo "missing $BIN/$b — run 'make build'"; exit 1; }
done

# raise UDP buffers for QUIC (best effort)
sysctl -w net.core.rmem_max=2500000 net.core.wmem_max=2500000 >/dev/null 2>&1 || true

pids=()
cleanup() {
  echo "--- cleanup ---"
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  # remove the masquerade rule we added (best effort)
  iptables -t nat -D POSTROUTING -s 10.99.0.0/24 -o "$UPLINK" -j MASQUERADE 2>/dev/null || true
}
trap cleanup EXIT

echo "--- start broker ---"
"$BIN/revquic-broker" -quic :4242 -http :8080 -admin-token "$ADMIN_TOKEN" -node-token "$NODE_TOKEN" \
  -seed-user "$CLIENT_TOKEN:$REGION" &
pids+=($!); sleep 1

echo "--- start exit (region=$REGION uplink=$UPLINK) ---"
"$BIN/revquic-exit" -broker localhost:4242 -nodeId exit-1 -region "$REGION" -uplink "$UPLINK" -token "$NODE_TOKEN" &
pids+=($!); sleep 1

echo "--- admin API: list nodes ---"
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/api/v1/nodes | tee /tmp/revquic-nodes.json
grep -q exit-1 /tmp/revquic-nodes.json || { echo "FAIL: exit not registered"; exit 1; }

echo "--- start client (region=$REGION) ---"
"$BIN/revquic-client" -broker localhost:4242 -region "$REGION" -token "$CLIENT_TOKEN" &
pids+=($!); sleep 2

echo "--- relay round-trip: ping exit gateway 10.99.0.1 over the tunnel ---"
if ping -c 3 -W 2 10.99.0.1; then
  echo "PASS: IP packets round-trip A -> B -> C"
else
  echo "FAIL: no round-trip"; exit 1
fi

echo "--- admin API: active sessions ---"
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/api/v1/sessions

echo "ALL CHECKS PASSED"
