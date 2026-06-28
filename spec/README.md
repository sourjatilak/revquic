# Revquic — Reverse-Proxy VPN Specification

A spec for a VPN in which the **exit node dials outward** to a public broker, letting users egress from a
chosen region through nodes that live behind NAT (residential/cloud). Synthesized from the problem
statement and five existing proxy implementations (frp, grepplabs/reverse-http, rust-rpxy, quic-proxy,
root-gg/wsp) — see **References & credits** in the [project README](../README.md).

> **Status note:** this folder is the original **design spec / feasibility study**. The system has since
> been implemented and tested — see the [project README](../README.md) and [TESTING.md](../TESTING.md).
>
> **Chosen direction:** the **custom client with a pure-QUIC + ICE direct path** is the primary
> architecture (B = auth + ICE mediator + STUN/TURN); the IP data plane uses **QUIC datagrams (RFC 9221)**.
> **OpenVPN-TCP-over-QUIC** is a named fallback for stock clients. The user's three-part rough spec has
> been reconciled and validated — see [`reconciliation-and-validation.md`](./reconciliation-and-validation.md).

## Documents

| Doc | Read it for |
|---|---|
| [`feasibility.md`](./feasibility.md) | **Read first.** Which network layer (answer: L3-over-QUIC-datagram); can stock OpenVPN do it (relay yes, direct no); is A→C hole punching real (yes, with relay fallback); the don't-tunnel-IP-over-reliable-streams finding; NAT success matrix; per-capability verdicts. |
| [`reconciliation-and-validation.md`](./reconciliation-and-validation.md) | Reconciles the three-part rough spec with this design; validates its claims (pion/ice, coturn TURN REST, trickle ICE, NAT matrix); documents the **one substantive correction** (custom-client data plane must use QUIC datagrams, not reliable streams); lists nuances and open gaps. |
| [`high-level-design.md`](./high-level-design.md) | What to build: the A/B/C roles, the two connection models, the services inside the broker (B), exit node (C), and client (A), the recommended tech stack, how each reference maps onto Revquic, and the security model. |
| [`low-level-design.md`](./low-level-design.md) | Step-by-step per component: control-protocol messages, data-plane encapsulation, ICE + shared-socket QUIC/STUN demux, startup/serving/connect procedures, the hole-punch procedure, sequence diagrams (relay / direct / fallback), broker data structures, and a phased build plan. |
| [`alternative-strategy-openvpn-quic.md`](./alternative-strategy-openvpn-quic.md) | The named fallback: stock OpenVPN-TCP relayed over QUIC (B as opaque byte relay, OpenVPN TLS end-to-end A↔C). When to use it, design, and its real performance cost. |
| [`admin-web-ui.md`](./admin-web-ui.md) | The broker's management plane: **user management** (CRUD + per-user region assignment) and **device management** (real-time, region-grouped view of connected C exit nodes with config and live parallel-user counts). Real-time presence via an event bus + WebSocket/SSE; embedded Vue SPA (frp pattern). |
| [`phase2-direct-path.md`](./phase2-direct-path.md) | Phase 2: upgrading a session from relay to a direct hole-punched `A⇄C` QUIC path. ICE roles + trickle candidate exchange, shared-socket QUIC+STUN demux, the NAT→direct/relay decision matrix, the relay↔direct migration state machine, and session continuity. Maps to the `internal/directpath` + `internal/ice` code in the spike. |

## Executive summary

- **Roles.** A = client; B = thin public broker (auth + registry + signaling + STUN + relay fallback);
  C = exit node that dials out to B and egresses to the internet in the target region.
- **Layer.** A real "all-protocols" VPN is **Layer 3** (a TUN device). The transport carrying those IP
  packets between A/B/C is **Layer 4 (QUIC/UDP)**. So Revquic = **L3-over-QUIC**. A proxy-only variant would
  be L4-only (SOCKS/CONNECT) but would not cover all protocols.
- **Critical encapsulation rule.** Carry IP packets in **QUIC unreliable datagrams (RFC 9221)** or
  **WireGuard** — never in reliable QUIC streams/TCP (that causes TCP-over-TCP meltdown). The L4 reference
  proxies stream-per-connection, which is fine for them but wrong for a VPN.
- **Two models.** Model 1 **relay** (`A→B→C`) always works and supports a **stock OpenVPN/WireGuard
  client**; build it first — it is also the mandatory fallback. Model 2 **direct** (`A⇄C`, hole-punched,
  B signals only) is lower-latency but needs a **custom A-side agent** and fails on symmetric/CGNAT
  (then relays).
- **OpenVPN verdict.** Stock OpenVPN can reach B (relay) but **cannot** do STUN/ICE hole punching or QUIC;
  the direct path requires a custom agent (or WireGuard + orchestrator, the Tailscale/Netbird model).
- **Reuse.** `reverse-http` ≈ Model 1 (agent dial-out + QUIC + agentID routing); `frp` ≈ Model 2 + control
  plane (mine `pkg/nathole`, `pkg/msg`, `server/registry`, `pkg/vnet`); `quic-proxy` ≈ QUIC adapters;
  `wsp` ≈ pooling/ACLs; `rust-rpxy` ≈ TLS/mTLS termination + hot-reload patterns.

## Architecture at a glance

```mermaid
graph LR
    A["A — Client<br/>(stock OpenVPN/WG, or custom agent)"]
    B["B — Broker<br/>auth · registry · signaling · STUN · relay"]
    C["C — Exit node<br/>reverse dial-out + TUN + NAT"]
    NET["Internet (region X egress)"]

    C -- "dial out + register" --> B
    A -- "auth + request region" --> B
    A -. "direct (hole-punched, QUIC-datagram)" .-> C
    A == "relay fallback" ==> B == "" ==> C
    C --> NET
```

## Recommended next step
Build **Phase 0** from the LLD: C dials B over QUIC/mTLS and registers; B selects it; one relay session
carries IP packets A↔B↔C over **QUIC datagrams** with `MASQUERADE` at C. That proves the L3-over-datagram
data plane end-to-end before adding auth, multi-exit selection, and hole punching.
