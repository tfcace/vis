package vis

// These tests came from a fifth review of the fleet layer, each a reproducer of a defect it found.

import (
	"strings"
	"testing"
	"time"
)

// F6: a lone member's route that went from 500 requests to none is "too few
// requests to judge", and the rollout adds "no harm seen": the drop is never named.
func TestALoneMembersDropIsNamed(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 500, "GET /k errors": 0}, After: map[string]float64{"GET /k requests": 0, "cpu_used": 0.01}}
	harm, unjudged, evidence := gate(im, objs, "b")
	if harm != "" || !strings.Contains(unjudged, "500 requests in the window before, 0 after") {
		t.Errorf("harm=%q unjudged=%q evidence=%q; want can't-judge, naming the drop", harm, unjudged, evidence)
	}
}
