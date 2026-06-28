#!/bin/sh
# Minimal NAT router for the multi-NAT testbed. MASQUERADEs traffic leaving its LAN subnet with
# --random-fully (randomized source ports) to approximate a SYMMETRIC NAT, which defeats UDP hole
# punching and forces ICE to fall back to the TURN relay. Requires NET_ADMIN (or privileged).
set -e
: "${LAN_SUBNET:?set LAN_SUBNET, e.g. 10.81.0.0/24}"

sysctl -w net.ipv4.ip_forward=1 2>/dev/null || true   # may be preset via compose sysctls
iptables -t nat -A POSTROUTING -s "$LAN_SUBNET" ! -d "$LAN_SUBNET" -j MASQUERADE --random-fully
iptables -A FORWARD -j ACCEPT

echo "[nat] routing $LAN_SUBNET -> public with --random-fully (symmetric-NAT-like)"
ip -brief addr show
exec tail -f /dev/null
