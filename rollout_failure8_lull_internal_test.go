package vis

// These tests came from an eighth review of the fleet layer, each a reproducer of a defect it found.

import (
	"testing"
	"time"
)

// The whole fleet's load falls to a fifth (a client backing off, a load phase ending);
// the change does nothing. The canary and the rest fall alike.
func TestAFleetWideLullIsntHarm(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Done: true,
		Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 600, "GET /k rps": 20, "GET /k errors": 0},
		After:  map[string]float64{"GET /k p99": 10, "GET /k requests": 40, "GET /k rps": 4, "GET /k errors": 0},
		Rest: &Rest{Members: []string{"a", "c"},
			Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 1200, "GET /k errors": 0},
			After:  map[string]float64{"GET /k p99": 10, "GET /k requests": 240, "GET /k errors": 0}}}
	harm, unj, ev := gate(im, objs, "b")
	t.Logf("harm=%q\nunjudged=%q\nevidence=%q", harm, unj, ev)
	if harm != "" {
		t.Errorf("the rest fell to a fifth as the canary did, yet: %q", harm)
	}
}
