package vis

// These tests came from a seventh review of the fleet layer, each a reproducer of a defect it found.

import (
	"testing"
	"time"
)

func TestASpanMatchesItsWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// steady 1 rps from t0 to t0+120s
	mk := func(from, to time.Duration, skip func(time.Duration) bool) *routeTable {
		var rt routeTable
		for d := from; d < to; d += time.Second {
			if skip != nil && skip(d) {
				continue
			}
			rt.record("r", time.Millisecond, 200, t0.Add(d))
		}
		return &rt
	}
	for _, ph := range []time.Duration{0, 1 * time.Millisecond, 3 * time.Second, 9999 * time.Millisecond} {
		now := t0.Add(100*time.Second + ph)
		rt := mk(0, 100*time.Second+ph, nil)
		h, n, _ := rt.window("r", 20*time.Second, now)
		_ = h
		s := rt.span("r", 20*time.Second, time.Time{}, now)
		from := now.Add(-20 * time.Second)
		st := rt.since("r", from, now)
		ss := rt.span("r", 0, from, now)
		if r, rs := float64(n)/s.Seconds(), float64(st.Requests)/ss.Seconds(); r < .9 || r > 1.1 || rs < .9 || rs > 1.1 {
			t.Errorf("phase %v: 1 rps read as %.3f over the window (n=%d, span %v) and %.3f since (n=%d, span %v)", ph, r, n, s, rs, st.Requests, ss)
		}
	}
	// a new route: first request at t0+12s, read at t0+15.5s
	rt := mk(12*time.Second, 16*time.Second, nil)
	now := t0.Add(15500 * time.Millisecond)
	_, n, _ := rt.window("r", 20*time.Second, now)
	s := rt.span("r", 20*time.Second, time.Time{}, now)
	t.Logf("new route: n=%d span=%v rate=%.3f (true 1.0)", n, s, float64(n)/s.Seconds())
	// no requests 40-80 s, then read at 100 s
	rt = mk(0, 100*time.Second, func(d time.Duration) bool { return d >= 40*time.Second && d < 80*time.Second })
	now = t0.Add(100 * time.Second)
	_, n, _ = rt.window("r", 20*time.Second, now)
	s = rt.span("r", 20*time.Second, time.Time{}, now)
	st := rt.since("r", t0.Add(75*time.Second), now)
	ss := rt.span("r", 0, t0.Add(75*time.Second), now)
	t.Logf("gap: window n=%d span=%v; since(75s) n=%d span=%v", n, s, st.Requests, ss)
	// idle over an hour, then one request, read
	rt = mk(0, 10*time.Second, nil)
	rt.record("r", time.Millisecond, 200, t0.Add(2*time.Hour))
	now = t0.Add(2*time.Hour + 5*time.Second)
	_, n, _ = rt.window("r", 20*time.Second, now)
	s = rt.span("r", 20*time.Second, time.Time{}, now)
	t.Logf("after long idle: n=%d span=%v", n, s)
	// since with from inside the current bucket
	rt = mk(0, 100*time.Second, nil)
	now = t0.Add(105 * time.Second)
	st = rt.since("r", t0.Add(102*time.Second), now)
	ss = rt.span("r", 0, t0.Add(102*time.Second), now)
	t.Logf("since inside cur: n=%d span=%v", st.Requests, ss)
}
