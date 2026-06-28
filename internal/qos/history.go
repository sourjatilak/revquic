// SPDX-License-Identifier: GPL-3.0-or-later

package qos

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
)

// HistoryStore persists QoS history events so they survive a broker restart. Append MUST be
// non-blocking and safe to call while the tracker lock is held (the relay hot path calls it via
// appendEvent); implementations therefore buffer writes and flush them on a background goroutine.
// Load returns the most-recent events first.
type HistoryStore interface {
	Append(Event)
	Load(limit int) ([]Event, error)
	Close() error
}

// backend is the synchronous write/load/close contract wrapped by asyncStore.
type backend interface {
	write(Event) error
	load(limit int) ([]Event, error)
	close() error
}

// asyncStore decouples disk I/O from the tracker lock: Append enqueues to a buffered channel
// (dropping if full — the in-memory ring stays authoritative for the newest events) and a single
// goroutine performs the actual writes.
type asyncStore struct {
	b      backend
	ch     chan Event
	wg     sync.WaitGroup
	closed atomic.Bool
}

func newAsyncStore(b backend, buf int) *asyncStore {
	if buf <= 0 {
		buf = 1024
	}
	a := &asyncStore{b: b, ch: make(chan Event, buf)}
	a.wg.Add(1)
	go a.run()
	return a
}

func (a *asyncStore) run() {
	defer a.wg.Done()
	for ev := range a.ch {
		_ = a.b.write(ev)
	}
}

// Append enqueues an event without blocking (drops on a full buffer or after Close). The recover
// tolerates the rare race where Close closes the channel between the closed check and the send.
func (a *asyncStore) Append(ev Event) {
	if a.closed.Load() {
		return
	}
	defer func() { _ = recover() }()
	select {
	case a.ch <- ev:
	default: // buffer full: skip persistence for this event
	}
}

// Load returns up to limit most-recent persisted events, newest first.
func (a *asyncStore) Load(limit int) ([]Event, error) { return a.b.load(limit) }

// Close drains the queue, stops the writer, and closes the backend. Idempotent.
func (a *asyncStore) Close() error {
	if a.closed.Swap(true) {
		return nil
	}
	close(a.ch)
	a.wg.Wait()
	return a.b.close()
}

// --- JSONL file backend ---

// fileBackend appends one JSON object per line. On open it trims the file to the most recent
// maxRows lines to bound growth.
type fileBackend struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	maxRows int
}

const defaultFileMaxRows = 5000

// NewFileHistory opens (creating if needed) a JSONL-backed history store at path.
func NewFileHistory(path string) (HistoryStore, error) {
	fb := &fileBackend{path: path, maxRows: defaultFileMaxRows}
	if err := fb.trim(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	fb.f = f
	return newAsyncStore(fb, 1024), nil
}

func (fb *fileBackend) write(ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	_, err = fb.f.Write(append(b, '\n'))
	return err
}

func (fb *fileBackend) load(limit int) ([]Event, error) {
	evs, err := readAllEvents(fb.path)
	if err != nil {
		return nil, err
	}
	// newest first
	out := make([]Event, 0, len(evs))
	for i := len(evs) - 1; i >= 0; i-- {
		out = append(out, evs[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (fb *fileBackend) close() error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.f == nil {
		return nil
	}
	return fb.f.Close()
}

// trim rewrites the file keeping only the most recent maxRows lines (no-op if absent/small).
func (fb *fileBackend) trim() error {
	evs, err := readAllEvents(fb.path)
	if err != nil || len(evs) <= fb.maxRows {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keep := evs[len(evs)-fb.maxRows:]
	tmp := fb.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, ev := range keep {
		b, _ := json.Marshal(ev)
		_, _ = w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, fb.path)
}

// readAllEvents parses every JSONL record from path (returns nil if the file does not exist).
func readAllEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}
