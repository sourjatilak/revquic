# Revquic — Alternative Strategy: OpenVPN (TCP) ↔ QUIC Relay

> **Status:** named fallback strategy, **not** the primary design. The primary design is the custom
> client with a pure-QUIC + ICE direct path (see [`high-level-design.md`](./high-level-design.md) and
> [`low-level-design.md`](./low-level-design.md)). Use this strategy only when the custom client is not
> viable. The two strategies are **composable** — they share B's control plane, C's egress NAT, and the
> user database — so one deployment can serve both simultaneously.

## 1. When to use this strategy

Choose the alternative when **any** of these hold:
- **A cannot run custom software** (locked-down/corporate device, app-store-only OpenVPN, embedded box).
- **The deployment already standardizes on OpenVPN** (existing PKI, `ccd` per-client configs, accounting
  tied to `client-connect`/`client-disconnect`). C just gets an `egress-agent` shim in front of the
  existing OpenVPN server.
- **P2P is known to be impossible** (symmetric NAT both ends, UDP blocked) — ICE machinery would add
  complexity for zero benefit, and an always-relayed path is simpler.

## 2. Design

Stock OpenVPN client on A connects over **TCP/443 to B**; B is a transparent **L4 byte relay** that
bridges A's TCP connection onto a **QUIC stream** riding C's persistent reverse tunnel; C terminates
OpenVPN locally and NATs to the internet. **OpenVPN TLS runs end-to-end A↔C** — B sees only ciphertext.

```mermaid
sequenceDiagram
    participant A as A — Stock OpenVPN (proto tcp-client)
    participant B as B — Broker (TCP listener + QUIC relay)
    participant C as C — Egress (OpenVPN tcp-server + TUN + NAT)
    participant NET as Internet

    Note over C,B: C maintains persistent QUIC reverse tunnel to B (frpc→frps)
    C->>B: QUIC dial (mTLS) + Register{nodeID, region}
    A->>B: TCP SYN :443 (OpenVPN)
    B->>B: select C by region; allocate session
    B->>C: open bidi QUIC stream on C's tunnel (streamID=sessionID)
    C->>C: pipe QUIC stream ↔ 127.0.0.1:1194 (openvpn tcp-server)
    A->>C: OpenVPN TLS handshake (B opaque)
    C->>C: auth-user-pass-verify → broker-control REST
    C-->>A: auth OK, push redirect-gateway, IP 10.99.0.x
    A->>C: app traffic → OpenVPN frame → (B QUIC stream) → C TUN
    C->>NET: MASQUERADE egress
    NET-->>C: reply → C TUN → (B QUIC stream) → A
```

### Why a QUIC **stream** is correct here (and not for the primary)
The payload on this path is **OpenVPN's single TCP byte stream** — already an ordered reliable channel. A
QUIC **stream** is the correct, matched transport for it. This is the opposite of the custom-client path,
where the payload is **raw IP packets** and must use **QUIC datagrams** (see
[`reconciliation-and-validation.md`](./reconciliation-and-validation.md) §3).

## 3. Component inventory

| Component | Host | Built/Reused | Responsibility |
|---|---|---|---|
| OpenVPN client | A | Reuse | `proto tcp-client` to `B:443`, `auth-user-pass`, `dev tun` |
| `broker-tcp-listener` | B | Build (Go) | accept A's TCP, allocate session, splice to QUIC stream on C's tunnel |
| `broker-quic-relay` | B | Build (`quic-go`) | maintain C's reverse tunnel, open per-session streams, `io.Copy` bridge |
| `broker-control` | B | Build | registry, region select, auth-token validation for C's OpenVPN plugin |
| `egress-agent` | C | Build (`quic-go`) | dial tunnel to B, register, accept streams, pipe to localhost OpenVPN |
| `egress-openvpn-server` | C | Reuse OpenVPN | `proto tcp-server` on `127.0.0.1:1194`, `dev tun`, `auth-user-pass-verify` |
| `egress-nat` | C | Reuse iptables | `MASQUERADE` TUN subnet → WAN, `ip_forward=1` |

## 4. Key implementation points

### B — byte relay (transparent)
```go
// A's TCP conn  <->  QUIC stream on C's persistent reverse tunnel
stream, _ := cConn.OpenStreamSync(ctx)   // cConn = selected C's QUIC connection
go io.Copy(stream, tcpConn)               // A → C
go io.Copy(tcpConn, stream)               // C → A
```
- Optional first-byte demux: OpenVPN opcode `0x38` vs TLS `0x16` to reject non-OpenVPN flows on `:443`.
- C's tunnel stays warm with QUIC PING keepalives (~15s); on C drop, B marks it down and A's OpenVPN
  auto-reconnects (lands on another C).

### A — stock `.ovpn` (points at B, not C)
```
client
dev tun
proto tcp-client
remote b.example.com 443
remote-cert-tls server
verify-x509-name C-egress name     # C's server cert CN (end-to-end identity)
auth-user-pass
cipher AES-256-GCM
data-ciphers AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305
redirect-gateway def1 def2 bypass-dhcp
dhcp-option DNS 1.1.1.1
<ca> ... C's CA ... </ca>
```
From A's view there is one `remote` (B) and one OpenVPN TLS peer (C, reached transparently).

### C — OpenVPN server + delegated auth
- `proto tcp-server`, `dev tun`, `topology subnet`, `server 10.99.0.0/24`.
- `auth-user-pass-verify` script calls `broker-control` REST so the **user DB lives at B** while **C owns
  the OpenVPN TLS keys** (server cert authenticates C to A).
- `client-connect`/`client-disconnect`/`learn-address` hooks → B for session tracking, per-client IP,
  accounting; `client-config-dir` for static per-user IP.
- `tls-crypt` wraps the control channel (light obfuscation + control-channel auth).
- NAT: `sysctl net.ipv4.ip_forward=1`; `iptables -t nat -A POSTROUTING -s 10.99.0.0/24 -o eth0 -j MASQUERADE`.

## 5. Performance reality (important)

This path carries the user's **application TCP inside OpenVPN's TCP** inside a QUIC stream. The dominant
cost is **not** the QUIC layer (QUIC-around-TCP is mild — fast, ACK-clocked loss recovery). It is the
inherent **app-TCP-over-OpenVPN-TCP** penalty — the same reason OpenVPN-UDP normally outperforms
OpenVPN-TCP:

| Concern | Effect | Mitigation |
|---|---|---|
| App-TCP-over-tunnel-TCP (the real one) | competing retransmits / window collapse under loss | accept as fallback cost; prefer the primary custom-client path when possible |
| QUIC stream HOL within a session | one lost packet stalls that session's subsequent bytes | inherent; QUIC's loss detection is faster than TCP's; other sessions' streams are unaffected |
| Double congestion control (inner TCP + QUIC) | possible under-utilization | tune QUIC CC for relay streams; clamp OpenVPN `--mssfix`/`--fragment` under QUIC PMTU |
| Always via B; no direct path | latency + B bandwidth | by design; switch to the primary strategy to get a direct path |

**Conclusion:** acceptable as a compatibility/fallback path; **not** a performance target. It exists so a
deployment never has to tell a stock-OpenVPN user "no" — only "slower."

## 6. Relationship to the primary strategy

| Dimension | Primary (custom client, pure QUIC + ICE) | Alternative (OpenVPN-TCP over QUIC) |
|---|---|---|
| A's client | custom binary (`quic-go` + `pion/ice` + TUN) | stock OpenVPN, one `.ovpn` |
| A→C path | direct P2P QUIC (datagrams) when NAT permits; else TURN relay | always A→B→C |
| Data-plane encap | **QUIC datagrams** (raw IP) | QUIC **stream** (OpenVPN TCP byte stream) |
| Transport | pure QUIC/UDP everywhere | TCP (A↔B) + QUIC (B↔C) |
| B sees plaintext? | no (QUIC TLS A↔C) | no (OpenVPN TLS A↔C) |
| Latency / throughput | best (direct) | higher latency, lower throughput |
| Reuses OpenVPN infra on C | no | yes |
| Best when | latency-critical, custom client OK | stock client required / OpenVPN shop / P2P impossible |

Shared between both: B's auth + registry + region selection + user DB, and C's egress NAT. B routes each
session to the appropriate C-side listener (QUIC-datagram endpoint vs local OpenVPN server), so both
classes of user run on one deployment.
