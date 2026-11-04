package vis

// These tests came from a seventh review of the fleet layer, each a reproducer of a defect it found.

import (
	"strings"
	"testing"
	"time"
)

func TestHelpedMeansTheCanaryItselfGotBetter(t *testing.T) {
	objs := []sloDef{{"GET /k", 300 * time.Millisecond}}
	for _, c := range []struct {
		b, a, rb, ra float64
		want         string
	}{
		{100, 110, 100, 200, "held"},   // canary 10% worse, rest doubled: not helped
		{100, 100, 100, 160, "held"},   // canary flat, rest +60%
		{100, 90, 100, 150, "helped"},  // canary 10% better, rest +50%
		{400, 250, 400, 600, "helped"}, // canary into objective, rest worse
		{400, 290, 400, 100, "no clear change (the rest moved as much or more)"}, // canary into objective, rest much better
	} {
		im := Impact{Done: true,
			Before: map[string]float64{"GET /k p99": c.b, "GET /k requests": 500, "GET /k rps": 20},
			After:  map[string]float64{"GET /k p99": c.a, "GET /k requests": 400, "GET /k rps": 20},
			Rest: &Rest{Members: []string{"a", "c"},
				Before: map[string]float64{"GET /k p99": c.rb, "GET /k requests": 1000},
				After:  map[string]float64{"GET /k p99": c.ra, "GET /k requests": 1000}}}
		harm, unj, ev := gate(im, objs, "b")
		if harm != "" || unj != "" || !strings.HasPrefix(ev, c.want) {
			t.Errorf("%v: harm=%q unjudged=%q evidence=%q; want it to begin %q", c, harm, unj, ev, c.want)
		}
	}
}
