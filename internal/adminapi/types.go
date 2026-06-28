// SPDX-License-Identifier: GPL-3.0-or-later

// Package adminapi holds the data-transfer types for the broker admin API. They mirror the
// schemas in spec/api/admin-openapi.yaml. This package is a leaf (no internal deps) so it can
// be imported by events, userstore, and the admin server without cycles.
package adminapi

import "time"

// User statuses.
const (
	UserEnabled  = "enabled"
	UserDisabled = "disabled"
)

// Node statuses.
const (
	NodeOnline   = "online"
	NodeDraining = "draining"
	NodeDown     = "down"
)

// User is a persisted VPN user. AllowedRegions gates which regions the user may egress through
// (["*"] = any, [] = none).
type User struct {
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	AllowedRegions     []string  `json:"allowedRegions"`
	Status             string    `json:"status"`
	ActiveSessionCount int       `json:"activeSessionCount"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// UserCreate is the create-user request body.
type UserCreate struct {
	Username       string   `json:"username"`
	Credential     string   `json:"credential"`
	AllowedRegions []string `json:"allowedRegions"`
	Status         string   `json:"status"`
}

// UserUpdate is the patch-user request body. Nil fields are left unchanged.
type UserUpdate struct {
	AllowedRegions *[]string `json:"allowedRegions,omitempty"`
	Status         *string   `json:"status,omitempty"`
	Credential     *string   `json:"credential,omitempty"`
}

// Region summarizes a region's live counts.
type Region struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	NodeCount   int    `json:"nodeCount"`
	OnlineNodes int    `json:"onlineNodes"`
	ActiveUsers int    `json:"activeUsers"`
}

// DeviceConfigSummary is the basic config view of an exit node.
type DeviceConfigSummary struct {
	Region        string `json:"region"`
	Capacity      int    `json:"capacity"`
	VPNType       string `json:"vpnType"`
	DataplaneMode string `json:"dataplaneMode"`
	UplinkIface   string `json:"uplinkIface,omitempty"`
	Version       string `json:"version,omitempty"`
}

// DeviceView is the live, derived view of a connected exit node (not persisted).
type DeviceView struct {
	NodeID        string              `json:"nodeId"`
	Name          string              `json:"name,omitempty"`
	Region        string              `json:"region"`
	Status        string              `json:"status"`
	Version       string              `json:"version,omitempty"`
	NATType       string              `json:"natType,omitempty"`
	System        string              `json:"system,omitempty"`
	PublicAddr    string              `json:"publicAddr,omitempty"`
	Capacity      int                 `json:"capacity"`
	ActiveUsers   int                 `json:"activeUsers"`
	LoadPct       float64             `json:"loadPct"`
	CPUPct        float64             `json:"cpuPct,omitempty"`
	MemPct        float64             `json:"memPct,omitempty"`
	DiskPct       float64             `json:"diskPct,omitempty"`
	ThroughputBps float64             `json:"throughputBps,omitempty"`
	RTTms         int                 `json:"rttMs,omitempty"`
	ConnectedAt   time.Time           `json:"connectedAt"`
	LastSeen      time.Time           `json:"lastSeen"`
	Config        DeviceConfigSummary `json:"config"`
}

// Session is an active client session.
type Session struct {
	SessionID     string    `json:"sessionId"`
	UserID        string    `json:"userId,omitempty"`
	Username      string    `json:"username,omitempty"`
	Name          string    `json:"name,omitempty"`
	NodeID        string    `json:"nodeId"`
	Region        string    `json:"region"`
	Mode          string    `json:"mode"`
	State         string    `json:"state"`
	StartedAt     time.Time `json:"startedAt"`
	BytesUp       uint64    `json:"bytesUp"`
	BytesDown     uint64    `json:"bytesDown"`
	ThroughputBps float64   `json:"throughputBps,omitempty"`
	RTTms         int       `json:"rttMs,omitempty"`
	Host          string    `json:"host,omitempty"`
	TunName       string    `json:"tunName,omitempty"`
	OS            string    `json:"os,omitempty"`
	CPUPct        float64   `json:"cpuPct,omitempty"`
	MemPct        float64   `json:"memPct,omitempty"`
	DiskPct       float64   `json:"diskPct,omitempty"`
}

// Event types for the /events live feed.
const (
	EvSnapshot         = "Snapshot"
	EvNodeConnected    = "NodeConnected"
	EvNodeUpdated      = "NodeUpdated"
	EvNodeDisconnected = "NodeDisconnected"
	EvSessionStarted   = "SessionStarted"
	EvSessionEnded     = "SessionEnded"
)

// Snapshot is the initial full state sent on subscribe.
type Snapshot struct {
	Nodes    []DeviceView `json:"nodes"`
	Sessions []Session    `json:"sessions"`
}

// Event is one live-feed message. The Type selects which payload field is populated.
type Event struct {
	Type     string      `json:"type"`
	TS       time.Time   `json:"ts"`
	Snapshot *Snapshot   `json:"snapshot,omitempty"`
	Node     *DeviceView `json:"node,omitempty"`
	NodeID   string      `json:"nodeId,omitempty"`
	Session  *Session    `json:"session,omitempty"`
}

// Error is the standard error body.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
