# Revquic — Reconciliation & Validation of the Rough Spec

This document reconciles the three-part rough spec (the `chat.z.ai` conversation, now provided verbatim)
with the Revquic spec in this folder, and assesses the **feasibility of its specific claims**. The three
parts are:

- **Part 1** — full HLD + LLD with both modes (relay + direct) and an OpenVPN compatibility path.
- **Part 2** — refined scope: **custom client, pure QUIC end-to-end, B as ICE mediator** (drops the
  TCP/OpenVPN path). *This is the chosen primary direction.*
- **Part 3** — **Alternative Strategy**: stock **OpenVPN-TCP relayed over QUIC** for when the custom
  client is not viable.

Overall verdict: **the rough spec is technically sound and well-aligned with this folder's design.** There
is **one substantive correction** (data-plane encapsulation for the custom client), plus a few nuances and
gaps. Everything else validates.

## 1. Strong agreements (no change needed)

| Topic | Rough spec position | This spec | Status |
|---|---|---|---|
| Three-layer model | L3 VPN (TUN) / L4 QUIC transport / L7 control | identical | ✅ agree |
| B is L4 relay + L7 mediator, never an L3 router | yes | yes | ✅ agree |
| C dials out (reverse tunnel), can be NAT'd | yes (frp `frpc→frps` model) | yes | ✅ agree |
| Standard OpenVPN cannot do the direct path | yes, 3 reasons | yes (feasibility §4) | ✅ agree |
| Custom client mandatory for direct | yes | yes | ✅ agree |
| TURN relay is a mandatory fallback | yes (symmetric/both-NAT) | yes (feasibility §5) | ✅ agree |
| "Start relayed, upgrade to direct" runtime pattern | yes (iroh/QUIC NAT-traversal draft) | yes | ✅ agree |
| Region-based exit selection at B | yes (least-load/RTT) | yes | ✅ agree |
| Reverse-tunnel + QUIC reuse from frp / reverse-http | yes | yes | ✅ agree |

The Part 2 decision — **custom client + pure QUIC + ICE, B as mediator** — is exactly the direction this
spec recommends as the high-value target. Adopted as primary.

## 2. Validated technical claims (correct as written)

- **ICE via `pion/ice`, controlling = A (dialer), controlled = C.** Correct per RFC 8445 convention; the
  `AgentConfig` fields cited (`URLs`, `CandidateTypes`, `NetworkTypes`, `MulticastDNSMode`,
  `KeepaliveInterval`, `DisconnectedTimeout`, `FailedTimeout`) are real pion/ice fields, and
  `Agent.Dial`/`Agent.Accept(ctx, ufrag, pwd)` is the correct lifecycle.
- **coturn TURN REST credentials:** username `"<expiry_unix>:<sessionID>"`, password
  `base64(HMAC-SHA1(secret, username))`, server `--use-auth-secret --static-auth-secret`. Correct — this
  is the standard TURN REST API; minting short-lived creds at B and feeding them to `ice.AgentConfig.URLs`
  is the right pattern.
- **Trickle ICE over a gRPC `SignalChannel` bidi stream**, with B as an opaque candidate ferry (WebRTC
  signaling-server role). Correct and idiomatic; trickle reduces setup latency.
- **NAT/candidate matrix** (host / srflx / prflx / relay; symmetric-both → TURN). Correct, including the
  reason srflx fails for symmetric NAT (per-destination port mapping).
- **One QUIC connection per peer multiplexing many sessions as streams (relay path).** Correct — QUIC
  connection IDs avoid socket/port exhaustion at B. (Note the data-plane caveat in §3 below.)
- **mDNS candidate mode to hide host IPs.** Correct privacy measure.
- **OpenVPN `http-proxy` requires `proto tcp`; UDP-over-proxy is unreliable.** Correct — this is a real
  OpenVPN constraint and a valid reason the compat path is TCP-only.
- **B keeps OpenVPN TLS end-to-end (A↔C), acting as an opaque byte relay in the alternative strategy.**
  Correct and important: B never holds data-channel keys.

## 3. The one substantive correction — data-plane encapsulation (custom client)

**Claim under review (Part 2, §3.2.3 and §3.3.3):** the custom client and egress bridge IP packets to/from
a QUIC **stream** (`directStream.Read/Write`, `stream.Read → tun.Write`).

**Problem:** a QUIC stream is **reliable and ordered**. Carrying raw IP packets — which include the user's
own TCP — over a single reliable ordered stream reintroduces exactly the pathology a VPN must avoid:

- **Head-of-line blocking:** one lost packet on the tunnel stalls *every* inner flow multiplexed on that
  stream until QUIC retransmits.
- **Double reliability:** the user's inner TCP retransmits *and* QUIC retransmits the same loss — the
  "TCP-over-reliable-tunnel" penalty. This is precisely why OpenVPN-UDP is preferred over OpenVPN-TCP and
  why WireGuard uses UDP.

**Correction:** for the **custom-client L3 data plane, carry each IP packet in a QUIC DATAGRAM frame
(RFC 9221)**, not a stream. quic-go exposes `Connection.SendDatagram()` / `ReceiveDatagram()`. Datagrams
are unreliable + unordered (correct for IP), still inside QUIC's TLS 1.3, congestion control, path
validation, and **connection migration** (which the spec rightly wants for the relay→direct upgrade).
Reserve QUIC **streams** for control/signaling and reliable side-channels only.

Implications to apply (done in `low-level-design.md`):
- A and C pump `TUN ⇄ QUIC datagram` (not stream).
- **MTU:** datagrams are not fragmented by QUIC, so set the TUN MTU below the path MTU minus
  QUIC/UDP/IP/datagram overhead (≈1280–1350), and/or clamp inner TCP MSS, to avoid drops of oversized
  packets.
- **Relay path through B:** B must relay **datagrams** bound to a session, not splice a byte stream. Two
  clean options: (a) a dedicated QUIC connection per session between B and each peer (datagrams need no
  demux), or (b) a small session-id prefix on each relayed datagram. (Streams are fine only for the
  OpenVPN-TCP *alternative* strategy — see §4.)

> This does **not** apply to the Alternative Strategy: there the payload *is* a single OpenVPN TCP byte
> stream, so carrying it over a QUIC **stream** is the correct match. The datagram rule is specific to the
> custom client's raw-IP data plane.

## 4. Nuances to tighten

### 4.1 The Alternative Strategy's real performance cost is app-TCP-over-OpenVPN-TCP, not "TCP-over-QUIC"
Part 3 §4 argues that OpenVPN-TCP-inside-a-QUIC-stream "is not the classic TCP-over-TCP meltdown." That is
true for the *outer* layer (QUIC's loss recovery is faster and ACK-clocked, so QUIC-around-TCP is mild).
**But the dominant penalty is one layer deeper and independent of QUIC:** the user's application TCP runs
*inside* OpenVPN's TCP byte stream, so you already have **app-TCP-over-tunnel-TCP** — the well-known reason
OpenVPN-UDP outperforms OpenVPN-TCP. Adding a reliable QUIC stream around it is a *third* reliable layer.
Net: the alternative strategy inherits OpenVPN-over-TCP's inherent meltdown tendency regardless of QUIC.
This is exactly why it is a **fallback**, not the primary. (The custom-client + QUIC-datagram path avoids
all of this — it is UDP-like end to end.)

### 4.2 RFC citations to verify before publishing
- **RFC 9221 (QUIC Datagram)** — correct, use it.
- The "shared UDP socket, demux QUIC vs STUN by wire format, `tr.ReadNonQUICPacket`" mechanism is **real
  and supported by quic-go** (built for exactly this P2P/WebTransport case). The *specific RFC number*
  cited for the demux (RFC 9443) should be **verified** — the mechanism is sound regardless of the exact
  reference. Don't block on it; do confirm the citation.
- The IETF **QUIC NAT-traversal** work and **connection migration (RFC 9000 §9)** support the
  relay→direct upgrade as described. ✅

### 4.3 Shared-socket ICE+QUIC vs coturn roles (clarify, not contradict)
Part 2 uses both (a) coturn as external STUN/TURN and (b) the shared-socket `ReadNonQUICPacket` demux.
These are complementary, not redundant: coturn provides **address discovery (STUN binding)** and the
**relay (TURN)**; the shared-socket demux is for the **ICE agent's own connectivity-check STUN traffic
co-existing with QUIC on the punched 5-tuple**. Document both roles so they aren't conflated.

### 4.4 "Start relayed, upgrade to direct" — build order vs runtime order
The user's instruction "start with the custom client implementation" is a **scope/build** decision (build
the custom client + ICE first; defer the stock-OpenVPN compat path to the Alternative Strategy). The
"start relayed → upgrade to direct" is a **runtime** behavior of that same custom client (bootstrap over a
QUIC relay through B for instant connectivity, then migrate to the hole-punched direct path). Both hold
simultaneously; the relay bootstrap/fallback through B is still required even in the "custom client first"
plan. Reflected in the HLD phasing.

## 5. Gaps to close before implementation

| # | Gap | Why it matters |
|---|---|---|
| 1 | **Data-plane crypto identity** when using QUIC datagrams A↔C: who authenticates whom? | The direct QUIC connection needs its own TLS identity (C presents a cert; A verifies via a token/cert minted by B). Define the trust chain so a punched path can't be hijacked. |
| 2 | **Session binding across relay→direct migration** | When A upgrades from relay to direct, C must map both to the same logical session/TUN/IP and accounting. Define the session-id continuity. |
| 3 | **MTU/MSS policy** | Datagram path drops oversized packets silently; need TUN MTU + MSS clamp defaults. |
| 4 | **Per-session isolation on C** | Multiple A's egress through one C; need per-session routing/firewall/IP so tenants can't see each other or C's LAN. |
| 5 | **Exit abuse/logging policy** | C egresses arbitrary user traffic; legal/abuse posture must be decided before any public C. |
| 6 | **B HA / registry persistence** | Single-broker is fine for MVP; multi-broker needs the `agentID→addr` style shared store (reverse-http uses memcached). |
| 7 | **DNS + kill switch on A** | Prevent leaks when tunnel drops / before it comes up. |

## 6. Net recommendation

1. **Adopt Part 2 as the primary architecture** (custom client, pure QUIC, ICE, B as mediator) — it is the
   right target and is feasible.
2. **Apply the §3 correction**: custom-client IP data plane = **QUIC datagrams (RFC 9221)**, not streams.
   This is the single most important change.
3. **Keep Part 3 as the named Alternative Strategy** (see
   [`alternative-strategy-openvpn-quic.md`](./alternative-strategy-openvpn-quic.md)), explicitly a
   relay-only fallback, with the §4.1 performance caveat documented.
4. **Design B's control plane once** so both strategies share it (auth, registry, region selection, TURN
   credential minting) — the rough spec already does this, which is the right call.
5. Close the §5 gaps during Phase 0/1.
