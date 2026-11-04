package vis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Any instance coordinates: a tool call naming a member runs there, forwarded
// over the peer port with the caller's own token, which that member checks
// itself, so no instance acts with more rights than its caller. And the fleet
// view asks every member for what can be compared, merges latency from
// histograms (never by averaging percentiles), and says who stands out.

func (o on) target() string { return o.Instance }

type targeted interface{ target() string }

// addTool registers a tool that runs on the member its input names, or here.
func addTool[In targeted, Out any](t *mcpTools, s *mcp.Server, tool *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		if name := in.target(); name != "" && name != t.in.name {
			var out Out
			err := t.forward(ctx, req, name, tool.Name, in, &out)
			return nil, out, err
		}
		if in.target() == "" && tool.Annotations != nil && !tool.Annotations.ReadOnlyHint { // a change, in a fleet: on which member?
			if ms := t.in.Members(); len(ms) > 1 {
				var names []string
				for _, m := range ms {
					names = append(names, m.Name)
				}
				slices.Sort(names)
				var out Out
				return nil, out, fmt.Errorf("this is %s, one of %d members (%s), and a change lands on one of them: say which with instance. "+
					"Behind a load balancer, the member you reach is any of them, and a change's lease and change numbers are that member's own", t.in.name, len(ms), strings.Join(slices.Compact(names), ", "))
			}
		}
		return h(ctx, req, in)
	})
}

// sharedNames are the names two live members answer to at different
// addresses: misconfigured instances on one host, all named by its hostname.
// (A member restarted in place keeps its address, and replaces its old entry.)
func (in *Instance) sharedNames() map[string][]string {
	byName := map[string][]Member{}
	for _, m := range in.Members() {
		if m.State == "alive" {
			byName[m.Name] = append(byName[m.Name], m)
		}
	}
	addrs := map[string][]string{}
	for name, ms := range byName {
		for i, a := range ms {
			for _, b := range ms[i+1:] {
				// a restarted member's old entry stopped rising before its new one was heard of
				if a.Addr != b.Addr && a.rose.After(b.seen) && b.rose.After(a.seen) {
					for _, x := range []string{a.Addr, b.Addr} {
						if !slices.Contains(addrs[name], x) {
							addrs[name] = append(addrs[name], x)
						}
					}
				}
			}
		}
		slices.Sort(addrs[name])
	}
	return addrs
}

// member finds a live member by name, or says which there are.
func (in *Instance) member(name string) (Member, error) {
	if addrs, ok := in.sharedNames()[name]; ok {
		return Member{}, fmt.Errorf("vis: more than one instance answers as %s (at %s), so a call named for it may land on either: give each its own VIS_INSTANCE_NAME", name, strings.Join(addrs, " and "))
	}
	ms := in.Members()
	var names []string
	var found *Member
	for i, m := range ms {
		names = append(names, m.Name)
		if m.Name == name && (found == nil || m.Quiet < found.Quiet) { // a restarted member: the one heard from last
			found = &ms[i]
		}
	}
	if found == nil {
		return Member{}, fmt.Errorf("vis: no member named %q; the fleet is %s", name, strings.Join(slices.Compact(names), ", "))
	}
	if found.State == "dead" {
		return Member{}, fmt.Errorf("vis: %s is dead: not heard from for %s", name, found.Quiet.Round(time.Second))
	}
	return *found, nil
}

// forward runs a tool on the named member and decodes its answer into out.
func (t *mcpTools) forward(ctx context.Context, req *mcp.CallToolRequest, name, tool string, in, out any) error {
	r := t.in.ring.Load()
	if r == nil {
		return fmt.Errorf("vis: this instance has no fleet, so there is no %q: it is %s (instance is for an instance in a ring, see vis.Seeds)", name, t.in.name)
	}
	m, err := t.in.member(name)
	if err != nil {
		return unsent{err} // nothing was sent: no such member, or a dead one
	}
	b, _ := json.Marshal(in)
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // a big integer stays exact on its way to the member
	dec.Decode(&args)
	args["instance"] = name // the member runs it as its own, named, as a change in a fleet must be
	var auth string
	if req.Extra != nil {
		auth = req.Extra.Header.Get("Authorization")
	}
	res, err := r.callTool(ctx, m.Addr, auth, tool, args)
	if err != nil {
		return fmt.Errorf("vis: on %s: %w", name, err)
	}
	if res.IsError {
		var msg []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg = append(msg, tc.Text)
			}
		}
		return toolError{name, strings.Join(msg, " ")}
	}
	b, _ = json.Marshal(res.StructuredContent)
	return json.Unmarshal(b, out)
}

// toolError is a member's refusal of a forwarded call (out of range, frozen,
// not this writer's): it answered, and changed nothing.
type toolError struct{ member, msg string }

func (e toolError) Error() string { return "on " + e.member + ": " + e.msg }

// unsent is a forwarded call that was never sent.
type unsent struct{ error }

func (e unsent) Unwrap() error { return e.error }

// errNoAnswer is a forwarded call whose handshake failed (no answer in time,
// or a broken one): the call itself was never sent.
var errNoAnswer = errors.New("no answer")

// patience is how long a forwarded request waits for a member to start
// answering. From outside, a member starved of CPU and one that is paused look
// alike: a read gives up soon, but a change waits longer, since the member
// slowest to answer is often the one that needs the fix.
// It is capped, however high VIS_FLEET_WAIT is set: the call it starts has to
// answer within the minute an MCP client waits.
func (r *ring) patience(change bool) time.Duration {
	if change {
		return min(max(15*time.Second, 3*r.wait), 20*time.Second)
	}
	return min(max(5*time.Second, r.wait), 10*time.Second)
}

// forwardedWithin bounds a forwarded call, its handshake included: under the
// minute an MCP client waits for the call it came from.
const forwardedWithin = 55 * time.Second

// noClientWaits marks a context no MCP client waits on (a rollout's job), so
// its forwarded calls take their full time.
type noClientWaits struct{}

// changes says whether a tool is part of making a change: the changes
// themselves, and reading what one did, which a rollout waits on.
func changes(tool string) bool {
	switch tool {
	case "set_knob", "revert", "set_rule", "impact":
		return true
	}
	return false
}

// callTool calls a tool on the member at addr, as the caller whose
// Authorization header it carries.
//
// The handshake is bounded by its patience (see patience): a member that
// accepts connections and never answers, such as a paused one, fails there.
// The whole call, handshake included, gets forwardedWithin, under the minute
// an MCP client waits; a rollout's job, which no client waits on, gets the
// longest a tool takes besides.
func (r *ring) callTool(ctx context.Context, addr, auth, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	hc := &http.Client{Transport: peerAuth{r: r, auth: auth}, Timeout: longestCall}
	c := mcp.NewClient(&mcp.Implementation{Name: "vis-peer", Version: r.self.Build}, nil)
	callBy := time.Now().Add(forwardedWithin)
	patience := r.patience(changes(tool))
	if ctx.Value(noClientWaits{}) != nil { // a rollout's job, past its answer: no client waits on this call
		callBy = time.Now().Add(longestCall + patience)
	}
	hctx, cancel := context.WithTimeout(ctx, patience)
	defer cancel()
	cs, err := c.Connect(hctx, &mcp.StreamableClientTransport{Endpoint: "http://" + addr + peerPath + "mcp", HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if err != nil { // no tool call is sent before the handshake ends: whatever failed, the call never started
		switch {
		case ctx.Err() != nil:
			return nil, fmt.Errorf("%w: the caller gave up before %s answered: %w", errNoAnswer, addr, ctx.Err())
		case hctx.Err() != nil:
			if th := r.in.Runtime(fleetWindow).Throttled; th >= .5 { // a starved caller waits on everyone
				return nil, fmt.Errorf("%w in %s; but this instance, %s, is itself throttled in %.0f%% of CPU periods, so the wait may be here, not there (fleet shows what that member says of itself)", errNoAnswer, patience, r.in.name, th*100)
			}
			return nil, fmt.Errorf("%w in %s: it accepts connections but doesn't respond (paused, or starved of CPU)", errNoAnswer, patience)
		}
		return nil, fmt.Errorf("%w: the handshake failed, so the call was never sent: %w", errNoAnswer, err)
	}
	defer cs.Close()
	cctx, cancel2 := context.WithDeadline(ctx, callBy)
	defer cancel2()
	res, err := cs.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil && cctx.Err() != nil && ctx.Err() == nil && changes(tool) && tool != "impact" { // sent, and cut: it may still land
		return nil, fmt.Errorf("no answer within %s of asking, so this call gives up to answer in time; the change was sent and may still land: see history with instance: %w", short(forwardedWithin), err)
	}
	return res, err
}

// forwardUI sends a page's API request to the member at addr, as person as.
func (r *ring) forwardUI(ctx context.Context, addr, method, path string, body []byte, as Writer) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+peerPath+"ui"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Vis-UI-As", as.String())
	req.Header.Set("Content-Type", "application/json")
	r.sign(req, body) // last: it covers the person
	// A page's reads are quick: a member that doesn't answer in the fleet wait
	// is paused or starved, and a page polling it mustn't pile up behind it. Its
	// changes wait longer: the starved member is the one that needs them.
	return (&http.Client{Transport: r.client.Transport, Timeout: r.patience(method != http.MethodGet)}).Do(req)
}

// peerAuth signs each request as the ring's and carries the caller's token.
type peerAuth struct {
	r    *ring
	auth string
}

func (p peerAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		req.Body.Close()
	}
	req = req.Clone(req.Context())
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	if p.auth != "" {
		req.Header.Set("Authorization", p.auth)
	}
	p.r.sign(req, body) // last: it covers the caller's token
	return p.r.client.Transport.RoundTrip(req)
}

// peerSummary is what a member tells the fleet view about itself.
type peerSummary struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Build     string            `json:"build"`
	Frozen    bool              `json:"frozen"`
	FrozenBy  string            `json:"frozen_by,omitempty"`
	CPUUsed   float64           `json:"cpu_used"`
	CPULimit  float64           `json:"cpu_limit"`
	Throttled float64           `json:"throttled"`
	SLOs      []peerSLO         `json:"slos"`
	Knobs     map[string]int64  `json:"knobs"`
	SetBy     map[string]string `json:"set_by,omitempty"` // knobs off their default, and who set them
	Held      []Held            `json:"held,omitempty"`   // the same, with their values and when they end
	Pressures []Pressure        `json:"pressures,omitempty"`
	Mem       *memDigest        `json:"mem,omitempty"`
	Dropped   []Held            `json:"dropped,omitempty"` // what earlier deaths of it dropped and nothing has set again (stillDropped)
	Env       []envChange       `json:"env,omitempty"`     // what changed in its environment lately, such as its CPU limit
	Changes   []string          `json:"changes,omitempty"` // its knob changes in the last minute: "fleet_test_knob 5 → 2 by agent:claude"
	Last      string            `json:"last,omitempty"`    // its newest knob change by a person or an agent, or a lease of theirs ending, of any age
	LastAgo   time.Duration     `json:"last_ago,omitempty"`
}

type peerSLO struct {
	Route     string        `json:"route"`
	Objective time.Duration `json:"objective"`
	Hist      [][2]uint32   `json:"hist,omitempty"` // bucket, count: only the buckets with requests; none when relayed
	P99       time.Duration `json:"p99,omitempty"`  // what a relay carries instead
	Requests  int64         `json:"requests"`
	Errors    int64         `json:"errors"`
}

// envChange is something vis noticed change in an instance's environment.
type envChange struct {
	Event string        `json:"event"` // cpu_limit_changed, gomaxprocs_changed
	Old   float64       `json:"old"`
	New   float64       `json:"new"`
	Ago   time.Duration `json:"ago"`
}

// envLately is how far back a summary reports environment changes.
const envLately = 10 * time.Minute

// fleetWindow is what the fleet view compares: the window an SLO is judged on.
const fleetWindow = 20 * time.Second

func (in *Instance) summary() peerSummary {
	f := in.FreezeState()
	rt := in.Runtime(fleetWindow)
	s := peerSummary{ID: in.id, Name: in.name, Build: in.build, Frozen: f.Frozen, CPUUsed: rt.CPUUsed, CPULimit: rt.CPULimit, Throttled: rt.Throttled,
		Knobs: map[string]int64{}, Pressures: in.Pressures(), Mem: in.memDigest(rt)}
	if f.Frozen {
		s.FrozenBy = f.By.String()
	}
	now := time.Now()
	for _, o := range in.objectives() {
		h, req, errs := in.routes.window(o.route, fleetWindow, now)
		ps := peerSLO{Route: o.route, Objective: o.p99, Requests: req, Errors: errs, P99: h.quantile(.99)}
		for i, c := range h.counts {
			if c > 0 {
				ps.Hist = append(ps.Hist, [2]uint32{uint32(i), c})
			}
		}
		s.SLOs = append(s.SLOs, ps)
	}
	if last, at := in.lastChange(); last != "" {
		s.Last, s.LastAgo = last, max(now.Sub(at), time.Millisecond)
	}
	for _, e := range in.History(MaxHistory) {
		if e.Kind == "change" && e.Fields["layer"] != RuleLayer.String() && fmt.Sprint(e.Fields["old"]) != fmt.Sprint(e.Fields["new"]) && now.Sub(e.At) <= impactWindow+settleFor+5*time.Second { // a rule's reaction is the system at work; a masked write changed nothing
			s.Changes = append(s.Changes, fmt.Sprintf("%v %v → %v by %v", e.Fields["knob"], e.Fields["old"], e.Fields["new"], e.Fields["actor"]))
		}
		if e.Kind != "environment" || now.Sub(e.At) > envLately {
			continue
		}
		ev, _ := e.Fields["event"].(string)
		s.Env = append(s.Env, envChange{Event: ev, Old: toFloat(e.Fields["old"]), New: toFloat(e.Fields["new"]), Ago: now.Sub(e.At)})
	}
	snap := in.Snapshot()
	for _, d := range allDecls() {
		s.Knobs[d.name] = snap.get(d)
		if st, err := in.Stack(d.name); err == nil && st[0].Layer != DefaultLayer {
			if s.SetBy == nil {
				s.SetBy = map[string]string{}
			}
			s.SetBy[d.name] = st[0].By.String()
			info, _ := Lookup(d.name)
			s.Held = append(s.Held, Held{Knob: d.name, Value: fmt.Sprint(showValue(info, st[0].Value)), By: st[0].By.String(), EndsIn: st[0].Left})
		}
	}
	for _, d := range in.stillDropped(s.Held) {
		s.Dropped = append(s.Dropped, d.Held)
	}
	return s
}

func histOf(pairs [][2]uint32) hist {
	var h hist
	for _, p := range pairs {
		if int(p[0]) < histBuckets {
			h.counts[p[0]] += p[1]
			h.n += uint64(p[1])
		}
	}
	return h
}

// liveOnce is the live members, each name once: a member restarted moments
// ago is listed under its new ID and, until it goes quiet, its old one.
func liveOnce(ms []Member) []Member {
	best := map[string]Member{}
	var order []string
	for _, m := range ms {
		if m.State != "alive" {
			continue
		}
		b, seen := best[m.Name]
		if !seen {
			order = append(order, m.Name)
		}
		if !seen || m.Quiet < b.Quiet {
			best[m.Name] = m
		}
	}
	out := make([]Member, 0, len(order))
	for _, n := range order {
		out = append(out, best[n])
	}
	return out
}

// restNow is the rest of the fleet's objectives over the last 20 s, merged
// from the other members' histograms: those named, or every live one, each
// name once. A member too busy to answer is taken from the digest it last
// gossiped itself, as the fleet view takes it, and named in gossiped: silence
// is a finding, not a member gone missing from the comparison.
func (in *Instance) restNow(only []string) (out map[string]float64, members, gossiped, changed []string) {
	r := in.ring.Load()
	if r == nil {
		return nil, nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.wait)
	defer cancel()
	var targets []Member
	for _, m := range liveOnce(in.Members()[1:]) {
		if only == nil || slices.Contains(only, m.Name) {
			targets = append(targets, m)
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	hists := map[string]*hist{}
	reqs, errs := map[string]int64{}, map[string]int64{}
	add := func(name string, slos []peerSLO, fromGossip bool, changes []string) {
		mu.Lock()
		defer mu.Unlock()
		members = append(members, name)
		for _, c := range changes {
			changed = append(changed, name+": "+c)
		}
		if fromGossip {
			gossiped = append(gossiped, name)
		}
		for _, o := range slos {
			h := histOf(o.Hist)
			if hists[o.Route] == nil {
				hists[o.Route] = &hist{}
			}
			hists[o.Route].merge(&h)
			reqs[o.Route] += o.Requests
			errs[o.Route] += o.Errors
		}
	}
	for _, m := range targets {
		wg.Go(func() {
			var s peerSummary
			if r.post(ctx, m.Addr, "summary", struct{}{}, &s) == nil {
				add(m.Name, s.SLOs, false, s.Changes)
			} else if h, ok := r.healthOf(m.ID); ok {
				add(m.Name, h.SLOs, true, nil)
			}
		})
	}
	wg.Wait()
	out = map[string]float64{}
	for route, h := range hists {
		if h.n > 0 {
			out[route+" p99"] = msf(h.quantile(.99))
			out[route+" errors"] = float64(errs[route]) / float64(max(reqs[route], 1))
			out[route+" requests"] = float64(reqs[route])
		}
	}
	slices.Sort(members)
	slices.Sort(gossiped)
	slices.Sort(changed)
	return out, members, gossiped, changed
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// common says what every member that answered shares: a pressure, a change
// in its environment, a value it holds above its default. A comparison is
// blind to these, and in a fault on every member they are the story: the
// cause, and a fix that lapses everywhere at once.
func common(ms []FleetMember, sums []*peerSummary) []string {
	var have []*peerSummary
	for i, s := range sums {
		if s == nil || i < len(ms) && ms[i].Silent {
			continue
		}
		have = append(have, s)
	}
	if len(have) < 2 {
		return nil
	}
	every := func(match func(*peerSummary) bool) bool {
		for _, s := range have {
			if !match(s) {
				return false
			}
		}
		return true
	}
	var out []string
	// A lease that ended on every member about at once: a fix that lapsed
	// everywhere, which is what to check first when the objective broke since.
	if last := have[0].Last; strings.HasSuffix(last, "'s lease ended") {
		newest, oldest := have[0].LastAgo, have[0].LastAgo
		if every(func(s *peerSummary) bool {
			newest, oldest = min(newest, s.LastAgo), max(oldest, s.LastAgo)
			return s.Last == last
		}) && oldest-newest <= 2*time.Minute {
			what, who, _ := strings.Cut(last, ": ")
			on := "every member"
			if len(have) < len(ms) {
				on = fmt.Sprintf("every member that answered (%d of %d)", len(have), len(ms))
			}
			out = append(out, fmt.Sprintf("%s ended on %s, about %s ago (%s): if the objective broke since, the fix lapsed everywhere at once", strings.TrimSuffix(who, " ended"), on, short(max(newest.Round(time.Minute), time.Minute)), what))
		}
	}
	var answered []*peerSummary // pressures and environment changes don't travel in gossip
	for i, s := range sums {
		if s != nil && i < len(ms) && !ms[i].Silent && ms[i].Gossiped == 0 {
			answered = append(answered, s)
		}
	}
	whom := "every member"
	if len(answered) < len(ms) {
		whom = fmt.Sprintf("every member that answered (%d of %d)", len(answered), len(ms))
	}
	everyAnswered := func(match func(*peerSummary) bool) bool {
		for _, s := range answered {
			if !match(s) {
				return false
			}
		}
		return len(answered) >= 2
	}
	for _, p := range have[0].Pressures {
		if everyAnswered(func(s *peerSummary) bool {
			return slices.ContainsFunc(s.Pressures, func(q Pressure) bool { return q.Resource == p.Resource })
		}) {
			out = append(out, fmt.Sprintf("%s is short of %s", whom, p.Resource))
		}
	}
	for _, e := range have[0].Env {
		var newest, oldest time.Duration = e.Ago, e.Ago
		if everyAnswered(func(s *peerSummary) bool {
			i := slices.IndexFunc(s.Env, func(f envChange) bool { return f.Event == e.Event && f.New == e.New })
			if i >= 0 {
				newest, oldest = min(newest, s.Env[i].Ago), max(oldest, s.Env[i].Ago)
			}
			return i >= 0
		}) {
			when := short(newest) + " ago" // a spread says the members' changes weren't one event
			if oldest-newest >= 5*time.Second {
				when = fmt.Sprintf("%s to %s ago", short(newest), short(oldest))
			}
			switch {
			case e.Event == "cpu_limit_changed" && whom == "every member":
				out = append(out, fmt.Sprintf("every member's CPU limit changed %g → %g cores, %s", e.Old, e.New, when))
			case e.Event == "cpu_limit_changed":
				out = append(out, fmt.Sprintf("the CPU limit changed %g → %g cores on %s, %s", e.Old, e.New, whom, when))
			default:
				out = append(out, fmt.Sprintf("%s saw %s (%g → %g), %s", whom, e.Event, e.Old, e.New, when))
			}
		}
	}
	for _, h := range have[0].Held {
		var first, last time.Duration = -1, 0
		open := 0
		var by []string
		if every(func(s *peerSummary) bool {
			i := slices.IndexFunc(s.Held, func(g Held) bool { return g.Knob == h.Knob && g.Value == h.Value })
			if i >= 0 {
				if e := s.Held[i].EndsIn; e == 0 {
					open++
				} else {
					if first < 0 || e < first {
						first = e
					}
					last = max(last, e)
				}
				if !slices.Contains(by, s.Held[i].By) {
					by = append(by, s.Held[i].By)
				}
			}
			return i >= 0
		}) {
			var ends string
			switch {
			case first < 0:
				ends = "with no end"
			case open > 0:
				ends = fmt.Sprintf("with no end on %d, ending in %s to %s on the others", open, ceilMinute(first), ceilMinute(last))
			case last-first <= 2*time.Minute:
				ends = fmt.Sprintf("ending in %s to %s, on every member about at once", ceilMinute(first), ceilMinute(last))
			default:
				ends = fmt.Sprintf("ending in %s to %s", ceilMinute(first), ceilMinute(last))
			}
			out = append(out, fmt.Sprintf("%s %s on every member (set by %s), %s", h.Knob, h.Value, strings.Join(by, ", "), ends))
		}
	}
	return out
}

// ceilMinute is d in whole minutes, rounded up: a lease ending "in 20m".
func ceilMinute(d time.Duration) string {
	if d < time.Minute {
		return short(d)
	}
	return fmt.Sprintf("%dm", (d+time.Minute-1)/time.Minute)
}

// FleetReport compares the members of the fleet, as one instance sees them.
type FleetReport struct {
	Members  []FleetMember
	SLOs     []FleetSLO   // each objective across the fleet, from merged histograms
	StandOut []StandOut   // members that differ from the rest in a way that matters
	Differ   []Difference // what isn't the same on every member: its CPU limit, its build, its knobs
	Covers   string       // "3 of 3 members": a partial answer says so
	Missing  []string     // members that didn't answer, and why
	Common   []string     // what every member shares: a pressure, an environment change, a value held
	At       time.Time    // when it was asked
}

// Held is a value a member holds above its default: who set it, and when it
// ends (0: it doesn't). A restart drops it: values set at runtime live in the process.
type Held struct {
	Knob   string        `json:"knob"`
	Value  string        `json:"value"`
	By     string        `json:"by"`
	EndsIn time.Duration `json:"ends_in"`
	// For a kept value: whether a restart set it back, and how long it has
	// been in effect since it was written: what the breaker reads (keepHolds).
	Back bool          `json:"back,omitempty"`
	Age  time.Duration `json:"age,omitempty"`
	// Unkept is a dropped kept value that vis stopped setting back: the
	// process it was set back on died within keepHolds, so it may be what kills it.
	Unkept bool `json:"unkept,omitempty"`
}

// FleetMember is one member's side of the comparison.
type FleetMember struct {
	Member
	Frozen    bool
	CPUUsed   float64 // cores, over the last 20 s
	CPULimit  float64
	Throttled float64
	SLOs      []MemberSLO
	// Gossiped is how old its numbers are when it didn't answer and they come
	// from its last gossip; zero when it answered.
	Gossiped time.Duration
	// Silent is a member with no numbers to show: not heard from, refusing
	// connections, or alive with no gossip yet. It is unaccounted for, never
	// taken as healthy; SilentWhy says which.
	Silent    bool
	SilentWhy string
	Held      []Held        // values above its defaults, with who set them and when they end
	Restarts  int           // under its name, in the last 10 minutes
	Restarted time.Duration // since the last of them
	Lost      []Held        // what the last restart dropped and it doesn't hold again
	Died      string        // what most likely killed it the last time it restarted; "" if it stopped cleanly
	// Its container's memory, when it says: bytes used and the limit (0 when
	// unlimited), and how soon its forecast reaches the limit (0: it isn't heading there).
	MemUsed, MemLimit uint64
	MemKill           time.Duration
	Pressures         []Pressure // what it is short of, with the knobs that spend it
	// LastChange is its newest knob change by a person or an agent, LastAgo
	// how long ago: where a lapsed fix or a harmful change starts.
	LastChange string
	LastAgo    time.Duration
}

// refused is a call that failed to connect at all: nothing listens there, or
// the member can't be reached, so it isn't busy, it's gone (stopped, crashed,
// restarting) or cut off.
func refused(err error) bool {
	var op *net.OpError
	var dns *net.DNSError
	return errors.As(err, &op) && op.Op == "dial" || errors.As(err, &dns)
}

// unreachable says how a call that couldn't connect failed, as people say it.
func unreachable(err error) string {
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "connection refused") {
		return "it refused the connection"
	}
	return "it couldn't be reached"
}

// MemberSLO is one objective on one member, over the last 20 s.
type MemberSLO struct {
	Route    string        `json:"route"`
	P99      time.Duration `json:"p99"`
	Requests int64         `json:"requests"`
	Met      bool          `json:"met"`
}

// FleetSLO is one objective across every member that answered.
type FleetSLO struct {
	Route     string
	Objective time.Duration
	P99       time.Duration
	Requests  int64
	Met       bool
	Partial   string // members whose requests aren't in P99, and what their gossip says
}

// StandOut is a member that differs from the rest, and the call to look closer.
type StandOut struct {
	Member string
	Why    string
	Look   string
}

// Difference is something not the same on every member that answered: its
// CPU limit, its build, or a knob's value.
type Difference struct {
	What   string            // cpu_limit, build, or a knob's name
	Values map[string]string // by member name, as people read them
	SetBy  map[string]string // a knob's: who set it, where it isn't at its default
}

// Fleet asks every member for its side and compares them. A member that
// doesn't answer within the fleet wait is shown, and merged, from what its
// gossip last carried; a member not heard from at all is listed as such, and
// the fleet's objective can't read as met while one is unaccounted for:
// silence is a finding, never absence.
func (in *Instance) Fleet(ctx context.Context) FleetReport {
	ms := in.Members()
	r := in.ring.Load()
	wait := peerTimeout
	if r != nil {
		wait = r.wait
	}
	sums := make([]*peerSummary, len(ms))
	errs := make([]error, len(ms))
	self := in.summary()
	sums[0] = &self
	if r != nil {
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		var wg sync.WaitGroup
		for i, m := range ms[1:] {
			if m.State == "dead" {
				errs[i+1] = errors.New("dead: not heard from for " + m.Quiet.Round(time.Second).String())
				continue
			}
			wg.Go(func() {
				var s peerSummary
				if err := r.post(ctx, m.Addr, "summary", struct{}{}, &s); err != nil {
					errs[i+1] = err
					return
				}
				sums[i+1] = &s
			})
		}
		wg.Wait()
	}

	rep := FleetReport{At: time.Now()}
	answered := 0
	var gossiped, silent []string
	for i, m := range ms {
		fm := FleetMember{Member: m}
		s := sums[i]
		switch {
		case s != nil:
			answered++
		case m.State != "alive": // not heard from: nothing here can say how it's doing
			fm.Silent, fm.SilentWhy = true, fmt.Sprintf("not heard from for %s", short(m.Quiet))
			silent = append(silent, fmt.Sprintf("%s (%s, not heard from for %s)", m.Name, m.State, short(m.Quiet)))
			rep.Missing = append(rep.Missing, fmt.Sprintf("%s: %v", m.Name, errs[i]))
		case refused(errs[i]): // its port is closed: gone, whatever its last heartbeat says
			fm.Silent, fm.SilentWhy = true, unreachable(errs[i])
			silent = append(silent, fmt.Sprintf("%s (%s)", m.Name, fm.SilentWhy))
			rep.Missing = append(rep.Missing, fmt.Sprintf("%s: %v", m.Name, errs[i]))
		default: // alive, too busy to answer: its own gossip says how it was doing
			rep.Missing = append(rep.Missing, fmt.Sprintf("%s: %v", m.Name, errs[i]))
			if h, ok := r.healthOf(m.ID); ok {
				s = h.summary(m)
				sums[i] = s
				fm.Gossiped = max(m.Quiet, time.Millisecond) // by this clock: the digest came with its heartbeat, heard m.Quiet ago
				gossiped = append(gossiped, m.Name)
			} else {
				fm.Silent, fm.SilentWhy = true, "it didn't answer, and its gossip carries no numbers yet"
				silent = append(silent, fmt.Sprintf("%s (alive, but it didn't answer and its gossip carries no numbers yet)", m.Name))
			}
		}
		if r != nil {
			var last restart
			fm.Restarts, fm.Restarted, last = r.restartsOf(m.Name)
			fm.Lost = unkept(slices.Clone(last.lost), last.last)
			if fm.Restarts > 0 && !last.clean {
				fm.Died = deathWhy(last.last != nil && killedForMemory(last.last.Mem))
			}
		}
		if m.Self { // a fresh process's ring saw its name restart once at most; its note kept the rest
			in.deathMu.Lock()
			ds := recentDeaths(in.ownDeaths)
			in.deathMu.Unlock()
			if len(ds) > fm.Restarts {
				fm.Restarts, fm.Restarted = len(ds), time.Since(time.Unix(0, ds[len(ds)-1]))
				if f := in.former.Load(); f != nil && f.died {
					fm.Died = deathWhy(f.forMemory)
				}
			}
		}
		if s != nil {
			fm.Held, fm.Pressures, fm.LastChange, fm.LastAgo = s.Held, s.Pressures, s.Last, s.LastAgo
			for _, d := range s.Dropped { // a crash loop's earlier deaths, which the last restart's own drop doesn't name
				if !slices.ContainsFunc(fm.Lost, func(l Held) bool { return heldKey(l) == heldKey(d) }) {
					fm.Lost = append(fm.Lost, d)
				}
			}
			if s.Mem != nil {
				fm.MemUsed, fm.MemLimit, fm.MemKill = s.Mem.Used, s.Mem.Limit, s.Mem.Kill
			}
			fm.Lost = slices.DeleteFunc(fm.Lost, func(l Held) bool { // set again since: not lost
				return slices.ContainsFunc(s.Held, func(h Held) bool { return h.Knob == l.Knob && h.Value == l.Value })
			})
			fm.Frozen, fm.CPUUsed, fm.CPULimit, fm.Throttled = s.Frozen, round2(s.CPUUsed), s.CPULimit, round2(s.Throttled)
			for _, o := range s.SLOs {
				h := histOf(o.Hist)
				p := h.quantile(.99)
				if h.n == 0 && o.P99 > 0 { // relayed: a p99 without its histogram
					p = o.P99
				}
				fm.SLOs = append(fm.SLOs, MemberSLO{Route: o.Route, P99: p, Requests: o.Requests, Met: o.Requests == 0 || p <= o.Objective})
			}
		}
		rep.Members = append(rep.Members, fm)
	}
	rep.Covers = fmt.Sprintf("%d of %d members", answered, len(ms))
	if len(gossiped) > 0 {
		rep.Covers += fmt.Sprintf("; %s from gossip: %s didn't answer in %s", list(gossiped), plural(len(gossiped), "it", "they"), wait)
	}
	if len(silent) > 0 {
		rep.Covers += "; can't vouch for " + list(silent)
	}

	// Each objective across the fleet: histograms merged, gossiped ones too, then the percentile.
	for _, o := range in.objectives() {
		var h hist
		var req int64
		for _, s := range sums {
			if s == nil {
				continue
			}
			for _, ps := range s.SLOs {
				if ps.Route == o.route {
					m := histOf(ps.Hist)
					h.merge(&m)
					req += ps.Requests
				}
			}
		}
		p := h.quantile(.99)
		fs := FleetSLO{Route: o.route, Objective: o.p99, P99: p, Requests: req, Met: req == 0 || p <= o.p99}
		var notes []string
		for _, m := range rep.Members {
			merged := false
			if i := slices.IndexFunc(ms, func(x Member) bool { return x.ID == m.ID }); i >= 0 && sums[i] != nil {
				merged = slices.ContainsFunc(sums[i].SLOs, func(x peerSLO) bool { return x.Route == o.route && len(x.Hist) > 0 })
			}
			switch {
			case m.Gossiped > 0 && !merged: // relayed: only its p99 came, so it isn't in the merged one
				for _, x := range m.SLOs {
					if x.Route == o.route {
						fs.Met = fs.Met && x.Met
						notes = append(notes, fmt.Sprintf("%s's p99 %s from relayed gossip, %s old, not merged in", m.Name, latency(x.P99), short(m.Gossiped)))
					}
				}
			case m.Gossiped > 0:
				notes = append(notes, fmt.Sprintf("%s's requests from its gossip, %s old", m.Name, short(m.Gossiped)))
			case m.Silent:
				fs.Met = false // unaccounted for, it can't be vouched for
				notes = append(notes, fmt.Sprintf("can't vouch for %s: %s, and it may still be taking traffic", m.Name, m.SilentWhy))
			}
		}
		fs.Partial = strings.Join(notes, "; ")
		rep.SLOs = append(rep.SLOs, fs)
	}
	rep.Differ = differences(rep.Members, sums)
	rep.Common = common(rep.Members, sums)
	rep.StandOut = standOut(rep.Members, rep.Differ, wait)
	return rep
}

// share is how much of a window ending now came before a change this long ago.
func share(ago time.Duration) string {
	if ago <= fleetWindow/2 {
		return "mostly"
	}
	return "partly"
}

// standOut names members over an objective the rest meet, and members
// throttled where the rest aren't.
func standOut(ms []FleetMember, diffs []Difference, wait time.Duration) []StandOut {
	var out []StandOut
	look := func(name string) string {
		return fmt.Sprintf(`metrics, knobs, history or profile with instance=%q`, name)
	}
	// A starved instance waits on everyone: a member that didn't answer it in
	// time but says in its gossip that it is healthy is waiting on this one.
	selfStarved := slices.ContainsFunc(ms, func(m FleetMember) bool { return m.Self && m.Throttled >= .5 })
	healthy := func(m FleetMember) bool {
		return m.Throttled < .1 && len(m.SLOs) > 0 && !slices.ContainsFunc(m.SLOs, func(s MemberSLO) bool { return !s.Met })
	}
	var waitedOn []string
	for _, m := range ms {
		if selfStarved && m.Gossiped > 0 && m.Gossiped <= 3*time.Second && healthy(m) { // fresh word only: a peer gone sick since can't be vouched for
			waitedOn = append(waitedOn, m.Name)
		}
	}
	for i, m := range ms {
		if m.Silent {
			out = append(out, StandOut{m.Name, fmt.Sprintf("%s, and it didn't say it was leaving: it may still be taking traffic, and nothing here can say how it is doing", m.SilentWhy),
				"check that the instance is running and whether the load balancer still sends to it; it leaves this list when it restarts under its name, or after 10 minutes"})
			continue
		}
		// One reason per member: its symptoms, then what is different about it.
		var why []string
		switch {
		case m.Gossiped > 0 && slices.Contains(waitedOn, m.Name): // healthy by its own word: not why it stands out
		case m.Gossiped > 0:
			why = append(why, fmt.Sprintf("alive, but didn't answer in %s: an instance too busy to answer is a likely culprit (its numbers here are from its gossip, %s old)", wait, short(m.Gossiped)))
		}
		for j, s := range m.SLOs {
			if s.Met {
				continue
			}
			var meet []string
			for k, o := range ms {
				if k != i && j < len(o.SLOs) && o.SLOs[j].Route == s.Route && o.SLOs[j].Met {
					meet = append(meet, o.Name)
				}
			}
			if len(meet) > 0 {
				why = append(why, fmt.Sprintf("%s p99 %s is over its objective while %s %s it", s.Route, latency(s.P99), list(meet), plural(len(meet), "meets", "meet")))
			}
		}
		if m.Throttled >= .1 && len(ms) > 1 {
			calm := true
			for k, o := range ms {
				if k != i && !o.Silent && o.Throttled >= .02 {
					calm = false
				}
			}
			if calm {
				why = append(why, fmt.Sprintf("throttled in %.0f%% of CPU periods while the rest aren't", m.Throttled*100))
			}
		}
		if m.Self && len(waitedOn) > 0 {
			why = append(why, fmt.Sprintf("%s didn't answer it in %s, yet %s healthy in %s own gossip: the wait is likely here, on this instance, not there", list(waitedOn), wait, plural(len(waitedOn), "is", "are"), plural(len(waitedOn), "its", "their")))
		}
		var lost, refused []string
		for _, l := range m.Lost {
			if l.Unkept {
				refused = append(refused, fmt.Sprintf("%s %s (%s)", l.Knob, l.Value, l.By))
				continue
			}
			lost = append(lost, fmt.Sprintf("%s %s (%s)", l.Knob, l.Value, l.By))
		}
		died := ""
		if m.Died != "" {
			died = ", " + m.Died
		}
		switch {
		case m.Restarts >= 2:
			why = append(why, fmt.Sprintf("restarted %d times in the last 10 minutes, the last %s ago%s", m.Restarts, short(m.Restarted), died))
		case m.Restarts == 1 && (len(lost) > 0 || m.Died != ""):
			why = append(why, fmt.Sprintf("restarted %s ago%s", short(m.Restarted), died))
		}
		if len(lost) > 0 {
			why = append(why, "the restart dropped what was set on it at runtime: "+strings.Join(lost, ", "))
		}
		if len(refused) > 0 {
			why = append(why, "vis stopped setting back "+strings.Join(refused, ", ")+", though its writer asked to keep it: the process it was set back on died within a minute, so it may be what kills it")
		}
		// Short of memory where the rest aren't: the next to be killed. Every member short is what they share (common).
		shortOf := func(m FleetMember) bool {
			return slices.ContainsFunc(m.Pressures, func(p Pressure) bool { return p.Resource == Memory })
		}
		shortAlone := shortOf(m) && slices.ContainsFunc(ms, func(o FleetMember) bool { return o.Name != m.Name && !o.Silent && !shortOf(o) })
		if len(why) == 0 && !shortAlone {
			continue
		}
		if m.LastChange != "" && m.LastAgo < fleetWindow { // its numbers straddle its newest change
			why = append(why, fmt.Sprintf("changed %s ago (%s), so these numbers, over the last %s, are %s from before it: impact with instance=%q says what the change did", short(m.LastAgo), m.LastChange, short(fleetWindow), share(m.LastAgo), m.Name))
		}
		for _, p := range m.Pressures { // what it's short of, and what spends it there
			var lv []string
			for _, l := range p.Levers[:min(len(p.Levers), 3)] {
				if l.People {
					lv = append(lv, l.Knob+" at "+l.Value+" (a person's to change)")
					continue
				}
				lv = append(lv, l.Knob+" at "+l.Value)
			}
			switch {
			case len(lv) > 0:
				why = append(why, fmt.Sprintf("short of %s there (%s); what spends it there: %s", p.Resource, p.Why, strings.Join(lv, ", ")))
			case p.Resource == Memory && shortAlone:
				why = append(why, fmt.Sprintf("short of memory there (%s)", p.Why))
			}
		}
		w := strings.Join(why, "; ")
		switch c := causes(m, diffs); {
		case len(c) > 0:
			w += ". What's different about it: " + strings.Join(c, "; ")
		case shortAlone: // a leak: the rest, set alike, hold
			w += fmt.Sprintf(". Yet nothing vis compares differs between it and the rest (build, CPU limit, knobs), so what grows is likely in the code or the traffic it gets: profile with instance=%q says what holds its heap", m.Name)
		}
		out = append(out, StandOut{m.Name, w, look(m.Name)})
	}
	return out
}

// differences lists what isn't the same on every member with numbers to show:
// the CPU limit and the build first, which often explain a member alone, then knobs.
func differences(ms []FleetMember, sums []*peerSummary) []Difference {
	var out []Difference
	add := func(what string, value func(i int) (string, bool), setBy func(i int) string) {
		d := Difference{What: what, Values: map[string]string{}}
		seen := map[string]bool{}
		for i, m := range ms {
			if m.Silent {
				continue
			}
			if v, ok := value(i); ok {
				d.Values[m.Name] = v
				seen[v] = true
				if by := setBy(i); by != "" {
					if d.SetBy == nil {
						d.SetBy = map[string]string{}
					}
					d.SetBy[m.Name] = by
				}
			}
		}
		if len(seen) > 1 {
			out = append(out, d)
		}
	}
	none := func(int) string { return "" }
	add("cpu_limit", func(i int) (string, bool) {
		if l := ms[i].CPULimit; l > 0 {
			return fmt.Sprintf("%g cores", l), true
		}
		return "no limit", true
	}, none)
	add("build", func(i int) (string, bool) { b := ms[i].Build; return b[:min(len(b), 7)], b != "" }, none)
	for _, d := range allDecls() {
		info, _ := Lookup(d.name)
		add(d.name, func(i int) (string, bool) {
			if i >= len(sums) || sums[i] == nil || sums[i].Knobs == nil {
				return "", false
			}
			v, ok := sums[i].Knobs[d.name]
			return fmt.Sprint(showValue(info, v)), ok
		}, func(i int) string {
			if i < len(sums) && sums[i] != nil {
				return sums[i].SetBy[d.name]
			}
			return ""
		})
	}
	return out
}

// causes says what is different about m from the rest, where the rest agree.
func causes(m FleetMember, diffs []Difference) []string {
	var out []string
	for _, d := range diffs {
		v, ok := d.Values[m.Name]
		if !ok {
			continue
		}
		rest := ""
		for name, o := range d.Values {
			if name == m.Name {
				continue
			}
			if rest != "" && o != rest {
				rest = "" // the rest disagree among themselves: no odd one out
				break
			}
			rest = o
		}
		if rest == "" || rest == v {
			continue
		}
		switch d.What {
		case "cpu_limit":
			out = append(out, fmt.Sprintf("its container's CPU limit is %s, while the others have %s", v, rest))
		case "build":
			out = append(out, fmt.Sprintf("it runs build %s, while the others run %s", v, rest))
		default:
			b := d.SetBy[m.Name]
			if r, ok := strings.CutPrefix(b, "rule:"); ok { // its rule reacting: worth knowing, not a cause
				out = append(out, fmt.Sprintf("its rule %s moved %s to %s (the others have %s)", r, d.What, v, rest))
				continue
			}
			by := ""
			if b != "" {
				by = " (set by " + b + ")"
			}
			out = append(out, fmt.Sprintf("its %s is %s%s, while the others have %s", d.What, v, by, rest))
		}
	}
	return out
}

// latency reads as people say it: 20 ms, 2.6 s.
func latency(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

func list(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

type (
	fleetMemberOut struct {
		Name      string         `json:"name"`
		ID        string         `json:"id"`
		Addr      string         `json:"addr,omitempty"`
		Build     string         `json:"build"`
		State     string         `json:"state" jsonschema:"alive, suspect (not heard from for 5 s) or dead (15 s)"`
		Self      bool           `json:"self,omitempty" jsonschema:"the instance you are connected to"`
		Frozen    bool           `json:"frozen,omitempty"`
		CPUUsed   float64        `json:"cpu_used"`
		CPULimit  float64        `json:"cpu_limit" jsonschema:"cores; 0 when unlimited or unknown"`
		Throttled float64        `json:"throttled" jsonschema:"share of CPU periods its container was paused at its limit"`
		SLOs      []memberSLOOut `json:"slos"`
		Gossiped  string         `json:"from_gossip,omitempty" jsonschema:"it didn't answer: these numbers are from its last gossip, this old"`
		Silent    bool           `json:"unaccounted,omitempty" jsonschema:"no numbers to show: not heard from, so it can't be vouched for, and it may still be taking traffic"`
		Held      []heldOut      `json:"held" jsonschema:"values it holds above its defaults: who set each, and when it ends"`
		Restarts  int            `json:"restarts,omitempty" jsonschema:"restarts under its name in the last 10 minutes"`
		Restarted string         `json:"restarted,omitempty" jsonschema:"how long ago the last one was"`
		Lost      []heldOut      `json:"lost,omitempty" jsonschema:"what its last restart dropped: runtime values live in the process"`
		Died      string         `json:"died,omitempty" jsonschema:"what most likely killed it the last time it restarted"`
		MemUsed   uint64         `json:"mem_used_mb,omitempty" jsonschema:"its container's memory in use, MiB"`
		MemLimit  uint64         `json:"mem_limit_mb,omitempty" jsonschema:"its container's memory limit, MiB"`
		MemKill   string         `json:"mem_limit_in,omitempty" jsonschema:"how soon its memory reaches the limit at the current rate, or now; absent when it isn't heading there"`
		Pressures []Pressure     `json:"pressures,omitempty" jsonschema:"what it is short of, with the knobs that spend it there"`
		Last      string         `json:"last_change,omitempty" jsonschema:"its newest knob change by a person or an agent, or a lease of theirs ending, and how long ago: where a lapsed fix or a harmful change starts"`
	}
	heldOut struct {
		Knob   string `json:"knob"`
		Value  string `json:"value"`
		By     string `json:"by"`
		EndsIn string `json:"ends_in,omitempty" jsonschema:"when it falls back to the layer below; absent if never"`
		Unkept bool   `json:"unkept,omitempty" jsonschema:"a kept value vis stopped setting back: the process it was set back on died within a minute, so it may be what kills it"`
	}
	memberSLOOut struct {
		Route    string  `json:"route"`
		P99      float64 `json:"p99_ms"`
		Requests int64   `json:"requests"`
		Met      bool    `json:"met"`
	}
	fleetSLOOut struct {
		Route     string  `json:"route"`
		Objective float64 `json:"objective_ms"`
		P99       float64 `json:"fleet_p99_ms" jsonschema:"the p99 of every member's requests together, from merged histograms"`
		Requests  int64   `json:"requests"`
		Met       bool    `json:"met"`
		Partial   string  `json:"partial,omitempty" jsonschema:"members whose requests aren't merged in, because they didn't answer, and what their gossip says"`
	}
	standOutOut struct {
		Member string `json:"member"`
		Why    string `json:"why"`
		Look   string `json:"look" jsonschema:"how to look closer and act on that member alone"`
	}
	differOut struct {
		What   string            `json:"what" jsonschema:"cpu_limit, build, or a knob's name"`
		Values map[string]string `json:"values" jsonschema:"by member name"`
		SetBy  map[string]string `json:"set_by,omitempty" jsonschema:"a knob's: who set it, on members where it isn't at its default"`
	}
	fleetOut struct {
		Covers   string           `json:"covers" jsonschema:"how many members answered: a partial answer says so"`
		Age      string           `json:"age" jsonschema:"how long ago the members were asked"`
		Members  []fleetMemberOut `json:"members"`
		SLOs     []fleetSLOOut    `json:"slos"`
		StandOut []standOutOut    `json:"stand_out" jsonschema:"members that differ from the rest in a way that matters, each with how to look closer"`
		Differ   []differOut      `json:"differ" jsonschema:"what isn't the same on every member: its CPU limit, its build, its knobs"`
		Missing  []string         `json:"missing,omitempty" jsonschema:"members that didn't answer, and why"`
		Common   []string         `json:"common,omitempty" jsonschema:"what every member shares, which a comparison can't show: a pressure, a change in their environment, a value they all hold and when it ends"`
		Rollout  *rolloutReport   `json:"last_rollout,omitempty" jsonschema:"the fleet's last rollout in the last 30 minutes, whichever member ran it: its state, and once stage 2 is measured, each member's before and after and the fleet's"`
	}
)

func fleetJSON(r FleetReport) fleetOut {
	out := fleetOut{Covers: r.Covers, Common: r.Common, Age: short(time.Since(r.At)), Members: []fleetMemberOut{}, SLOs: []fleetSLOOut{}, StandOut: []standOutOut{}, Differ: []differOut{}, Missing: r.Missing}
	for _, m := range r.Members {
		fm := fleetMemberOut{Name: m.Name, ID: m.ID, Addr: m.Addr, Build: m.Build, State: m.State, Self: m.Self, Frozen: m.Frozen,
			CPUUsed: m.CPUUsed, CPULimit: m.CPULimit, Throttled: m.Throttled, SLOs: []memberSLOOut{}}
		if m.Gossiped > 0 {
			fm.Gossiped = short(m.Gossiped)
		}
		fm.Silent = m.Silent
		held := func(hs []Held) []heldOut {
			out := []heldOut{}
			for _, h := range hs {
				o := heldOut{Knob: h.Knob, Value: h.Value, By: h.By, Unkept: h.Unkept}
				if h.EndsIn > 0 {
					o.EndsIn = ceilSecond(h.EndsIn).String()
				}
				out = append(out, o)
			}
			return out
		}
		fm.Held, fm.Lost, fm.Restarts, fm.Pressures = held(m.Held), held(m.Lost), m.Restarts, m.Pressures
		if m.Restarts > 0 {
			fm.Restarted = short(m.Restarted)
		}
		fm.Died, fm.MemUsed, fm.MemLimit = m.Died, m.MemUsed>>20, m.MemLimit>>20
		switch {
		case m.MemKill >= time.Second:
			fm.MemKill = about(m.MemKill)
		case m.MemKill > 0:
			fm.MemKill = "now"
		}
		if m.LastChange != "" {
			fm.Last = m.LastChange + ", " + short(m.LastAgo) + " ago"
			if m.LastAgo < fleetWindow {
				fm.Last += fmt.Sprintf(": its numbers here, over the last %s, are %s from before it", short(fleetWindow), share(m.LastAgo))
			}
		}
		for _, s := range m.SLOs {
			fm.SLOs = append(fm.SLOs, memberSLOOut{s.Route, msf(s.P99), s.Requests, s.Met})
		}
		out.Members = append(out.Members, fm)
	}
	for _, s := range r.SLOs {
		out.SLOs = append(out.SLOs, fleetSLOOut{s.Route, msf(s.Objective), msf(s.P99), s.Requests, s.Met, s.Partial})
	}
	for _, s := range r.StandOut {
		out.StandOut = append(out.StandOut, standOutOut(s))
	}
	for _, d := range r.Differ {
		out.Differ = append(out.Differ, differOut(d))
	}
	return out
}
