package cadu

import (
	"bytes"
	"testing"
)

// makeCADU builds one 1024-byte frame for vcid with the given counter.
func makeCADU(vcid, counter int) []byte {
	f := make([]byte, frameSize)
	copy(f, syncMarker[:])
	f[4] = 0x40 // version/SCID bits, irrelevant to the parser
	f[5] = byte(vcid & 0x3F)
	f[6] = byte(counter >> 16)
	f[7] = byte(counter >> 8)
	f[8] = byte(counter)
	return f
}

func stream(frames ...[]byte) *bytes.Buffer {
	var b bytes.Buffer
	for _, f := range frames {
		b.Write(f)
	}
	return &b
}

func TestPerfectStream(t *testing.T) {
	st, err := FromReader(stream(
		makeCADU(5, 100), makeCADU(5, 101), makeCADU(5, 102), makeCADU(5, 103),
	))
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 4 || st.Expected != 4 || st.LossPct != 0 || st.LargestGap != 0 {
		t.Errorf("perfect stream: %+v", st)
	}
}

func TestGapDetection(t *testing.T) {
	// counters 100,101,105,106: 3 frames missing (102..104).
	st, err := FromReader(stream(
		makeCADU(5, 100), makeCADU(5, 101), makeCADU(5, 105), makeCADU(5, 106),
	))
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 4 {
		t.Errorf("received = %d, want 4", st.Received)
	}
	if st.Expected != 7 {
		t.Errorf("expected = %d, want 7", st.Expected)
	}
	if st.LargestGap != 3 {
		t.Errorf("largest gap = %d, want 3", st.LargestGap)
	}
	if st.LossPct < 42.8 || st.LossPct > 42.9 { // 3/7
		t.Errorf("loss = %.2f%%, want ~42.86%%", st.LossPct)
	}
}

func TestBusiestChannelWins(t *testing.T) {
	// VCID 9 has more frames than VCID 5; fill (63) is ignored entirely.
	st, err := FromReader(stream(
		makeCADU(5, 1), makeCADU(5, 2),
		makeCADU(9, 10), makeCADU(9, 11), makeCADU(9, 13),
		makeCADU(63, 1), makeCADU(63, 2), makeCADU(63, 3), makeCADU(63, 4), makeCADU(63, 5), makeCADU(63, 6),
	))
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 3 || st.Expected != 4 || st.LargestGap != 1 {
		t.Errorf("busiest-channel stats: %+v", st)
	}
}

func TestCounterWrap(t *testing.T) {
	// 24-bit counter wraps from 0xFFFFFF to 0.
	st, err := FromReader(stream(
		makeCADU(5, 0xFFFFFE), makeCADU(5, 0xFFFFFF), makeCADU(5, 0), makeCADU(5, 1),
	))
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 4 || st.Expected != 4 || st.LossPct != 0 {
		t.Errorf("wrap handling: %+v", st)
	}
}

func TestCorruptCounterGuard(t *testing.T) {
	// A wild counter jump (> maxPlausibleStep) must not explode 'expected'.
	st, err := FromReader(stream(
		makeCADU(5, 100), makeCADU(5, 5_000_000), makeCADU(5, 5_000_001),
	))
	if err != nil {
		t.Fatal(err)
	}
	if st.Expected > 10 {
		t.Errorf("corrupt counter inflated expected to %d", st.Expected)
	}
}

func TestGarbageBetweenFramesIgnored(t *testing.T) {
	garbage := make([]byte, frameSize) // no sync marker
	st, err := FromReader(stream(makeCADU(5, 1), garbage, makeCADU(5, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 2 || st.Expected != 2 {
		t.Errorf("garbage frame not ignored: %+v", st)
	}
}

func TestTruncatedTailTolerated(t *testing.T) {
	b := stream(makeCADU(5, 1), makeCADU(5, 2))
	b.Write(syncMarker[:]) // partial trailing frame
	st, err := FromReader(b)
	if err != nil {
		t.Fatal(err)
	}
	if st.Received != 2 {
		t.Errorf("truncated tail broke parsing: %+v", st)
	}
}

func TestNoDataIsError(t *testing.T) {
	if _, err := FromReader(bytes.NewReader(nil)); err == nil {
		t.Fatal("empty stream should be an error")
	}
	// Fill-only stream is also useless for stats.
	if _, err := FromReader(stream(makeCADU(63, 1), makeCADU(63, 2))); err == nil {
		t.Fatal("fill-only stream should be an error")
	}
}
