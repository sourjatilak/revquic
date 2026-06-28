<div align="center">

# Revquic

**A reverse‑proxy VPN over QUIC — internet egress from a private network, no public IP, no port‑forwarding.**

[![CI](https://github.com/sourjatilak/revquic/actions/workflows/ci.yml/badge.svg)](https://github.com/sourjatilak/revquic/actions/workflows/ci.yml)
[![Release](https://github.com/sourjatilak/revquic/actions/workflows/release.yml/badge.svg)](https://github.com/sourjatilak/revquic/actions/workflows/release.yml)
[![Go Version](https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License: GPL v3](https://img.shields.io/badge/license-GPLv3-blue.svg)](LICENSE)
[![Platforms](https://img.shields.io/badge/platforms-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey.svg)](#)
[![Pure Go](https://img.shields.io/badge/cgo-free-success.svg)](#)

[Quickstart](#quickstart) · [Architecture](ARCHITECTURE.md) · [Usage Guide](USAGE.md) · [Contributing](#contributing)

</div>

---

Revquic lets an **exit node** on an ordinary private network (home, cloud, or container) provide internet
egress to remote clients **without a public IP or port‑forwarding**. The exit **dials out** to a small
public **broker**, which authenticates and rendezvous‑pairs clients to exits by region, then relays QUIC
datagrams (raw IP packets) between them. Sessions begin on the broker‑relayed path and transparently
**upgrade to a direct peer‑to‑peer path** via ICE hole‑punching (with TURN fallback) when the network
allows — combining the reachability of a relay with the latency and throughput of P2P.

> [!NOTE]
> **Status:** a working **reference / learning implementation** with an end‑to‑end test suite — **not a
> security‑audited product**. Replace the demo secrets and add a security review before exposing it to
> real users.

## Highlights

- 🌐 **No public IP required** — the exit dials out; works behind NAT/CGNAT.
- ⚡ **Relay → direct P2P upgrade** — live session migration via ICE (STUN hole‑punch, TURN fallback), with seamless fall‑back to the relay if a direct path drops.
- 🔒 **Layered security** — mutual TLS (control *and* direct data plane), OIDC or token auth, PBKDF2 admin passwords, HMAC‑hashed client tokens.
- 🛡️ **Per‑session isolation & rate limiting** on the exit (no client‑to‑client / client‑to‑LAN; anti‑spoofing).
- ⚖️ **Load balancing** across exits (least‑connections / round‑robin / random) with capacity limits.
- 📊 **Live QoS & telemetry** — per‑session throughput, RTT, path, host CPU/RAM/disk, speed‑drop detection, persisted event history, and an embedded **Vue admin dashboard**.
- 🖥️ **Exit status page** — each exit serves a no‑auth local dashboard (`localhost:8085`) showing connected clients, path mode (direct/relay), latency, volume, and speed. See [USAGE.md](USAGE.md#exit-status-web-ui-localhost8085).
- ♻️ **Resilient** — client *and* exit auto‑reconnect with backoff; sleep/wake aware.
- 🎯 **Per‑application routing** — a built‑in SOCKS5 proxy (`-socks`) sends only the apps you point at it (e.g. Chrome) through the exit, without touching the host's default route. See [USAGE.md](USAGE.md#per-application-proxy-route-only-one-app-eg-chrome).
- 📦 **Pure Go, cgo‑free** — single static binaries; cross‑compiles to 15 platforms with no C toolchain.
- 🐧 **Cross‑platform data plane** — Linux, macOS (`utun`), and Windows (Wintun + WinNAT).

## Architecture at a glance

```
   You (A)                 Broker (B, public)              Exit node (C, in region X)
  ┌────────┐   auth + match  ┌──────────────┐   reverse tunnel ┌──────────────┐   NAT
  │ client │ ───────────────▶│ auth/registry│◀─────────────────│ exit agent   │ ───────▶ Internet
  │  + TUN │                 │ signaling    │                  │  TUN + NAT   │        (egress here)
  └────────┘                 │ STUN/TURN    │                  └──────────────┘
       └──────────── direct P2P (when NAT permits) ─────────────────┘
```

Two data paths, chosen per session:

| Path | Route | Notes |
|---|---|---|
| **Relay** | `A → B → C` | Always works. The bootstrap and the fallback. |
| **Direct (P2P)** | `A ⇄ C` | Lower latency, cheap for the broker. ICE hole‑punch + TURN fallback. |

IP packets ride **QUIC DATAGRAM** frames ([RFC 9221](https://www.rfc-editor.org/rfc/rfc9221)) — unreliable,
like WireGuard's UDP, to avoid TCP‑over‑TCP meltdown. Full design in **[ARCHITECTURE.md](ARCHITECTURE.md)**.

## Quickstart

### Option A — Docker (one command)

Requires a Linux Docker daemon. From the `docker/` directory:

```bash
docker compose up --build --abort-on-container-exit --exit-code-from client
```

Prints `SMOKE PASS` when the relay path works end‑to‑end. Direct‑P2P, mTLS, OIDC, TURN, and tenant‑isolation
variants are in the [Usage Guide](USAGE.md#run-it-with-docker-quickstart).

### Option B — bare metal (one Linux box, three terminals)

```bash
make build

# Terminal 1 — broker (no root needed)
./bin/revquic-broker -seed-user alice-token:us-west

# Terminal 2 — exit node (root: creates a TUN + NATs out your WAN nic)
sudo ./bin/revquic-exit -broker localhost:4242 -region us-west -uplink eth0

# Terminal 3 — client (root: creates a TUN, gets a VPN IP)
sudo ./bin/revquic-client -broker localhost:4242 -region us-west -token alice-token

# Verify the tunnel (packets travel A → B → C):
ping -c3 10.99.0.1
```

Then open the admin dashboard at **<http://localhost:8080>** (login `admin` / `admin`).

> [!IMPORTANT]
> `-uplink` must be your **real internet‑facing interface**. Find it with `ip route get 1.1.1.1` (look at
> the `dev <iface>` field) — on cloud VMs it's often `ens5`/`enp1s0`, not `eth0`. A wrong `-uplink` looks
> healthy (`ping 10.99.0.1` works) but silently drops client→internet traffic. See the
> [pre‑flight checklist](USAGE.md#before-you-run-the-exit-pre-flight--find-your-uplink-interface).

## Install / build

Requires **Go 1.25+**. Everything is **pure Go** (`CGO_ENABLED=0`) — no C toolchain.

```bash
make build      # all binaries → bin/
make test       # go test ./...
make web        # build + embed the Vue admin dashboard
make release    # cross-compile the full matrix → dist/<os>-<arch>/
```

| Binary | Role |
|---|---|
| `revquic-broker` | public rendezvous + relay + admin dashboard |
| `revquic-exit` | egress node (TUN + NAT) |
| `revquic-client` | the device that wants a VPN |
| `revquic-certgen` | mTLS certificate helper |

## Platform support

✅ supported · ⚠️ supported with conditions · ❌ not supported

| Binary | Linux¹ | macOS | Windows | Elevation |
|---|:---:|:---:|:---:|---|
| `revquic-broker` | ✅ | ✅ | ✅ | none (pure user‑space) |
| `revquic-certgen` | ✅ | ✅ | ✅ | none |
| `revquic-client` | ✅ | ✅ (`utun`) | ✅ (Wintun²) | root on Linux/macOS · Administrator on Windows |
| `revquic-exit` | ✅ (recommended) | ❌³ | ⚠️ Windows 11 + WinNAT⁴ | root on Linux · Administrator on Windows |

> ¹ **Linux** covers `amd64`, `arm64`, `arm`, `386`, `riscv64`, `ppc64le`, `s390x`. The exit uses
> `iptables` for NAT **and** enforces per‑tenant isolation here — this is the recommended exit.
>
> ² **Windows client** needs `wintun.dll` (matching the CPU arch) beside the executable and must run
> **as Administrator**. The release zips bundle the DLL.
>
> ³ **macOS exit is not supported** — there's no `iptables`‑equivalent for NAT + per‑tenant isolation.
> (macOS works fine as the *client* and *broker*.)
>
> ⁴ **Windows exit (native `revquic-exit.exe`)** works **only on Windows 11 Pro/Enterprise/Server with
> WinNAT** (run as Administrator). If WinNAT is absent (e.g. Windows Home) the exit **stops at startup**
> with a clear message. It has **no per‑tenant isolation** (`-isolate` is a no‑op on Windows). For
> isolation — or on editions without WinNAT — run the exit inside **WSL2 (Debian)**, which is real Linux
> (full `iptables` NAT + isolation). See [USAGE.md › Windows](USAGE.md#windows-client).

## Documentation

| Document | What's inside |
|---|---|
| **[ARCHITECTURE.md](ARCHITECTURE.md)** | Network layers, component & package breakdown, connection sequence diagrams, load balancing, QoS/telemetry, resilience, security model, references. |
| **[USAGE.md](USAGE.md)** | All modes (auth, storage, NAT traversal), config files, full CLI reference, user management, per‑platform setup (macOS/Windows/Linux), throughput tuning, Docker & bare‑metal examples (simple → hardened production), known limitations. |
| [`TESTING.md`](TESTING.md) | End‑to‑end test matrix and how to run it. |
| [`docker/README.md`](docker/README.md) | Docker Compose matrix details. |
| [`spec/`](spec/) | Design docs: feasibility study, HLD, LLD, Phase 2 (direct path), admin UI, OpenAPI. |

## Repository layout

```
revquic/
├── cmd/                 revquic-broker · revquic-exit · revquic-client · revquic-certgen
├── internal/            proto · quicx · tunnel · netcfg · ippool · ratelimit · directpath
│                        ice · directlink · icewire · session · oidc · pki · pwhash · turncred
│                        userstore · adminstore · auth · events · adminserver · lb · qos
│                        telemetry · sysstat · conf · logx · shutdown
├── web/admin/           Vue 3 + Vite admin dashboard (embedded into the broker)
├── docker/              Dockerfiles + Compose matrix (relay/direct/mtls/oidc/nat/isolation)
├── spec/                design documents
├── .github/workflows/   CI (fmt/vet/build/test) + release (cross-compile & publish on tag)
└── Makefile · README.md · ARCHITECTURE.md · USAGE.md · TESTING.md
```

## Contributing

Contributions are welcome! To get started:

1. Fork the repo and create a feature branch.
2. Make your change with tests — keep it `CGO_ENABLED=0`.
3. Run the local checks before opening a PR:
   ```bash
   gofmt -l .          # must print nothing
   go vet ./...
   make test
   ```
4. Open a pull request describing the change and how you tested it.

CI (gofmt, `go vet`, build, and the full test suite) runs on every push and PR. For larger features,
please open an issue first to discuss the design — the [`spec/`](spec/) folder is a good reference for the
project's conventions.

## License

Licensed under the **GNU General Public License v3.0** — see [`LICENSE`](LICENSE).

```
Revquic — a reverse-proxy VPN over QUIC
Copyright (C) 2026 The Revquic authors

This program is free software: you can redistribute it and/or modify it under the terms of the GNU
General Public License as published by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version. This program is distributed in the hope that it will be useful, but
WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR
PURPOSE. See the GNU General Public License for more details.
```

GPLv3 is compatible with the licenses of the dependencies and the projects studied (Apache‑2.0, MIT, BSD) —
none of their code is vendored here. If you redistribute Revquic (modified or not), you must keep it under
GPLv3 and make the corresponding source available.

<div align="center">
<sub>Built with Go, quic-go, and pion/ice. Not affiliated with any VPN vendor.</sub>
</div>
