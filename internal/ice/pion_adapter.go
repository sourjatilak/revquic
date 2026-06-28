// SPDX-License-Identifier: GPL-3.0-or-later

// pion_adapter.go implements ice.Agent on top of github.com/pion/ice/v2. It is the production
// ICE backend for the Phase 2 direct path (see spec/phase2-direct-path.md). Kept separate from the
// dependency-free interface in ice.go so the rest of the code (and tests) depend only on the seam.
package ice

import (
	"context"
	"time"

	pion "github.com/pion/ice/v2"
	"github.com/pion/stun"
)

// PionConfig configures a pion-backed agent.
type PionConfig struct {
	Role              Role
	STUNURLs          []string // e.g. "stun:broker:3478"
	TURNURLs          []string // e.g. "turn:broker:3478"
	TURNUser          string
	TURNPass          string
	KeepaliveInterval time.Duration // STUN keepalive cadence on the selected pair (default 1s)
	FailedTimeout     time.Duration // default 10s
	// DisconnectedTimeout is how long without traffic before pion marks the pair disconnected
	// (default 5s). Together with KeepaliveInterval it governs how aggressively the NAT binding
	// is kept fresh vs. how quickly a dead direct path is detected.
	DisconnectedTimeout time.Duration
	// DisableMDNS turns off mDNS candidate obfuscation (production keeps it ON for privacy;
	// tests disable it so host candidates carry real IPs).
	DisableMDNS bool
	// IncludeLoopback gathers loopback candidates (useful for same-host loopback tests).
	IncludeLoopback bool
}

type pionAgent struct {
	agent *pion.Agent
	role  Role
}

// NewPionAgent builds an ice.Agent backed by pion/ice. Candidate types are enabled based on the
// URLs provided (host always; server-reflexive if STUN; relay if TURN).
func NewPionAgent(cfg PionConfig) (Agent, error) {
	var urls []*stun.URI
	candTypes := []pion.CandidateType{pion.CandidateTypeHost}

	for _, u := range cfg.STUNURLs {
		uri, err := stun.ParseURI(u)
		if err != nil {
			return nil, err
		}
		urls = append(urls, uri)
	}
	if len(cfg.STUNURLs) > 0 {
		candTypes = append(candTypes, pion.CandidateTypeServerReflexive)
	}
	for _, u := range cfg.TURNURLs {
		uri, err := stun.ParseURI(u)
		if err != nil {
			return nil, err
		}
		uri.Username = cfg.TURNUser
		uri.Password = cfg.TURNPass
		urls = append(urls, uri)
	}
	if len(cfg.TURNURLs) > 0 {
		candTypes = append(candTypes, pion.CandidateTypeRelay)
	}

	ka := cfg.KeepaliveInterval
	if ka == 0 {
		ka = 1 * time.Second
	}
	ft := cfg.FailedTimeout
	if ft == 0 {
		ft = 10 * time.Second
	}
	dt := cfg.DisconnectedTimeout
	if dt == 0 {
		dt = 5 * time.Second
	}

	mdns := pion.MulticastDNSModeQueryAndGather
	if cfg.DisableMDNS {
		mdns = pion.MulticastDNSModeDisabled
	}

	a, err := pion.NewAgent(&pion.AgentConfig{
		Urls:                urls,
		NetworkTypes:        []pion.NetworkType{pion.NetworkTypeUDP4, pion.NetworkTypeUDP6},
		CandidateTypes:      candTypes,
		MulticastDNSMode:    mdns,
		IncludeLoopback:     cfg.IncludeLoopback,
		KeepaliveInterval:   &ka,
		FailedTimeout:       &ft,
		DisconnectedTimeout: &dt,
	})
	if err != nil {
		return nil, err
	}
	return &pionAgent{agent: a, role: cfg.Role}, nil
}

func (p *pionAgent) LocalCredentials() (Credentials, error) {
	uf, pw, err := p.agent.GetLocalUserCredentials()
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Ufrag: uf, Pwd: pw}, nil
}

func (p *pionAgent) OnCandidate(cb func(*Candidate)) {
	_ = p.agent.OnCandidate(func(c pion.Candidate) {
		if c == nil {
			cb(nil) // end of candidates
			return
		}
		cb(fromPion(c))
	})
}

func (p *pionAgent) GatherCandidates() error { return p.agent.GatherCandidates() }

func (p *pionAgent) AddRemoteCandidate(c Candidate) error {
	pc, err := pion.UnmarshalCandidate(c.Raw)
	if err != nil {
		return err
	}
	return p.agent.AddRemoteCandidate(pc)
}

func (p *pionAgent) Connect(ctx context.Context, remote Credentials) (Conn, error) {
	if p.role == RoleControlling {
		return p.agent.Dial(ctx, remote.Ufrag, remote.Pwd)
	}
	return p.agent.Accept(ctx, remote.Ufrag, remote.Pwd)
}

func (p *pionAgent) Restart() error { return p.agent.Restart("", "") }

func (p *pionAgent) SelectedPair() (CandidateType, CandidateType, bool) {
	pair, err := p.agent.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return "", "", false
	}
	return CandidateType(pair.Local.Type().String()), CandidateType(pair.Remote.Type().String()), true
}

func (p *pionAgent) Close() error { return p.agent.Close() }

func fromPion(c pion.Candidate) *Candidate {
	return &Candidate{
		Type:       CandidateType(c.Type().String()),
		Foundation: c.Foundation(),
		Protocol:   c.NetworkType().NetworkShort(),
		Address:    c.Address(),
		Port:       c.Port(),
		Priority:   c.Priority(),
		Raw:        c.Marshal(),
	}
}
