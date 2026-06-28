# Revquic — Containerized Relay Smoke (Docker)

Runs the Phase 0/1 **relay** data path (`A → B → C`) across three Alpine containers and verifies an IP
packet round-trips by pinging the exit's tunnel gateway over the tunnel. This is the Linux environment the
data plane needs (TUN + iptables), which macOS can't provide directly.

## Layout
```
docker/
  server/Dockerfile   # revquic-broker  (Alpine, multi-stage, CGO_ENABLED=0)
  exit/Dockerfile     # revquic-exit    (+ iproute2, iptables)
  client/Dockerfile   # revquic-client  (+ iputils, curl) -> runs smoke.sh
  client/smoke.sh     # the in-container relay test (pass/fail exit code)
  docker-compose.yml  # wires server + exit + client on one network
```
All three are multi-stage builds: `golang:1.22-alpine` (with `GOPROXY=direct`, `CGO_ENABLED=0`) compiles
the binary, then it's copied into `alpine:3.20`. Build context is the **revquic module root** (`..`).

## Run
```bash
cd revquic/docker
docker compose up --build --abort-on-container-exit --exit-code-from client
```
- `server` (broker): QUIC `:4242`, admin HTTP `:8080`, seeds user `alice-token` for `us-west`.
- `exit`: dials the broker, brings up a TUN, masquerades `10.99.0.0/24` out `eth0`
  (`cap_add: NET_ADMIN`, `/dev/net/tun`, `sysctls: net.ipv4.ip_forward=1`).
- `client`: connects, then `smoke.sh` confirms `exit-1` is registered via the admin API and pings
  `10.99.0.1` (the exit's tunnel gateway) through the tunnel. Container exit code = smoke result.

Expected tail: `SMOKE PASS: IP packets round-trip A -> B -> C` and compose exits `0`.

## Direct path (Phase 2)
```bash
cd revquic/docker
docker compose -f docker-compose.yml -f docker-compose.direct.yml up --build \
    --abort-on-container-exit --exit-code-from client
```
This enables `-direct` on the exit and client. On a single Compose network there is no NAT between
containers, so ICE connects via **host candidates** (no STUN/TURN needed); the client bootstraps on the
relay then migrates to the direct ICE/QUIC path. The smoke additionally asserts the
`upgraded to DIRECT` log line. For **cross-NAT** testing, add a `coturn` STUN/TURN service and pass
`-stun stun:coturn:3478 -turn turn:coturn:3478 -turn-user … -turn-pass …` to both the exit and client.

## Control-plane mTLS (verified)
```bash
docker compose -f docker-compose.yml -f docker-compose.mtls.yml up --build \
    --abort-on-container-exit --exit-code-from client
```
A one-shot `certgen` service writes a CA + broker/node/client leaf certs into a **named volume** (`certs`)
— not a host bind mount, so it works inside the colima/Docker VM. The broker runs with
`-tls-ca/-tls-cert/-tls-key` (requires + verifies client certs); the exit and client present their certs
and verify the broker (`-tls-server-name server`). Verified: server logs `control-plane mTLS enabled`,
exit logs `control-plane mTLS to broker enabled`, client `(mtls=1)` pings 3/3 → `SMOKE PASS`. Combine with
`-f docker-compose.direct.yml` to run mTLS control plane + direct data path together.

## STUN/TURN via coturn (config template — cross-NAT only)
```bash
docker compose -f docker-compose.yml -f docker-compose.coturn.yml up --build \
    --abort-on-container-exit --exit-code-from client
```
Adds a coturn server (config via CLI args, no host mount) and points the exit + client at it with
`-direct -stun/-turn`. **Caveat:** on one flat Compose network there is no NAT, so TURN relay is
unnecessary (host candidates already work — use the `docker-compose.direct.yml` smoke for a flat-network
pass). With coturn configured here, ICE/QUIC still negotiates and coturn shows TURN allocations, but the
TURN-**relayed** data path needs a correct `--relay-ip`/`--external-ip` and a real NAT to carry traffic.
Treat this file as the **template for a cross-NAT deployment**: put coturn on B's public IP and mint
short-lived TURN REST credentials instead of the static `revquic:revquicpass` user.

## Per-session (tenant) isolation — verified
```bash
docker compose -f docker-compose.yml -f docker-compose.isolation.yml up --build -d
docker compose -f docker-compose.yml -f docker-compose.isolation.yml logs --tail=30 client2
docker compose -f docker-compose.yml -f docker-compose.isolation.yml down -v
```
Two clients share one exit. The exit enforces isolation by default (`-isolate`): **anti-spoofing** (it
drops inbound packets whose source IP ≠ the session's assigned VPN IP) plus iptables FORWARD drops that
block **client-to-client** and **client-to-LAN/RFC1918** traffic (only public egress is allowed; traffic
to the exit's own tunnel gateway is local INPUT and unaffected). `client` (10.99.0.2) holds its
connection; `client2` (10.99.0.3) confirms the gateway is reachable (`SMOKE PASS`) but the peer is **not**
(`ISOLATION PASS`). Verified discriminating: with `-isolate=false` the peer becomes reachable
(`ISOLATION FAIL`).

## Multi-NAT testbed — TURN relay fallback (verified)
```bash
docker compose -f docker-compose.nat.yml up --build -d
docker compose -f docker-compose.nat.yml logs --tail=40 client coturn exit
docker compose -f docker-compose.nat.yml down -v
```
Puts the client and exit on **separate isolated LANs**, each behind a NAT router that MASQUERADEs with
`--random-fully` (symmetric-NAT-like). Hole punching therefore fails and ICE falls back to the **coturn
TURN relay** on the public network (coturn runs with `--relay-ip/--external-ip=10.80.0.20`). Run it
**detached** and poll `ps`/`logs` — don't block.

**Verified:** client logs `upgraded to DIRECT` and `SMOKE PASS` + `DIRECT PASS` with ping 3/3 0% loss;
coturn logs **two TURN allocations** (client + exit). Because the two LANs are mutually unreachable
except via TURN, the relayed path is what carries the traffic — i.e. the TURN relay fallback is actually
exercised. This is the topology to extend for symmetric-NAT and CGNAT scenarios.

**Symmetric-NAT / CGNAT + explicit relay assertion.** The NAT routers use `MASQUERADE --random-fully`,
which randomizes the source-port mapping per destination — the defining behavior of a **symmetric NAT**
(and the common CGNAT case) that makes STUN-discovered `srflx` candidates useless and defeats hole
punching. The direct path therefore selects a **relay** candidate. `directlink` logs the negotiated pair
(`ICE pair: local=… remote=…`) and the smoke asserts it with `RELAY_EXPECT=1` → `RELAY PASS: ICE fell
back to a TURN relay candidate`. (coturn issues per-session **TURN REST credentials** minted by the
broker — `--use-auth-secret`.)

## Status in this environment
- **Files created and validated**: `docker compose config` passes; the binaries cross-compile for Linux
  (`GOOS=linux CGO_ENABLED=0 go build ./...` OK), so the images will build.
- **Not executed here**: Docker Desktop on this host is gated by an Amazon org policy
  (*"Sign in to continue… Membership in the [amazonians] organization is required"*), so the daemon
  refuses image/build operations until an authorized user signs in. Run the command above after signing
  in to Docker Desktop (or on any Linux host with Docker) to execute the smoke.

## Notes / caveats
- This exercises the **relay** path only (Phase 0/1). The direct/ICE path (Phase 2) needs STUN/TURN +
  signaling wiring (in progress) and a second NAT hop to be meaningful.
- The ping target `10.99.0.1` is the exit's TUN gateway, so the test passes even without real internet
  egress (masquerade only matters for external destinations). For full egress, give the `client` a default
  route via the tunnel (`revquic-client -full …`) and `curl https://api.ipify.org` — omitted here to avoid
  changing container routing in the automated smoke.
- If `/dev/net/tun` is unavailable in your engine, add `privileged: true` to the `exit` and `client`
  services.
