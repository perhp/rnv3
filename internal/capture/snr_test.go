package capture

import "testing"

func TestSNRStatsParsesVariants(t *testing.T) {
	s := &SNRStats{}
	s.Feed("Viterbi BER: 0.0032, SNR: 12.5 dB")
	s.Feed("(NOAA APT) SNR : 8.0")
	s.Feed("Progress 42.1%, no reading here")
	s.Feed("SNR=-3.5dB")
	max, avg, ok := s.Result()
	if !ok {
		t.Fatal("expected readings")
	}
	if max != 12.5 {
		t.Errorf("max = %v, want 12.5", max)
	}
	want := (12.5 + 8.0 - 3.5) / 3
	if avg < want-0.001 || avg > want+0.001 {
		t.Errorf("avg = %v, want %v", avg, want)
	}
}

func TestSNRStatsNoReadings(t *testing.T) {
	s := &SNRStats{}
	s.Feed("nothing to see")
	if _, _, ok := s.Result(); ok {
		t.Fatal("expected no readings")
	}
}

func TestSNRStatsAllNegative(t *testing.T) {
	// max must be the true maximum even when every reading is negative
	// (zero-value bug guard).
	s := &SNRStats{}
	s.Feed("SNR: -8.2")
	s.Feed("SNR: -3.1")
	max, _, ok := s.Result()
	if !ok || max != -3.1 {
		t.Errorf("max = %v ok=%v, want -3.1", max, ok)
	}
}

func TestCleanLineStripsANSI(t *testing.T) {
	in := "\x1b[32m[INFO]\x1b[0m SNR: 9.9"
	if got := CleanLine(in); got != "[INFO] SNR: 9.9" {
		t.Errorf("CleanLine = %q", got)
	}
}
