// Package livelog fans the running capture's decoder output out to the web
// panel. RN2 had the PHP panel tail a shared log file every five seconds;
// here the capture runner publishes each cleaned SatDump line into the hub
// and the passes page receives them over Server-Sent Events as they happen.
package livelog

import "sync"

// ringSize is how many recent lines are kept for late joiners (a fullscreen
// terminal's worth of scrollback).
const ringSize = 500

// subBuffer is the per-subscriber channel depth; a subscriber that falls
// this far behind loses lines rather than stalling the capture.
const subBuffer = 256

// Line is one published line with its position in the stream. Sequence
// numbers are monotonic across captures, so a client that took a snapshot
// can drop any live line it already has.
type Line struct {
	Seq  uint64
	Text string
}

// Hub is a bounded scrollback plus a set of live subscribers.
type Hub struct {
	mu     sync.Mutex
	lines  []Line
	seq    uint64
	subs   map[chan Line]struct{}
	passID int64
}

func New() *Hub {
	return &Hub{subs: map[chan Line]struct{}{}}
}

// Reset clears the scrollback at the start of a new capture. Subscribers
// stay attached; the panel decides how to present the boundary.
func (h *Hub) Reset(passID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.passID = passID
	h.lines = h.lines[:0]
}

// PassID is the pass the scrollback belongs to (0 before the first capture).
func (h *Hub) PassID() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.passID
}

// Publish appends one line and delivers it to every subscriber without
// blocking: a slow subscriber drops the line.
func (h *Hub) Publish(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	line := Line{Seq: h.seq, Text: text}
	if len(h.lines) == ringSize {
		copy(h.lines, h.lines[1:])
		h.lines = h.lines[:ringSize-1]
	}
	h.lines = append(h.lines, line)
	for ch := range h.subs {
		select {
		case ch <- line:
		default:
		}
	}
}

// Snapshot returns up to n most recent lines (all when n <= 0), the pass
// they belong to, and the sequence number of the last line published — the
// point from which live lines continue.
func (h *Hub) Snapshot(n int) (lines []string, passID int64, lastSeq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n <= 0 || n > len(h.lines) {
		n = len(h.lines)
	}
	lines = make([]string, n)
	for i, l := range h.lines[len(h.lines)-n:] {
		lines[i] = l.Text
	}
	return lines, h.passID, h.seq
}

// Subscribe registers a live listener. The returned function detaches it;
// after that the channel is closed.
func (h *Hub) Subscribe() (<-chan Line, func()) {
	ch := make(chan Line, subBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}
