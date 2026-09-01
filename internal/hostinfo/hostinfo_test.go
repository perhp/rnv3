package hostinfo

import "testing"

func TestParseCPUTimesAndUsage(t *testing.T) {
	a, ok := parseCPUTimes("cpu  100 0 50 800 20 0 5 0 0 0\ncpu0 50 0 25 400 10 0 2 0 0 0\n")
	if !ok || a.total != 975 || a.idle != 820 {
		t.Fatalf("a = %+v ok=%v", a, ok)
	}
	b := cpuTimes{total: a.total + 1000, idle: a.idle + 900}
	if pct, ok := cpuUsage(a, b); !ok || pct != 10 {
		t.Errorf("usage = %v ok=%v, want 10", pct, ok)
	}
	if _, ok := cpuUsage(b, a); ok {
		t.Error("counter going backwards must not yield a value")
	}
	if _, ok := parseCPUTimes("intr 1 2 3\n"); ok {
		t.Error("no cpu line must fail")
	}
}
