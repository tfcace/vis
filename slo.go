package vis

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Service names the service this instance belongs to, as the UI's header,
// MCP's describe and every story show it.
func Service(name string) InstanceOption { return func(in *Instance) { in.service = name } }

// SLO gives one instance a route's latency objective, p99 at or under the
// target. To declare an objective for the service, use NewSLO beside the
// handler instead.
func SLO(route string, p99 time.Duration) InstanceOption {
	return func(in *Instance) { in.slos = append(in.slos, sloDef{route, p99}) }
}

// Objective is a route's declared SLO.
type Objective struct{ d sloDef }

// Target is what an objective asks of its route.
type Target struct{ p99 time.Duration }

// P99 asks that 99% of the route's requests finish within d, measured inside
// the service.
func P99(d time.Duration) Target { return Target{p99: d} }

// NewSLO declares a route's objective, at package level beside the handler
// that serves it: vis.NewSLO("GET /k/{key}", vis.P99(50*time.Millisecond)).
// The route is named as http.ServeMux registered it (or as RouteName names
// it). The UI's headline, agents, impact reports and rules judge the service
// by it; the first one declared is the service's main objective. Every
// instance has it.
func NewSLO(route string, t Target) Objective {
	if route == "" || t.p99 <= 0 {
		panic(fmt.Sprintf("vis: SLO %q needs a route and a positive target, for example vis.P99(50*time.Millisecond)", route))
	}
	d := sloDef{route, t.p99}
	declaredSLOs.Lock()
	defer declaredSLOs.Unlock()
	for _, o := range declaredSLOs.defs {
		if o.route == route {
			panic(fmt.Sprintf("vis: SLO %q is declared twice", route))
		}
	}
	declaredSLOs.defs = append(declaredSLOs.defs, d)
	return Objective{d}
}

// Route is the route the objective judges.
func (o Objective) Route() string { return o.d.route }

// P99 is the objective's p99 target.
func (o Objective) P99() time.Duration { return o.d.p99 }

var declaredSLOs struct {
	sync.Mutex
	defs []sloDef
}

// objectives are this instance's SLOs: those given with the SLO option, then
// those declared with NewSLO, one per route. An instance given its own
// objective (a test, say) keeps it first.
func (in *Instance) objectives() []sloDef {
	out := append([]sloDef(nil), in.slos...)
	declaredSLOs.Lock()
	defer declaredSLOs.Unlock()
next:
	for _, s := range declaredSLOs.defs {
		for _, o := range out {
			if o.route == s.route {
				continue next
			}
		}
		out = append(out, s)
	}
	return out
}

type sloDef struct {
	route string
	p99   time.Duration
}

// SLOStatus is a route against its objective over the last minute.
type SLOStatus struct {
	Route    string
	P99      time.Duration // the objective
	Now      time.Duration // p99 over the last 20 seconds, measured inside the service: what Met judges
	Minute   time.Duration // p99 over the last minute, for context: it lags a recovery
	Requests int64         // over the last 20 seconds
	Met      bool          // true with no requests: nothing missed it
	Warning  string        // a reason to doubt the SLO is set up right
}

// sloWarning doubts an SLO whose route has never served a request while
// others have: its name probably doesn't match the ServeMux pattern.
func (in *Instance) sloWarning(route string) string {
	if in.routes.get(route, false) != nil {
		return ""
	}
	var served []string
	for _, n := range in.routes.names() {
		if n != "unmatched" {
			served = append(served, strconv.Quote(n))
		}
	}
	if len(served) == 0 {
		return ""
	}
	if len(served) > 3 {
		served = append(served[:3], "…")
	}
	return fmt.Sprintf("No request has reached %q since the service started, while %s have. An SLO names its route by the ServeMux pattern it is registered under.",
		route, strings.Join(served, ", "))
}

// Service is the service's name.
func (in *Instance) Service() string { return in.service }

// SLOs reports each declared objective, in declaration order, against the last
// 20 seconds, so it reads as met soon after a recovery, with the last minute alongside.
func (in *Instance) SLOs() []SLOStatus {
	objs := in.objectives()
	out := make([]SLOStatus, len(objs))
	for i, s := range objs {
		r, m := in.Route(s.route, 20*time.Second), in.Route(s.route, time.Minute)
		out[i] = SLOStatus{Route: s.route, P99: s.p99, Now: r.P99, Minute: m.P99, Requests: r.Requests, Met: r.Requests == 0 || r.P99 <= s.p99,
			Warning: in.sloWarning(s.route)}
	}
	return out
}

func (in *Instance) sloFor(route string) (time.Duration, bool) {
	for _, s := range in.objectives() {
		if s.route == route {
			return s.p99, true
		}
	}
	return 0, false
}
