package vis

// These tests came from a seventh review of the fleet layer, each a reproducer of a defect it found.

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// A route that collapses from 20 rps to 1.5 rps on the canary (stragglers),
// while the rest keep their load. Before span 30 s (600 requests), after span 10 s (15).
func TestARouteCollapsedToItsStragglersIsHarm(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{
		Before: map[string]float64{"GET /k p99": 20, "GET /k requests": 600, "GET /k rps": 20, "GET /k errors": 0},
		After:  map[string]float64{"GET /k p99": 9000, "GET /k requests": 15, "GET /k rps": 1.5, "GET /k errors": 0},
		Rest: &Rest{Members: []string{"a", "c"},
			Before: map[string]float64{"GET /k p99": 20, "GET /k requests": 1000},
			After:  map[string]float64{"GET /k p99": 20, "GET /k requests": 1000}},
		Done: true,
	}
	harm, unjudged, ev := gate(im, objs, "b")
	t.Logf("harm=%q\nunjudged=%q\nevidence=%q", harm, unjudged, ev)
	if harm == "" {
		t.Errorf("a 93%% collapse in requests on the canary, the rest steady: not harm")
	}
	if strings.Contains(ev, "no other member to compare with") {
		t.Errorf("evidence says no other member to compare with, with a and c holding 1000 requests")
	}
	// Same impact without rps keys (the parent commit's view):
	delete(im.Before, "GET /k rps")
	delete(im.After, "GET /k rps")
	harm, unjudged, _ = gate(im, objs, "b")
	t.Logf("without rates: harm=%q unjudged=%q", harm, unjudged)
}

// Poisson traffic at a steady rate, a change that does nothing, the rest
// steady: how often does the gate call it harm (stopped answering)?
func TestAQuietRoutesNoiseIsntHarm(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	pois := func(r *rand.Rand, l float64) float64 {
		// Knuth for small l; normal approx otherwise
		if l > 60 {
			v := l + r.NormFloat64()*sqrt(l)
			if v < 0 {
				v = 0
			}
			return float64(int(v + .5))
		}
		L, k, p := exp(-l), 0.0, 1.0
		for {
			p *= r.Float64()
			if p <= L {
				return k
			}
			k++
		}
	}
	for _, lambda := range []float64{0.7, 1, 1.5, 2, 3} {
		r := rand.New(rand.NewPCG(1, uint64(lambda*10)))
		harms, stopped, n := 0, 0, 20000
		for i := 0; i < n; i++ {
			phase := r.Float64() * 10 // change at this offset into its bucket
			spanB := 20 + phase       // window(20s) at At reaches back to the start of the bucket holding At-20
			spanA := 20 - phase       // buckets that began at or after At+10, up to At+30
			if phase == 0 {
				spanA = 20
			}
			nb := pois(r, lambda*spanB)
			na := pois(r, lambda*spanA)
			im := Impact{
				Before: map[string]float64{"GET /k p99": 10, "GET /k requests": nb, "GET /k errors": 0},
				After:  map[string]float64{"GET /k p99": 10, "GET /k errors": 0},
				Rest: &Rest{Members: []string{"a", "c"},
					Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 2 * lambda * 20},
					After:  map[string]float64{"GET /k p99": 10, "GET /k requests": 2 * lambda * 20}},
				Done: true,
			}
			if nb > 0 {
				im.Before["GET /k rps"] = nb / spanB
			}
			if na > 0 {
				im.After["GET /k requests"] = na
				im.After["GET /k rps"] = na / spanA
			} else {
				delete(im.After, "GET /k p99")
			}
			harm, unjudged, _ := gate(im, objs, "b")
			if harm != "" {
				harms++
			} else if unjudged != "" {
				stopped++
			}
		}
		if float64(harms)/float64(n) > .01 {
			t.Errorf("%.1f rps: a no-op read as harm (undone) in %.1f%% of windows", lambda, 100*float64(harms)/float64(n))
		}
	}
}
