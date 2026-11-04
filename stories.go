package vis

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Story is what happened, told for people and agents alike: the UI shows it,
// and MCP's history tool returns it. One story follows one thing to its end,
// so it stays true as that thing unfolds: a rule's whole run, a change with
// its warning and its expiry, a freeze until it lifts.
type Story struct {
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`  // rule, change, freeze, environment, watcher
	At    time.Time `json:"at"`    // when it began
	Until time.Time `json:"until"` // its latest event
	Alert bool      `json:"alert"` // it needs attention now
	Who   string    `json:"who"`   // who did it, as the viewer reads it: "you", "agent oncall", "rule batch_backoff"
	Title string    `json:"title"`
	// TitleParts says Title in runs, marking who acted, a rule, a knob and its
	// values, for a page that sets them apart from the words between. They
	// join to Title exactly.
	TitleParts []TitlePart `json:"title_parts,omitempty"`
	Body       string      `json:"body"`
	Knob       string      `json:"knob,omitempty"`
	Lease      uint64      `json:"lease,omitempty"` // the lease to revert, while it lasts
	Mark       string      `json:"mark,omitempty"`  // a short label for a timeline: "batch_size 32"

	// The story's parts, for a page that shows them apart from Body (which says
	// them all, for agents and copying): why, in one line; what it moved,
	// measured After its settle time; and what else changed in that window.
	Why        string   `json:"why,omitempty"`
	Effects    []Moved  `json:"effects,omitempty"`
	After      string   `json:"after,omitempty"` // "30s"
	Confounded []string `json:"confounded,omitempty"`
}

// TitlePart is a run of a story's title. Kind is actor (a person or an agent),
// rule, knob or value, or empty for the words between them.
type TitlePart struct {
	Text string `json:"text"`
	Kind string `json:"kind,omitempty"`
}

// title sets the story's title, and its parts, from runs: strings for the words
// between, and parts.
func (d *draft) title(runs ...any) {
	d.TitleParts = nil
	for _, r := range runs {
		switch x := r.(type) {
		case string:
			if x != "" {
				d.TitleParts = append(d.TitleParts, TitlePart{Text: x})
			}
		case TitlePart:
			d.TitleParts = append(d.TitleParts, x)
		case []TitlePart:
			d.TitleParts = append(d.TitleParts, x...)
		}
	}
	var b strings.Builder
	for _, p := range d.TitleParts {
		b.WriteString(p.Text)
	}
	d.Title = b.String()
}

func part(kind, text string) TitlePart { return TitlePart{Text: text, Kind: kind} }

// whoParts is who acted, as teller.who says it, with the name marked: "agent
// claude" is the word agent and the actor claude. first capitalizes it, to begin a title.
func whoParts(who string, first bool) []TitlePart {
	cap := func(s string) string {
		if first {
			return sentence(s)
		}
		return s
	}
	for _, k := range []struct{ prefix, kind string }{{"agent ", "actor"}, {"rule ", "rule"}, {"rollout ", "actor"}} {
		if id, ok := strings.CutPrefix(who, k.prefix); ok {
			return []TitlePart{{Text: cap(k.prefix)}, {Text: id, Kind: k.kind}}
		}
	}
	if who == "a lease" {
		return []TitlePart{{Text: cap(who)}}
	}
	return []TitlePart{{Text: cap(who), Kind: "actor"}}
}

// measured fills a story's effects from a change's impact report, once it's measured.
func (d *draft) measured(im Impact) {
	d.Effects, d.After, d.Confounded = im.Effects, short(im.Settle), im.Confounded
}

// Stories tells the recent history, newest first, as viewer reads it: their
// own actions are "you". Times are relative to now, so a story is as true when
// read as when it was written.
func (in *Instance) Stories(n int, viewer Writer) []Story {
	t := teller{in: in, viewer: viewer, now: time.Now()}
	for _, e := range in.History(MaxHistory) {
		t.add(e)
	}
	out := make([]Story, 0, len(t.stories))
	for _, s := range t.stories {
		out = append(out, s.render(&t))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

type teller struct {
	in      *Instance
	viewer  Writer
	now     time.Time
	stories []*draft
	runs    map[string]*draft // open rule runs, by rule and knob
	leases  map[uint64]*draft // stories by the leases they hold
	pins    map[string]*draft // open pins without a lease, by knob
	freeze  *draft
	env     map[string]*draft // the latest environment story of each kind
	deaths  map[string]*draft // each member's latest death story: a crash loop's deaths join it
}

// draft collects a story's events; render writes its sentences.
type draft struct {
	Story
	events     []Event
	render     func(*teller) Story
	superseded bool // a later story of the same kind replaced it
}

func (d *draft) touch(e Event) { d.events = append(d.events, e); d.Until = e.At }

func (t *teller) start(kind string, e Event, render func(d *draft, t *teller)) *draft {
	d := &draft{Story: Story{ID: fmt.Sprintf("%s-%d", kind, e.At.UnixNano()), Kind: kind, At: e.At, Until: e.At}}
	d.events = []Event{e}
	d.render = func(t *teller) Story { render(d, t); return d.Story }
	t.stories = append(t.stories, d)
	return d
}

func (t *teller) add(e Event) {
	if t.runs == nil {
		t.runs, t.leases, t.pins = map[string]*draft{}, map[uint64]*draft{}, map[string]*draft{}
	}
	f := e.Fields
	str := func(k string) string { s, _ := f[k].(string); return s }
	lease, _ := f["lease_id"].(uint64)
	switch e.Kind {
	case "change":
		switch op := str("op"); {
		case op == "step" || op == "set" && strings.HasPrefix(str("actor"), "rule:"):
			key := str("actor") + "|" + str("knob")
			run := t.runs[key]
			if run == nil {
				run = t.start("rule", e, renderRun)
				t.runs[key] = run
			} else {
				run.touch(e)
			}
			if lease != 0 {
				t.leases[lease] = run
			}
		case op == "set":
			d := t.start("change", e, renderChange)
			if lease != 0 {
				t.leases[lease] = d
			} else if str("layer") == "pin" {
				t.pins[str("knob")] = d
			}
		case op == "expire" || op == "revert":
			if d := t.leases[lease]; d != nil {
				d.touch(e)
				delete(t.leases, lease)
				for k, run := range t.runs {
					if run == d {
						delete(t.runs, k) // a rule's next step starts a new run
					}
				}
			}
		case op == "clear":
			if d := t.pins[str("knob")]; d != nil && str("layer") == "pin" {
				d.touch(e)
				delete(t.pins, str("knob"))
			}
		}
	case "rule":
		switch str("event") {
		case "stalled", "cleared", "stopped":
			if run := t.runs["rule:"+str("rule")+"|"+str("knob")]; run != nil {
				run.touch(e)
			}
		case "disabled", "enabled":
			t.start("rule", e, renderSwitch)
		}
	case "lease":
		if d := t.leases[lease]; d != nil {
			d.touch(e)
		}
	case "freeze":
		if t.freeze == nil {
			t.freeze = t.start("freeze", e, renderFreeze)
		} else {
			t.freeze.touch(e) // a second freeze: its reason joins the first
		}
	case "unfreeze":
		if t.freeze != nil {
			t.freeze.touch(e)
			t.freeze = nil
		}
	case "environment":
		if t.env == nil {
			t.env = map[string]*draft{}
		}
		if prev := t.env[str("event")]; prev != nil {
			prev.superseded = true // the CPU limit changed again: the old story no longer needs attention
		}
		t.env[str("event")] = t.start("environment", e, renderEnvironment)
	case "death": // a member's deaths within keepDead of each other are one crash loop, told from the newest
		if t.deaths == nil {
			t.deaths = map[string]*draft{}
		}
		if d := t.deaths[str("member")]; d != nil && e.At.Sub(d.Until) < keepDead {
			d.touch(e)
			d.At = e.At // it stands where its newest death does
			break
		}
		t.deaths[str("member")] = t.start("death", e, renderDeath)
	case "onchange_panic":
		t.start("watcher", e, func(d *draft, t *teller) {
			d.Alert, d.Knob = true, str("knob")
			d.Title = fmt.Sprintf("A watcher on %s panicked.", str("knob"))
			d.Body = fmt.Sprintf("Its Bind callback panicked with %q. vis recovered and kept going; the app's state built from %s may be stale.", fmt.Sprint(f["panic"]), str("knob"))
		})
	}
}

// renderRun tells a rule's run: how far it moved its knob, whether it helped,
// and whether its change still holds.
func renderRun(d *draft, t *teller) {
	var steps []Event
	var stalled, cleared, ended, stopped *Event
	for i := range d.events {
		e := &d.events[i]
		switch {
		case e.Kind == "change" && (e.Fields["op"] == "step" || e.Fields["op"] == "set"):
			steps = append(steps, *e)
		case e.Kind == "rule" && e.Fields["event"] == "stalled":
			stalled = e
		case e.Kind == "rule" && e.Fields["event"] == "cleared":
			cleared = e
		case e.Kind == "rule" && e.Fields["event"] == "stopped":
			stopped = e
		case e.Kind == "change":
			ended = e
		}
	}
	first, last := steps[0].Fields, steps[len(steps)-1].Fields
	knob, _ := first["knob"].(string)
	rule := strings.TrimPrefix(str(first, "actor"), "rule:")
	d.Knob, d.Who = knob, "rule "+rule
	d.Mark = knob + " " + t.value(knob, last["new"])
	if l, _ := last["lease_id"].(uint64); ended == nil {
		d.Lease = l
	}
	end := "."
	switch {
	case stopped != nil && str(stopped.Fields, "verdict") == "failed":
		end = ", then vis undid it and stopped the rule: requests failed."
	case stopped != nil && str(stopped.Fields, "verdict") == "no_closer":
		end = ", then vis undid it and stopped the rule: its steps didn't bring the SLO closer."
	case stopped != nil:
		end = ", then vis undid it and stopped the rule: it made the SLO worse."
	case stalled != nil:
		end = ", then stopped: its steps didn't help."
	}
	d.title("Rule ", part("rule", rule), " moved ", part("knob", knob), " from ", part("value", t.value(knob, first["old"])),
		" to ", part("value", t.value(knob, last["new"])), end)
	var body []string
	if stopped != nil {
		body = append(body, sentence(str(stopped.Fields, "reason"))+", so its lever is the wrong one for what is happening now.")
		d.Why = "Its lever is the wrong one for what is happening now." // the effects carry the numbers
	} else if stalled != nil {
		s := stalled.Fields
		unit := str(s, "unit")
		body = append(body, fmt.Sprintf("%s stayed at %s against its %s limit.", str(s, "signal_name"), measure(s["signal"], unit), measure(s["limit"], unit)))
		d.Why = body[0]
	} else if r := str(last, "reason"); r != "" {
		body = append(body, fmt.Sprintf("Its last step: %s.", r))
		d.Why = body[0]
	}
	switch {
	case stopped != nil && ended != nil:
		body = append(body, fmt.Sprintf("vis undid its change: %s is back to %s. The rule stays off until a person or an agent switches it back on.", knob, t.value(knob, ended.Fields["new"])))
	case ended != nil && ended.Fields["op"] == "revert":
		body = append(body, fmt.Sprintf("%s switched it off: %s is back to %s.", sentence(t.who(str(ended.Fields, "actor"))), knob, t.value(knob, ended.Fields["new"])))
	case ended != nil:
		body = append(body, fmt.Sprintf("Its change ran out %s after its last step: %s is back to %s.", short(ended.At.Sub(steps[len(steps)-1].At)), knob, t.value(knob, ended.Fields["new"])))
	default:
		if lease, err := time.ParseDuration(str(last, "lease")); err == nil {
			left := steps[len(steps)-1].At.Add(lease).Sub(t.now)
			body = append(body, fmt.Sprintf("It holds %s at %s for another %s, then %s goes back to %s.", knob, t.value(knob, last["new"]), short(left), knob, t.value(knob, last["then"])))
		}
	}
	if cleared != nil && stopped == nil {
		body = append(body, fmt.Sprintf("Its condition cleared %s ago.", short(t.now.Sub(cleared.At))))
	}
	d.Alert = stalled != nil && ended == nil && cleared == nil || stopped != nil && t.in.rules.stoppedWhy(rule) != ""
	if id, _ := first["change"].(uint64); id != 0 {
		if im, ok := t.in.Impact(id); ok && im.Done {
			body = append(body, "Its first step: "+strings.ToLower(im.Summary[:1])+im.Summary[1:])
			d.measured(im)
		}
	}
	if stopped != nil { // what vis judged it by beats what its first step did
		if fx, ok := stopped.Fields["effects"].([]Moved); ok {
			d.Effects, d.After, d.Confounded = fx, "", nil
		}
	}
	d.Body = strings.Join(body, " ")
}

// renderChange tells a deliberate change by a person or an agent: why, what
// it replaced, and how it ends.
func renderChange(d *draft, t *teller) {
	f := d.events[0].Fields
	knob, layer := str(f, "knob"), str(f, "layer")
	d.Knob, d.Who = knob, t.who(str(f, "actor"))
	verb := "set"
	if layer == "pin" {
		verb = "pinned"
	}
	d.title(whoParts(d.Who, true), " "+verb+" ", part("knob", knob), " to ", part("value", t.value(knob, f["value"])), ".")
	d.Mark = knob + " " + t.value(knob, f["value"])
	var body []string
	if r := str(f, "reason"); r != "" {
		body = append(body, "“"+r+"”")
		d.Why = r
	}
	body = append(body, fmt.Sprintf("It was %s.", t.value(knob, f["old"])))
	if eff := str(f, "in_effect"); eff != "" && eff != layer {
		body = append(body, fmt.Sprintf("It sits under the %s layer, which still holds %s at %s.", eff, knob, t.value(knob, f["new"])))
	}
	lease, _ := time.ParseDuration(str(f, "lease"))
	var warned, end *Event
	for i := range d.events[1:] {
		e := &d.events[i+1]
		if e.Kind == "lease" {
			warned = e
		} else {
			end = e
		}
	}
	switch {
	case end != nil && end.Fields["op"] == "expire":
		body = append(body, fmt.Sprintf("It ended after %s: %s is back to %s.", short(lease), knob, t.value(knob, end.Fields["new"])))
	case end != nil && end.Fields["op"] == "revert":
		body = append(body, fmt.Sprintf("%s reverted it %s later: %s is back to %s.", sentence(t.who(str(end.Fields, "actor"))), short(end.At.Sub(d.At)), knob, t.value(knob, end.Fields["new"])))
	case end != nil: // a pin removed
		body = append(body, fmt.Sprintf("%s removed it %s later: %s is back to %s.", sentence(t.who(str(end.Fields, "actor"))), short(end.At.Sub(d.At)), knob, t.value(knob, end.Fields["new"])))
	case lease == 0:
		body = append(body, "It stays until someone removes the pin.")
	case warned != nil:
		body = append(body, fmt.Sprintf("Ends in %s, then back to %s.", short(d.At.Add(lease).Sub(t.now)), t.value(knob, f["then"])))
		d.Alert = true
	default:
		body = append(body, fmt.Sprintf("For %s, then back to %s.", short(lease), t.value(knob, f["then"])))
	}
	if end == nil {
		d.Lease, _ = f["lease_id"].(uint64)
	}
	if id, _ := f["change"].(uint64); id != 0 {
		if im, ok := t.in.Impact(id); ok && im.Done {
			body = append(body, im.Summary)
			d.measured(im)
		}
	}
	d.Body = strings.Join(body, " ")
}

func renderSwitch(d *draft, t *teller) {
	f := d.events[0].Fields
	d.Who, d.Knob = t.who(str(f, "actor")), str(f, "knob")
	verb := "switched off"
	if f["event"] == "enabled" {
		verb = "switched on"
	}
	d.title(whoParts(d.Who, true), " "+verb+" rule ", part("rule", str(f, "rule")), ".")
	d.Mark = "rule " + str(f, "rule") + " " + strings.TrimPrefix(verb, "switched ")
	if r := str(f, "reason"); r != "" {
		d.Body, d.Why = "“"+r+"”", r
	}
}

func renderFreeze(d *draft, t *teller) {
	first := d.events[0].Fields
	d.Who, d.Mark = t.who(str(first, "actor")), "frozen"
	d.title(whoParts(d.Who, true), " froze automated changes.")
	var body []string
	for _, e := range d.events {
		if r := str(e.Fields, "reason"); r != "" && e.Kind == "freeze" {
			body = append(body, "“"+r+"”")
			if d.Why == "" {
				d.Why = r
			}
		}
	}
	last := d.events[len(d.events)-1]
	if last.Kind == "unfreeze" {
		d.title(whoParts(d.Who, true), " froze automated changes; ", whoParts(t.who(str(last.Fields, "actor")), false), " unfroze them.")
		s := fmt.Sprintf("Unfrozen %s later.", short(last.At.Sub(d.At)))
		if r := str(last.Fields, "reason"); r != "" {
			s = fmt.Sprintf("Unfrozen %s later: “%s”", short(last.At.Sub(d.At)), r)
		}
		body = append(body, s)
	} else {
		d.Alert = true
		body = append(body, fmt.Sprintf("Rules, agents and rollouts hold, every knob keeps its value, and lease clocks stop. Frozen for %s so far.", short(t.now.Sub(d.At))))
	}
	d.Body = strings.Join(body, " ")
}

func renderEnvironment(d *draft, t *teller) {
	f := d.events[0].Fields
	d.Who = "the environment"
	switch str(f, "event") {
	case "cpu_limit_changed":
		old, _ := f["old"].(float64)
		nu, _ := f["new"].(float64)
		switch {
		case old == 0:
			d.Title = fmt.Sprintf("The container now has a CPU limit of %s cores.", num(nu))
		case nu == 0:
			d.Title = fmt.Sprintf("The container's CPU limit of %s cores was removed.", num(old))
		default:
			d.Title = fmt.Sprintf("The container's CPU limit changed from %s to %s cores.", num(old), num(nu))
		}
		d.Alert = nu != 0 && (old == 0 || nu < old) && !d.superseded
		d.Body = "Nobody changed it through vis: an orchestrator or a person resized the container."
		d.Mark = "cpu " + num(nu)
		if nu == 0 {
			d.Mark = "cpu unlimited"
		}
	case "gomaxprocs_changed":
		d.Title = fmt.Sprintf("Go now runs goroutines on %v threads, from %v.", f["new"], f["old"])
		d.Body = "Go follows the container's CPU limit (GOMAXPROCS)."
		d.Mark = fmt.Sprintf("gomaxprocs %v", f["new"])
	}
}

// value is a knob's value as people read it, with its unit: "2000 MiB", "5ms".
func (t *teller) value(knob string, v any) string {
	d := lookup(knob)
	switch x := v.(type) {
	case string: // durations are recorded formatted
		return x
	case int64:
		s := strconv.FormatInt(x, 10)
		if d != nil && d.unit != "" {
			s += " " + counted(x, d.unit)
		}
		return s
	}
	return fmt.Sprint(v)
}

// counted is a unit for n of it: "1 item", "2 items". Only a plural noun is
// made singular (lowercase letters ending in a single s); "ms", "rps" and
// "keys/s" are left as they are.
func counted(n int64, unit string) string {
	if n != 1 && n != -1 || len(unit) < 4 || !strings.HasSuffix(unit, "s") || strings.HasSuffix(unit, "ss") {
		return unit
	}
	for _, c := range unit {
		if c < 'a' || c > 'z' {
			return unit
		}
	}
	return unit[:len(unit)-1]
}

// who names an actor as the viewer reads them.
func (t *teller) who(actor string) string {
	if actor == t.viewer.String() {
		return "you"
	}
	kind, id, _ := strings.Cut(actor, ":")
	switch kind {
	case "human":
		return id
	case "agent":
		return "agent " + id
	case "rule":
		return "rule " + id
	case "fleet":
		return "rollout " + id
	case "lease":
		return "a lease"
	}
	return actor
}

func str(f map[string]any, k string) string { s, _ := f[k].(string); return s }

func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// measure is a signal reading as people read it: "604 ms", "0.25".
func measure(v any, unit string) string {
	f, _ := v.(float64)
	s := num(f)
	if unit != "" {
		s += " " + unit
	}
	return s
}

func num(f float64) string {
	if math.Abs(f) >= 100 {
		return strconv.FormatFloat(math.Round(f), 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', 3, 64)
}

// short is a duration as people say it: "3m", "49s", "1h5m", "30m".
func short(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 && h == 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
