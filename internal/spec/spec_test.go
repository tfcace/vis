package spec

import "testing"

// A fixed step lurches where one step cuts the knob by more than half, within
// the rule's reach from the default.
func TestLurchFindsTheStepThatCutsMoreThanHalf(t *testing.T) {
	cases := []struct {
		why                  string
		def, lo, step, drift int64
		from, to             int64
		lurches              bool
	}{
		{"cacheproxy's batch: 64, 48, 32, 16, then 1", 64, 1, -16, 0, 16, 1, true},
		{"a drift that stops at 16", 64, 1, -16, 48, 0, 0, false},
		{"a quality nowhere near its floor", 95, 1, -5, 0, 0, 0, false},
		{"a cut to zero", 30, 0, -10, 0, 10, 0, true},
		{"a step that grows the knob", 4, 1, 16, 0, 0, 0, false},
		{"a range below zero, such as a log level", 4, -4, -2, 0, 0, 0, false},
	}
	for _, c := range cases {
		from, to, ok := Lurch(c.def, c.lo, c.step, c.drift)
		if ok != c.lurches || from != c.from || to != c.to {
			t.Errorf("%s: Lurch = %d → %d, %v; want %d → %d, %v", c.why, from, to, ok, c.from, c.to, c.lurches)
		}
	}
}
