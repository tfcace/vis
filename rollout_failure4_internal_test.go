package vis

// These tests came from a fourth review of the fleet layer, each a reproducer of a defect it found.

import (
	"testing"
	"time"
)

// With no rest to compare with, a route going quiet may be a lull: it can't
// be judged, rather than harm that undoes the change.
func TestALoneMembersLullIsNotHarm(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 500, "GET /k errors": 0}, After: map[string]float64{"cpu_used": 0.01}}
	if harm, _, _ := gate(im, objs, "b"); harm != "" {
		t.Errorf("a lone member's lull read as harm: %q", harm)
	}
}
