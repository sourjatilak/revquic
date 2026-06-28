# Usage Guide

> For the project overview and quickstart see [README.md](README.md).
> For the detailed technical architecture see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## Table of contents

- [Modes of operation (with config)](#modes-of-operation-with-config)
- [Managing users (clients)](#managing-users-clients)
- [Per‑application proxy (route only one app, e.g. Chrome)](#per-application-proxy-route-only-one-app-eg-chrome)
- [Exit status web UI (localhost:8085)](#exit-status-web-ui-localhost8085)
- [How to run on darwin (macOS)](#how-to-run-on-darwin-macos)
- [Windows client](#windows-client)
- [Throughput tuning (UDP buffers)](#throughput-tuning-udp-buffers)
- [Run it with Docker (quickstart)](#run-it-with-docker-quickstart)
- [Run it without Docker (bare metal)](#run-it-without-docker-bare-metal)
- [Command‑line reference](#command-line-reference)
- [Build & test](#build--test)
- [Cross‑compiling & releases](#cross-compiling--releases)
- [Known limitations](#known-limitations)

---

## Modes of operation (with config)

Revquic is configured entirely with **command‑line flags** (no config file required). Below are the modes;
combine flags as needed.

### 1) Authentication

**Client identity** — pick one:

| Mode | Broker flags | Client |
|---|---|---|
| **Token** (simple) | *(default; seed users with)* `-seed-user token:region` | `-token <token>` |
| **OIDC** (real SSO) | `-oidc-issuer <url> -oidc-audience <id> -oidc-jwks-url <url>` + `-seed-oidc-user email:region` | passes an **ID token** as `-token` |

**Node identity (exit ↔ broker) & data‑plane** — pick one:

| Mode | How |
|---|---|
| **Shared token** (default) | broker `-node-token <secret>`, exit `-token <secret>` |
| **mutual TLS** | generate certs with `revquic-certgen`, then pass `-tls-ca/-tls-cert/-tls-key` to broker, exit and client. |

**Admin** (dashboard/API): `-admin-user/-admin-pass` (passwords are PBKDF2‑hashed), or a bootstrap
`-admin-token`. Serve the dashboard over HTTPS with `-http-tls-cert`/`-http-tls-key`.

### 2) Where users/admins are stored

```
-store mem      # in-memory (default, non-persistent)
-store file     # JSON files at -userdb / -admindb (+ QoS history at -qosdb)
-store sqlite   # SQLite DB at -userdb / -admindb (+ QoS history at -qosdb)  (pure-Go, no cgo)
```

### 3) Data path / NAT traversal

| Mode | How | When |
|---|---|---|
| **Relay** | default | always works; bootstrap + fallback |
| **Direct (P2P)** | exit `-direct`, client `-direct` | low latency when NAT permits |
| **TURN relay** | broker `-turn-secret <s> -turn-url turn:host:3478 -stun-url stun:host:3478`; run **coturn** | symmetric‑NAT / CGNAT where hole punching fails |

### 4) Per‑session protections (exit)

```
-isolate            # (default on) block client↔client and client↔LAN; anti-spoofing on inbound
-rate-bytes 1000000 # per-session bandwidth cap (bytes/sec; 0 = unlimited)
```

### 5) Client routing

```
-full                       # send ALL traffic through the tunnel (full-tunnel)
-broker-ip <ip> -gw <gw> -gw-dev <dev>   # keep a host route to the broker so the tunnel doesn't loop
```

### 6) Config files

Any flag can live in a config file instead of the command line (`-config <path>`). Format: `key = value`,
one per line; `#` or `;` for comments; bare key sets a boolean to true. CLI flags always override.

Example `broker.conf`:
```ini
# revquic-broker config
quic = :4242
http = :8080
lb   = round-robin
store = sqlite
log-type = json
log-file = /var/log/revquic-broker.log
```
Run: `revquic-broker -config broker.conf`

**Ready‑to‑edit samples** for all three binaries (every option, documented) ship in
[`conf/`](conf/) and are bundled into the release archives:
`conf/sample_revquic_broker.conf`, `conf/sample_revquic_exit.conf`, `conf/sample_revquic_client.conf`.
Copy one, replace the `CHANGE_ME_*` values, and run e.g. `revquic-exit -config sample_revquic_exit.conf`
(comments must be on their own line; CLI flags still override the file).

---

## Managing users (clients)

Each VPN client authenticates with a **unique token**; the mTLS certs are shared across clients (they
secure the channel, the token identifies the user). Tokens are stored as HMAC‑SHA256 hashes.

### From the admin dashboard

Sign in to `http://<broker>:8080`, open **Users**, and use **Add user**: enter a username and allowed
regions. Leave the **token** field blank to have the broker generate one (shown **once**).

### From the REST API

```bash
# 1) Log in as an admin.
TOK=$(curl -s -X POST http://localhost:8080/api/v1/admin/login \
        -d '{"username":"admin","password":"<adminpwd>"}' | jq -r .token)

# 2a) Create a client (broker generates the token, returned once):
curl -s -X POST http://localhost:8080/api/v1/users -H "Authorization: Bearer $TOK" \
  -d '{"username":"bob","allowedRegions":["us-west"],"status":"enabled"}'

# 2b) …or supply your own token:
curl -s -X POST http://localhost:8080/api/v1/users -H "Authorization: Bearer $TOK" \
  -d '{"username":"carol","credential":"my-fixed-token","allowedRegions":["*"]}'
```

| Method | Path | Purpose |
|---|---|---|
| `GET`    | `/api/v1/users` | list users |
| `POST`   | `/api/v1/users` | create user |
| `GET`    | `/api/v1/users/{id}` | fetch one user |
| `PATCH`  | `/api/v1/users/{id}` | update regions/status/token |
| `DELETE` | `/api/v1/users/{id}` | remove a user |

> Token validation depends on a stable `-cred-pepper`. Use `-store file|sqlite` for persistence.

---

## Per‑application proxy (route only one app, e.g. Chrome)

Sometimes you don't want the **whole machine** on the VPN — you just want, say, **Chrome** to egress
through the exit while everything else uses your normal connection. The client can expose a local
**SOCKS5 proxy** whose outbound sockets are **bound to the tunnel interface**, so *only* the apps you
point at it go through the exit. This needs **no `-full`** and **does not change your default route**, so
there's no risk of cutting your own connectivity.

```bash
# Start the client with a SOCKS5 proxy on 127.0.0.1:1080 (no -full needed):
sudo ./bin/revquic-client -broker BROKER_HOST:4242 -region us-west -token "$CLIENT_TOKEN" \
  -socks 127.0.0.1:1080
# log: SOCKS5 proxy listening on 127.0.0.1:1080 → tunnel utun7 (point an app at socks5://127.0.0.1:1080)
```

Then launch the application pointed at that proxy. Only that app's traffic — including its DNS — goes
through the exit.

### Launch Chrome through the proxy

Use a **separate profile** (`--user-data-dir`) so it doesn't disturb your normal Chrome, and
`--proxy-server` with a SOCKS5 URL. Quitting that Chrome leaves your other windows untouched.

**macOS:**
```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --proxy-server="socks5://127.0.0.1:1080" \
  --user-data-dir="/tmp/revquic-chrome"
```

**Linux:**
```bash
google-chrome \
  --proxy-server="socks5://127.0.0.1:1080" \
  --user-data-dir="/tmp/revquic-chrome"
# (chromium users: swap in `chromium` or `chromium-browser`)
```

**Windows (PowerShell):**
```powershell
& "C:\Program Files\Google\Chrome\Application\chrome.exe" `
  --proxy-server="socks5://127.0.0.1:1080" `
  --user-data-dir="$env:TEMP\revquic-chrome"
```

> **DNS:** the proxy already resolves proxied hostnames **over the tunnel** (falling back to the system
> resolver only if the tunnel's DNS path can't be reached), so your browsing DNS doesn't leak to your
> local network. Verify your egress IP at <https://ifconfig.me> inside that Chrome window — it should be
> the exit's.

### Other apps

Any SOCKS5‑aware app works the same way:
- **Firefox:** Settings → Network Settings → Manual proxy → SOCKS v5 host `127.0.0.1` port `1080`, and
  tick *"Proxy DNS when using SOCKS v5"*.
- **curl:** `curl --socks5-hostname 127.0.0.1:1080 https://ifconfig.me`
- **Anything via a wrapper:** `proxychains-ng` / `tsocks` for apps without native proxy support.

### Notes & limits

- **CONNECT (TCP) only.** SOCKS5 `BIND` and `UDP ASSOCIATE` are not implemented, so UDP‑only apps and
  protocols (e.g. QUIC/HTTP‑3 in the browser) won't use the proxy — most browsers fall back to TCP.
- Works on **Linux, macOS, and Windows** (binds via `SO_BINDTODEVICE` / `IP_BOUND_IF` / `IP_UNICAST_IF`).
- Bind the proxy to `127.0.0.1` (the default examples) so it isn't reachable from your LAN.
- This routes by **application**, not by destination — use it instead of `-full` (the two are
  **mutually exclusive**: `-full` puts the whole machine on the tunnel, `-socks` routes only the apps
  you point at it; the client refuses to start if both are given).

---

## Exit status web UI (localhost:8085)

Each exit node serves a small **local status dashboard** — no admin login, meant for the operator of
that box. It runs by default on **`http://127.0.0.1:8085`** (bound to localhost only) and shows, live:

- **which clients are connected** (their VPN IP),
- **path mode** per client — **direct** (P2P) or **relay**, and whether **TURN** is in use,
- **latency** (RTT) to each client,
- **volume** — bytes up (client → internet) and down (exit → client), plus total,
- **speed** — current up/down throughput per client, and totals across all clients,
- how long each client has been connected.

It auto‑refreshes every 2 seconds. Just open it in a browser on the exit host:

```bash
# Exit started normally; the status UI comes up automatically:
sudo ./bin/revquic-exit -broker BROKER_HOST:4242 -region us-west -uplink eth0 -direct
# open http://127.0.0.1:8085

# Change the address, or disable it entirely:
sudo ./bin/revquic-exit ... -status-addr 127.0.0.1:9000     # custom port
sudo ./bin/revquic-exit ... -status-addr off                 # disable
```

> **Security:** the status UI has **no authentication** and exposes connection metadata. Keep it bound
> to **`127.0.0.1`** (the default). Do **not** bind it to `0.0.0.0` / a public address — there's no auth
> in front of it. If you need remote access, tunnel to it over SSH (`ssh -L 8085:127.0.0.1:8085 …`).

---

## How to run on darwin (macOS)


`revquic-client` runs natively on macOS using a kernel **`utun`** device. Requires `sudo`. Build with
`make darwin` or use `dist/darwin-arm64/revquic-client`.

```bash
DEF=$(route -n get default)
GW=$(echo "$DEF"  | awk '/gateway:/{print $2}')
DEV=$(echo "$DEF" | awk '/interface:/{print $2}')
BROKER_IP=$(dig +short broker.example.com | tail -1)

# Full-tunnel:
sudo ./bin/revquic-client \
  -broker broker.example.com:4242 -region us-west -token "$CLIENT_TOKEN" \
  -direct -full -broker-ip "$BROKER_IP" -gw "$GW" -gw-dev "$DEV" \
  -tls-ca certs/ca.pem -tls-cert certs/client-cert.pem -tls-key certs/client-key.pem \
  -tls-server-name broker.example.com

# Split-tunnel (drop -full and the routing flags):
sudo ./bin/revquic-client \
  -broker broker.example.com:4242 -region us-west -token "$CLIENT_TOKEN" -direct \
  -tls-ca certs/ca.pem -tls-cert certs/client-cert.pem -tls-key certs/client-key.pem \
  -tls-server-name broker.example.com
```

**Gotchas:** `sudo` required; `-tls-server-name` must match a SAN in the broker cert; with `-full` you
must pass `-broker-ip`/`-gw`/`-gw-dev`; an exit must be registered in the requested region. The client
spawns a cleanup‑supervisor child that restores routes if the parent exits for any reason.

---

## Windows client

`revquic-client` runs natively on Windows using **Wintun** (WireGuard's signed TUN driver). Place
`wintun.dll` beside the exe; run **as Administrator**.

The client creates a Wintun adapter named **`Revquic`**; the split‑default routes (`0.0.0.0/1` +
`128.0.0.0/1`) and the broker host route are managed via `netsh`/`route`. On exit the adapter is removed
automatically (its lifetime is tied to the process).

### Windows exit (native)

The native `revquic-exit.exe` works on **Windows 11 with WinNAT**. Run **as Administrator**. It uses
Wintun for the adapter and WinNAT (`New-NetNat`) for the NAT, and automatically adds a Windows Defender
Firewall allow rule.

Requirements:
- **WinNAT is required.** Ships on Windows 11 Pro/Enterprise/Server. If absent, the exit **stops at
  startup** with a clear message.
- Avoid a WinNAT **prefix conflict** (WSL2/Docker/Hyper‑V may already hold a NAT — check `Get-NetNat`).
- **No per‑tenant isolation** on Windows (`-isolate` is a no‑op).
- Test egress with `curl`/TCP — WinNAT often doesn't forward `ping`/ICMP.

### Windows exit via WSL2 (for isolation)

For per‑tenant isolation (or if WinNAT is unavailable), run the exit in **WSL2 (Debian)**:

```powershell
wsl --install Debian        # elevated PowerShell
```
```bash
sudo apt-get update && sudo apt-get install -y iptables iproute2
case "$(uname -m)" in
  x86_64)        ARCH=linux-amd64 ;;
  aarch64|arm64) ARCH=linux-arm64 ;;
esac
# copy the $ARCH build into Debian, then:
sudo ./revquic-exit -broker broker.example.com:4242 -nodeId exit-win-1 -region us-west \
  -uplink eth0 -token node-secret -direct -isolate
```

---

## Throughput tuning (UDP buffers)

QUIC needs large UDP socket buffers. On Linux the default `net.core.rmem_max` (~208 KB) is far below
what quic-go wants (~7 MB), capping throughput. Raise the limits on broker + exit + client hosts:

```bash
# Temporary (until reboot):
sudo sysctl -w net.core.rmem_max=7340032
sudo sysctl -w net.core.wmem_max=7340032

# Persistent:
printf 'net.core.rmem_max=7340032\nnet.core.wmem_max=7340032\n' | sudo tee /etc/sysctl.d/99-revquic.conf
sudo sysctl --system
```

Then **restart** the revquic process. Notes:
- `net.core.*` are host‑wide (not per‑container). With Docker, set on the **host**. With WSL2, run
  inside the WSL2 distro.
- This tunes the **relay** path. The direct/ICE path's buffers can't be tuned.

---

## Run it with Docker (quickstart)

Requires a Linux Docker daemon. From `docker/`:

```bash
# 1) Relay path (A → B → C)
docker compose up --build --abort-on-container-exit --exit-code-from client

# 2) Direct P2P
docker compose -f docker-compose.yml -f docker-compose.direct.yml up --build \
    --abort-on-container-exit --exit-code-from client

# 3) Mutual TLS
docker compose -f docker-compose.yml -f docker-compose.mtls.yml up --build \
    --abort-on-container-exit --exit-code-from client

# 4) OIDC sign-in via Dex
docker compose -f docker-compose.yml -f docker-compose.oidc.yml up --build \
    --abort-on-container-exit --exit-code-from client

# 5) Multi-NAT testbed (forces TURN relay)
docker compose -f docker-compose.nat.yml up --build -d

# 6) Tenant isolation
docker compose -f docker-compose.yml -f docker-compose.isolation.yml up --build \
    --abort-on-container-exit --exit-code-from client2
```

See [`docker/README.md`](docker/README.md) and [`TESTING.md`](TESTING.md) for the full matrix.

---

## Run it without Docker (bare metal)

Key facts:
- **`revquic-broker`** — any OS, no root.
- **`revquic-client`** — Linux/macOS (`sudo`) or Windows (Administrator).
- **`revquic-exit`** — Linux + root (recommended); Windows with WinNAT (limited); macOS unsupported.

### Build the binaries

```bash
make build            # → bin/revquic-broker, bin/revquic-exit, bin/revquic-client, bin/revquic-certgen
```

### Before you run the exit (pre‑flight) — find your uplink interface

```bash
# 1) Find the internet-facing interface:
ip route get 1.1.1.1                  # look at 'dev <iface>'

# 2) Enable IP forwarding:
sudo sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' | sudo tee /etc/sysctl.d/99-revquic-fwd.conf

# 3) Start the exit with the correct interface:
sudo ./bin/revquic-exit -broker localhost:4242 -region us-west -uplink ens5
```

> **If client → internet fails:** watch `sudo iptables -t nat -nvL POSTROUTING` and
> `sudo iptables -nvL FORWARD`. Test with `curl`, not `ping`.

### Example 1 — single host, relay path (simplest)

```bash
# Terminal 1 — broker
./bin/revquic-broker -seed-user alice-token:us-west

# Terminal 2 — exit (root)
sudo ./bin/revquic-exit -broker localhost:4242 -region us-west -uplink eth0

# Terminal 3 — client (root)
sudo ./bin/revquic-client -broker localhost:4242 -region us-west -token alice-token

# Verify
ping -c3 10.99.0.1
```

### Example 2 — full‑tunnel

```bash
sudo ./bin/revquic-client \
  -broker BROKER_HOST:4242 -region us-west -token alice-token \
  -full -broker-ip BROKER_PUBLIC_IP -gw 192.168.1.1 -gw-dev wlan0
curl -s https://ifconfig.me ; echo     # should print the exit's IP
```

### Example 3 — direct P2P

```bash
# Broker (public)
./bin/revquic-broker -seed-user alice-token:us-west
# Exit (behind NAT)
sudo ./bin/revquic-exit -broker BROKER_HOST:4242 -region home -uplink eth0 -direct -stun stun:BROKER_HOST:3478
# Client
sudo ./bin/revquic-client -broker BROKER_HOST:4242 -region home -token alice-token -direct -stun stun:BROKER_HOST:3478
```

### Example 4 — symmetric NAT / CGNAT via TURN

```bash
# coturn
turnserver --use-auth-secret --static-auth-secret=SECRET --listening-port=3478 --fingerprint --no-cli
# Broker
./bin/revquic-broker -seed-user alice-token:us-west \
  -turn-secret SECRET -turn-url turn:TURN_HOST:3478 -stun-url stun:TURN_HOST:3478
# Exit + client just need -direct
sudo ./bin/revquic-exit   -broker BROKER_HOST:4242 -region us-west -uplink eth0 -direct
sudo ./bin/revquic-client -broker BROKER_HOST:4242 -region us-west -token alice-token -direct
```

### Example 5 — hardened production (mTLS + OIDC + SQLite + TURN)

```bash
./bin/revquic-certgen -out certs -broker-san broker.example.com,localhost -days 365

./bin/revquic-broker \
  -quic :4242 -http :8080 -store sqlite -cred-pepper "$REVQUIC_PEPPER" \
  -admin-user admin -admin-pass "$ADMIN_PASS" \
  -tls-ca certs/ca.pem -tls-cert certs/broker-cert.pem -tls-key certs/broker-key.pem \
  -oidc-issuer https://idp.example.com -oidc-audience revquic -oidc-jwks-url https://idp.example.com/keys \
  -seed-oidc-user alice@example.com:us-west \
  -turn-secret "$TURN_SECRET" -turn-url turn:turn.example.com:3478 -stun-url stun:turn.example.com:3478

sudo ./bin/revquic-exit \
  -broker broker.example.com:4242 -nodeId exit-uswest-1 -region us-west -uplink eth0 -direct \
  -tls-ca certs/ca.pem -tls-cert certs/node-cert.pem -tls-key certs/node-key.pem \
  -tls-server-name broker.example.com -isolate -rate-bytes 6250000

sudo ./bin/revquic-client \
  -broker broker.example.com:4242 -region us-west -token "$ID_TOKEN" -direct -full \
  -broker-ip BROKER_IP -gw GW -gw-dev DEV \
  -tls-ca certs/ca.pem -tls-cert certs/client-cert.pem -tls-key certs/client-key.pem \
  -tls-server-name broker.example.com
```

### Example 6 — hardened without OIDC (token auth)

Same as Example 5 but drop `-oidc-*` flags, add `-seed-user "$CLIENT_TOKEN:us-west"` on the broker, and
the client uses `-token "$CLIENT_TOKEN"`.

### Example 7 — per‑application proxy (only Chrome through the exit)

**Use case:** keep the machine on its normal connection, but make **just Chrome** egress through the
exit. No `-full`, no default‑route change — the client runs a SOCKS5 proxy bound to the tunnel and you
point Chrome at it. (See [Per‑application proxy](#per-application-proxy-route-only-one-app-eg-chrome).)

```bash
# Broker (public) + a seeded user:
./bin/revquic-broker -seed-user alice-token:us-west

# Exit (root):
sudo ./bin/revquic-exit -broker BROKER_HOST:4242 -region us-west -uplink eth0

# Client: bring up the tunnel AND a per-app SOCKS5 proxy on 127.0.0.1:1080 (no -full):
sudo ./bin/revquic-client -broker BROKER_HOST:4242 -region us-west -token alice-token \
  -socks 127.0.0.1:1080
# log: SOCKS5 proxy listening on 127.0.0.1:1080 → tunnel utun7 ...

# Launch Chrome through the proxy in a throwaway profile (macOS shown; see the section above
# for the Linux/Windows commands):
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --proxy-server="socks5://127.0.0.1:1080" --user-data-dir="/tmp/revquic-chrome"

# Verify: in that Chrome, https://ifconfig.me shows the EXIT's IP, while your other apps/browsers
# still show your real IP. Quitting that Chrome leaves the rest of the system untouched.
```

### Setting the secrets

```bash
export REVQUIC_PEPPER="$(openssl rand -hex 32)"
export NODE_SECRET="$(openssl rand -hex 32)"
export CLIENT_TOKEN="$(openssl rand -hex 24)"
```

Keep them matched: broker uses all three; exit uses `-token "$NODE_SECRET"`; client uses `-token "$CLIENT_TOKEN"`. `REVQUIC_PEPPER` must stay stable (changing it invalidates all tokens).

### Gotchas

- Root + Linux for exit/client. Broker runs anywhere.
- `-uplink` must be your real WAN interface.
- `-full` must be paired with `-broker-ip`/`-gw`/`-gw-dev`.
- `-rate-bytes` is bytes/sec (50 Mbit/s ≈ `6250000`).
- Default secrets are demo only — replace them.

---

## Command‑line reference

**Common flags (all three binaries):**
```
-config <path>      # key=value config file (CLI overrides)
-log-file <path>    # write logs to file (default: stderr)
-log-type text|json # log format
```

### `revquic-broker`  (any OS; no root)
```
-quic :4242                          # QUIC listen address
-http :8080                          # admin API + dashboard
-http-tls-cert / -http-tls-key      # HTTPS for the admin web
-store mem|file|sqlite               # user/admin store backend
-userdb / -admindb / -qosdb         # store paths
-cred-pepper <secret>                # pepper for hashing tokens
-seed-user token:region              # seed a client user
-seed-oidc-user email:region         # seed an OIDC user
-admin-user / -admin-pass            # admin credentials
-admin-token <token>                 # bootstrap admin bearer token
-node-token <secret>                 # shared exit node secret
-lb least-conn|round-robin|random    # LB strategy
-session-resume-ttl 1h               # how long a disconnected client's session stays resumable (0 = off)
-tls-ca / -tls-cert / -tls-key      # mTLS
-oidc-issuer / -oidc-audience / -oidc-jwks-url / -oidc-jwks-file  # OIDC
-turn-secret / -turn-url / -stun-url # TURN/STUN
```

### `revquic-exit`  (Linux + root)
```
-broker host:4242                    # broker address
-nodeId exit-1                       # node id (default: derived from -name, else random exit-<n>)
-name "Home Exit"                    # optional display name (shown in the dashboard; ids derive from it)
-status-addr 127.0.0.1:8085          # local status web UI (no auth; localhost only); empty/'off' disables
-region us-west                      # served region
-capacity 0                          # max sessions (0 = broker default 100)
-uplink eth0                         # WAN interface
-gw 10.99.0.1/24                     # TUN gateway CIDR
-token <secret>                      # node secret
-isolate                             # per-tenant isolation (default: true)
-rate-bytes 0                        # per-session cap (0 = unlimited)
-report-interval 5s                  # QoS report cadence
-direct                              # enable ICE/QUIC direct paths
-direct-mode any|p2p-only            # direct upgrade policy
-ice-keepalive 1s                    # STUN keepalive on the ICE pair
-stun / -turn / -turn-user / -turn-pass  # STUN/TURN
-tls-ca / -tls-cert / -tls-key / -tls-server-name  # mTLS
```

### `revquic-client`  (Linux/macOS root, Windows Administrator)
```
-broker host:4242                    # broker address
-region us-west                      # desired region
-token <token>                       # client token or OIDC ID token
-name "Alice laptop"                 # optional display name (shown in the dashboard)
-socks 127.0.0.1:1080                # run a per-app SOCKS5 proxy bound to the tunnel (see "Per-application proxy")
-exit <nodeId>                       # pin a specific exit (empty = auto)
-list-exits                          # print exits (no root needed), then exit
-direct                              # enable direct path
-direct-mode any|p2p-only            # direct upgrade policy
-ice-keepalive 1s                    # STUN keepalive on the ICE pair
-full                                # full-tunnel (replace default route)
-broker-ip / -gw / -gw-dev          # routing flags (with -full)
-rate-bytes 0                        # per-session cap
-report-interval 5s                  # QoS report cadence
-stun / -turn / -turn-user / -turn-pass  # STUN/TURN
-tls-ca / -tls-cert / -tls-key / -tls-server-name  # mTLS
```

### `revquic-certgen`
```
-out certs                           # output directory
-broker-san server,localhost,broker   # broker DNS SANs
-days 365                            # validity
# emits: ca.pem, broker-cert/key.pem, node-cert/key.pem, client-cert/key.pem
```

---

## Build & test

```bash
go mod tidy
make build            # all binaries into bin/ (CGO_ENABLED=0)
make darwin           # macOS-native (broker, client, certgen)
make test             # go test ./...
make web              # build Vue admin SPA + embed
```

> Build with `CGO_ENABLED=0` (the Makefile does this) — avoids macOS dyld issues.

---

## Cross‑compiling & releases

Everything is pure Go (`CGO_ENABLED=0`), so cross‑compile with no C toolchain:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/linux-arm64/ ./cmd/...
make release          # full matrix into dist/<os>-<arch>/
make package VERSION=v0.1.0   # archives + SHA256SUMS for GitHub Release
```

`make release` builds: `linux/{amd64,arm64,arm,386,riscv64,ppc64le,s390x}`,
`darwin/{amd64,arm64}`, `windows/{amd64,arm64,386}`, `freebsd/{amd64,arm64}`, `openbsd/amd64`.

Pushing a `vX.Y.Z` tag triggers the release workflow.

---

## Known limitations

- **WSL2 exits without *mirrored* networking are relay‑only.** WSL2 in default NAT mode adds its own
  NAT layer, blocking ICE hole‑punching. For P2P, use `networkingMode=mirrored` (Win11 22H2+) or a
  non‑WSL2 Linux host. If mirrored mode is unavailable, accept the relay (`-direct-mode p2p-only`).
- **Windows exit requires WinNAT** (Pro/Enterprise/Server). If absent the exit stops with an error.
  `-isolate` is a no‑op on Windows. Test egress with `curl`, not `ping`.
- **macOS exit is not supported** (no iptables equivalent for NAT + isolation).
