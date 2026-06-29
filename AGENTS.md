# AGENTS.md — Revquic

Guidance for AI agents (and human contributors) working in this repository. Read this before making changes. For the user-facing intro see [README.md](README.md); for deep design see [ARCHITECTURE.md](ARCHITECTURE.md); for full operating instructions see [USAGE.md](USAGE.md).

> **Status:** a working **reference / learning implementation** with an end‑to‑end test suite — **not** a security‑audited product. Default secrets are dev placeholders.

---

## What Revquic is

A **reverse‑proxy VPN over QUIC**. An **exit node** on a private network (home/cloud/container) dials *out* to a small public **broker**, which authenticates clients, pairs them to exits by region, and relays **QUIC datagrams** (raw IP packets) between them. Sessions start on the broker‑relayed path and transparently **upgrade to a direct peer‑to‑peer path** via ICE hole‑punching (TURN fallback) when the network allows.

Three roles → four binaries:

| Binary | Role | Runs on |
|---|---|---|
| `revquic-broker` (B) | public rendezvous + relay + admin dashboard | any OS, no root |
| `revquic-exit` (C) | egress node: TUN + NAT + per‑session isolation | **Linux + root** (recommended); Windows 11 + WinNAT (limited); **macOS unsupported** |
| `revquic-client` (A) | the VPN device: TUN + default route / SOCKS5 | Linux/macOS (root) · Windows (Admin + Wintun) |
| `revquic-certgen` | mTLS certificate helper | any OS |

---

## Hard invariants — do not break these

1. **`CGO_ENABLED=0` is mandatory.** The Makefile exports it; CI sets `CGO_ENABLED: '0'`. Pure‑Go only — SQLite is `modernc.org/sqlite`, TUN is `songgao/water`, no C toolchain. On macOS hosts cgo's external linker emits a Mach‑O without `LC_UUID` → `dyld` "Abort trap: 6". **Always build/test via `make` targets; for raw `go` commands prefix `CGO_ENABLED=0`.**
2. **Go 1.25+** (per `go.mod`: `go 1.25.0`). CI uses `go-version-file: go.mod`.
3. **Every `.go` file starts with `// SPDX-License-Identifier: GPL-3.0-or-later`** followed by a package doc comment. The project is GPLv3 (`LICENSE`). All 89 tracked Go files conform — keep new files conforming.
4. **Module path:** `github.com/sourjatilak/revquic`. Internal imports are `github.com/sourjatilak/revquic/internal/<pkg>`.
5. **Never tunnel IP packets over a reliable QUIC stream.** The data plane uses **QUIC DATAGRAM frames (RFC 9221)** — unreliable, like WireGuard's UDP — to avoid TCP‑over‑TCP meltdown. Only control/signalling uses reliable streams. This is load‑bearing; see `spec/reconciliation-and-validation.md`.

---

## Build, test, lint — the commands you must run

Before opening a PR (and before claiming a task is done), all of these must pass:

```bash
gofmt -l .            # MUST print nothing (CI fails on any unformatted file)
go vet ./...          # MUST be clean
make build            # = CGO_ENABLED=0 go build → bin/ (all four binaries)
make test             # = CGO_ENABLED=0 go test ./...
```

Other useful targets (`make help`-style — read the Makefile for the full set):

| Target | What it does |
|---|---|
| `make build` | All four binaries into `bin/` |
| `make broker` / `exit` / `client` / `certgen` | One binary |
| `make darwin` | macOS‑native broker + client + certgen (exit is Linux‑only) |
| `make test` | `go test ./...` |
| `make fmt` | `go fmt ./...` (writer; the CI gate is `gofmt -l .`) |
| `make vet` | `go vet ./...` |
| `make web` | Build the Vue admin SPA (`web/admin/`) and embed it into `internal/adminserver/web/`. Requires node/npm. |
| `make release` | Cross‑compile the platform matrix → `dist/<os>-<arch>/` |
| `make package` | `release` + source tarball + per‑platform archives + `SHA256SUMS` (used by the release workflow) |
| `make smoke` | Loopback smoke (Linux + root); runs `scripts/smoke-test.sh` |
| `make clean` | Remove `bin/` and `dist/` |

CI (`.github/workflows/ci.yml`) runs on every push/PR: `gofmt -l .`, `go vet ./...`, `go build ./...` (+ a `GOOS=linux GOARCH=arm64` cross‑build check), and `go test ./...`. Releases (`release.yml`) trigger on `v*` tags → `make package` → GitHub Release.

### Targeted / race tests

```bash
CGO_ENABLED=0 go test ./internal/userstore/... ./internal/session/... ./internal/directpath/...
CGO_ENABLED=0 go test -race ./internal/userstore/... ./internal/session/...   # concurrency-sensitive pkgs
```

---

## Repository layout

```
cmd/                 revquic-broker · revquic-exit · revquic-client · revquic-certgen (package main each)
internal/            all library packages (see table below)
web/admin/           Vue 3 + Vite admin dashboard; `make web` embeds its build into the broker
docker/              Dockerfiles + Compose matrix (relay/direct/mtls/oidc/nat/isolation)
conf/                ready-to-edit sample config files (shipped in release archives)
spec/                design docs: feasibility, HLD, LLD, phase plans, admin UI, OpenAPI
scripts/             smoke-test.sh
.github/workflows/   ci.yml + release.yml
Makefile · README.md · ARCHITECTURE.md · USAGE.md · TESTING.md
```

### Internal packages

> **Note:** the README/ARCHITECTURE package roll‑call omits two real packages: **`adminapi`** (shared admin/user REST types) and **`socks`** (the per‑app SOCKS5 proxy). They exist and are imported — trust the directory listing over those prose lists when in doubt.

| Package | Responsibility |
|---|---|
| `proto` | Control‑plane messages (length‑prefixed JSON on a QUIC stream) + the datagram codec (8‑byte BE session id + IP packet). All `MsgType` constants + `Control` envelope + `CloseClientShutdown` error code live here. |
| `quicx` | QUIC config (datagrams on) + TLS: self‑signed, **mTLS**, and no‑SNI mTLS for the dynamic direct path. |
| `tunnel` | TUN device wrapper (`songgao/water`) + TUN⇄datagram pumps. Per‑OS backends (`tun.go`, `tun_other.go`, `tun_windows.go`, `wintun_windows.go`). |
| `netcfg` | Per‑OS network setup: Linux `ip`/`iptables`, macOS `ifconfig`/`route`, Windows `netsh`/WinNAT. Addresses, routes, `MASQUERADE`, per‑session isolation. Build‑constrained files (`_linux.go`, `_darwin.go`, `_windows.go`, `_other.go`, `netcfg_wincmds.go`). |
| `ippool` | Per‑session VPN address allocation with reclamation. |
| `ratelimit` | Token‑bucket per‑session bandwidth limiter. |
| `directpath` | NAT‑type → direct/relay decision policy + relay↔direct migration state machine. |
| `lb` | Exit load‑balancing: `least-conn` (default) / `round-robin` / `random`. Pure & dependency‑free. |
| `qos` | QoS tracker: per‑exit load, per‑session stats, speed‑drop detection, event history ring + SQLite/file persistence. |
| `telemetry` | Endpoint QoS reporter; RTT measurement. |
| `ice` | ICE agent seam + `pion/ice` adapter (gather, trickle, dial/accept, selected‑pair). |
| `directlink` | QUIC‑datagram connection over the ICE‑nominated path. |
| `icewire` | Drives ICE negotiation over the broker's signalling relay. |
| `session` | Binds the migration state machine to a swappable datagram path (relay ↔ direct) + rate limit. |
| `oidc` | OIDC ID‑token verification (RS256 + JWKS, issuer/audience/expiry). |
| `pki` | Tiny CA: generate CA + issue ECDSA leaf certs (used by `revquic-certgen`). |
| `pwhash` | PBKDF2‑HMAC‑SHA256 admin password hashing (RFC 7914 KAV). |
| `turncred` | coturn REST credential minting (HMAC‑SHA1, short‑lived). |
| `userstore` / `adminstore` | User & admin records behind a `Store` interface: in‑memory, JSON file, or SQLite. Tokens stored as HMAC‑SHA256(pepper, token); admin passwords as PBKDF2. |
| `adminapi` | Shared admin/user REST API request/response types (`User`, `UserCreate`, `UserUpdate`, `Event`, etc.). Imported by stores + adminserver. |
| `auth` | Constant‑time secret comparison (used for node‑token checks). |
| `events` | In‑process event bus (publish/subscribe) feeding the admin SSE stream. |
| `adminserver` | Admin HTTP API + embedded Vue SPA (served at `/`). The built SPA lives in `internal/adminserver/web/` (generated by `make web`, git‑ignored). |
| `sysstat` | Host CPU/RAM/disk sampler (Linux `/proc` + `statfs`; stub on other platforms). |
| `socks` | Per‑application SOCKS5 proxy bound to the TUN interface (`-socks` on the client). Per‑OS socket bind (`bind_linux.go`/`_darwin.go`/`_windows.go`/`_other.go`). |
| `conf` | `key=value` config‑file loader into a `flag.FlagSet`; CLI flags always win. |
| `logx` | Log setup: text/json format, optional file output. |
| `shutdown` | Double‑press Ctrl‑C/Ctrl‑Z interactive exit; SIGTERM immediate. Per‑OS (`shutdown_unix.go`, `shutdown_windows.go`). |

---

## Code conventions

- **License header + package doc.** Every `.go` file:
  ```go
  // SPDX-License-Identifier: GPL-3.0-or-later

  // Package <name> <one-paragraph description of what it does and why>.
  //
  // <longer notes: backends, key constraints, references to spec docs>
  package <name>
  ```
  Look at `internal/lb/lb.go`, `internal/userstore/userstore.go`, `internal/proto/proto.go` for the house style. The package doc explains *why*, not just *what*.
- **Standard library `testing`.** Tests are colocated (`<pkg>/<pkg>_test.go`, same package) and use the `testing` package directly — `t.Fatalf`/`t.Errorf`, table‑driven where natural. `testify` is present only as an indirect dependency of `quic-go`/`pion`; **do not introduce it** in new tests.
- **Build‑constraint files** for per‑OS behavior use the `_linux.go` / `_darwin.go` / `_windows.go` / `_other.go` suffix convention (see `internal/netcfg`, `internal/sysstat`, `internal/shutdown`, `internal/socks`, `internal/tunnel`). `netcfg_wincmds.go` holds unit‑testable Windows command construction separately from the build‑constrained caller.
- **Concurrency:** shared state is guarded with `sync.RWMutex`; hot lock‑free counters use `atomic` (e.g. `exitNode.active`, `cpuBits` storing `math.Float64bits`). When adding shared state, follow the existing pattern.
- **Errors:** packages define sentinel errors (`ErrNotFound`, `ErrConflict`, …) returned to callers. Don't wrap internal sentinels away.
- **Flags:** all configuration is via `flag.String/Bool/Int/Float64/Duration`. Every binary accepts `-config <path>` (loaded by `internal/conf`), `-log-file`, `-log-type`. CLI always overrides config file. When adding a flag, it automatically becomes a config‑file key.
- **Comments:** narrate *why*, not *what*. Don't restate code. Reference spec docs where a design decision is non‑obvious (e.g. "see spec/reconciliation-and-validation.md §3").
- **No external deps without justification.** The dependency set is small and intentional (`quic-go`, `pion/ice`, `pion/stun`, `songgao/water`, `modernc.org/sqlite`, `golang.org/x/sys`). Prefer the standard library.

---

## The two data paths (essential context)

| Path | Route | When |
|---|---|---|
| **Relay** | `A → B → C` | Always works; the bootstrap and the fallback. |
| **Direct (P2P)** | `A ⇄ C` | Lower latency. ICE hole‑punch (`host`/`srflx`); TURN fallback. Controlled by `-direct` + `-direct-mode any\|p2p-only`. |

- IP packets ride **QUIC DATAGRAM** frames, prefixed with an **8‑byte big‑endian session id** so the broker demuxes many sessions over one exit connection.
- Control messages are length‑prefixed JSON on a single bidirectional QUIC stream (`proto.ReadControl`/`proto.WriteControl`).
- The relay↔direct migration is a live state machine (`internal/directpath`, `internal/session`) — traffic moves without dropping the session.
- `-direct-mode p2p-only` stays on the broker relay if the only direct option is TURN‑relayed (avoids trading a fast relay for a slow one under symmetric NAT/CGNAT).

---

## Testing strategy (three layers)

1. **Unit/integration (host, no root):** `make test`. Pure‑Go, fast. Covers pwhash KAVs, userstore region policy + persistence, adminstore, events bus, directpath decision matrix + state machine, loopback ICE connectivity, QUIC‑over‑ICE round‑trip, ICE signaling negotiation, session path routing, PKI, and QUIC mTLS handshakes.
2. **Live control‑plane smoke (host, no root):** start the broker and curl the admin API (login → token, user CRUD, RBAC, SSE, dashboard). See [TESTING.md §2](TESTING.md).
3. **Container data‑plane smokes (Linux via Docker/colima):** the real TUN + QUIC + NAT tests. Relay, direct‑P2P upgrade, mTLS, OIDC (Dex), multi‑NAT TURN fallback, and tenant isolation. See `docker/README.md` and [TESTING.md §3](TESTING.md). Run from `docker/`:
   ```bash
   docker compose up --build --abort-on-container-exit --exit-code-from client   # relay
   docker compose -f docker-compose.yml -f docker-compose.direct.yml up --build --abort-on-container-exit --exit-code-from client  # direct
   ```
   **Docker on macOS:** Docker Desktop may be org‑gated; use colima (`colima start --cpu 4 --memory 6 --disk 30`).

When you add a feature, add/extend unit tests in layer 1. Data‑plane changes should get a container smoke variant if practical.

---

## Things to watch out for

- **`-uplink` must be the real internet‑facing interface** on the exit (find with `ip route get 1.1.1.1`). A wrong `-uplink` looks healthy (`ping 10.99.0.1` works) but silently drops client→internet traffic.
- **UDP buffer sizing:** QUIC wants large socket buffers. On Linux raise `net.core.rmem_max`/`wmem_max` to ~7 MB (`7340032`) on broker + exit + client hosts. This tunes the relay path; the ICE path's buffers can't be tuned.
- **`-full` (replace default route) and `-socks` (per‑app proxy) are mutually exclusive.** The client refuses to start if both are given. `-full` requires `-broker-ip`/`-gw`/`-gw-dev` to keep a host route to the broker.
- **`-cred-pepper` must stay stable** — changing it invalidates every stored client token. Admin passwords use a separate PBKDF2 hash.
- **`make web` is required** if you change the Vue dashboard — the broker serves the *embedded* build from `internal/adminserver/web/`, not `web/admin/dist/` directly. That directory is git‑ignored.
- **macOS exit is unsupported** (no `iptables`‑equivalent for NAT + isolation). macOS works as broker and *client* (`utun`).
- **Session resumption:** disconnected clients' sessions are "parked" for `-session-resume-ttl` (default 1h) so a quick reconnect resumes the same exit + VPN IP. A *deliberate* client exit (Ctrl‑C) signals `proto.CloseClientShutdown` and tears down immediately instead of parking.

---

## References inside the repo

- [README.md](README.md) — overview, quickstart, platform matrix, repo layout.
- [ARCHITECTURE.md](ARCHITECTURE.md) — network layers, component/package breakdown, connection sequence diagrams, load balancing, QoS/telemetry, security model.
- [USAGE.md](USAGE.md) — every mode, full CLI reference, per‑platform setup, Docker & bare‑metal examples, throughput tuning, known limitations.
- [TESTING.md](TESTING.md) — the three‑layer test matrix.
- [docker/README.md](docker/README.md) — Compose matrix details.
- [`spec/`](spec/) — feasibility study, HLD, LLD, phase plans; the authority on *why* design decisions were made.
