// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"runtime"
	"sort"
	"time"
)

// statusInfo is the exit's identity/summary for the status page.
type statusInfo struct {
	NodeID  string  `json:"nodeId"`
	Name    string  `json:"name,omitempty"`
	Region  string  `json:"region"`
	OS      string  `json:"os"`
	Clients int     `json:"clients"`
	Parked  int     `json:"parked"`
	BpsUp   float64 `json:"bpsUp"`
	BpsDown float64 `json:"bpsDown"`
}

// statusSession is the per-client view rendered by the status page.
type statusSession struct {
	SessionID   uint64  `json:"sessionId"`
	ClientIP    string  `json:"clientIp"`
	Mode        string  `json:"mode"`  // "direct" | "relay"
	State       string  `json:"state"` // "active" | "parked"
	LatencyMs   int     `json:"latencyMs"`
	BytesUp     uint64  `json:"bytesUp"`
	BytesDown   uint64  `json:"bytesDown"`
	BpsUp       float64 `json:"bpsUp"`
	BpsDown     float64 `json:"bpsDown"`
	UsingTurn   bool    `json:"usingTurn"`
	DurationSec int     `json:"durationSec"`
}

// sampleThroughput recomputes per-session up/down throughput every 2s with light EWMA smoothing, so
// the status page shows current speed independent of how often the browser polls.
func (e *exit) sampleThroughput() {
	const dt = 2 * time.Second
	type prev struct{ up, down uint64 }
	last := map[uint64]prev{}
	t := time.NewTicker(dt)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
		}
		e.mu.RLock()
		snap := make(map[uint64]*exitSession, len(e.sessions))
		for sid, es := range e.sessions {
			snap[sid] = es
		}
		e.mu.RUnlock()

		seen := map[uint64]bool{}
		for sid, es := range snap {
			seen[sid] = true
			down, _, _ := es.sess.Stats()
			up := es.bytesUp.Load()
			p := last[sid]
			last[sid] = prev{up: up, down: down}
			// First observation: no rate yet.
			if p.up == 0 && p.down == 0 {
				continue
			}
			upBps := float64(up-p.up) / dt.Seconds()
			downBps := float64(down-p.down) / dt.Seconds()
			es.bpsUpBits.Store(math.Float64bits(ewma(math.Float64frombits(es.bpsUpBits.Load()), upBps)))
			es.bpsDownBits.Store(math.Float64bits(ewma(math.Float64frombits(es.bpsDownBits.Load()), downBps)))
		}
		// Forget sessions that ended.
		for sid := range last {
			if !seen[sid] {
				delete(last, sid)
			}
		}
	}
}

// ewma applies a simple exponential moving average (alpha=0.5) for a stable-but-responsive rate.
func ewma(old, sample float64) float64 {
	if old == 0 {
		return sample
	}
	return 0.5*old + 0.5*sample
}

// statusViews snapshots the current sessions for the status page.
func (e *exit) statusViews() (statusInfo, []statusSession) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]statusSession, 0, len(e.sessions))
	var totUp, totDown float64
	parked := 0
	for sid, es := range e.sessions {
		down, _, _ := es.sess.Stats()
		mode := "relay"
		if es.sess.IsDirect() {
			mode = "direct"
		}
		state := "active"
		if es.suspended {
			state = "parked"
			parked++
		}
		bpsUp := math.Float64frombits(es.bpsUpBits.Load())
		bpsDown := math.Float64frombits(es.bpsDownBits.Load())
		totUp += bpsUp
		totDown += bpsDown
		out = append(out, statusSession{
			SessionID:   sid,
			ClientIP:    es.clientIP,
			Mode:        mode,
			State:       state,
			LatencyMs:   es.rtt.Millis(),
			BytesUp:     es.bytesUp.Load(),
			BytesDown:   down,
			BpsUp:       bpsUp,
			BpsDown:     bpsDown,
			UsingTurn:   es.turn != "",
			DurationSec: int(time.Since(es.startedAt).Seconds()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	info := statusInfo{
		NodeID: e.nodeID, Name: e.displayName, Region: e.region,
		OS: runtime.GOOS + "/" + runtime.GOARCH, Clients: len(out) - parked, Parked: parked, BpsUp: totUp, BpsDown: totDown,
	}
	return info, out
}

// statusHandler builds the status UI HTTP handler (extracted so it can be tested via httptest).
func (e *exit) statusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		info, sessions := e.statusViews()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"info": info, "sessions": sessions})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(statusHTML))
	})
	return mux
}

// serveStatus runs the local, unauthenticated status web UI. Bind to localhost only.
func (e *exit) serveStatus(addr string) error {
	log.Printf("status web UI at http://%s (no auth; localhost only)", addr)
	srv := &http.Server{Addr: addr, Handler: e.statusHandler(), ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}

const statusHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Revquic Exit — status</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 1.5rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  .sub { color: #888; font-size: .85rem; margin-bottom: 1rem; }
  .cards { display: flex; gap: .75rem; flex-wrap: wrap; margin-bottom: 1rem; }
  .card { border: 1px solid #8884; border-radius: 8px; padding: .6rem .9rem; min-width: 120px; }
  .card .n { font-size: 1.4rem; font-weight: 600; }
  .card .l { color: #888; font-size: .75rem; text-transform: uppercase; letter-spacing: .04em; }
  table { border-collapse: collapse; width: 100%; font-size: .9rem; }
  th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #8883; white-space: nowrap; }
  th { color: #888; font-weight: 600; font-size: .78rem; text-transform: uppercase; letter-spacing: .03em; }
  .badge { padding: .1rem .45rem; border-radius: 999px; font-size: .75rem; font-weight: 600; background:#8883; }
  .badge.direct { background:#2e7d3233; color:#2e7d32; }
  .badge.relay  { background:#1565c033; color:#1565c0; }
  .mono { font-variant-numeric: tabular-nums; }
  .empty { color:#888; padding: 1rem 0; }
  .off { color:#c62828; }
</style></head>
<body>
  <h1>Revquic Exit <span id="name"></span></h1>
  <div class="sub" id="sub">connecting…</div>
  <div class="cards">
    <div class="card"><div class="n" id="c-clients">–</div><div class="l">clients</div></div>
    <div class="card"><div class="n" id="c-parked">–</div><div class="l">parked</div></div>
    <div class="card"><div class="n mono" id="c-down">–</div><div class="l">total ↓ down</div></div>
    <div class="card"><div class="n mono" id="c-up">–</div><div class="l">total ↑ up</div></div>
  </div>
  <table>
    <thead><tr>
      <th>client</th><th>mode</th><th>latency</th><th>↓ down</th><th>↑ up</th>
      <th>↓ rate</th><th>↑ rate</th><th>volume</th><th>up&nbsp;for</th>
    </tr></thead>
    <tbody id="rows"></tbody>
  </table>
  <p class="empty" id="empty" style="display:none">No clients connected.</p>
<script>
function bytes(n){ if(!n) return "0 B"; const u=["B","KB","MB","GB","TB"]; let i=0; while(n>=1024&&i<u.length-1){n/=1024;i++;} return n.toFixed(i?1:0)+" "+u[i]; }
function bps(n){ if(!n||n<1) return "—"; return bytes(n)+"/s"; }
function dur(s){ if(s<60) return s+"s"; const m=Math.floor(s/60); if(m<60) return m+"m "+(s%60)+"s"; const h=Math.floor(m/60); return h+"h "+(m%60)+"m"; }
async function tick(){
  try {
    const r = await fetch("/api/status"); const d = await r.json();
    const info = d.info||{}; const ss = d.sessions||[];
    document.getElementById("name").textContent = info.name ? "· "+info.name : "";
    document.getElementById("sub").textContent = (info.nodeId||"?")+"  ·  region "+(info.region||"?")+"  ·  "+(info.os||"");
    document.getElementById("c-clients").textContent = info.clients||0;
    document.getElementById("c-parked").textContent = info.parked||0;
    document.getElementById("c-down").textContent = bps(info.bpsDown);
    document.getElementById("c-up").textContent = bps(info.bpsUp);
    const rows = document.getElementById("rows"); rows.innerHTML = "";
    document.getElementById("empty").style.display = ss.length ? "none" : "block";
    for (const s of ss) {
      const tr = document.createElement("tr");
      if (s.state === "parked") tr.style.opacity = "0.55";
      const turn = s.usingTurn ? ' <span class="badge">TURN</span>' : '';
      const parked = s.state === "parked" ? ' <span class="badge">parked (resumable)</span>' : '';
      tr.innerHTML =
        '<td class="mono">'+s.clientIp+'</td>'+
        '<td><span class="badge '+s.mode+'">'+s.mode+'</span>'+turn+parked+'</td>'+
        '<td class="mono">'+(s.latencyMs?s.latencyMs+' ms':'—')+'</td>'+
        '<td class="mono">'+bytes(s.bytesDown)+'</td>'+
        '<td class="mono">'+bytes(s.bytesUp)+'</td>'+
        '<td class="mono">'+bps(s.bpsDown)+'</td>'+
        '<td class="mono">'+bps(s.bpsUp)+'</td>'+
        '<td class="mono">'+bytes(s.bytesDown+s.bytesUp)+'</td>'+
        '<td class="mono">'+dur(s.durationSec)+'</td>';
      rows.appendChild(tr);
    }
  } catch(e) {
    document.getElementById("sub").innerHTML = '<span class="off">exit not reachable — is it running?</span>';
  }
}
tick(); setInterval(tick, 2000);
</script>
</body></html>`
