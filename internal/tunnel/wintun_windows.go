// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

// Wintun backend for Windows. This is a minimal, self-contained binding to wintun.dll (the
// WireGuard project's signed TUN driver) using x/sys/windows — no extra Go module dependency.
//
// wintun.dll must sit next to revquic-client.exe (the release archive bundles it). The process must
// run elevated (Administrator) to create the adapter. The driver itself is installed automatically
// by the DLL on first use; there is no separate installer.
package tunnel

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modwintun                = windows.NewLazyDLL("wintun.dll")
	procWintunCreateAdapter  = modwintun.NewProc("WintunCreateAdapter")
	procWintunCloseAdapter   = modwintun.NewProc("WintunCloseAdapter")
	procWintunStartSession   = modwintun.NewProc("WintunStartSession")
	procWintunEndSession     = modwintun.NewProc("WintunEndSession")
	procWintunAllocateSend   = modwintun.NewProc("WintunAllocateSendPacket")
	procWintunSendPacket     = modwintun.NewProc("WintunSendPacket")
	procWintunReceivePacket  = modwintun.NewProc("WintunReceivePacket")
	procWintunReleaseRecv    = modwintun.NewProc("WintunReleaseReceivePacket")
	procWintunGetReadWaitEvt = modwintun.NewProc("WintunGetReadWaitEvent")
)

// 4 MiB ring buffer (must be a power of two between 128 KiB and 64 MiB per the Wintun API).
const wintunRingCapacity = 0x400000

func utf16(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// wintunBackend implements the tunnel.backend interface on top of a Wintun session.
//
// NOTE: Read/Write below convert the uintptr returned by WintunReceivePacket/AllocateSendPacket to
// a pointer into Wintun's ring buffer. `GOOS=windows go vet` reports a benign "possible misuse of
// unsafe.Pointer" for this — the memory is driver-owned (not Go-managed), so it is not subject to
// GC movement and the conversion is safe. (Host `go vet ./...` does not compile this file.)
type wintunBackend struct {
	name      string
	adapter   uintptr
	session   uintptr
	readEvent windows.Handle

	mu     sync.Mutex
	closed bool
}

// wintunOpen creates a Wintun adapter named name and starts a session.
func wintunOpen(name string) (*wintunBackend, error) {
	if err := modwintun.Load(); err != nil {
		return nil, fmt.Errorf("load wintun.dll (place wintun.dll next to revquic-client.exe — download from https://www.wintun.net): %w", err)
	}
	adapter, _, e := procWintunCreateAdapter.Call(
		uintptr(unsafe.Pointer(utf16(name))),
		uintptr(unsafe.Pointer(utf16("Revquic"))),
		0, // RequestedGUID = NULL
	)
	if adapter == 0 {
		return nil, fmt.Errorf("WintunCreateAdapter(%q): %w (are you running as Administrator?)", name, e)
	}
	session, _, e := procWintunStartSession.Call(adapter, uintptr(wintunRingCapacity))
	if session == 0 {
		procWintunCloseAdapter.Call(adapter)
		return nil, fmt.Errorf("WintunStartSession: %w", e)
	}
	evt, _, _ := procWintunGetReadWaitEvt.Call(session)
	return &wintunBackend{name: name, adapter: adapter, session: session, readEvent: windows.Handle(evt)}, nil
}

func (w *wintunBackend) Name() string { return w.name }

// Read blocks until one IP packet is available, copies it into p, and returns its length.
func (w *wintunBackend) Read(p []byte) (int, error) {
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return 0, fmt.Errorf("tun closed")
		}
		var size uint32
		ptr, _, e := procWintunReceivePacket.Call(w.session, uintptr(unsafe.Pointer(&size)))
		w.mu.Unlock()

		if ptr != 0 {
			n := int(size)
			if n > len(p) {
				n = len(p)
			}
			copy(p[:n], unsafe.Slice((*byte)(unsafe.Pointer(ptr)), int(size)))
			procWintunReleaseRecv.Call(w.session, ptr)
			return n, nil
		}
		// No packet ready: wait on the read event, then retry. Any other error is fatal.
		if en, ok := e.(syscall.Errno); ok && en == windows.ERROR_NO_MORE_ITEMS {
			if _, err := windows.WaitForSingleObject(w.readEvent, windows.INFINITE); err != nil {
				return 0, err
			}
			continue
		}
		return 0, fmt.Errorf("WintunReceivePacket: %w", e)
	}
}

// Write injects one IP packet into the kernel.
func (w *wintunBackend) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fmt.Errorf("tun closed")
	}
	ptr, _, e := procWintunAllocateSend.Call(w.session, uintptr(len(p)))
	if ptr == 0 {
		return 0, fmt.Errorf("WintunAllocateSendPacket: %w", e)
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(p)), p)
	procWintunSendPacket.Call(w.session, ptr)
	return len(p), nil
}

// Close ends the session and removes the adapter.
func (w *wintunBackend) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.session != 0 {
		procWintunEndSession.Call(w.session)
		w.session = 0
	}
	if w.adapter != 0 {
		procWintunCloseAdapter.Call(w.adapter)
		w.adapter = 0
	}
	return nil
}
