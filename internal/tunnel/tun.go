// SPDX-License-Identifier: GPL-3.0-or-later

// Package tunnel wraps a TUN device and provides the IP-packet <-> QUIC-datagram pumps.
//
// The OS-specific TUN backend lives in tun_other.go (Linux/macOS/BSD, via songgao/water) and
// tun_windows.go (Windows, via Wintun). Everything below is platform-neutral.
package tunnel

import "context"

// backend is the per-platform TUN implementation (water on Unix, Wintun on Windows).
type backend interface {
	Name() string
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// Device is a TUN interface (L3, carries raw IP packets).
type Device struct {
	b backend
}

// Name returns the OS interface name (e.g. tun0 / utunN, or the adapter name on Windows).
func (d *Device) Name() string { return d.b.Name() }

// Read reads one IP packet from the kernel.
func (d *Device) Read(p []byte) (int, error) { return d.b.Read(p) }

// Write injects one IP packet into the kernel.
func (d *Device) Write(p []byte) (int, error) { return d.b.Write(p) }

// Close tears down the device.
func (d *Device) Close() error { return d.b.Close() }

// DatagramSender is satisfied by *quic.Connection (SendDatagram).
type DatagramSender interface {
	SendDatagram(b []byte) error
}

// PumpToSender reads IP packets from the TUN and hands each to encode() then sends it.
// encode adds the session-id prefix (see proto.EncodeDatagram). It returns when ctx is
// cancelled or the device errors.
func (d *Device) PumpToSender(ctx context.Context, send DatagramSender, encode func([]byte) []byte) error {
	buf := make([]byte, 65535)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := d.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if err := send.SendDatagram(encode(buf[:n])); err != nil {
			// Datagram too large / temporarily unsendable: drop (UDP semantics). Real
			// code should honor the connection's datagram MTU and clamp inner MSS.
			continue
		}
	}
}
