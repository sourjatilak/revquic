// SPDX-License-Identifier: GPL-3.0-or-later

// Package adminserver implements the broker admin API (spec/api/admin-openapi.yaml) over
// net/http, backed by the user store, admin store, event bus, and a live NodeProvider. It also
// serves an embedded static admin dashboard. Uses Go 1.22 method-aware ServeMux routing.
package adminserver

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sourjatilak/revquic/internal/adminapi"
	"github.com/sourjatilak/revquic/internal/adminstore"
	"github.com/sourjatilak/revquic/internal/auth"
	"github.com/sourjatilak/revquic/internal/events"
	"github.com/sourjatilak/revquic/internal/qos"
	"github.com/sourjatilak/revquic/internal/userstore"
)

//go:embed web
var webFS embed.FS

// NodeProvider exposes the broker's live exit-node and session state to the admin API.
type NodeProvider interface {
	ListNodes() []adminapi.DeviceView
	ListSessions() []adminapi.Session
}

// QoSProvider exposes the broker's quality-of-service tracker: per-exit load, per-session live
// stats, and the event history. Optional — when nil the /qos endpoints return empty arrays.
type QoSProvider interface {
	QoSExits() []qos.ExitStat
	QoSSessions() []qos.SessionStat
	QoSHistory(limit int) []qos.Event
}

// Server bundles the admin API dependencies.
type Server struct {
	Users  userstore.Store
	Admins adminstore.Store
	Bus    *events.Bus
	Nodes  NodeProvider
	QoS    QoSProvider
	// BootstrapToken, if set, is accepted in addition to login sessions (CI/smoke convenience).
	BootstrapToken string

	once     sync.Once
	sessions *sessionMgr
}

func (s *Server) init() {
	s.once.Do(func() { s.sessions = newSessionMgr() })
}

// Handler returns the mounted admin API router + embedded dashboard.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	a := func(h http.HandlerFunc) http.HandlerFunc { return auth.RequireToken(s.validate, h) }

	mux.HandleFunc("POST /api/v1/admin/login", s.login)
	mux.HandleFunc("GET /api/v1/users", a(s.listUsers))
	mux.HandleFunc("POST /api/v1/users", a(s.createUser))
	mux.HandleFunc("GET /api/v1/users/{id}", a(s.getUser))
	mux.HandleFunc("PATCH /api/v1/users/{id}", a(s.updateUser))
	mux.HandleFunc("DELETE /api/v1/users/{id}", a(s.deleteUser))
	mux.HandleFunc("GET /api/v1/regions", a(s.listRegions))
	mux.HandleFunc("GET /api/v1/nodes", a(s.listNodes))
	mux.HandleFunc("GET /api/v1/nodes/{id}", a(s.getNode))
	mux.HandleFunc("GET /api/v1/sessions", a(s.listSessions))
	mux.HandleFunc("GET /api/v1/qos/exits", a(s.qosExits))
	mux.HandleFunc("GET /api/v1/qos/sessions", a(s.qosSessions))
	mux.HandleFunc("GET /api/v1/qos/history", a(s.qosHistory))
	mux.HandleFunc("GET /api/v1/events", a(s.events))

	// Embedded static dashboard at "/" (API routes above are more specific and win).
	if sub, err := fs.Sub(webFS, "web"); err == nil {
		mux.Handle("GET /", http.FileServer(http.FS(sub)))
	}
	return mux
}

func (s *Server) validate(token string) bool {
	if s.BootstrapToken != "" && auth.ConstantEqual(token, s.BootstrapToken) {
		return true
	}
	return s.sessions.valid(token)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, adminapi.Error{Code: errCode, Message: msg})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	if s.Admins == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "no admin store configured")
		return
	}
	pub, err := s.Admins.Verify(req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "bad credentials")
		return
	}
	tok, exp := s.sessions.issue(pub.Role)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "role": pub.Role, "expiresAt": exp})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, s.Users.List(q.Get("region"), q.Get("status")))
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in adminapi.UserCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "validation_error", "invalid body")
		return
	}
	// If no credential is supplied, generate a unique client token server-side. It is returned
	// exactly once in this response (only its HMAC is stored), so the admin can copy it.
	if in.Credential == "" {
		in.Credential = randomToken()
	}
	u, err := s.Users.Create(in)
	if err == userstore.ErrConflict {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	// Echo the effective token once (generated or supplied) alongside the created user.
	writeJSON(w, http.StatusCreated, struct {
		adminapi.User
		Token string `json:"token,omitempty"`
	}{User: u, Token: in.Credential})
	return
}

// randomToken returns a 24-byte URL-safe random client token.
func randomToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.Users.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var in adminapi.UserUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "validation_error", "invalid body")
		return
	}
	u, err := s.Users.Update(r.PathValue("id"), in)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.Users.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	nodes := s.Nodes.ListNodes()
	if region != "" {
		filtered := make([]adminapi.DeviceView, 0, len(nodes))
		for _, n := range nodes {
			if n.Region == region {
				filtered = append(filtered, n)
			}
		}
		nodes = filtered
	}
	writeJSON(w, http.StatusOK, nodes)
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, n := range s.Nodes.ListNodes() {
		if n.NodeID == id {
			writeJSON(w, http.StatusOK, n)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not_found", "node not found")
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Nodes.ListSessions())
}

func (s *Server) qosExits(w http.ResponseWriter, r *http.Request) {
	if s.QoS == nil {
		writeJSON(w, http.StatusOK, []qos.ExitStat{})
		return
	}
	writeJSON(w, http.StatusOK, s.QoS.QoSExits())
}

func (s *Server) qosSessions(w http.ResponseWriter, r *http.Request) {
	if s.QoS == nil {
		writeJSON(w, http.StatusOK, []qos.SessionStat{})
		return
	}
	writeJSON(w, http.StatusOK, s.QoS.QoSSessions())
}

func (s *Server) qosHistory(w http.ResponseWriter, r *http.Request) {
	if s.QoS == nil {
		writeJSON(w, http.StatusOK, []qos.Event{})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, s.QoS.QoSHistory(limit))
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	type agg struct{ online, users int }
	m := map[string]*agg{}
	for _, n := range s.Nodes.ListNodes() {
		a := m[n.Region]
		if a == nil {
			a = &agg{}
			m[n.Region] = a
		}
		if n.Status == adminapi.NodeOnline {
			a.online++
		}
		a.users += n.ActiveUsers
	}
	out := make([]adminapi.Region, 0, len(m))
	for code, a := range m {
		out = append(out, adminapi.Region{Code: code, Name: code, NodeCount: a.online, OnlineNodes: a.online, ActiveUsers: a.users})
	}
	writeJSON(w, http.StatusOK, out)
}

// events streams the live feed as Server-Sent Events: a Snapshot, then deltas from the bus.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := s.Bus.Subscribe()
	defer cancel()

	snap := adminapi.Event{
		Type:     adminapi.EvSnapshot,
		TS:       time.Now().UTC(),
		Snapshot: &adminapi.Snapshot{Nodes: s.Nodes.ListNodes(), Sessions: s.Nodes.ListSessions()},
	}
	if !writeSSE(w, flusher, snap) {
		return
	}
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !writeSSE(w, flusher, ev) {
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, ev adminapi.Event) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return true
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return false
	}
	if _, err := w.Write(b); err != nil {
		return false
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return false
	}
	f.Flush()
	return true
}

// --- session manager ---

type sessionMgr struct {
	mu sync.Mutex
	m  map[string]sessionEntry
}

type sessionEntry struct {
	role string
	exp  time.Time
}

func newSessionMgr() *sessionMgr { return &sessionMgr{m: map[string]sessionEntry{}} }

func (sm *sessionMgr) issue(role string) (string, time.Time) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	exp := time.Now().Add(8 * time.Hour).UTC()
	sm.mu.Lock()
	sm.m[tok] = sessionEntry{role: role, exp: exp}
	sm.mu.Unlock()
	return tok, exp
}

func (sm *sessionMgr) valid(tok string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	e, ok := sm.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(e.exp) {
		delete(sm.m, tok)
		return false
	}
	return true
}
