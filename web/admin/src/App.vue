<script setup>
import { ref, reactive, computed, watch, onUnmounted } from 'vue'

const token = ref('')
const role = ref('')
const loginErr = ref('')
const username = ref('admin')
const password = ref('admin')

const nodes = reactive(new Map())     // nodeId -> DeviceView
const sessions = reactive(new Map())  // sessionId -> Session
const users = ref([])
const nu = reactive({ username: '', regions: 'us-west', credential: '' })
const newToken = ref('')   // last generated/created token, shown once
const createErr = ref('')
const live = ref(false)
let abort = null

// Auto-refresh: poll /nodes + /sessions so live metrics (bandwidth, latency, CPU/RAM/disk) update
// even though the SSE feed only pushes connect/disconnect + node-status events.
const refreshSecs = ref(5)
let pollTimer = null
const openDevices = reactive(new Set())   // nodeIds with the detail row expanded
const openSessions = reactive(new Set())  // sessionIds with the detail row expanded

const auth = () => ({ Authorization: 'Bearer ' + token.value })

async function login() {
  loginErr.value = ''
  const r = await fetch('/api/v1/admin/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: username.value, password: password.value }),
  })
  if (!r.ok) { loginErr.value = 'invalid credentials'; return }
  const j = await r.json()
  token.value = j.token; role.value = j.role
  await refreshUsers()
  streamEvents()
  setupPolling()
}

// pollNow refreshes nodes + sessions from the metric-merged REST endpoints.
async function pollNow() {
  if (!token.value) return
  try {
    const [nr, sr] = await Promise.all([
      fetch('/api/v1/nodes', { headers: auth() }),
      fetch('/api/v1/sessions', { headers: auth() }),
    ])
    if (nr.ok) {
      const list = (await nr.json()) || []
      nodes.clear(); list.forEach(n => nodes.set(n.nodeId, n))
    }
    if (sr.ok) {
      const list = (await sr.json()) || []
      sessions.clear(); list.forEach(s => sessions.set(s.sessionId, s))
    }
  } catch (e) { /* transient; next tick retries */ }
}

function setupPolling() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null }
  if (refreshSecs.value > 0) {
    pollNow()
    pollTimer = setInterval(pollNow, refreshSecs.value * 1000)
  }
}

function toggleDevice(id) { openDevices.has(id) ? openDevices.delete(id) : openDevices.add(id) }
function toggleSession(id) { openSessions.has(id) ? openSessions.delete(id) : openSessions.add(id) }

async function refreshUsers() {
  const r = await fetch('/api/v1/users', { headers: auth() })
  users.value = (await r.json()) || []
}

async function createUser() {
  createErr.value = ''; newToken.value = ''
  if (!nu.username.trim()) { createErr.value = 'username required'; return }
  const body = {
    username: nu.username.trim(),
    allowedRegions: nu.regions.split(',').map(s => s.trim()).filter(Boolean),
    status: 'enabled',
  }
  if (nu.credential.trim()) body.credential = nu.credential.trim()
  const r = await fetch('/api/v1/users', {
    method: 'POST', headers: { ...auth(), 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!r.ok) {
    const j = await r.json().catch(() => ({}))
    createErr.value = j.message || ('error ' + r.status)
    return
  }
  const j = await r.json()
  newToken.value = j.token || ''   // shown once; only the hash is stored server-side
  nu.username = ''; nu.credential = ''
  await refreshUsers()
}

function copyToken() { if (newToken.value) navigator.clipboard?.writeText(newToken.value) }

function applyEvent(ev) {
  switch (ev.type) {
    case 'Snapshot':
      nodes.clear(); sessions.clear()
      ;(ev.snapshot.nodes || []).forEach(n => nodes.set(n.nodeId, n))
      ;(ev.snapshot.sessions || []).forEach(s => sessions.set(s.sessionId, s))
      break
    case 'NodeConnected':
    case 'NodeUpdated': nodes.set(ev.node.nodeId, ev.node); break
    case 'NodeDisconnected': nodes.delete(ev.nodeId); break
    case 'SessionStarted':
      sessions.set(ev.session.sessionId, ev.session); bump(ev.session.nodeId, +1); break
    case 'SessionEnded':
      sessions.delete(ev.session.sessionId); bump(ev.session.nodeId, -1); break
  }
}
function bump(nodeId, d) { const n = nodes.get(nodeId); if (n) { n.activeUsers = Math.max(0, (n.activeUsers || 0) + d); nodes.set(nodeId, { ...n }) } }

async function streamEvents() {
  while (token.value) {
    try {
      abort = new AbortController()
      const r = await fetch('/api/v1/events', { headers: auth(), signal: abort.signal })
      if (!r.ok || !r.body) throw new Error('events ' + r.status)
      live.value = true
      const reader = r.body.getReader(); const dec = new TextDecoder(); let buf = ''
      for (;;) {
        const { value, done } = await reader.read(); if (done) break
        buf += dec.decode(value, { stream: true })
        let i
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const block = buf.slice(0, i); buf = buf.slice(i + 2)
          for (const line of block.split('\n'))
            if (line.startsWith('data: ')) { try { applyEvent(JSON.parse(line.slice(6))) } catch (e) {} }
        }
      }
    } catch (e) { /* reconnect */ }
    live.value = false
    await new Promise(r => setTimeout(r, 1500))
  }
}

const nodeList = computed(() => Array.from(nodes.values()))
const sessionList = computed(() => Array.from(sessions.values()))

// Group active sessions under their exit node for the realtime "Live connections" view.
const connectionsByExit = computed(() => {
  const byNode = {}
  for (const s of sessionList.value) {
    (byNode[s.nodeId] || (byNode[s.nodeId] = [])).push(s)
  }
  const groups = nodeList.value.map(n => ({ node: n, sessions: byNode[n.nodeId] || [] }))
  for (const id of Object.keys(byNode)) {
    if (!nodes.has(id)) groups.push({ node: { nodeId: id, region: '—', system: '—', capacity: 0 }, sessions: byNode[id] })
  }
  return groups
})

function fmtBytes(n) {
  n = n || 0
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++ }
  return n.toFixed(i ? 1 : 0) + ' ' + u[i]
}
function fmtBps(b) { return b ? fmtBytes(b) + '/s' : '—' }
function fmtRtt(ms) { return ms ? ms + ' ms' : '—' }
function fmtPct(p) { return (p || p === 0) && p > 0 ? p.toFixed(0) + '%' : '—' }
const totalUsers = computed(() => nodeList.value.reduce((a, n) => a + (n.activeUsers || 0), 0))

watch(refreshSecs, setupPolling)
onUnmounted(() => { abort && abort.abort(); if (pollTimer) clearInterval(pollTimer) })
</script>

<template>
  <header>
    <h1>Revquic Admin</h1>
    <span class="muted" v-if="role">signed in ({{ role }})</span>
    <span class="live" :class="{ on: live }">{{ live ? '● live' : '○ offline' }}</span>
    <label v-if="token" class="refresh">refresh
      <select v-model.number="refreshSecs">
        <option :value="0">Off</option>
        <option :value="2">2s</option>
        <option :value="5">5s</option>
        <option :value="10">10s</option>
        <option :value="30">30s</option>
      </select>
    </label>
  </header>

  <main v-if="!token" class="card login">
    <h2>Sign in</h2>
    <input v-model="username" placeholder="username" />
    <input v-model="password" type="password" placeholder="password" @keyup.enter="login" />
    <button @click="login">Sign in</button>
    <span class="muted">{{ loginErr }}</span>
  </main>

  <main v-else>
    <div class="card">
      <strong>Overview</strong>
      <span class="muted stats">{{ nodes.size }} node(s) · {{ totalUsers }} user(s) · {{ sessions.size }} session(s)</span>
    </div>

    <div class="card">
      <strong>Devices</strong>
      <table>
        <thead><tr><th>status</th><th>device</th><th>region</th><th>system</th><th>NAT</th><th>addr</th><th>users</th><th></th></tr></thead>
        <tbody>
          <template v-for="n in nodeList" :key="n.nodeId">
          <tr>
            <td><span class="dot" :class="n.status"></span>{{ n.status }}</td>
            <td><template v-if="n.name"><strong>{{ n.name }}</strong> <span class="muted">{{ n.nodeId }}</span></template><template v-else>{{ n.nodeId }}</template></td><td>{{ n.region }}</td><td>{{ n.system || '—' }}</td><td>{{ n.natType || '—' }}</td>
            <td>{{ n.publicAddr || '—' }}</td>
            <td><span class="badge">{{ n.activeUsers || 0 }} / {{ n.capacity || 0 }}</span></td>
            <td><button class="detail" :class="{ open: openDevices.has(n.nodeId) }" @click="toggleDevice(n.nodeId)" title="details">▸</button></td>
          </tr>
          <tr v-if="openDevices.has(n.nodeId)" class="detailrow">
            <td colspan="8">
              <div class="metrics">
                <span><b>CPU</b> {{ fmtPct(n.cpuPct) }}</span>
                <span><b>RAM</b> {{ fmtPct(n.memPct) }}</span>
                <span><b>Disk</b> {{ fmtPct(n.diskPct) }}</span>
                <span><b>Connections</b> {{ n.activeUsers || 0 }} / {{ n.capacity || 0 }}</span>
                <span><b>Bandwidth</b> {{ fmtBps(n.throughputBps) }}</span>
              </div>
            </td>
          </tr>
          </template>
        </tbody>
      </table>
      <p v-if="!nodes.size" class="muted">No exit nodes connected.</p>
    </div>

    <div class="card">
      <strong>Live connections</strong>
      <span class="muted stats">realtime — which users are on each exit</span>

      <div v-for="g in connectionsByExit" :key="g.node.nodeId" class="exitgroup">
        <div class="exithdr">
          <span class="dot online"></span>
          <strong>{{ g.node.name || g.node.nodeId }}</strong>
          <span v-if="g.node.name" class="muted">{{ g.node.nodeId }}</span>
          <span class="muted">{{ g.node.region }} · {{ g.node.system || '—' }}</span>
          <span class="badge" style="margin-left:auto">{{ g.sessions.length }} / {{ g.node.capacity || 0 }} connected</span>
        </div>
        <table v-if="g.sessions.length">
          <thead><tr>
            <th>user</th><th>client host</th><th>system</th><th>path</th>
            <th>latency</th><th>↑ up</th><th>↓ down</th><th>rate</th><th>state</th><th></th>
          </tr></thead>
          <tbody>
            <template v-for="s in g.sessions" :key="s.sessionId">
            <tr>
              <td><template v-if="s.name"><strong>{{ s.name }}</strong> <span class="muted">{{ s.username || s.userId || '' }}</span></template><template v-else>{{ s.username || s.userId || '—' }}</template></td>
              <td>{{ s.host || '—' }}</td>
              <td>{{ s.os || '—' }}</td>
              <td><span class="badge" :class="{ direct: s.mode === 'direct' }">{{ s.mode || 'relay' }}</span></td>
              <td>{{ fmtRtt(s.rttMs) }}</td>
              <td>{{ fmtBytes(s.bytesUp) }}</td>
              <td>{{ fmtBytes(s.bytesDown) }}</td>
              <td>{{ fmtBps(s.throughputBps) }}</td>
              <td><span class="dot" :class="{ online: s.state === 'active' }"></span>{{ s.state || 'active' }}</td>
              <td><button class="detail" :class="{ open: openSessions.has(s.sessionId) }" @click="toggleSession(s.sessionId)" title="details">▸</button></td>
            </tr>
            <tr v-if="openSessions.has(s.sessionId)" class="detailrow">
              <td colspan="10">
                <div class="metrics">
                  <span><b>Client</b> {{ s.username || s.userId || '—' }} ({{ s.host || '—' }})</span>
                  <span><b>Exit host</b> {{ g.node.nodeId }}</span>
                  <span><b>Path</b> {{ s.mode || 'relay' }}</span>
                  <span><b>Latency</b> {{ fmtRtt(s.rttMs) }}</span>
                  <span><b>Bandwidth</b> ↑ {{ fmtBytes(s.bytesUp) }} · ↓ {{ fmtBytes(s.bytesDown) }} · {{ fmtBps(s.throughputBps) }}</span>
                  <span><b>Client CPU</b> {{ fmtPct(s.cpuPct) }}</span>
                  <span><b>Client RAM</b> {{ fmtPct(s.memPct) }}</span>
                  <span><b>Client Disk</b> {{ fmtPct(s.diskPct) }}</span>
                  <span><b>System</b> {{ s.os || '—' }}</span>
                  <span><b>TUN</b> {{ s.tunName || '—' }}</span>
                  <span><b>Region</b> {{ s.region || g.node.region }}</span>
                </div>
              </td>
            </tr>
            </template>
          </tbody>
        </table>
        <p v-else class="muted">No clients connected to this exit.</p>
      </div>
      <p v-if="!connectionsByExit.length" class="muted">No exit nodes connected.</p>
    </div>

    <div class="card">
      <strong>Users</strong>

      <div class="addform">
        <input v-model="nu.username" placeholder="username" @keyup.enter="createUser" />
        <input v-model="nu.regions" placeholder="regions (comma-sep, or *)" />
        <input v-model="nu.credential" placeholder="token (blank = auto-generate)" />
        <button @click="createUser">Add user</button>
      </div>
      <p v-if="createErr" class="err">{{ createErr }}</p>
      <div v-if="newToken" class="token-reveal">
        <span>Client token (copy now — shown once):</span>
        <code>{{ newToken }}</code>
        <button @click="copyToken">copy</button>
      </div>

      <table>
        <thead><tr><th>username</th><th>regions</th><th>status</th></tr></thead>
        <tbody>
          <tr v-for="u in users" :key="u.id">
            <td>{{ u.username }}</td><td>{{ (u.allowedRegions || []).join(', ') }}</td><td>{{ u.status }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </main>
</template>

<style>
:root { --bg:#0f1419; --panel:#1a2129; --line:#2a333d; --fg:#e6edf3; --muted:#8b98a5; --green:#2ea043; --accent:#388bfd; }
body { margin:0; font:14px/1.5 system-ui,sans-serif; background:var(--bg); color:var(--fg); }
header { display:flex; gap:16px; align-items:center; padding:12px 20px; border-bottom:1px solid var(--line); }
header h1 { font-size:16px; margin:0; }
.live { margin-left:auto; color:var(--muted); } .live.on { color:var(--green); }
main { padding:20px; max-width:1000px; margin:0 auto; }
.card { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:16px; margin-bottom:16px; }
table { width:100%; border-collapse:collapse; margin-top:8px; }
th,td { text-align:left; padding:8px 10px; border-bottom:1px solid var(--line); }
th { color:var(--muted); font-size:12px; text-transform:uppercase; }
.muted { color:var(--muted); }
.stats { display:block; text-align:right; margin-top:4px; }
.badge { background:#23303b; border-radius:10px; padding:1px 8px; }
.badge.direct { background:#10331c; color:var(--green); }
.exitgroup { margin-top:12px; }
.exithdr { display:flex; align-items:center; gap:8px; padding:6px 0; border-bottom:1px solid var(--line); }
.refresh { color:var(--muted); font-size:12px; display:flex; align-items:center; gap:4px; }
.refresh select { background:#0d1117; color:var(--fg); border:1px solid var(--line); border-radius:6px; padding:2px 4px; }
.detail { background:none; border:1px solid var(--line); color:var(--muted); border-radius:6px; padding:0 6px; cursor:pointer; transition:transform .1s; }
.detail.open { transform:rotate(90deg); color:var(--accent); }
.detailrow td { background:#0d1117; }
.metrics { display:flex; flex-wrap:wrap; gap:16px; padding:4px 2px; }
.metrics b { color:var(--muted); font-weight:600; margin-right:4px; }
.dot { display:inline-block; width:9px; height:9px; border-radius:50%; margin-right:6px; background:var(--muted); }
.dot.online { background:var(--green); }
.login { max-width:320px; } .login input { display:block; width:100%; margin:6px 0; padding:8px; background:#0d1117; color:var(--fg); border:1px solid var(--line); border-radius:6px; }
button { padding:8px 12px; border-radius:6px; border:1px solid var(--accent); background:var(--accent); color:#fff; cursor:pointer; }
.addform { display:flex; gap:8px; margin:8px 0; flex-wrap:wrap; }
.addform input { flex:1; min-width:130px; padding:8px; background:#0d1117; color:var(--fg); border:1px solid var(--line); border-radius:6px; }
.err { color:#f85149; margin:4px 0; }
.token-reveal { margin:8px 0; padding:10px; background:#0d1117; border:1px solid var(--green); border-radius:6px; display:flex; align-items:center; gap:8px; flex-wrap:wrap; }
.token-reveal code { color:var(--green); word-break:break-all; }
</style>
