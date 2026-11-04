package vis

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// MaxRoutes caps how many routes an instance tracks. Route names can come from
// request paths, which are user input, so requests beyond the cap count under "other".
const MaxRoutes = 256

const (
	bucketWidth = 10 * time.Second // the UI's strip: one bucket per 10 seconds
	keepFull    = 30               // full histograms kept, so windows reach back 5 minutes
	keepHistory = 360              // summaries kept: one hour
)

// unmatchedRoute is the route requests no router matched count under.
const unmatchedRoute = "unmatched"

// HTTPOption configures Instance.HTTP.
type HTTPOption func(*httpOptions)

type httpOptions struct {
	route func(*http.Request) string
}

// RouteName names a request's route for routers that don't fill in
// http.Request.Pattern, for example chi's RouteContext(r.Context()).RoutePattern().
// It runs after the handler, when routing is done.
func RouteName(fn func(*http.Request) string) HTTPOption {
	return func(o *httpOptions) { o.route = fn }
}

// HTTP wraps h so every request carries one snapshot of the knobs in its
// context, read with Int.From and Duration.From, and is timed per route.
// Routes are named by http.ServeMux patterns by default ("GET /k/{key}"), so
// it works with the standard library and any router built on net/http.
func (in *Instance) HTTP(h http.Handler, opts ...HTTPOption) http.Handler {
	o := httpOptions{route: func(r *http.Request) string { return r.Pattern }}
	for _, opt := range opts {
		opt(&o)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, end := in.StartRequest(r.Context())
		r = r.WithContext(ctx) // the router fills in r.Pattern on this request
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		panicked := true
		defer func() {
			if panicked {
				sw.status = http.StatusInternalServerError // net/http will log it and drop the connection
			}
			end(o.route(r), sw.status)
		}()
		h.ServeHTTP(sw, r)
		panicked = false
	})
}

// StartRequest is what framework adapters build on: it puts a snapshot in ctx
// and returns a function to call when the request is done, with the route and
// the status. Instance.HTTP is StartRequest plus a ResponseWriter that records the status.
func (in *Instance) StartRequest(ctx context.Context) (context.Context, func(route string, status int)) {
	start := time.Now()
	rc := &requestCtx{Context: WithSnapshot(ctx, in.Snapshot())}
	rp := &rc.probes
	return rc, func(route string, status int) {
		now := time.Now()
		d := now.Sub(start)
		in.recordProbes(in.routes.record(route, d, status, now), rp, d, status, now)
	}
}

type snapshotKey struct{}

// WithSnapshot puts a snapshot in ctx, for code that starts work outside a
// request, such as a background worker that wants one consistent view.
func WithSnapshot(ctx context.Context, c *Config) context.Context {
	return context.WithValue(ctx, snapshotKey{}, c)
}

// SnapshotFrom returns the snapshot a request carries.
func SnapshotFrom(ctx context.Context) (*Config, bool) {
	c, ok := ctx.Value(snapshotKey{}).(*Config)
	return c, ok && c != nil
}

// From reads the knob from the snapshot the request carries, so a request
// sees one consistent set of values. Outside a request vis served, it reads
// the value now in the default instance, and vis reports the reads with how
// to fix them (Instance.Warnings, the UI, MCP, and once in the log).
func (k Int) From(ctx context.Context) int { return k.Get(snapshotFor(ctx, k.decl().name)) }

// From reads the knob from the snapshot the request carries. See Int.From.
func (k Duration) From(ctx context.Context) time.Duration {
	return k.Get(snapshotFor(ctx, k.decl().name))
}

// statusWriter records the status code. It keeps Flush working, and Unwrap
// lets http.ResponseController reach the rest (hijacking, deadlines).
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RouteStats summarizes one route over a window.
type RouteStats struct {
	Route         string
	Requests      int64
	Errors        int64 // responses with a 5xx status, panics included
	Refused       int64 // responses with a 429: the service turned them away, fast, so they pull p99 down
	P50, P95, P99 time.Duration
}

// Bucket is one 10-second slice of a route's history.
type Bucket struct {
	Start         time.Time
	Requests      int64
	Errors        int64
	Refused       int64
	P50, P95, P99 time.Duration
}

// Route summarizes one route over the last window, up to 5 minutes.
func (in *Instance) Route(route string, window time.Duration) RouteStats {
	return in.routes.stats(route, window, time.Now())
}

// Routes summarizes every route seen in the last window.
func (in *Instance) Routes(window time.Duration) []RouteStats {
	now := time.Now()
	var out []RouteStats
	for _, name := range in.routes.names() {
		if s := in.routes.stats(name, window, now); s.Requests > 0 {
			out = append(out, s)
		}
	}
	return out
}

// RouteHistory is a route's last hour in 10-second buckets, oldest first.
func (in *Instance) RouteHistory(route string) []Bucket {
	return in.routes.history(route, time.Now())
}

// routeTable holds per-route recorders.
type routeTable struct {
	max    int // MaxRoutes when zero
	mu     sync.Mutex
	byName map[string]*recorder
	order  []string
}

func (t *routeTable) get(name string, create bool) *recorder {
	t.mu.Lock()
	defer t.mu.Unlock()
	if name == "" {
		name = unmatchedRoute
	}
	if r := t.byName[name]; r != nil || !create {
		return r
	}
	limit := t.max
	if limit == 0 {
		limit = MaxRoutes
	}
	if len(t.byName) >= limit && name != "other" {
		if r := t.byName["other"]; r != nil {
			return r
		}
		name = "other"
	}
	if t.byName == nil {
		t.byName = map[string]*recorder{}
	}
	r := &recorder{name: name}
	t.byName[name] = r
	t.order = append(t.order, name)
	return r
}

func (t *routeTable) names() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.order...)
}

// record times a request and returns the name it was filed under: "other" past the cap.
func (t *routeTable) record(route string, d time.Duration, status int, now time.Time) string {
	r := t.get(route, true)
	r.record(d, status, now)
	return r.name
}

func (t *routeTable) stats(route string, window time.Duration, now time.Time) RouteStats {
	if route == "" {
		route = unmatchedRoute
	}
	r := t.get(route, false)
	if r == nil {
		return RouteStats{Route: route}
	}
	return r.stats(window, now)
}

func (t *routeTable) history(route string, now time.Time) []Bucket {
	r := t.get(route, false)
	if r == nil {
		return nil
	}
	return r.history(now)
}

// recorder is one route: the current bucket, the last 5 minutes in full, and
// an hour of summaries. Buckets rotate when a request or a query arrives, so
// no goroutine ticks for an idle route.
type recorder struct {
	mu      sync.Mutex
	name    string
	start   time.Time // start of the current bucket
	cur     slice
	full    []slice  // newest last, at most keepFull
	summary []Bucket // newest last, at most keepHistory
}

type slice struct {
	start    time.Time
	h        hist
	requests int64
	errors   int64 // 5xx
	refused  int64 // 429
}

func (r *recorder) record(d time.Duration, status int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotate(now)
	r.cur.h.add(d)
	r.cur.requests++
	switch {
	case status >= 500:
		r.cur.errors++
	case status == http.StatusTooManyRequests:
		r.cur.refused++
	}
}

// rotate closes the current bucket if now is past it, filling any gap with empty buckets.
func (r *recorder) rotate(now time.Time) {
	b := now.Truncate(bucketWidth)
	if r.start.IsZero() {
		r.start, r.cur.start = b, b
		return
	}
	for r.start.Before(b) {
		r.full = append(r.full, r.cur)
		if len(r.full) > keepFull {
			r.full = r.full[1:]
		}
		r.summary = append(r.summary, r.cur.bucket())
		if len(r.summary) > keepHistory {
			r.summary = r.summary[1:]
		}
		r.start = r.start.Add(bucketWidth)
		if gap := b.Sub(r.start); gap > keepHistory*bucketWidth { // a long idle: skip ahead
			r.start = b
		}
		r.cur = slice{start: r.start}
	}
}

func (s *slice) bucket() Bucket {
	return Bucket{Start: s.start, Requests: s.requests, Errors: s.errors, Refused: s.refused,
		P50: s.h.quantile(.50), P95: s.h.quantile(.95), P99: s.h.quantile(.99)}
}

func (r *recorder) stats(window time.Duration, now time.Time) RouteStats {
	merged := r.window(window, now)
	b := merged.bucket()
	return RouteStats{Route: r.name, Requests: b.Requests, Errors: b.Errors, Refused: b.Refused, P50: b.P50, P95: b.P95, P99: b.P99}
}

// window merges the buckets that reach into the last window, the current one included.
func (r *recorder) window(window time.Duration, now time.Time) slice {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotate(now)
	merged := r.cur
	from := now.Add(-min(window, keepFull*bucketWidth))
	for i := len(r.full) - 1; i >= 0 && !r.full[i].start.Add(bucketWidth).Before(from); i-- {
		merged.h.merge(&r.full[i].h)
		merged.requests += r.full[i].requests
		merged.errors += r.full[i].errors
		merged.refused += r.full[i].refused
	}
	return merged
}

// window is a route's histogram over the last window, for merging across a fleet.
func (t *routeTable) window(route string, window time.Duration, now time.Time) (hist, int64, int64) {
	r := t.get(route, false)
	if r == nil {
		return hist{}, 0, 0
	}
	s := r.window(window, now)
	return s.h, s.requests, s.errors
}

// span is how long the buckets a read takes in cover, from the start of the
// earliest to now: a window's (from zero) reaches back to the start of every
// bucket it touches, since's (from set) only takes buckets that began at or
// after from. Requests over it are a rate two reads can compare.
func (t *routeTable) span(route string, window time.Duration, from, now time.Time) time.Duration {
	r := t.get(route, false)
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotate(now)
	earliest := now
	if from.IsZero() || !r.cur.start.Before(from) {
		earliest = r.cur.start
	}
	if from.IsZero() {
		from = now.Add(-min(window, keepFull*bucketWidth))
		for i := len(r.full) - 1; i >= 0 && !r.full[i].start.Add(bucketWidth).Before(from); i-- {
			earliest = r.full[i].start
		}
	} else {
		for i := len(r.full) - 1; i >= 0 && !r.full[i].start.Before(from); i-- {
			earliest = r.full[i].start
		}
	}
	return now.Sub(earliest)
}

// since summarizes the requests in buckets that began at or after t, so none
// of them were served before it. Nothing yet means no requests.
func (t *routeTable) since(route string, from, now time.Time) RouteStats {
	r := t.get(route, false)
	if r == nil {
		return RouteStats{Route: route}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotate(now)
	var merged slice
	if !r.cur.start.Before(from) {
		merged.h.merge(&r.cur.h)
		merged.requests, merged.errors, merged.refused = r.cur.requests, r.cur.errors, r.cur.refused
	}
	for i := len(r.full) - 1; i >= 0 && !r.full[i].start.Before(from); i-- {
		merged.h.merge(&r.full[i].h)
		merged.requests += r.full[i].requests
		merged.errors += r.full[i].errors
		merged.refused += r.full[i].refused
	}
	b := merged.bucket()
	return RouteStats{Route: r.name, Requests: b.Requests, Errors: b.Errors, Refused: b.Refused, P50: b.P50, P95: b.P95, P99: b.P99}
}

func (r *recorder) history(now time.Time) []Bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotate(now)
	return append(append([]Bucket(nil), r.summary...), r.cur.bucket())
}
