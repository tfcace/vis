package vis

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/user"
	"sort"
	"strings"
	"time"
)

//go:embed ui/live.html
var uiPage []byte

// UIOption configures Instance.UI.
type UIOption func(*uiOptions)

type uiOptions struct {
	user  Writer
	hosts map[string]bool
}

// UIUser names the person using the UI. Changes made there are their pins.
// The UI listens on localhost, so the person at this machine is who acts;
// it defaults to the operating system's user name.
func UIUser(id string) UIOption { return func(o *uiOptions) { o.user = Writer{Kind: Human, ID: id} } }

// UIHosts allows Host names beyond localhost, for example a tailnet name.
// Every other Host is refused, against DNS rebinding.
func UIHosts(hosts ...string) UIOption {
	return func(o *uiOptions) {
		for _, h := range hosts {
			o.hosts[h] = true
		}
	}
}

// UI serves the control UI and its JSON API. Keep it on localhost (reach it
// through a port-forward): it acts as one person, without other auth.
//
// The API, version 1, snake_case throughout:
//
//	GET  /api/state      everything the page shows: service, started, slos, routes (with 10-second history), runtime,
//	                     signals, knobs (with stacks), rules, freeze, stories, and the raw history
//	GET  /api/events     the same as a stream (text/event-stream): a whole "state" event on connect, then
//	                     one as soon as the history records something and every 2 s, each carrying only
//	                     the newest two buckets of a route's history (replace buckets by their start)
//	POST /api/preview    {set, for, keep, reason}: what a change would do, changing nothing
//	POST /api/apply      {set, for, keep, reason}: the change, as UIUser's pin; returns change, lease and effects
//	POST /api/clear      {knob, reason}: remove UIUser's pin
//	POST /api/revert     {lease, reason}: end a lease early
//	POST /api/rule       {rule, enabled, reason}: switch a rule off or on
//	POST /api/freeze     {reason}, POST /api/unfreeze {reason}
//	POST /api/forget     {member, reason}: drop a member gone for good from the fleet
//
// Scripts may read it too, from localhost: it's the same contract the page uses.
func (in *Instance) UI(opts ...UIOption) http.Handler {
	o := uiOptions{hosts: map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}}
	if u, err := user.Current(); err == nil && u.Username != "" {
		o.user = Writer{Kind: Human, ID: u.Username}
	} else {
		o.user = Writer{Kind: Human, ID: "local"}
	}
	for _, opt := range opts {
		opt(&o)
	}
	u := &uiServer{in: in, t: &mcpTools{in: in}, o: o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(uiPage)
	})
	mux.HandleFunc("GET /api/events", u.events)
	u.api(mux)
	return &uiHandler{Handler: u.guard(u.member(mux)), user: o.user}
}

// api is the JSON API but the stream, here and for a member's page forwarding to this one.
func (u *uiServer) api(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", u.state)
	mux.HandleFunc("POST /api/preview", u.preview)
	mux.HandleFunc("POST /api/apply", u.apply)
	mux.HandleFunc("POST /api/clear", u.clear)
	mux.HandleFunc("POST /api/revert", u.revert)
	mux.HandleFunc("POST /api/rule", u.rule)
	mux.HandleFunc("POST /api/freeze", u.freezeOp(u.in.Freeze))
	mux.HandleFunc("POST /api/unfreeze", u.freezeOp(u.in.Unfreeze))
	mux.HandleFunc("POST /api/forget", u.forget)
}

// forget drops a member that is gone for good from the fleet, as this page's person.
func (u *uiServer) forget(w http.ResponseWriter, r *http.Request) {
	var req struct{ Member, Reason string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uiError(w, err)
		return
	}
	if err := u.in.Forget(u.o.user, req.Member, req.Reason); err != nil {
		uiError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, map[string]string{"forgot": req.Member})
}

// uiAs serves the API as the person a member's page forwards: inside a ring,
// a member trusts its peers' pages to say who is acting (the ring's key
// signs every request), so a person can work on any member from one page.
func (in *Instance) uiAs(as Writer) http.Handler {
	u := &uiServer{in: in, t: &mcpTools{in: in}, o: uiOptions{user: as}}
	mux := http.NewServeMux()
	u.api(mux)
	return mux
}

// member sends an API request naming another member (?instance=cp-2) there,
// as this page's person, and passes its answer back.
func (u *uiServer) member(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("instance")
		if name == "" || name == u.in.name || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		ring := u.in.ring.Load()
		if ring == nil {
			writeUIJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("vis: this instance has no fleet, so there is no %q", name)})
			return
		}
		if r.URL.Path == "/api/events" {
			writeUIJSON(w, http.StatusBadRequest, map[string]string{"error": "vis: another member's state isn't streamed: poll /api/state?instance=" + name})
			return
		}
		m, err := u.in.member(name)
		if err != nil {
			writeUIJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPeerBody))
		if err != nil {
			uiError(w, err)
			return
		}
		res, err := ring.forwardUI(r.Context(), m.Addr, r.Method, r.URL.Path, body, u.o.user)
		if err != nil {
			writeUIJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("vis: %s didn't answer: %v", name, err)})
			return
		}
		defer res.Body.Close()
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	})
}

// uiHandler is the UI's handler, remembering who it acts as for Serve's log line.
type uiHandler struct {
	http.Handler
	user Writer
}

type uiServer struct {
	in *Instance
	t  *mcpTools
	o  uiOptions
}

// guard refuses requests for another Host (DNS rebinding) and from another
// origin (any web page the user visits could otherwise POST to localhost).
func (u *uiServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !u.o.hosts[host] {
			http.Error(w, "vis UI: host not allowed", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
			http.Error(w, "vis UI: cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type bucketOut struct {
	Start    string  `json:"start"`
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	P50      float64 `json:"p50_ms"`
	P95      float64 `json:"p95_ms"`
	P99      float64 `json:"p99_ms"`
	Refused  int64   `json:"refused,omitempty"`
}

type routeView struct {
	Route   string      `json:"route"`
	SLO     float64     `json:"slo_ms,omitempty"`
	Now     routeOut    `json:"now"`     // the last minute
	History []bucketOut `json:"history"` // 10-second buckets, oldest first, the current one last
}

func (u *uiServer) state(w http.ResponseWriter, r *http.Request) {
	writeUIJSON(w, http.StatusOK, u.snapshot(0))
}

// streamEvery is how often a stream sends the measurements when nothing else happened.
const streamEvery = 2 * time.Second

// events streams the state: whole on connect, then on every recorded change
// (a burst coalesced over 50 ms) and every streamEvery, with only the newest
// buckets of each route's hour. Every event is otherwise whole, so a reconnect
// needs no replay. It ends with the request.
func (u *uiServer) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // a proxy that buffers would hold the stream back
	send := func(tail int) bool {
		b, err := json.Marshal(u.snapshot(tail))
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if _, err := io.WriteString(w, "retry: 2000\n\n"); err != nil {
		return
	}
	wake := u.in.changed() // taken before each send, so a change during it isn't missed
	if !send(0) {
		return
	}
	tick := time.NewTicker(streamEvery)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		case <-wake:
			select { // let a burst of changes land as one event
			case <-time.After(50 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		wake = u.in.changed()
		if !send(2) {
			return
		}
	}
}

// snapshot is everything the page shows. tail > 0 keeps only the newest tail
// buckets of each route's history, for a stream that already sent the rest.
func (u *uiServer) snapshot(tail int) map[string]any {
	routes := u.in.Routes(5 * time.Minute)
	sort.Slice(routes, func(i, j int) bool { // the routes with an SLO first, then the busiest
		_, si := u.in.sloFor(routes[i].Route)
		_, sj := u.in.sloFor(routes[j].Route)
		if si != sj {
			return si
		}
		return routes[i].Requests > routes[j].Requests
	})
	var views []routeView
	for _, rs := range routes {
		now := u.in.Route(rs.Route, time.Minute)
		target, _ := u.in.sloFor(rs.Route)
		v := routeView{Route: rs.Route, SLO: msf(target),
			Now: routeOut{now.Route, now.Requests, now.Errors, msf(now.P50), msf(now.P95), msf(now.P99), now.Refused}}
		hist := u.in.RouteHistory(rs.Route)
		if tail > 0 && len(hist) > tail {
			hist = hist[len(hist)-tail:]
		}
		for _, b := range hist {
			v.History = append(v.History, bucketOut{b.Start.UTC().Format(time.RFC3339), b.Requests, b.Errors, msf(b.P50), msf(b.P95), msf(b.P99), b.Refused})
		}
		v.History = nonNil(v.History)
		views = append(views, v)
	}
	var slo map[string]any
	if s := u.in.SLOs(); len(s) > 0 { // the headline judges the first declared objective
		slo = map[string]any{"route": s[0].Route, "p99_ms": msf(s[0].P99), "warning": s[0].Warning}
	}
	var events []eventOut
	for _, e := range u.in.History(200) {
		events = append(events, eventOut{At: e.At.UTC().Format(time.RFC3339), Kind: e.Kind, Fields: e.Fields})
	}
	return map[string]any{
		"api": 1, "service": u.in.service, "started": u.in.started.UTC().Format(time.RFC3339), "slos": nonNil(u.t.slos()), "build": u.in.build, "user": u.o.user.String(), "now": time.Now().UTC().Format(time.RFC3339),
		"slo": slo, "routes": nonNil(views), "runtime": runtimeJSON(u.in.Runtime(10 * time.Second)), "signals": nonNil(u.in.Signals()),
		"knobs": nonNil(u.t.knobs("")), "rules": nonNil(u.in.Rules()), "freeze": u.t.freeze(), "history": nonNil(events),
		"stories": nonNil(u.in.Stories(30, u.o.user)), "pressures": nonNil(u.in.Pressures()), "warnings": nonNil(u.in.Warnings()), "memory": nonNil(u.in.memHistory()), "memory_limit_in_s": limitIn(u.in.Runtime(fleetWindow), trendTrusted(u.in.started, time.Now(), u.in.former.Load())).Seconds(),
		"fleet": u.fleet(context.Background()), // Fleet bounds its own wait
	}
}

// fleet is the comparison across members for the page, or nil without peers.
// The page asks often and the comparison asks every member, so it is reused for a few seconds.
func (u *uiServer) fleet(ctx context.Context) *fleetOut {
	r := u.in.ring.Load()
	if r == nil {
		return nil
	}
	rep := r.cachedFleet(ctx)
	out := fleetJSON(rep)
	out.Rollout = u.in.lastRolloutAnywhere() // the page shows the fleet's last rollout, as the fleet tool does
	return &out
}

type uiChange struct {
	Set    map[string]any `json:"set"`
	For    string         `json:"for"`  // empty: a pin with no end
	Keep   bool           `json:"keep"` // set back after a restart, for what is left of its lease (Tx.Keep)
	Reason string         `json:"reason"`
}

func (u *uiServer) tx(r *http.Request) (Tx, time.Duration, error) {
	var c uiChange
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		return Tx{}, 0, err
	}
	tx := Tx{By: u.o.user, Reason: c.Reason, Keep: c.Keep}
	var lease time.Duration
	if c.For != "" {
		d, err := time.ParseDuration(c.For)
		if err != nil {
			return Tx{}, 0, fmt.Errorf("for: %w", err)
		}
		lease, tx.For = d, d
	}
	names := make([]string, 0, len(c.Set))
	for n := range c.Set {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		info, ok := Lookup(n)
		if !ok {
			return Tx{}, 0, fmt.Errorf("%w: %q", ErrUnknownKnob, n)
		}
		v, err := info.Parse(fmt.Sprint(c.Set[n]))
		if err != nil {
			return Tx{}, 0, err
		}
		tx.Set = append(tx.Set, Write{Knob: n, Value: v})
	}
	return tx, lease, nil
}

func (u *uiServer) preview(w http.ResponseWriter, r *http.Request) {
	tx, _, err := u.tx(r)
	if err == nil {
		var rec Record
		if rec, err = u.in.Preview(tx); err == nil {
			writeUIJSON(w, http.StatusOK, u.t.change(rec, 0))
			return
		}
	}
	uiError(w, err)
}

func (u *uiServer) apply(w http.ResponseWriter, r *http.Request) {
	tx, lease, err := u.tx(r)
	if err == nil {
		var rec Record
		if rec, err = u.in.Apply(tx); err == nil {
			writeUIJSON(w, http.StatusOK, u.t.change(rec, lease))
			return
		}
	}
	uiError(w, err)
}

func (u *uiServer) clear(w http.ResponseWriter, r *http.Request) {
	var req struct{ Knob, Reason string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uiError(w, err)
		return
	}
	eff, err := u.in.Clear(req.Knob, PinLayer, u.o.user, req.Reason)
	if err != nil {
		uiError(w, err)
		return
	}
	info, _ := Lookup(req.Knob)
	writeUIJSON(w, http.StatusOK, effectOut{Knob: req.Knob, Value: showValue(info, eff.Value), Layer: eff.Layer.String()})
}

func (u *uiServer) revert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Lease  uint64 `json:"lease"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uiError(w, err)
		return
	}
	rec, err := u.in.Revert(req.Lease, u.o.user, req.Reason)
	if err != nil {
		uiError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, u.t.change(rec, 0))
}

func (u *uiServer) rule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rule    string `json:"rule"`
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		uiError(w, err)
		return
	}
	if err := u.in.SetRule(req.Rule, req.Enabled, u.o.user, req.Reason); err != nil {
		uiError(w, err)
		return
	}
	writeUIJSON(w, http.StatusOK, map[string]any{"rules": u.in.Rules()})
}

func (u *uiServer) freezeOp(op func(Writer, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Reason string }
		json.NewDecoder(r.Body).Decode(&req)
		if err := op(u.o.user, req.Reason); err != nil {
			uiError(w, err)
			return
		}
		writeUIJSON(w, http.StatusOK, u.t.freeze())
	}
}

func writeUIJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func uiError(w http.ResponseWriter, err error) {
	writeUIJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}
