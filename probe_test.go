package vis_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// a cache: every request looks up, odd keys miss and go to a slow origin
func probedService(in *vis.Instance) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k/{key}", func(w http.ResponseWriter, r *http.Request) {
		done := vis.Probe(r.Context(), "lookup")
		time.Sleep(time.Millisecond)
		done()
		if n, _ := strconv.Atoi(r.PathValue("key")); n%2 == 1 {
			defer vis.Probe(r.Context(), "origin")()
			time.Sleep(20 * time.Millisecond)
		}
	})
	return in.HTTP(mux)
}

// Probes split a route's latency by where requests spend it, and group
// requests by the probes they ran: a hit and a miss, with nothing else declared.
func TestProbesBreakARouteDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		h := probedService(in)
		for i := range 100 {
			serve(h, "GET", "/k/"+strconv.Itoa(i))
		}
		b := in.Breakdown("GET /k/{key}", time.Minute)
		if len(b.Probes) != 2 || b.Probes[0].Probe != "lookup" || b.Probes[0].Requests != 100 || b.Probes[0].Share != 1 {
			t.Fatalf("probes = %+v, want lookup on every request first", b.Probes)
		}
		origin := b.Probes[1]
		if origin.Probe != "origin" || origin.Requests != 50 || origin.Share != .5 || !near(origin.P99, 20*time.Millisecond) {
			t.Fatalf("origin = %+v, want half the requests at about 20 ms", origin)
		}
		if len(b.Paths) != 2 {
			t.Fatalf("paths = %+v, want hits and misses", b.Paths)
		}
		for _, p := range b.Paths {
			want := time.Millisecond
			if slices.Equal(p.Probes, []string{"lookup", "origin"}) {
				want = 21 * time.Millisecond
			} else if !slices.Equal(p.Probes, []string{"lookup"}) {
				t.Fatalf("path %v, want lookup or lookup then origin", p.Probes)
			}
			if p.Requests != 50 || p.Share != .5 || !near(p.P99, want) {
				t.Fatalf("path %v = %+v, want half the requests at about %v", p.Probes, p, want)
			}
		}
	})
}

// A probe run more than once in a request counts its total time, once.
func TestAProbeRunTwiceSums(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /twice", func(w http.ResponseWriter, r *http.Request) {
			for range 2 {
				done := vis.Probe(r.Context(), "retry")
				time.Sleep(10 * time.Millisecond)
				done()
			}
		})
		serve(in.HTTP(mux), "GET", "/twice")
		b := in.Breakdown("GET /twice", time.Minute)
		if len(b.Probes) != 1 || b.Probes[0].Requests != 1 || !near(b.Probes[0].P99, 20*time.Millisecond) {
			t.Fatalf("probes = %+v, want one request with 20 ms in retry", b.Probes)
		}
		if len(b.Paths) != 1 || !slices.Equal(b.Paths[0].Probes, []string{"retry"}) {
			t.Fatalf("paths = %+v, want retry once", b.Paths)
		}
	})
}

// Outside a request a probe does nothing, so library code can always call it.
func TestProbeOutsideARequestIsANoop(t *testing.T) {
	vis.Probe(context.Background(), "background")()
}

// Paths keep the order probes started in, so a probe that wraps another comes first.
func TestNestedProbesKeepTheirStartOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /n", func(w http.ResponseWriter, r *http.Request) {
			defer vis.Probe(r.Context(), "lookup")()
			defer vis.Probe(r.Context(), "origin")()
			time.Sleep(time.Millisecond)
		})
		serve(in.HTTP(mux), "GET", "/n")
		b := in.Breakdown("GET /n", time.Minute)
		if len(b.Paths) != 1 || !slices.Equal(b.Paths[0].Probes, []string{"lookup", "origin"}) || b.Probes[0].Probe != "lookup" {
			t.Fatalf("breakdown = %+v, want lookup then origin", b)
		}
	})
}

// Each route keeps its own probes, and too many probe names on one route
// (built from input, say) fold into "other" there without touching the rest.
func TestProbesAreKeptPerRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /r/{n}", func(w http.ResponseWriter, r *http.Request) {
			for p := range 8 {
				vis.Probe(r.Context(), "step"+strconv.Itoa(p))()
			}
		})
		mux.HandleFunc("GET /host/{h}", func(w http.ResponseWriter, r *http.Request) {
			vis.Probe(r.Context(), "origin "+r.PathValue("h"))()
		})
		h := in.HTTP(mux, vis.RouteName(func(r *http.Request) string { // 40 routes /r/0 to /r/39, and one host route
			if strings.HasPrefix(r.URL.Path, "/host/") {
				return r.Pattern
			}
			return r.URL.Path
		}))
		for i := range 100 {
			serve(h, "GET", "/host/"+strconv.Itoa(i))
		}
		for i := range 40 {
			serve(h, "GET", "/r/"+strconv.Itoa(i))
		}
		for i := range 40 {
			if b := in.Breakdown("/r/"+strconv.Itoa(i), time.Minute); len(b.Probes) != 8 {
				t.Fatalf("route %d: %d probes, want all 8", i, len(b.Probes))
			}
		}
		b := in.Breakdown("GET /host/{h}", time.Minute)
		if len(b.Probes) > 16 || b.Probes[len(b.Probes)-1].Probe != "other" || len(b.Paths) > 16 {
			t.Fatalf("host route: %d probes, %d paths, want at most 16 each with the rest as other", len(b.Probes), len(b.Paths))
		}
	})
}
