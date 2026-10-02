package main

import (
	"fmt"
	"math"
	"time"
)

// minRequestsForP95: a minute with fewer requests than this is too noisy to judge latency.
const minRequestsForP95 = 5

// Signal is what the analytics say about an app over the averaging window.
type Signal struct {
	RPS          float64 // average requests/s over the window (all replicas together)
	P95Ms        float64 // worst per-minute p95 latency among minutes with enough traffic
	ErrorPercent float64 // 5xx responses as % of requests
	Minutes      int     // complete minutes used
}

// minutesOf rounds a duration up to whole minutes (never negative).
func minutesOf(d time.Duration) int {
	return max(int(math.Ceil(d.Minutes()-1e-9)), 0)
}

// RangeFor picks the analytics range that covers `window` complete minutes ending `delay` before
// the running minute, plus the running one.
func RangeFor(window, delay time.Duration) string {
	n := minutesOf(window) + minutesOf(delay)
	switch {
	case n <= 5:
		return "5m"
	case n <= 15:
		return "15m"
	default:
		return "1h"
	}
}

// ComputeSignal averages `window` complete minutes of the series, ending `delay` before the
// running minute. The Miabi analytics only publishes a minute about 100s after it closes, so the
// newest minutes are still empty; `delay` skips them instead of reading them as "no traffic".
// The minute in progress is always ignored (it is partial). A window the series does not cover is an error, never "zero traffic":
// scaling down on missing data would be unsafe.
func ComputeSignal(series []Bucket, now time.Time, window, delay time.Duration) (Signal, error) {
	n := minutesOf(window)
	if n < 1 {
		n = 1
	}
	if len(series) == 0 {
		return Signal{}, fmt.Errorf("analytics sem dados")
	}
	first := series[0].T
	for _, b := range series {
		if b.T.Before(first) {
			first = b.T
		}
	}
	cutoff := now.UTC().Truncate(time.Minute).Add(-time.Duration(minutesOf(delay)) * time.Minute) // newest minute we trust, exclusive
	oldest := cutoff.Add(-time.Duration(n) * time.Minute)
	if oldest.Before(first.UTC().Truncate(time.Minute)) {
		return Signal{}, fmt.Errorf("a serie do analytics nao cobre a janela de %d min", n)
	}
	byMinute := make(map[time.Time]Bucket, len(series))
	for _, b := range series {
		byMinute[b.T.UTC().Truncate(time.Minute)] = b
	}
	var reqs, errs5, p95 float64
	for i := 1; i <= n; i++ {
		b := byMinute[cutoff.Add(-time.Duration(i)*time.Minute)] // missing minute == no traffic
		reqs += b.Requests
		errs5 += b.Errors5xx
		if b.Requests >= minRequestsForP95 && b.P95Ms > p95 {
			p95 = b.P95Ms
		}
	}
	s := Signal{RPS: reqs / (float64(n) * 60), P95Ms: p95, Minutes: n}
	if reqs > 0 {
		s.ErrorPercent = 100 * errs5 / reqs
	}
	return s, nil
}

// Recommend turns a signal into a raw replica recommendation (before stabilization).
func Recommend(p Policy, sig Signal, current int) (int, string) {
	rec, why := current, "dentro da tolerancia"
	if current < 1 {
		current = 1
	}
	ratio := sig.RPS / (float64(current) * p.TargetRPS)
	if math.Abs(ratio-1) > p.Tolerance {
		rec = int(math.Ceil(sig.RPS/p.TargetRPS - 1e-9))
		why = fmt.Sprintf("%.1f req/s, alvo %.1f por replica", sig.RPS, p.TargetRPS)
	}
	if p.MaxP95Ms > 0 && sig.P95Ms > p.MaxP95Ms && rec < current+1 {
		rec, why = current+1, fmt.Sprintf("p95 %.0f ms acima do limite de %.0f ms", sig.P95Ms, p.MaxP95Ms)
	}
	if p.MaxErrorPercent > 0 && sig.ErrorPercent >= p.MaxErrorPercent && rec < current {
		rec, why = current, fmt.Sprintf("erros 5xx em %.1f%%: sem reduzir", sig.ErrorPercent)
	}
	return clamp(rec, p.Min, p.Max), why
}

type recommendation struct {
	at    time.Time
	value int
}

// State keeps the recent recommendations of one app (in memory only).
type State struct {
	hist   []recommendation
	seeded bool
}

// Seed records the replica count found at startup, so a restart does not scale down right away.
func (s *State) Seed(now time.Time, current int) {
	if s.seeded {
		return
	}
	s.seeded = true
	s.hist = append(s.hist, recommendation{now, current})
}

// Decide applies the stabilization windows and rate limits (Kubernetes HPA style):
// scale-up needs the recommendation to have stayed high for the whole scaleUp window, and
// scale-down follows the highest recommendation seen during the scaleDown window.
func (s *State) Decide(now time.Time, p Policy, current, recommended int) int {
	s.hist = append(s.hist, recommendation{now, recommended})
	keep := max(p.ScaleUp.Stabilization.Duration, p.ScaleDown.Stabilization.Duration)
	i := 0
	for i < len(s.hist) && now.Sub(s.hist[i].at) > keep {
		i++
	}
	s.hist = s.hist[i:]

	up, down := recommended, recommended
	for _, h := range s.hist {
		age := now.Sub(h.at)
		if age <= p.ScaleUp.Stabilization.Duration {
			up = min(up, h.value)
		}
		if age <= p.ScaleDown.Stabilization.Duration {
			down = max(down, h.value)
		}
	}
	d := current
	if d < up {
		d = up
	}
	if d > down {
		d = down
	}
	switch {
	case d > current:
		d = min(d, current+p.ScaleUp.MaxStep)
	case d < current:
		d = max(d, current-p.ScaleDown.MaxStep)
	}
	return clamp(d, p.Min, p.Max)
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }
