// Package cadu derives frame-yield statistics from a Meteor LRPT .cadu file.
// Go port of RN2's scripts/tools/cadu_stats.py.
//
// A CADU is a 1024-byte frame starting with the attached sync marker
// 0x1ACFFC1D, followed by the VCDU primary header: 2 bytes version/SCID/VCID
// (VCID = low 6 bits of byte 5) and a 24-bit frame counter (bytes 6..8).
// Frame loss is measured on the busiest non-fill virtual channel (VCID 63 is
// fill), because that is the channel carrying imagery.
package cadu

import (
	"fmt"
	"io"
	"os"
)

const (
	frameSize = 1024
	fillVCID  = 63
	// maxPlausibleStep guards against corrupted counters producing absurd
	// gaps (same constant as cadu_stats.py).
	maxPlausibleStep = 1_000_000
	counterModulo    = 1 << 24
)

var syncMarker = [4]byte{0x1A, 0xCF, 0xFC, 0x1D}

// Stats is the frame-yield summary for one capture.
type Stats struct {
	Received   int     // frames seen on the busiest imagery channel
	Expected   int     // frames the counter says should have been seen
	LossPct    float64 // 100 * (expected-received)/expected
	LargestGap int     // largest single run of missing frames
}

// FromFile reads a .cadu file and computes stats.
func FromFile(path string) (Stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return Stats{}, err
	}
	defer f.Close()
	return FromReader(f)
}

// FromReader computes stats from a stream of 1024-byte CADUs.
func FromReader(r io.Reader) (Stats, error) {
	counters := map[int][]int{} // vcid -> frame counters in arrival order
	frame := make([]byte, frameSize)
	for {
		_, err := io.ReadFull(r, frame)
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			break // trailing partial frame: ignore, decoder was cut off mid-write
		}
		if err != nil {
			return Stats{}, err
		}
		if [4]byte(frame[:4]) != syncMarker {
			continue // resync is the decoder's job; skip anything unframed
		}
		vcid := int(frame[5] & 0x3F)
		counter := int(frame[6])<<16 | int(frame[7])<<8 | int(frame[8])
		counters[vcid] = append(counters[vcid], counter)
	}

	best := -1
	for vcid, list := range counters {
		if vcid == fillVCID {
			continue
		}
		if best == -1 || len(list) > len(counters[best]) {
			best = vcid
		}
	}
	if best == -1 {
		return Stats{}, fmt.Errorf("no non-fill CADUs found")
	}

	list := counters[best]
	st := Stats{Received: len(list), Expected: len(list)}
	prev := list[0]
	expected := 1
	for _, cur := range list[1:] {
		delta := (cur - prev + counterModulo) % counterModulo
		if delta == 0 || delta > maxPlausibleStep {
			// Corrupted counter or duplicate: count the frame, trust no gap.
			prev = cur
			expected++
			continue
		}
		expected += delta
		if gap := delta - 1; gap > st.LargestGap {
			st.LargestGap = gap
		}
		prev = cur
	}
	st.Expected = expected
	if st.Expected > 0 {
		st.LossPct = 100 * float64(st.Expected-st.Received) / float64(st.Expected)
	}
	return st, nil
}
