package livelog

import (
	"fmt"
	"testing"
)

func TestSnapshotRingAndReset(t *testing.T) {
	h := New()
	for i := 0; i < ringSize+10; i++ {
		h.Publish(fmt.Sprintf("line %d", i))
	}
	all, _, seq := h.Snapshot(0)
	if len(all) != ringSize {
		t.Fatalf("ring holds %d lines, want %d", len(all), ringSize)
	}
	if all[0] != "line 10" || all[len(all)-1] != fmt.Sprintf("line %d", ringSize+9) {
		t.Errorf("ring lost the wrong end: first=%q last=%q", all[0], all[len(all)-1])
	}
	if seq != ringSize+10 {
		t.Errorf("last seq = %d, want %d", seq, ringSize+10)
	}
	if tail, _, _ := h.Snapshot(3); len(tail) != 3 || tail[2] != all[len(all)-1] {
		t.Errorf("Snapshot(3) = %v", tail)
	}
	h.Reset(7)
	lines, passID, seq2 := h.Snapshot(0)
	if len(lines) != 0 || passID != 7 || h.PassID() != 7 {
		t.Error("Reset did not clear scrollback / set pass id")
	}
	if seq2 != seq {
		t.Error("sequence numbers must stay monotonic across a Reset")
	}
}

func TestSubscribeDeliversSequencedAndDropsWhenFull(t *testing.T) {
	h := New()
	h.Publish("before")
	ch, cancel := h.Subscribe()
	_, _, snapSeq := h.Snapshot(0)
	h.Publish("a")
	got := <-ch
	if got.Text != "a" || got.Seq != snapSeq+1 {
		t.Fatalf("got %+v, want seq %d", got, snapSeq+1)
	}
	for i := 0; i < subBuffer+50; i++ { // must not block the publisher
		h.Publish("x")
	}
	if len(ch) != subBuffer {
		t.Errorf("buffer holds %d, want %d", len(ch), subBuffer)
	}
	cancel()
	cancel() // idempotent
	for range ch {
	} // drains, then the closed channel ends the loop
	h.Publish("after") // no subscriber: must not panic
}
