package vis

// These tests came from a sixth review of the fleet layer, each a reproducer of a defect it found.

import (
	"strings"
	"testing"
	"time"
)

func TestTheVerdictWeighsTheRest(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	// The rest got just as much better: nothing attributable to the change.
	im := Impact{
		Before: map[string]float64{"GET /k p99": 2800, "GET /k requests": 500, "GET /k errors": 0},
		After:  map[string]float64{"GET /k p99": 18, "GET /k requests": 500, "GET /k errors": 0},
		Rest: &Rest{Members: []string{"a", "c"},
			Before: map[string]float64{"GET /k p99": 2800, "GET /k requests": 1000},
			After:  map[string]float64{"GET /k p99": 17, "GET /k requests": 1000}}}
	harm, unjudged, ev := gate(im, objs, "b")
	if strings.HasPrefix(ev, "helped") || !strings.Contains(ev, "the rest moved as much") {
		t.Errorf("the rest got as much better: evidence %q; want no clear change, saying the rest moved as much", ev)
	}

	// Confounded: stopped as unjudged, but the evidence's first word says helped.
	im2 := im
	im2.Rest = nil
	im2.ConfoundedBy = []Confounder{{What: "compress_level changed (9 → 1) by human:sam", Kind: "change", Layer: "human"}}
	harm, unjudged, ev = gate(im2, objs, "b")
	if strings.HasPrefix(ev, "helped") {
		t.Errorf("confounded, so unjudged (%q), yet the evidence says %q", unjudged, ev)
	}

	// Two routes: one 25% better, one 10% worse (under the harm bar): what word?
	objs2 := []sloDef{{"GET /k", 40 * time.Millisecond}, {"PUT /k", 40 * time.Millisecond}}
	im3 := Impact{
		Before: map[string]float64{"GET /k p99": 20, "GET /k requests": 500, "PUT /k p99": 20, "PUT /k requests": 500},
		After:  map[string]float64{"GET /k p99": 15, "GET /k requests": 500, "PUT /k p99": 22, "PUT /k requests": 500},
	}
	harm, unjudged, ev = gate(im3, objs2, "b")
	if !strings.HasPrefix(ev, "no clear change") {
		t.Errorf("one route better, one worse: evidence %q; want no clear change", ev)
	}

	// One route judged and helped, another too quiet to judge: helped?
	im4 := Impact{
		Before: map[string]float64{"GET /k p99": 20, "GET /k requests": 500, "PUT /k p99": 20, "PUT /k requests": 10},
		After:  map[string]float64{"GET /k p99": 15, "GET /k requests": 500, "PUT /k p99": 900, "PUT /k requests": 10},
	}
	harm, unjudged, ev = gate(im4, objs2, "b")
	_ = harm
	_ = unjudged
}

func TestTheFleetLineSaysWhenNoRequestsCame(t *testing.T) {
	before := FleetReport{SLOs: []FleetSLO{{Route: "GET /k", Objective: 40 * time.Millisecond, P99: 2300 * time.Millisecond, Requests: 900}}, Covers: "2 of 3 members; can't vouch for c"}
	after := FleetReport{SLOs: []FleetSLO{{Route: "GET /k", Objective: 40 * time.Millisecond, Requests: 0, Met: true}}, Covers: "3 of 3 members"}
	if r := fleetRow(before, after); !strings.Contains(r, "no requests") || strings.Contains(r, "meets") || !strings.Contains(r, "2 of 3") {
		t.Errorf("fleet row %q; want no requests after said as such, and the before's coverage where it differs", r)
	}
}

func TestAPendingMeasurementSaysSo(t *testing.T) {
	im := Impact{Before: map[string]float64{"GET /k p99": 2300, "GET /k requests": 500, "cpu_used": .25, "throttled": 1}, Summary: "Still measuring, 2s late"}
	if r := memberRow("c", im, []sloDef{{"GET /k", 40 * time.Millisecond}}); !strings.Contains(r, "still measuring") {
		t.Errorf("pending row %q; want it to say it is still being measured", r)
	}
}
