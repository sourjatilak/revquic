# Revquic — Feasibility Analysis

> **Working name:** Revquic (Reverse-Proxy VPN).
> **Status:** Feasibility study + design rationale. Read this before the HLD/LLD; it justifies the key
> architectural choices.
>
> **Note on the rough spec:** the three-part rough spec (originally a `chat.z.ai` link) has now been
> provided and **reconciled** — see [`reconciliation-and-validation.md`](./reconciliation-and-validation.md)
> for the claim-by-claim validation. Its conclusions agree with this analysis; the one correction is that
> the custom-client IP data plane must use **QUIC datagrams**, not reliable streams (§3 below).

## 1. The idea, restated precisely

Three roles:

- **A — Client.** An end user who wants their internet traffic to egress from a chosen region, as if they
  were physically there.
- **B — Broker.** A small, always-on server with a stable public IP/DNS name. It authenticates users,
  knows which exit nodes are online, and brokers connections. It is *not* meant to carry all traffic.
- **C — Exit node (the "reverse" part).** A node running **in the target region, typically behind NAT**
  (e.g. a residential or cloud box). Because it is behind NAT it cannot accept inbound connections, so it
  **dials out** to B and stays connected. C is where traffic finally egresses to the internet.

The defining characteristic — and the reason "reverse" is in the name — is that **the exit node initiates
the connection outward to the broker**, exactly like the agent in `reverse-http`, the client in `frp`, and
the client in `wsp`. This is what lets exit nodes live on networks that cannot host a normal inbound VPN
server.

Two ways A's traffic can reach C:

- **Model 1 — Relay:** `A → B → C`. B forwards. Always works; B pays the bandwidth.
- **Model 2 — Direct (hole-punched):** `A ⇄ C` directly; B only does authentication and signaling. Lower
  latency and cheap for B, but does not work behind every NAT.

## 2. Which network layer? (direct answer)

This is the most important early decision and the question you asked twice.

| Goal | Layer | Mechanism | Reference precedent |
|---|---|---|---|
| "All protocols, full internet, real VPN" | **L3 (IP)** | a **TUN** device captures IP packets; they are tunneled and egress at C with NAT/masquerade | OpenVPN, WireGuard, frp `pkg/vnet` (TUN) |
| "Just proxy my TCP (and some UDP)" | **L4** | SOCKS5 / HTTP `CONNECT`; one transport stream per connection | `reverse-http` (HTTP CONNECT), `quic-proxy`, frp tcp/http proxies |

Your requirement — *"a local VPN server should be running to access all protocols and be using the
internet"* — means **Layer 3**. You must run a **TUN** interface on A and route the default route into it.
C must own a TUN (or do kernel NAT) and `MASQUERADE` to its uplink.

**The transport that carries those L3 packets between A, B and C is Layer 4 (UDP/QUIC).** So the precise
framing is:

```
Revquic data plane = L3 (IP packets)  ENCAPSULATED IN  L4 (QUIC/UDP)  →  egress at C
                   └── what the user sees (a VPN) ──┘ └── how we move it ──┘
```

The reference proxies (`reverse-http`, `quic-proxy`) are **L4**: they tunnel individual TCP connections
over QUIC streams. That is correct for a CONNECT proxy but **not sufficient for a full VPN**. Revquic reuses
their *reverse-dial-out + QUIC-transport* ideas but adds an **L3 TUN data plane** on top.

## 3. Critical finding: do **not** tunnel IP over reliable QUIC streams

A subtle but decisive point that the reference L4 proxies do **not** have to deal with:

- `reverse-http` and `quic-proxy` map **one TCP connection to one QUIC stream**. Reliable-over-reliable is
  fine there because each stream carries exactly one already-reliable flow.
- A **VPN carries raw IP packets**, including the user's own TCP. If you put those packets on a **reliable,
  ordered** QUIC stream (or any TCP-based tunnel), you get **"TCP-over-TCP meltdown"**: the inner and outer
  retransmission timers fight, and one lost packet head-of-line-blocks every flow sharing the tunnel.

**Therefore the L3 data plane MUST use an unreliable datagram transport:**

- **QUIC DATAGRAM frames (RFC 9221)** — unreliable, unordered, but still inside QUIC's encryption,
  congestion control, path validation, and **connection migration** (great for roaming/hole-punch). This
  is the recommended encapsulation for Revquic's IP packets.
- **or WireGuard** — its own UDP + Noise crypto; kernel-fast; endpoints can be updated after a hole punch.

`quic-go` (used by `reverse-http`, and available to `frp`) supports datagram frames. This is feasible
today. **Verdict: use QUIC streams only for control/signaling and reliable side-channels; use QUIC
datagrams (or WireGuard) for the IP data plane.**

## 4. Can standard OpenVPN do this? (direct answer)

Short answer: **OpenVPN works for Model 1 (relay) but cannot do Model 2 (direct hole-punch). For the
direct path you need a custom client agent (or WireGuard + an orchestrator).**

### Why
| Capability Revquic needs | Stock OpenVPN | Consequence |
|---|---|---|
| Connect to a known public server (B) with user/pass or cert | ✅ yes | A can use a stock client to reach B |
| NAT hole punching / STUN / ICE rendezvous | ❌ none | OpenVPN cannot establish A⇄C directly; it only dials a fixed reachable server |
| QUIC transport | ❌ (its own proto; DCO is kernel datachannel, not QUIC) | the QUIC tunnels are B↔C / A↔C, independent of OpenVPN |
| Dynamic re-target to a chosen exit after auth | ❌ not in-protocol | B must do server-side routing to the selected C |
| Endpoint roaming after path change | ⚠️ limited (`float`) | weak for P2P; WireGuard handles roaming cleanly |

### Two viable stances
1. **Keep stock OpenVPN at A → relay-only (Model 1).** B *is* an OpenVPN server. After auth, B routes that
   user's tun traffic into a QUIC tunnel to the selected C. A needs no custom software. **This is the
   fastest path to a working product** and mirrors `reverse-http` exactly, just at L3.
2. **Custom client agent at A → enables direct (Model 2).** A small daemon authenticates to B, receives an
   exit assignment + ICE candidates, hole-punches to C, and runs the L3 data plane (QUIC-datagram or
   WireGuard) over the resulting path, falling back to relay via B when hole punching fails.

**Recommended:** support **both** — stock OpenVPN/WireGuard for the relay tier, and a custom agent for the
P2P tier. Make the broker protocol identical for both so C does not care which one it is serving.

### A note on "should standard OpenVPN do the hole punch, or a custom service at A?"
The hole punch **cannot** be done by the OpenVPN process. It must be done by a process that (a) speaks your
signaling protocol to B and (b) controls the local UDP socket used for the data plane. So either a custom
agent, or a WireGuard userspace orchestrator (the model Tailscale/Netbird/Netmaker use). OpenVPN, if used,
runs *after* the path exists — and in practice if you have a custom agent you would not also use OpenVPN
for the data plane; you would use QUIC-datagram or WireGuard.

## 5. Is A→C UDP hole punching actually feasible?

Yes for the **majority** of real-world NAT combinations, with a mandatory relay fallback. This is a solved
problem and there is a **working Go reference inside this repo**: `frp/pkg/nathole`.

### How it works (and what `frp` already implements)
- **STUN** (B runs a STUN server; `frp` uses `pion/stun`) lets A and C each discover their public
  *mapped* `ip:port`.
- B relays each side's candidate addresses (this is the **signaling** role).
- Both sides **send packets simultaneously** to the other's mapped address; each NAT, having just seen an
  outbound packet, accepts the inbound one. A bidirectional UDP path now exists.
- `frp/pkg/nathole` goes further: `analysis.go` / `classify.go` **fingerprint the NAT behavior** and pick a
  strategy; `NatHoleDetectBehavior` carries `CandidatePorts`, `SendRandomPorts`, `ListenRandomPorts`, TTL,
  and send delays to handle harder NATs via **port prediction and spraying**.

### NAT success matrix (what to expect)
| C \ A | Full-cone | Restricted-cone | Port-restricted | Symmetric |
|---|---|---|---|---|
| **Full-cone** | ✅ easy | ✅ | ✅ | ✅ (cone side predictable) |
| **Restricted-cone** | ✅ | ✅ | ✅ | ⚠️ port-prediction |
| **Port-restricted** | ✅ | ✅ | ✅ | ⚠️ port-prediction |
| **Symmetric** | ✅ | ⚠️ | ⚠️ | ❌ **relay required** |

- **Symmetric NAT on both ends (and most CGNAT)** → hole punching generally fails → **must relay via B**
  (the TURN-like fallback). Plan for ~10–30% of sessions to relay depending on your user population.
- This means **B always needs a relay data path**, even in the "P2P" design. ICE's lesson: *always have a
  TURN fallback.*

**Verdict:** direct A⇄C is feasible and worth building (latency + B cost savings), but it is an
**optimization layered on top of a relay that must exist anyway**. Build relay first, add hole punching
second. Reuse `frp/pkg/nathole`'s approach (or the library directly) rather than writing NAT classification
from scratch.

## 6. Overall feasibility verdict

| Capability | Feasible? | Confidence | Basis |
|---|---|---|---|
| Reverse dial-out exit nodes (C→B), broker registry | ✅ Yes | High | `reverse-http` (agentID store), `frp` (registry) do exactly this |
| Relay model A→B→C at **L4** (SOCKS/CONNECT) | ✅ Yes | High | `reverse-http` is this, minus exit-region selection |
| Relay model A→B→C at **L3** (full VPN) | ✅ Yes | Medium-High | TUN + QUIC-datagram; standard tech, more plumbing |
| Stock OpenVPN client for relay tier | ✅ Yes | Medium | B as OpenVPN server + server-side routing to C |
| Direct A⇄C hole punch | ✅ Mostly | Medium | `frp/pkg/nathole` proves it in Go; fails on symmetric/CGNAT → relay |
| Direct path with **stock OpenVPN** | ❌ No | High | OpenVPN has no STUN/ICE; needs custom agent or WireGuard |
| QUIC-datagram L3 encapsulation | ✅ Yes | High | `quic-go` supports RFC 9221 datagrams |
| Region selection / multi-exit | ✅ Yes | High | broker registry keyed by region + capacity/health |

### Headline recommendations
1. **Data plane = L3 (TUN) over QUIC datagrams** (or WireGuard). Never IP-over-reliable-stream.
2. **Build the relay tier first** (works for everyone, including stock OpenVPN). It is also the mandatory
   fallback for the P2P tier.
3. **Add the direct/hole-punch tier second**, requiring a **custom A-side agent**; reuse `frp/pkg/nathole`.
4. **Broker (B) stays thin**: auth + registry + signaling + STUN + TURN-fallback. Keep heavy bandwidth on
   C (and on direct paths) wherever possible.
5. Treat **C as an exit node with abuse/liability implications** — design ACLs, rate limits, and a logging
   policy in from the start (see HLD §Security).
