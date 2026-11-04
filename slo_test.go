package vis_test

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

func TestTheInstanceKnowsItsServiceAndSLOs(t *testing.T) {
	in := vis.NewInstance(vis.Service("cacheproxy"), vis.SLO("GET /k/{key}", 50*time.Millisecond), vis.SLO("GET /health", 5*time.Millisecond))
	if in.Service() != "cacheproxy" {
		t.Fatalf("service = %q", in.Service())
	}
	s := in.SLOs()
	if len(s) < 2 || s[0].Route != "GET /k/{key}" || s[0].P99 != 50*time.Millisecond || s[1].Route != "GET /health" {
		t.Fatalf("SLOs = %+v, want both first, in the order given", s)
	}
}

func TestSLOStatusJudgesTheLastMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /slow", 50*time.Millisecond), vis.SLO("GET /idle", 50*time.Millisecond))
		mux := http.NewServeMux()
		mux.HandleFunc("GET /slow", func(http.ResponseWriter, *http.Request) { time.Sleep(200 * time.Millisecond) })
		h := in.HTTP(mux)
		for range 10 {
			serve(h, "GET", "/slow")
		}
		s := in.SLOs()
		if s[0].Met || !near(s[0].Now, 200*time.Millisecond) || s[0].Requests != 10 {
			t.Fatalf("slow = %+v, want missed at about 200ms", s[0])
		}
		if !s[1].Met || s[1].Requests != 0 {
			t.Fatalf("idle = %+v, want met with no requests", s[1])
		}
	})
}

func TestAnUnnamedServiceHasADefault(t *testing.T) {
	if got := vis.NewInstance().Service(); got != "service" {
		t.Fatalf("service = %q, want a neutral default", got)
	}
}

// Right after recovery the SLO reads as met; the minute's figure still remembers the bad part.
func TestSLOStatusCatchesUpAfterRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		slow := true
		mux := http.NewServeMux()
		mux.HandleFunc("GET /k", func(http.ResponseWriter, *http.Request) {
			if slow {
				time.Sleep(200 * time.Millisecond)
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		})
		h := in.HTTP(mux)
		for range 10 {
			serve(h, "GET", "/k")
		}
		time.Sleep(30 * time.Second)
		slow = false
		for range 10 {
			serve(h, "GET", "/k")
		}
		s := in.SLOs()[0]
		if !s.Met || !near(s.Now, 10*time.Millisecond) {
			t.Fatalf("after recovery: %+v, want met at about 10ms", s)
		}
		if !near(s.Minute, 200*time.Millisecond) {
			t.Fatalf("minute = %v, want it to still show the slow part", s.Minute)
		}
	})
}

// An SLO is declared beside its handler, with its percentile explicit, and
// every instance judges it, made before or after.
func TestAnSLOIsDeclaredBesideItsHandler(t *testing.T) {
	before := vis.NewInstance()
	route := "GET /" + uniq("declared")
	o := vis.NewSLO(route, vis.P99(80*time.Millisecond))
	if o.Route() != route || o.P99() != 80*time.Millisecond {
		t.Fatalf("objective = %v %v", o.Route(), o.P99())
	}
	for _, in := range []*vis.Instance{before, vis.NewInstance()} {
		found := false
		for _, s := range in.SLOs() {
			found = found || s.Route == route && s.P99 == 80*time.Millisecond
		}
		if !found {
			t.Fatalf("SLOs = %+v, want the declared one", in.SLOs())
		}
	}
}

// An SLO whose route never sees a request while other routes do is probably
// misnamed: it says so instead of reading as met forever.
func TestAnSLOOnARouteThatNeverServesSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond), vis.SLO("GET /idle", 50*time.Millisecond))
		mux := http.NewServeMux()
		mux.HandleFunc("GET /k/{key}", func(http.ResponseWriter, *http.Request) {})
		h := in.HTTP(mux)
		serve(h, "GET", "/k/a")
		s := in.SLOs()
		if !strings.Contains(s[0].Warning, `"GET /k/{key}"`) {
			t.Fatalf("warning = %q, want it to name the route that does serve", s[0].Warning)
		}
		mux.HandleFunc("GET /idle", func(http.ResponseWriter, *http.Request) {})
		serve(h, "GET", "/idle")
		if s := in.SLOs(); s[1].Warning != "" {
			t.Fatalf("a route that has served = %q, want no warning", s[1].Warning)
		}
	})
}
