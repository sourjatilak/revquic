# Revquic — Phase 2: Direct (P2P) Path

Phase 2 upgrades a session from **relay** (`A→B→C`, Phase 0/1) to a **direct, hole-punched
`A⇄C` QUIC path**, with B acting only as auth + signaling + STUN/TURN mediator. It implements the
"start relayed, upgrade to direct" pattern from
[`../spec/reconciliation-and-validation.md`](../spec/reconciliation-and-validation.md) and the design in
[`../spec/low-level-design.md`](../spec/low-level-design.md) §3.1/§4.4.

> Status: with `GOPROXY=direct`, `pion/ice/v2` and `quic-go` now fetch and build. Shipped + verified: the
> pure-logic policy (`internal/directpath`: NAT decision + relay↔direct state machine) and the
> **`pion/ice` adapter** behind the `internal/ice.Agent` seam, including a passing loopback ICE
> connectivity test. Remaining: the shared-socket QUIC+STUN wiring, broker signaling/STUN/TURN, and the
> end-to-end relay→direct migration on Linux (see §8).

## 1. Runtime sequence (recap)

```mermaid
sequenceDiagram
    participant A as Client A
    participant B as Broker B (signaling + STUN/TURN)
    participant C as Exit C
    A->>B: authenticated; session in RELAYING (traffic already flowing A→B→C)
    A->>B: STUN srflx; trickle candidates (SignalChannel)
    C->>B: STUN srflx; trickle candidates
    B->>A: peer candidates + role=controlling + sid/nonce
    B->>C: peer candidates + role=controlled + sid/nonce
    par connectivity checks
        A-->>C: STUN checks over candidate pairs (same UDP socket as QUIC)
        C-->>A: responses
    end
    alt a pair nominated (CHECKING→DIRECT)
        A->>C: QUIC dial over punched 5-tuple; migrate TUN pumps to direct
        Note over A,C: relay stream torn down; B leaves data path
    else timeout / all pairs fail
        Note over A,C: stay RELAYING (traffic never dropped)
    end
```

## 2. ICE roles & candidate exchange
- **Roles (RFC 8445):** the dialer **A is controlling**, **C is controlled**. A nominates the pair.
- **Candidates:** `host`, `server-reflexive` (via B's STUN), `relay` (via B's TURN). `mDNS` mode hides
  host IPs. **Trickle ICE**: candidates are sent incrementally over B's `SignalChannel` (gRPC bidi /
  control stream) and forwarded to the peer; checks start before gathering completes.
- **Binding:** each exchange carries `sid` + `nonce` (cf. frp `NatHoleSid`) so probes are matched to the
  right session and can't be cross-injected.

## 3. Shared-socket QUIC + STUN
The ICE connectivity-check STUN traffic and the QUIC data plane run on the **same UDP socket** so the
punched NAT mapping is reused. With `quic-go`, the `quic.Transport` wraps the shared `*net.UDPConn`;
non-QUIC (STUN) packets are read via `Transport.ReadNonQUICPacket` and handed to the ICE agent
(QUIC vs STUN distinguished by wire format). After a pair is nominated, `transport.Dial(peerAddr, …)`
opens the direct QUIC connection; IP packets ride **QUIC datagrams** (RFC 9221), never streams.

## 4. NAT → direct/relay decision (implemented: `internal/directpath`)
The broker decides per session whether to attempt direct based on the requested `mode` and the two NAT
types it learns from STUN classification (cf. `frp/pkg/nathole/classify.go`).

| A \ C | full-cone | restricted/port-restricted | symmetric |
|---|---|---|---|
| **full-cone** | direct | direct | direct |
| **restricted/port-restricted** | direct | direct | **relay** |
| **symmetric** | direct | **relay** | **relay** |

- `mode=relay` → always relay. `mode=direct` → attempt direct, error if it fails (no fallback).
- `mode=auto` (default) → use the table; **unknown** NAT → attempt direct then fall back.
- Symmetric×(symmetric|restricted) → relay up front (port prediction too unreliable to be worth the
  setup latency). All "attempt direct" outcomes still fall back to relay if checks fail.

## 5. Relay↔direct migration (implemented: `internal/directpath` state machine)
A session's data path is a small state machine; **traffic is never interrupted** because it starts and
stays on the relay until a direct path is proven, then atomically switches the TUN pumps.

```mermaid
stateDiagram-v2
    [*] --> New
    New --> Relaying: StartRelay (bootstrap; traffic flows)
    Relaying --> Checking: BeginChecks (ICE, only if decision allows direct)
    Checking --> Direct: DirectEstablished (migrate pumps)
    Checking --> Relaying: ChecksFailed (stay relayed)
    Direct --> Relaying: DirectLost (fall back; may re-check)
    Relaying --> Closed: Close
    Checking --> Closed: Close
    Direct --> Closed: Close
```

Rules enforced by the machine:
- You can only `BeginChecks` from `Relaying` (never skip the bootstrap).
- `Direct` is reachable only via `Checking` (never relay→direct directly).
- `DirectLost` always returns to `Relaying` (relay must still exist as the safety net).
- `Close` is terminal from any live state.

## 6. Session continuity across migration
- The **session id** is stable across relay and direct; C maps both to the same TUN/assigned-IP/accounting
  (gap #2 in the reconciliation doc).
- The data-plane crypto identity on the direct path is its own QUIC TLS session (C presents a
  broker-issued cert; A verifies a broker-minted token/cert) — gap #1.
- MTU on the direct path may differ from the relay; renegotiate/clamp on switch (gap #3).

## 7. What ships in this repo now (verified)
- `internal/directpath/decision.go` — `Decide(mode, natA, natC) → Decision` implementing §4.
- `internal/directpath/state.go` — the §5 migration state machine.
- `internal/directpath/*_test.go` — table tests for the matrix + transition validity (`-race`).
- `internal/ice/ice.go` — the `Agent` interface seam (no external deps).
- `internal/ice/pion_adapter.go` — **`pion/ice/v2` implementation of `ice.Agent`** (gather, trickle via
  `OnCandidate`/`AddRemoteCandidate`, controlling=`Dial`/controlled=`Accept`, restart, STUN/TURN URLs,
  mDNS toggle). **Verified by `internal/ice/pion_adapter_test.go`**: two real agents establish an ICE
  connection over loopback host candidates and transfer data end to end.

## 8. What still needs wiring

Done + verified:
- **QUIC-over-ICE direct link** — `internal/ice/packetconn.go` (net.PacketConn over the ICE path) +
  `internal/quicx` datagram dial/listen helpers + `internal/directlink.Establish`. Test round-trips QUIC
  datagrams over a loopback ICE path both ways.
- **Migration glue** — `internal/session.Session` binds `directpath.Machine` to a swappable datagram
  `Path`; migration swaps it atomically. Tested (`-race`).
- **ICE signaling negotiation** — `internal/icewire.Negotiate` exchanges creds + trickles candidates over
  a broker-relayed signal channel, then establishes the direct QUIC link. Loopback test passes.
- **Broker signaling relay** — `MsgSignal` forwarded between client and exit, keyed by session id
  (serialized control writes).
- **Client/exit wiring** — both mains build the `ice.Agent` (A controlling, C controlled), trickle
  signals over the control stream, run `icewire.Negotiate`, and `session.UpgradeDirect` to migrate the
  TUN pumps off the relay (gated by `-direct`; works over host candidates with no STUN on a flat network).
  Builds for host + linux.

Remaining:
- **STUN/TURN service on B**: client/exit accept `-stun`/`-turn` (+creds) flags; for cross-NAT you must
  run a STUN/TURN server (coturn) and pass its URLs. An embedded STUN server on B is not built (use
  coturn). On a flat network (e.g. one Docker network) host candidates suffice — no STUN needed.
- **End-to-end Linux smoke** of the relay→direct upgrade: `docker/docker-compose.direct.yml` runs it and
  asserts the "upgraded to DIRECT" log (needs a usable Docker/colima daemon; see docker/README.md).

## 9. Verified end-to-end (containers, colima)
Both smokes passed in Alpine containers on a colima Linux VM:
- **Relay** (`docker compose up`): client got `10.99.0.2`, `ping 10.99.0.1` over the tunnel 3/3 0% loss →
  `SMOKE PASS: IP packets round-trip A -> B -> C`.
- **Direct** (`+ docker-compose.direct.yml`): exit logged `session 1: upgraded to DIRECT path`; ping still
  3/3 0% loss → `SMOKE PASS` + `DIRECT PASS: session migrated to the direct ICE/QUIC path`.
This confirms the whole chain on real Linux: TUN + QUIC-datagram relay, broker signaling relay, ICE
negotiation (host candidates), QUIC-over-ICE, and the relay→direct session migration.
