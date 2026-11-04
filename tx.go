package vis

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Tx is one change: one writer setting or clearing any number of knobs, all
// or nothing. Everything it sets shares one lease, so values set together
// expire, hold during a freeze, and revert together.
type Tx struct {
	By     Writer
	For    time.Duration // the lease; required when an automated writer sets values, optional for people
	Reason string
	Set    []Write
	Step   []Step
	Clear  []Removal
	// Keep sets the values back after a restart, for what is left of the lease:
	// for a fix the next restart would undo. It needs a lease.
	Keep bool

	back bool // a restart set it back (succeed): its next death may stop it (keepHolds)
}

// Step moves one knob by a relative amount from the value in effect, clamped
// to its range. vis computes it under its own lock, so a rule never races
// another writer between reading a value and writing the next one. A step that
// can't move, because the knob is at a bound, writes nothing.
type Step struct {
	Knob string
	By   int64
}

// Write sets one knob in a Tx. The layer follows from the Tx's writer.
type Write struct {
	Knob  string
	Value int64
}

// Removal clears one layer of one knob in a Tx. A person may clear any layer;
// an automated writer only its own.
type Removal struct {
	Knob  string
	Layer Layer
}

// Record is what a Tx did.
type Record struct {
	Change   uint64     // shared by every audit record the Tx wrote
	Lease    uint64     // the lease group its values share; 0 means no expiry
	Effects  []Effect   // one per knob: sets, then steps, then clears, each in the Tx's order
	Replaced []Replaced // values the Tx's writes took the place of, in its own layer
}

// Replaced is a value a write took the place of in its own layer: setting a
// knob where the writer's layer already held one ends that one. Left is what
// was left of its lease; zero means it had none.
type Replaced struct {
	Knob  string
	Value int64
	By    Writer
	Left  time.Duration
}

// Effect is what is in effect for one knob after a change. It may differ from
// what was asked for, for example when a higher layer still holds the knob.
type Effect struct {
	Knob   string
	Value  int64
	Layer  Layer
	Change uint64
	Lease  uint64
}

// Change writes one value into one knob. Set applies it as a one-entry Tx.
type Change struct {
	Knob   string
	Value  int64
	For    time.Duration // the lease; required for automated writers, optional for people
	By     Writer        // decides the layer: people pin, agents write the agent layer, and so on
	Reason string
}

// lease is a group of values set by one Tx. A member whose slot was since
// overwritten has left the group; a group with no members left is dropped.
type lease struct {
	id      uint64
	layer   Layer
	by      Writer
	total   time.Duration // the lease as granted
	warn    *time.Timer   // the "ending" warning, for deliberate changes; nil while frozen
	warned  bool
	until   time.Time
	left    time.Duration // time remaining, kept while a freeze stops the clock
	timer   *time.Timer   // nil while frozen
	members []int         // knob indexes
	keep    bool          // set back after a restart (lastword.go)
	back    bool          // set back by one (succeed)
	set     time.Time     // when it was written
}

// Set writes one value and returns what is in effect afterwards.
func (in *Instance) Set(c Change) (Effect, error) {
	rec, err := in.Apply(Tx{By: c.By, For: c.For, Reason: c.Reason, Set: []Write{{c.Knob, c.Value}}})
	if err != nil {
		return Effect{}, err
	}
	return rec.Effects[0], nil
}

// Clear removes one layer from a knob and returns what is in effect afterwards.
func (in *Instance) Clear(knob string, l Layer, by Writer, reason string) (Effect, error) {
	rec, err := in.Apply(Tx{By: by, Reason: reason, Clear: []Removal{{knob, l}}})
	if err != nil {
		return Effect{}, err
	}
	return rec.Effects[0], nil
}

type step struct {
	d     *decl
	layer Layer
	set   bool
	value int64
	step  int64 // the relative amount, for a Step
	noop  bool  // a Step at a bound: nothing to write
}

// Apply validates every part of tx before touching anything, then applies it
// in one step: one published snapshot, one change ID, one lease.
func (in *Instance) Apply(tx Tx) (Record, error) {
	in.mu.Lock()
	defer in.notify()
	defer in.mu.Unlock()

	if !tx.By.valid() {
		return Record{}, ErrActorRequired
	}
	if tx.Keep && tx.For <= 0 {
		return Record{}, errors.New("vis: keep needs a lease: a value kept with no end would outlive every restart, and the reason for it")
	}
	steps, err := in.check(tx)
	if err != nil {
		return Record{}, err
	}
	writes := 0
	for _, s := range steps {
		if !s.noop {
			writes++
		}
	}
	if writes == 0 { // only steps stuck at a bound: report where things stand, change nothing
		rec := Record{}
		for _, s := range steps {
			e := in.effect(s.d.idx)
			rec.Effects = append(rec.Effects, Effect{Knob: s.d.name, Value: e.Value, Layer: e.Layer})
		}
		return rec, nil
	}

	in.change++
	in.changedAt = time.Now()
	rec := Record{Change: in.change}
	var ls *lease
	if tx.For > 0 && slices.ContainsFunc(steps, func(s step) bool { return s.set && !s.noop }) {
		in.leaseN++
		ls = &lease{id: in.leaseN, layer: tx.By.Kind.layer(), by: tx.By, total: tx.For, until: time.Now().Add(tx.For), keep: tx.Keep, back: tx.back, set: time.Now()}
		if in.freeze.Frozen {
			ls.left = tx.For // the clock starts when the freeze lifts
		} else {
			ls.timer = in.leaseTimer(ls, tx.For)
			in.scheduleWarning(ls, tx.For)
		}
		in.leases[ls.id] = ls
		rec.Lease = ls.id
	}

	touched := make([]touch, len(steps))
	for i, s := range steps {
		touched[i] = touch{d: s.d, old: in.effect(s.d.idx), noop: s.noop}
	}
	for i, s := range steps {
		if s.noop {
			continue
		}
		attrs := []slog.Attr{slog.String("layer", s.layer.String()), slog.String("actor", tx.By.String())}
		if s.set {
			if old := in.knobs[s.d.idx].layers[s.layer]; old.set {
				r := Replaced{Knob: s.d.name, Value: old.value, By: old.by}
				if l := old.lease; l != nil && l.timer == nil {
					r.Left = l.left // frozen: its clock is stopped
				} else if l != nil {
					r.Left = max(time.Until(l.until), time.Nanosecond)
				}
				rec.Replaced = append(rec.Replaced, r)
			}
			in.replace(s.d.idx, s.layer, slot{set: true, value: s.value, by: tx.By, change: rec.Change, lease: ls})
			if ls != nil {
				ls.members = append(ls.members, s.d.idx)
				attrs = append(attrs, slog.String("lease", tx.For.String())) // readable in any log store, unlike JSON nanoseconds
				if ls.keep {
					attrs = append(attrs, slog.Bool("keep", true))
				}
			}
			op := "set"
			if s.step != 0 {
				op = "step"
				attrs = append(attrs, s.d.attr("by", s.step))
			}
			attrs = append(attrs, slog.String("op", op), s.d.attr("value", s.value), s.d.attr("then", below(&in.knobs[s.d.idx].layers, s.layer)))
		} else {
			in.replace(s.d.idx, s.layer, slot{})
			attrs = append(attrs, slog.String("op", "clear"))
		}
		if tx.Reason != "" {
			attrs = append(attrs, slog.String("reason", tx.Reason))
		}
		touched[i].attrs = attrs
	}
	rec.Effects = in.finish(rec.Change, rec.Lease, touched)
	return rec, nil
}

// Preview runs every check Apply would and reports what would be in effect
// afterwards, without changing anything: the plan a person or an agent reads
// before deciding. The record has no change or lease ID.
func (in *Instance) Preview(tx Tx) (Record, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	steps, err := in.check(tx)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	for _, s := range steps {
		layers := in.knobs[s.d.idx].layers // a copy: the array, not a pointer to it
		if !s.noop {
			if s.set {
				layers[s.layer] = slot{set: true, value: s.value}
			} else {
				layers[s.layer] = slot{}
			}
		}
		e := effectOf(&layers)
		rec.Effects = append(rec.Effects, Effect{Knob: s.d.name, Value: e.Value, Layer: e.Layer})
	}
	return rec, nil
}

// check is what Apply and Preview share: the writer, the lease, and every step. Callers hold in.mu.
func (in *Instance) check(tx Tx) ([]step, error) {
	if !tx.By.valid() {
		return nil, ErrActorRequired
	}
	if tx.For < 0 || tx.By.Kind != Human && len(tx.Set)+len(tx.Step) > 0 && tx.For == 0 {
		return nil, fmt.Errorf("%w (%v, lease %v)", ErrLeaseRequired, tx.By, tx.For)
	}
	return in.plan(tx)
}

// plan resolves and checks every part of tx. Nothing changes if any part fails.
func (in *Instance) plan(tx Tx) ([]step, error) {
	var steps []step
	seen := map[int]bool{}
	add := func(name string, l Layer, set bool, v int64) error {
		d, err := in.decl(name)
		if err != nil {
			return err
		}
		if seen[d.idx] {
			return fmt.Errorf("%w: %s appears twice in one change", ErrConflict, d.name)
		}
		seen[d.idx] = true
		switch {
		case set && !d.allowed(v):
			return fmt.Errorf("%w: %s=%s, allowed %s", ErrOutOfRange, d.name, d.format(v), d.rangeText())
		case set && d.isSpecial(v) && tx.By.Kind == Rule:
			return fmt.Errorf("%w: %s=%s: a rule never sets a knob's special value; people and agents choose it", ErrOutOfRange, d.name, d.format(v))
		case !set && (l <= DefaultLayer || int(l) >= numLayers):
			return fmt.Errorf("%w (layer %v)", ErrDefaultLayer, l)
		}
		if err := in.gate(d, tx.By, l); err != nil {
			return err
		}
		steps = append(steps, step{d: d, layer: l, set: set, value: v})
		return nil
	}
	for _, w := range tx.Set {
		if err := add(w.Knob, tx.By.Kind.layer(), true, w.Value); err != nil {
			return nil, err
		}
	}
	for _, st := range tx.Step {
		d, err := in.decl(st.Knob)
		if err != nil {
			return nil, err
		}
		cur := in.effect(d.idx).Value
		if d.isSpecial(cur) {
			return nil, fmt.Errorf("%w: %s is %s, its special value: a step has nothing to move from; set a value instead", ErrOutOfRange, d.name, d.format(cur))
		}
		to := clampAdd(cur, st.By, d.min, d.max)
		if err := add(st.Knob, tx.By.Kind.layer(), true, to); err != nil {
			return nil, err
		}
		steps[len(steps)-1].step, steps[len(steps)-1].noop = st.By, to == cur
	}
	for _, r := range tx.Clear {
		if err := add(r.Knob, r.Layer, false, 0); err != nil {
			return nil, err
		}
	}
	if len(steps) == 0 {
		return nil, ErrEmptyChange
	}
	return steps, nil
}

// below is what a knob falls back to when layer l ends: the top set layer under it.
func below(ls *[numLayers]slot, l Layer) int64 {
	for i := l - 1; i >= DefaultLayer; i-- {
		if ls[i].set {
			return ls[i].value
		}
	}
	return ls[DefaultLayer].value
}

// clampAdd is v+by kept inside min..max, without overflowing. A step from
// inside the range stays in it, so it never lands on the special value.
func clampAdd(v, by, lo, hi int64) int64 {
	switch { // v is inside lo..hi, so hi-v and lo-v can't overflow
	case by > 0 && by > hi-v:
		return hi
	case by < 0 && by < lo-v:
		return lo
	}
	return v + by
}

// Revert ends a lease group early, as if it had expired: every value it still
// holds drops to the layer below. It is gated like any change to those knobs.
func (in *Instance) Revert(leaseID uint64, by Writer, reason string) (Record, error) {
	in.mu.Lock()
	defer in.notify()
	defer in.mu.Unlock()
	ls := in.leases[leaseID]
	if ls == nil {
		if err := in.formerOf("lease", leaseID); err != nil {
			return Record{}, fmt.Errorf("%w: %w", ErrUnknownLease, err)
		}
		return Record{}, fmt.Errorf("%w: %d", ErrUnknownLease, leaseID)
	}
	for _, i := range in.live(ls) {
		if err := in.gate(in.decls[i], by, ls.layer); err != nil {
			return Record{}, err
		}
	}
	attrs := []slog.Attr{slog.String("op", "revert"), slog.String("actor", by.String())}
	if reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	return in.end(ls, attrs), nil
}

// gate is the one check every write, clear and revert goes through: who the
// writer is, which layer it may touch, the knob's pull policy, and the freeze.
// Callers hold in.mu.
func (in *Instance) gate(d *decl, by Writer, l Layer) error {
	switch {
	case !by.valid():
		return ErrActorRequired
	case by.Kind != Human && l != by.Kind.layer():
		return fmt.Errorf("%w: %v may not touch the %v layer", ErrNotAllowed, by, l)
	case !d.pulls(by.Kind):
		return fmt.Errorf("%w: %s does not pull from %v writers", ErrNotAllowed, d.name, by.Kind)
	case in.freeze.Frozen && by.Kind != Human:
		return fmt.Errorf("%w (by %v)", ErrFrozen, in.freeze.By)
	}
	return nil
}

// warnBefore is how long before a deliberate change runs out vis says so:
// a tenth of the lease, at most five minutes.
func warnBefore(total time.Duration) time.Duration { return min(total/10, 5*time.Minute) }

// scheduleWarning sets the "ending" warning for a lease with left to run.
// Only deliberate changes, by people and agents, warn: rule and fleet leases
// come and go as designed. Callers hold in.mu.
func (in *Instance) scheduleWarning(ls *lease, left time.Duration) {
	if ls.warned || ls.by.Kind != Human && ls.by.Kind != Agent {
		return
	}
	ls.warn = time.AfterFunc(max(left-warnBefore(ls.total), 0), func() { in.warnEnding(ls) })
}

// warnEnding records, for each knob a lease still holds, that it is about to
// fall back and to what, so whoever made the change can extend it or make it
// the default in code before the old value returns.
func (in *Instance) warnEnding(ls *lease) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.leases[ls.id] != ls || in.freeze.Frozen || ls.warned {
		return
	}
	ls.warned = true
	left := ls.until.Sub(time.Now()).Round(time.Second)
	for _, i := range in.live(ls) {
		d, then := in.decls[i], in.knobs[i].layers[DefaultLayer].value
		for l := ls.layer - 1; l >= DefaultLayer; l-- {
			if s := in.knobs[i].layers[l]; s.set {
				then = s.value
				break
			}
		}
		in.audit(slog.LevelWarn, "vis.lease", slog.String("event", "ending"), slog.Uint64("lease_id", ls.id), slog.String("knob", d.name),
			d.attr("value", in.knobs[i].layers[ls.layer].value), d.attr("then", then), slog.String("left", left.String()),
			slog.String("actor", ls.by.String()), slog.String("layer", ls.layer.String()))
	}
}

func (in *Instance) leaseTimer(ls *lease, d time.Duration) *time.Timer {
	return time.AfterFunc(d, func() { in.expire(ls) })
}

// expire ends a lease when its clock runs out.
func (in *Instance) expire(ls *lease) {
	in.mu.Lock()
	defer in.notify()
	defer in.mu.Unlock()
	if in.leases[ls.id] != ls || in.freeze.Frozen {
		return // reverted or emptied since, or a freeze stopped the clock as it fired
	}
	in.end(ls, []slog.Attr{slog.String("op", "expire"), slog.String("actor", "lease"), slog.String("holder", ls.by.String())})
}

// end drops every value a lease group still holds, in one step. Callers hold in.mu and defer in.notify.
func (in *Instance) end(ls *lease, attrs []slog.Attr) Record {
	in.change++
	in.changedAt = time.Now()
	rec := Record{Change: in.change, Lease: ls.id}
	var touched []touch
	for _, i := range in.live(ls) {
		touched = append(touched, touch{d: in.decls[i], old: in.effect(i),
			attrs: append([]slog.Attr{slog.String("layer", ls.layer.String())}, attrs...)})
		in.knobs[i].layers[ls.layer] = slot{}
	}
	in.drop(ls)
	rec.Effects = in.finish(rec.Change, rec.Lease, touched)
	return rec
}

// live lists the knobs a lease still holds. Callers hold in.mu.
func (in *Instance) live(ls *lease) []int {
	var out []int
	for _, i := range ls.members {
		if in.knobs[i].layers[ls.layer].lease == ls {
			out = append(out, i)
		}
	}
	return out
}

func (in *Instance) drop(ls *lease) {
	if ls.timer != nil {
		ls.timer.Stop()
	}
	if ls.warn != nil {
		ls.warn.Stop()
	}
	delete(in.leases, ls.id)
}

// replace swaps a slot. A lease left with nothing to hold is dropped, timer and all. Callers hold in.mu.
func (in *Instance) replace(i int, l Layer, s slot) {
	old := in.knobs[i].layers[l]
	in.knobs[i].layers[l] = s
	if old.lease != nil && old.lease != s.lease && len(in.live(old.lease)) == 0 {
		in.drop(old.lease)
	}
}

// touch is one knob a change touched: its effect before, and its audit attributes.
// actor is who an audit record names as making the change.
func actor(attrs []slog.Attr) string {
	for _, a := range attrs {
		if a.Key == "actor" {
			return a.Value.String()
		}
	}
	return ""
}

type touch struct {
	d     *decl
	old   Effect
	attrs []slog.Attr
	noop  bool // reported, not written or audited
}

// finish publishes one new Config for the whole change, writes one audit record
// per knob with the value actually in effect, and queues Bind callbacks.
// Callers hold in.mu and defer in.notify.
func (in *Instance) finish(change, leaseID uint64, touched []touch) []Effect {
	effects := make([]Effect, len(touched))
	var vals []int64
	for i, t := range touched {
		now := in.effect(t.d.idx)
		effects[i] = Effect{Knob: t.d.name, Value: now.Value, Layer: now.Layer, Change: change, Lease: leaseID}
		if now.Value != t.old.Value {
			if vals == nil {
				vals = append([]int64(nil), in.cfg.Load().vals...)
			}
			vals[t.d.idx] = now.Value
		}
	}
	if vals != nil {
		in.cfg.Store(&Config{vals: vals}) // one snapshot for the whole change
		var knobs []impactKnob
		for i, t := range touched {
			if !t.noop && effects[i].Value != t.old.Value {
				knobs = append(knobs, impactKnob{t.d.name, t.old.Value, effects[i].Value})
			}
		}
		in.beginImpact(change, time.Now(), knobs, actor(touched[0].attrs))
	}
	for i, t := range touched {
		now := effects[i]
		if t.noop {
			effects[i].Change, effects[i].Lease = 0, 0
			continue
		}
		attrs := append(t.attrs, slog.String("knob", t.d.name), t.d.attr("old", t.old.Value), t.d.attr("new", now.Value),
			slog.String("in_effect", now.Layer.String()), slog.Uint64("change", change), slog.String("build", in.build))
		if leaseID != 0 {
			attrs = append(attrs, slog.Uint64("lease_id", leaseID))
		}
		// Logged under mu on purpose: the audit trail's order is the publish order.
		// A slow slog handler slows writers and lease expiry, never readers.
		in.audit(slog.LevelInfo, "vis.change", attrs...)
		if now.Value != t.old.Value {
			in.nmu.Lock()
			for _, fn := range in.watch[t.d.idx] {
				d, old, nv := t.d, t.old.Value, now.Value
				in.pending = append(in.pending, func() { in.safely(d.name, func() { fn(old, nv) }) })
			}
			in.nmu.Unlock()
		}
	}
	return effects
}
