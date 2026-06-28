// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package tunnel

import "github.com/songgao/water"

// waterBackend uses songgao/water (Linux/macOS/BSD: TUN via /dev/net/tun or utun).
type waterBackend struct {
	ifce *water.Interface
}

func (w waterBackend) Name() string                { return w.ifce.Name() }
func (w waterBackend) Read(p []byte) (int, error)  { return w.ifce.Read(p) }
func (w waterBackend) Write(p []byte) (int, error) { return w.ifce.Write(p) }
func (w waterBackend) Close() error                { return w.ifce.Close() }

// Open creates a new TUN device. On Linux/macOS this requires root (or CAP_NET_ADMIN).
func Open() (*Device, error) {
	ifce, err := water.New(water.Config{DeviceType: water.TUN})
	if err != nil {
		return nil, err
	}
	return &Device{b: waterBackend{ifce: ifce}}, nil
}
