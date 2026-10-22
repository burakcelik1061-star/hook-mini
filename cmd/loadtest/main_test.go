package main

import (
	"testing"
	"time"
)

func TestPercentiles(t *testing.T) {
	if percentiles(nil) != nil {
		t.Error("no samples must give no percentiles, not zeros")
	}
	var d []time.Duration
	for i := 1; i <= 100; i++ { // 1ms .. 100ms, deliberately shuffled below
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	d[0], d[99] = d[99], d[0]
	got := percentiles(d)
	want := map[string]float64{"p50": 51, "p95": 96, "p99": 100, "max": 100}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	if d[0] != 100*time.Millisecond {
		t.Error("percentiles reordered the caller's slice")
	}
	if one := percentiles([]time.Duration{7 * time.Millisecond}); one["p50"] != 7 || one["p99"] != 7 {
		t.Errorf("a single sample: %v", one)
	}
}

func TestRound(t *testing.T) {
	for in, want := range map[float64]float64{0: 0, 1.04: 1, 1.05: 1.1, 123.456: 123.5} {
		if got := round(in); got != want {
			t.Errorf("round(%v) = %v, want %v", in, got, want)
		}
	}
}
