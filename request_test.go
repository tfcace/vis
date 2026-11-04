package vis_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHandlersReadKnobsFromTheRequest(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, By: sam})
	h := in.HTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, cacheSize.From(r.Context()), " ", timeout.From(r.Context()))
	}))
	if got := serve(h, "GET", "/").Body.String(); got != "2000 2s" {
		t.Fatalf("body = %q", got)
	}
}

// A request sees one snapshot from start to end, even if a knob changes meanwhile.
func TestARequestSeesOneSnapshotThroughout(t *testing.T) {
	in := vis.NewInstance()
	h := in.HTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		before := cacheSize.From(r.Context())
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 5000, By: sam})
		fmt.Fprint(w, before, " ", cacheSize.From(r.Context()))
	}))
	if got := serve(h, "GET", "/").Body.String(); got != "1000 1000" {
		t.Fatalf("body = %q, want the same value twice", got)
	}
}

// A knob read outside a request vis served reads its value now, and vis says
// so where people and agents look, with how to fix it: no request panics.
func TestReadingAKnobWithoutASnapshotWorksAndSaysHowToFixIt(t *testing.T) {
	k := vis.NewInt(fmt.Sprintf("test_unwrapped_%d", lateN.Add(1)), vis.Range(1, 100), vis.Default(7))
	for range 3 {
		if got := k.From(t.Context()); got != 7 {
			t.Fatalf("From without a snapshot = %d, want the value now", got)
		}
	}
	if n := vis.UnwrappedReads()[k.Name()]; n != 3 {
		t.Fatalf("unwrapped reads = %d, want 3", n)
	}
	var found bool
	for _, w := range vis.DefaultInstance().Warnings() { // the instance the reads used
		found = found || strings.Contains(w, k.Name()) && strings.Contains(w, "vis.HTTP") && strings.Contains(w, "3 times")
	}
	if !found {
		t.Fatalf("warnings = %q, want one naming %s, its reads and the fix", vis.DefaultInstance().Warnings(), k.Name())
	}
}

func TestRequestsAreTimedPerRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /k/{key}", func(w http.ResponseWriter, r *http.Request) { time.Sleep(10 * time.Millisecond) })
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { time.Sleep(time.Millisecond) })
		h := in.HTTP(mux)
		for i := range 100 {
			serve(h, "GET", "/k/"+strconv.Itoa(i))
			serve(h, "GET", "/health")
		}
		k := in.Route("GET /k/{key}", time.Minute)
		if k.Requests != 100 || !near(k.P99, 10*time.Millisecond) || !near(k.P50, 10*time.Millisecond) {
			t.Fatalf("/k = %+v, want 100 requests at about 10ms", k)
		}
		if hl := in.Route("GET /health", time.Minute); hl.Requests != 100 || !near(hl.P99, time.Millisecond) {
			t.Fatalf("/health = %+v, want 100 requests at about 1ms", hl)
		}
		names := map[string]bool{}
		for _, r := range in.Routes(time.Minute) {
			names[r.Route] = true
		}
		if !names["GET /k/{key}"] || !names["GET /health"] {
			t.Fatalf("routes = %v", names)
		}
	})
}

func TestServerErrorsAndPanicsCount(t *testing.T) {
	in := vis.NewInstance()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusBadGateway) })
	mux.HandleFunc("GET /missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	h := in.HTTP(mux)
	serve(h, "GET", "/fail")
	serve(h, "GET", "/missing")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed; it must reach net/http as before")
			}
		}()
		serve(h, "GET", "/boom")
	}()
	if r := in.Route("GET /fail", time.Minute); r.Errors != 1 {
		t.Fatalf("/fail = %+v, want 1 error", r)
	}
	if r := in.Route("GET /missing", time.Minute); r.Errors != 0 || r.Requests != 1 {
		t.Fatalf("/missing = %+v, want a 404 counted as a request, not a server error", r)
	}
	if r := in.Route("GET /boom", time.Minute); r.Errors != 1 {
		t.Fatalf("/boom = %+v, want the panic counted as an error", r)
	}
}

func TestRouteNamesComeFromTheRouterOrAnOption(t *testing.T) {
	in := vis.NewInstance()
	serve(in.HTTP(http.NewServeMux()), "GET", "/nothing-here")
	if r := in.Route("unmatched", time.Minute); r.Requests != 1 {
		t.Fatalf("unmatched = %+v, want the request counted under unmatched", r)
	}
	h := in.HTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		vis.RouteName(func(r *http.Request) string { return "chi:/users/{id}" }))
	serve(h, "GET", "/users/7")
	if r := in.Route("chi:/users/{id}", time.Minute); r.Requests != 1 {
		t.Fatalf("custom route = %+v", r)
	}
}

// Request paths are user input; they must not grow the route table without bound.
func TestRouteCountIsCapped(t *testing.T) {
	in := vis.NewInstance()
	h := in.HTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		vis.RouteName(func(r *http.Request) string { return r.URL.Path }))
	for i := range vis.MaxRoutes + 50 {
		serve(h, "GET", "/u/"+strconv.Itoa(i))
	}
	routes := in.Routes(time.Minute)
	if len(routes) != vis.MaxRoutes+1 {
		t.Fatalf("got %d routes, want %d plus other", len(routes), vis.MaxRoutes)
	}
	if r := in.Route("other", time.Minute); r.Requests != 50 {
		t.Fatalf("other = %+v, want the 50 overflow requests", r)
	}
}

func TestRouteHistoryKeepsTenSecondBuckets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /k", func(http.ResponseWriter, *http.Request) {})
		h := in.HTTP(mux)
		for range 3 { // three buckets of 5, 5 and 5 requests
			for range 5 {
				serve(h, "GET", "/k")
			}
			time.Sleep(10 * time.Second)
		}
		hist := in.RouteHistory("GET /k")
		if len(hist) != 4 || hist[3].Requests != 0 {
			t.Fatalf("history = %+v, want 3 closed buckets and the current, empty one", hist)
		}
		for _, b := range hist[:3] {
			if b.Requests != 5 {
				t.Fatalf("history = %+v, want 5 requests in each of the last 3 buckets", hist)
			}
		}
	})
}

func TestTheWrappedWriterStillFlushes(t *testing.T) {
	in := vis.NewInstance()
	h := in.HTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("the middleware hid http.Flusher; streaming handlers would break")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
	}))
	serve(h, "GET", "/")
}

// near allows the histogram's resolution, about 1 part in 8.
func near(got, want time.Duration) bool {
	d := got - want
	return d >= -want/8 && d <= want/8
}

// Knob reads outside a request warn on the default instance, which they read,
// not on an instance of a test's own.
func TestUnwrappedReadsWarnOnTheDefaultInstanceOnly(t *testing.T) {
	k := vis.NewInt(uniq("test_unwrapped_own"), vis.Range(1, 100), vis.Default(7))
	k.From(t.Context())
	for _, w := range vis.NewInstance().Warnings() {
		if strings.Contains(w, k.Name()) {
			t.Fatalf("a test's own instance warns about reads of the default one: %q", w)
		}
	}
}
