// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package tunnel

// Open creates a Wintun adapter named "Revquic". Requires Administrator and wintun.dll next to the
// executable. The adapter name is what netcfg uses with netsh/route, so it must match.
func Open() (*Device, error) {
	b, err := wintunOpen("Revquic")
	if err != nil {
		return nil, err
	}
	return &Device{b: b}, nil
}
