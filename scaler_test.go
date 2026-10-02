package main

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 10, 5, 30, 0, time.UTC) // 30s into minute 10:05

// buildSeries returns n complete minutes before t0's minute (oldest first) plus the running minute.
func buildSeries(reqs []float64, p95 []float64, err5 []float64) []Bucket {
	cutoff := t0.Truncate(time.Minute)
	var out []Bucket
	for i, r := range reqs {
		b := Bucket{T: cutoff.Add(-time.Duration(len(reqs)-i) * time.Minute), Requests: r}
		if p95 != nil {
			b.P95Ms = p95[i]
		}
		if err5 != nil {
			b.Errors5xx = err5[i]
		}
		out = append(out, b)
	}
	return append(out, Bucket{T: cutoff, Requests: 999999, P95Ms: 9999, Errors5xx: 999999}) // partial, must be ignored
}

func TestComputeSignalIgnoresRunningMinute(t *testing.T) {
	s, err := ComputeSignal(buildSeries([]float64{600, 1800}, nil, nil), t0, 2*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2400.0 / 120; s.RPS != want {
		t.Fatalf("rps = %v, want %v", s.RPS, want)
	}
	if s.ErrorPercent != 0 || s.P95Ms != 0 {
		t.Fatalf("partial minute leaked into the signal: %+v", s)
	}
}

func TestComputeSignalP95AndErrors(t *testing.T) {
	s, err := ComputeSignal(buildSeries([]float64{100, 3}, []float64{80, 900}, []float64{5, 0}), t0, 2*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.P95Ms != 80 { // the 3-request minute is too noisy to count
		t.Fatalf("p95 = %v, want 80", s.P95Ms)
	}
	if want := 100 * 5.0 / 103; s.ErrorPercent < want-0.001 || s.ErrorPercent > want+0.001 {
		t.Fatalf("error%% = %v, want %v", s.ErrorPercent, want)
	}
}

func TestComputeSignalMissingMinuteIsZeroTraffic(t *testing.T) {
	cutoff := t0.Truncate(time.Minute)
	series := []Bucket{ // 10:03 is absent, series still covers the window
		{T: cutoff.Add(-3 * time.Minute), Requests: 0},
		{T: cutoff.Add(-1 * time.Minute), Requests: 120},
		{T: cutoff, Requests: 1},
	}
	s, err := ComputeSignal(series, t0, 2*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.RPS != 1 {
		t.Fatalf("rps = %v, want 1", s.RPS)
	}
}

func TestComputeSignalRefusesUncoveredWindow(t *testing.T) {
	if _, err := ComputeSignal(nil, t0, time.Minute, 0); err == nil {
		t.Fatal("empty series must be an error")
	}
	if _, err := ComputeSignal(buildSeries([]float64{10}, nil, nil), t0, 5*time.Minute, 0); err == nil {
		t.Fatal("window larger than the series must be an error, not 'zero traffic'")
	}
}

func TestRangeFor(t *testing.T) {
	for window, want := range map[time.Duration]string{time.Minute: "5m", 5 * time.Minute: "5m", 6 * time.Minute: "15m", 15 * time.Minute: "15m", 16 * time.Minute: "1h", time.Hour: "1h"} {
		if got := RangeFor(window, 0); got != want {
			t.Errorf("RangeFor(%v) = %s, want %s", window, got, want)
		}
	}
	if got := RangeFor(2*time.Minute, 2*time.Minute); got != "5m" {
		t.Errorf("window 2m + delay 2m = %s, want 5m", got)
	}
	if got := RangeFor(4*time.Minute, 2*time.Minute); got != "15m" {
		t.Errorf("window 4m + delay 2m = %s, want 15m", got)
	}
}

// The analytics publishes a minute ~98s after it closes: the 2 newest complete minutes are still
// empty. Without a delay they read as "no traffic"; with it they are skipped.
func TestComputeSignalSkipsMinutesNotYetPublished(t *testing.T) {
	cutoff := t0.Truncate(time.Minute)
	series := []Bucket{
		{T: cutoff.Add(-4 * time.Minute), Requests: 1200},
		{T: cutoff.Add(-3 * time.Minute), Requests: 1200},
		{T: cutoff.Add(-2 * time.Minute), Requests: 0}, // not published yet
		{T: cutoff.Add(-1 * time.Minute), Requests: 0}, // not published yet
		{T: cutoff, Requests: 5},
	}
	s, err := ComputeSignal(series, t0, 2*time.Minute, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2400.0 / 120; s.RPS != want {
		t.Fatalf("rps = %v, want %v (the unpublished minutes must be skipped)", s.RPS, want)
	}
	if s0, _ := ComputeSignal(series, t0, 2*time.Minute, 0); s0.RPS != 0 {
		t.Fatalf("sanity: without delay the empty minutes count as zero traffic, got %v", s0.RPS)
	}
}

func testPolicy() Policy {
	p := DefaultPolicy()
	p.Min, p.Max, p.TargetRPS = 1, 10, 10
	return p
}

func TestRecommend(t *testing.T) {
	p := testPolicy()
	cases := []struct {
		name    string
		sig     Signal
		current int
		want    int
	}{
		{"on target", Signal{RPS: 30}, 3, 3},
		{"within tolerance above", Signal{RPS: 32}, 3, 3},
		{"above tolerance", Signal{RPS: 45}, 3, 5},
		{"below tolerance", Signal{RPS: 10}, 3, 1},
		{"idle goes to min", Signal{RPS: 0}, 4, 1},
		{"exact multiple does not round up", Signal{RPS: 40}, 2, 4},
		{"clamped to max", Signal{RPS: 5000}, 2, 10},
	}
	for _, c := range cases {
		if got, _ := Recommend(p, c.sig, c.current); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestRecommendGuards(t *testing.T) {
	p := testPolicy()
	p.MaxP95Ms, p.MaxErrorPercent = 200, 5
	if got, _ := Recommend(p, Signal{RPS: 30, P95Ms: 500}, 3); got != 4 {
		t.Errorf("slow p95 should add one replica, got %d", got)
	}
	if got, _ := Recommend(p, Signal{RPS: 5, ErrorPercent: 8}, 3); got != 3 {
		t.Errorf("high 5xx must block scale-down, got %d", got)
	}
	if got, _ := Recommend(p, Signal{RPS: 90, ErrorPercent: 8}, 3); got != 9 {
		t.Errorf("high 5xx must not block scale-up, got %d", got)
	}
}

func TestDecideScaleUpNeedsAFullWindow(t *testing.T) {
	p := testPolicy() // up 1m / maxStep 2
	var s State
	s.Seed(t0, 2)
	if d := s.Decide(t0, p, 2, 6); d != 2 {
		t.Fatalf("t+0s: got %d, want 2", d)
	}
	if d := s.Decide(t0.Add(30*time.Second), p, 2, 6); d != 2 {
		t.Fatalf("t+30s: got %d, want 2 (window not full yet)", d)
	}
	if d := s.Decide(t0.Add(61*time.Second), p, 2, 6); d != 4 {
		t.Fatalf("t+61s: got %d, want 4 (6 limited to +2 per step)", d)
	}
}

func TestDecideScaleUpIgnoresShortSpike(t *testing.T) {
	p := testPolicy()
	var s State
	s.Seed(t0.Add(-time.Hour), 2)
	s.Decide(t0, p, 2, 2)
	s.Decide(t0.Add(30*time.Second), p, 2, 8) // one-cycle spike
	if d := s.Decide(t0.Add(60*time.Second), p, 2, 2); d != 2 {
		t.Fatalf("a single high reading must not scale up, got %d", d)
	}
}

func TestDecideScaleDownIsSlowAndStepwise(t *testing.T) {
	p := testPolicy() // down 5m / maxStep 1
	var s State
	s.Seed(t0, 6) // restart: found 6 replicas
	for _, sec := range []int{0, 60, 120, 180, 240, 299} {
		if d := s.Decide(t0.Add(time.Duration(sec)*time.Second), p, 6, 2); d != 6 {
			t.Fatalf("t+%ds: got %d, want 6 (still inside the scale-down window)", sec, d)
		}
	}
	if d := s.Decide(t0.Add(301*time.Second), p, 6, 2); d != 5 {
		t.Fatalf("after the window: got %d, want 5 (one replica at a time)", d)
	}
}

func TestDecideScaleDownBlockedByRecentPeak(t *testing.T) {
	p := testPolicy()
	var s State
	s.Seed(t0.Add(-time.Hour), 4)
	s.Decide(t0, p, 4, 2)
	s.Decide(t0.Add(240*time.Second), p, 4, 8) // peak 4 minutes ago
	if d := s.Decide(t0.Add(400*time.Second), p, 4, 2); d != 4 {
		t.Fatalf("peak is still inside the 5m window, got %d, want 4", d)
	}
	if d := s.Decide(t0.Add(560*time.Second), p, 4, 2); d != 3 {
		t.Fatalf("peak left the window, got %d, want 3", d)
	}
}

func TestDecideRespectsBounds(t *testing.T) {
	p := testPolicy()
	p.Min, p.Max = 2, 3
	p.ScaleUp.Stabilization.Duration, p.ScaleDown.Stabilization.Duration = 0, 0
	var s State
	s.seeded = true
	if d := s.Decide(t0, p, 3, 50); d != 3 {
		t.Fatalf("above max: got %d", d)
	}
	if d := s.Decide(t0.Add(time.Second), p, 2, 0); d != 2 {
		t.Fatalf("below min: got %d", d)
	}
}
