#!/bin/sh
# Relay-path smoke run inside the client container: connect to the broker, then verify an IP packet
# round-trips A -> B -> C by pinging the exit's tunnel gateway (10.99.0.1) over the tunnel.
set -e

BROKER="${BROKER:-server:4242}"
ADMIN="${ADMIN:-server:8080}"
REGION="${REGION:-us-west}"
TOKEN="${TOKEN:-alice-token}"
ADMIN_TOKEN="${ADMIN_TOKEN:-admin-secret}"

if [[ $EUID -ne 0 ]]; then echo "must run as root (TUN + iptables)"; exit 1; fi 2>/dev/null || true

# Multi-NAT testbed: route to the public network via the NAT gateway before connecting.
if [ -n "${GW:-}" ]; then
  echo "[client] default route via NAT gateway ${GW}"
  ip route replace default via "${GW}"
fi

# Optional start delay so multiple clients get deterministic VPN IP assignment.
if [ -n "${START_DELAY:-}" ]; then
  echo "[client] start delay ${START_DELAY}s"
  sleep "${START_DELAY}"
fi

# OIDC mode: obtain an ID token from Dex (password grant) and use it as the connect token.
if [ "${OIDC:-0}" = "1" ]; then
  echo "[client] obtaining ID token from Dex (${DEX_TOKEN_URL})"
  for i in $(seq 1 15); do
    RESP=$(curl -fsS "${DEX_TOKEN_URL}" \
      -d grant_type=password -d scope="openid email" \
      -d client_id="${DEX_CLIENT_ID}" -d client_secret="${DEX_CLIENT_SECRET}" \
      -d username="${DEX_USER}" -d password="${DEX_PASS}" 2>/dev/null) && break
    sleep 1
  done
  IDT=$(printf '%s' "$RESP" | sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p')
  if [ -z "$IDT" ]; then echo "OIDC FAIL: no id_token from Dex; resp=$RESP"; exit 1; fi
  echo "[client] got ID token (${#IDT} chars)"
  TOKEN="$IDT"
fi

echo "[client] admin API: list nodes"
for i in $(seq 1 10); do
  if curl -fsS -H "Authorization: Bearer ${ADMIN_TOKEN}" "http://${ADMIN}/api/v1/nodes" 2>/dev/null | grep -q exit-1; then
    echo "[client] exit-1 registered"; break
  fi
  sleep 1
done

echo "[client] starting revquic-client -> ${BROKER} region=${REGION} (direct=${DIRECT:-0}, mtls=$([ -n "${TLS_CA:-}" ] && echo 1 || echo 0))"
DIRECT_ARGS=""
[ "${DIRECT:-0}" = "1" ] && DIRECT_ARGS="-direct"
TLS_ARGS=""
[ -n "${TLS_CA:-}" ] && TLS_ARGS="-tls-ca ${TLS_CA} -tls-cert ${TLS_CERT} -tls-key ${TLS_KEY} -tls-server-name ${TLS_SERVER_NAME:-server}"
STUN_ARGS=""
[ -n "${STUN:-}" ] && STUN_ARGS="-stun ${STUN} -turn ${TURN} -turn-user ${TURN_USER} -turn-pass ${TURN_PASS}"
revquic-client -broker "${BROKER}" -region "${REGION}" -token "${TOKEN}" ${DIRECT_ARGS} ${TLS_ARGS} ${STUN_ARGS} >/tmp/client.log 2>&1 &
CPID=$!
sleep 4

echo "[client] tun interfaces:"; ip -brief addr show | grep -i tun || true

echo "[client] ping exit gateway 10.99.0.1 over the tunnel"
if ping -c 3 -W 2 10.99.0.1; then
  echo "SMOKE PASS: IP packets round-trip A -> B -> C"
  RC=0
else
  echo "SMOKE FAIL: no round-trip"
  RC=1
fi

if [ "${DIRECT_EXPECT:-0}" = "1" ]; then
  if grep -q "upgraded to DIRECT" /tmp/client.log; then
    echo "DIRECT PASS: session migrated to the direct ICE/QUIC path"
  else
    echo "DIRECT FAIL: no direct upgrade observed"; echo "--- client log ---"; cat /tmp/client.log; RC=1
  fi
fi

# TURN relay assertion (symmetric-NAT/CGNAT): the selected ICE pair must be a relay candidate.
if [ "${RELAY_EXPECT:-0}" = "1" ]; then
  if grep -qE "ICE pair:.*relay" /tmp/client.log; then
    echo "RELAY PASS: ICE fell back to a TURN relay candidate"
    grep "ICE pair:" /tmp/client.log | tail -1
  else
    echo "RELAY FAIL: selected ICE pair is not a relay candidate"
    grep "ICE pair:" /tmp/client.log | tail -1; RC=1
  fi
fi

# Tenant isolation: a peer client's VPN IP must NOT be reachable.
if [ -n "${PEER_IP:-}" ]; then
  echo "[client] verifying peer ${PEER_IP} is NOT reachable (tenant isolation)"
  if ping -c 2 -W 2 "${PEER_IP}" >/dev/null 2>&1; then
    echo "ISOLATION FAIL: peer ${PEER_IP} is reachable"; RC=1
  else
    echo "ISOLATION PASS: peer ${PEER_IP} unreachable from another tenant"
  fi
fi

# Optionally hold the connection open (so a peer can run its checks against this client).
if [ "${HOLD:-0}" = "1" ]; then
  echo "[client] holding connection ${HOLD_SECS:-30}s"
  sleep "${HOLD_SECS:-30}"
fi

kill "$CPID" 2>/dev/null || true
exit "$RC"
