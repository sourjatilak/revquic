# Revquic — Testing Guide (Phase 1 & Phase 2)

How to verify the spike. Three layers: **unit/integration tests** (Go), a **live control-plane smoke**
(broker admin API, no root), and **container data-plane smokes** (relay and relay→direct, on Linux via
Docker/colima).

## 0. Prerequisites

```bash
cd revquic
go env -w GOPROXY=direct      # if proxy.golang.org is unreachable; fetches deps from source
go mod tidy
```

- **`CGO_ENABLED=0` is required** for build/test on macOS (the cgo `net` resolver triggers an external
  linker that emits a Mach-O without `LC_UUID` → `dyld` "Abort trap: 6"). The `Makefile` exports it, so
  prefer `make` targets; for raw `go test` prefix `CGO_ENABLED=0`.
- Container smokes need a Docker daemon. Docker Desktop here is org-sign-in gated; use **colima**:
  `colima start --cpu 4 --memory 6 --disk 30` then `docker context use colima`.

## 1. Unit & integration tests (host, no root)

```bash
make test                    # = CGO_ENABLED=0 go test ./...
# or targeted:
CGO_ENABLED=0 go test ./... 
CGO_ENABLED=0 go test -race ./internal/userstore/... ./internal/session/... ./internal/directpath/...
```

What each test covers:

| Package | Phase | Verifies |
|---|---|---|
| `internal/pwhash` | 1 | PBKDF2-HMAC-SHA256 **RFC 7914 known-answer vector**, hash/verify round-trip, unique salts, malformed-input rejection |
| `internal/userstore` | 1 | region allow/deny + wildcard, disabled user, username conflict, delete, credential rotation, **file persistence (no plaintext on disk, survives reopen, wrong-pepper rejected)** |
| `internal/adminstore` | 1 | admin create/verify, conflict, **no plaintext password on disk**, persistence round-trip |
| `internal/events` | 1 | event bus publish/subscribe + unsubscribe |
| `internal/directpath` | 2 | NAT→direct/relay decision matrix, punchability symmetry, migration state machine (valid + invalid transitions) |
| `internal/ice` | 2 | **loopback ICE connectivity** (two pion agents connect over host candidates, transfer bytes) |
| `internal/directlink` | 2 | **QUIC-datagram-over-ICE** round-trip both directions on loopback |
| `internal/icewire` | 2 | full **ICE signaling negotiation** (creds + trickle candidates over channels) → direct QUIC link |
| `internal/session` | 2 | relay→direct→fallback path routing, invalid upgrade rejected, `ErrNoPath` |
| `internal/pki` | mTLS | CA generation + leaf issuance (server/client EKU), chain verification, wrong-CA rejection |
| `internal/quicx` | mTLS | raw TLS mutual handshake; **real QUIC mTLS handshake** (valid client cert accepted + peer cert seen; no-cert client rejected) |

Benchmarks: `CGO_ENABLED=0 go test -bench=. ./internal/userstore/...` (auth hot path ≈ 0.4 µs/op).

## 2. Live control-plane smoke (Phase 1, host, no root)

Exercises the broker admin API (PBKDF2 login → session token, user CRUD, RBAC, SSE feed, dashboard).

```bash
make build
./bin/revquic-broker -quic :4242 -http :18080 \
  -admin-user admin -admin-pass admin -node-token node-secret -seed-user alice-token:us-west &

# login -> session token
TOK=$(curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' http://localhost:18080/api/v1/admin/login \
  | sed -n 's/.*"token":"\([a-f0-9]*\)".*/\1/p')

curl -s -o /dev/null -w 'bad pass -> %{http_code}\n' -X POST -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"nope"}' http://localhost:18080/api/v1/admin/login   # 401
curl -fsS -H "Authorization: Bearer $TOK" http://localhost:18080/api/v1/users              # seeded user
curl -s -o /dev/null -w 'no token -> %{http_code}\n' http://localhost:18080/api/v1/users   # 401
curl -fsS -X POST -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"username":"bob","credential":"bob-tok","allowedRegions":["eu-central"]}' \
  http://localhost:18080/api/v1/users                                                      # 201
curl -N -H "Authorization: Bearer $TOK" --max-time 2 http://localhost:18080/api/v1/events  # Snapshot
curl -s -o /dev/null -w 'dashboard -> %{http_code}\n' http://localhost:18080/              # 200
```

> On macOS, run the binary built with `CGO_ENABLED=0` (the Makefile does this). If it aborts with
> `missing LC_UUID`, rebuild via `make build`.

Expected: login returns a token; bad/no token → 401; users list shows the seeded user; create → 201; SSE
emits a `Snapshot`; dashboard → 200.

## 3. Container data-plane smokes (Phases 0–2, Linux via Docker/colima)

These are the real data-plane tests (TUN + QUIC datagrams + NAT). They need a Linux engine.

```bash
colima start --cpu 4 --memory 6 --disk 30      # if not already running
docker context use colima
cd revquic/docker
```

### 3a. Relay path (A → B → C)
```bash
docker compose up --build --abort-on-container-exit --exit-code-from client
```
Expected: `SMOKE PASS: IP packets round-trip A -> B -> C`; compose exits `0`. (The client pings the exit's
tunnel gateway `10.99.0.1` through the relay.)

### 3b. Direct path (relay → direct upgrade, Phase 2)
```bash
docker compose -f docker-compose.yml -f docker-compose.direct.yml up --build \
    --abort-on-container-exit --exit-code-from client
```
Expected: exit logs `session N: upgraded to DIRECT path`; client prints `SMOKE PASS` **and**
`DIRECT PASS: session migrated to the direct ICE/QUIC path`; compose exits `0`. On one Compose network ICE
uses **host candidates** (no STUN needed). Tear down with `docker compose ... down`.

### 3c. Control-plane mTLS (Phase 1 hardening)
```bash
docker compose -f docker-compose.yml -f docker-compose.mtls.yml up --build \
    --abort-on-container-exit --exit-code-from client
```
A one-shot `certgen` service writes a CA + leaf certs into a named volume; all services present
CA-signed certs and the broker requires + verifies client certs. Expected: server `control-plane mTLS
enabled`, exit `control-plane mTLS to broker enabled`, client `(mtls=1)` → `SMOKE PASS`. (Cert files for a
host run: `CGO_ENABLED=0 go run ./cmd/revquic-certgen -out certs -broker-san server,localhost,broker`.)

### 3d. STUN/TURN via coturn (cross-NAT template — not a flat-network gate)
```bash
docker compose -f docker-compose.yml -f docker-compose.coturn.yml up --build \
    --abort-on-container-exit --exit-code-from client
```
Demonstrates the `-stun`/`-turn` wiring against a coturn server (ICE/QUIC negotiates, coturn shows TURN
allocations). On a flat Compose network the TURN-relayed data path is **not** expected to carry traffic
(no NAT, and coturn needs a real `--relay-ip`); use 3b for a flat-network direct pass. This file is the
template for a real cross-NAT deployment.

### 3f. Per-session (tenant) isolation (verified)
```bash
docker compose -f docker-compose.yml -f docker-compose.isolation.yml up --build -d
docker compose -f docker-compose.yml -f docker-compose.isolation.yml logs --tail=30 client2
docker compose -f docker-compose.yml -f docker-compose.isolation.yml down -v
```
Two clients on one exit. The exit (`-isolate`, default on) does **anti-spoofing** (drops inbound packets
whose source ≠ the session's assigned IP) and blocks **client-to-client** + **client-to-LAN** forwarding.
`client2` confirms it reaches the gateway (`SMOKE PASS`) but **cannot** reach the peer client
(`ISOLATION PASS`). Discriminating: re-running with an exit `-isolate=false` override makes the peer
reachable (`ISOLATION FAIL`). Run detached + poll.
```bash
docker compose -f docker-compose.nat.yml up --build -d
docker compose -f docker-compose.nat.yml logs --tail=40 client coturn exit
docker compose -f docker-compose.nat.yml down -v
```
Client and exit are on separate isolated LANs, each behind a NAT router doing `MASQUERADE --random-fully`
(symmetric-NAT / CGNAT-like — randomized source ports defeat hole punching), so ICE falls back to the
coturn TURN relay (which uses broker-minted **TURN REST credentials**). Run **detached** and poll.
Expected: client `upgraded to DIRECT` + `SMOKE PASS` + `DIRECT PASS` (ping 3/3); coturn logs **two TURN
allocations**; and with `RELAY_EXPECT=1` the client asserts the selected ICE pair is a relay candidate
(`RELAY PASS`, logged as `ICE pair: local=relay remote=relay`). The relay carrying traffic across the
mutually-unreachable LANs is the symmetric-NAT/CGNAT validation.

### What the container smokes prove
Broker auth + region selection, exit-node TUN + `iptables` masquerade, client TUN, the QUIC-datagram
relay, the broker `MsgSignal` signaling relay, ICE negotiation, QUIC-over-ICE, and the live relay→direct
session migration — all on real Linux.

## 4. Static checks
```bash
gofmt -l .            # empty = formatted
go vet ./...
GOOS=linux CGO_ENABLED=0 go build ./...   # confirm Linux build (container target)
```

## 5. Last verified status (this repo)
- `go build ./...`, `go vet ./...`, `GOOS=linux` build: pass.
- `CGO_ENABLED=0 go test ./...`: all packages pass (incl. `-race` on the concurrency-sensitive ones).
- Live control-plane smoke: pass. Container relay smoke: pass. Container direct smoke: pass (colima).
