package vis

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// A process that dies without a word (an OOM kill gives none, nor does a
// crash) can't explain itself, and the one that replaces it starts blank. In
// the memory evals a container was killed every 20 to 30 s while vis said
// nothing, and a kill took away the knob value keeping the service up. So
// vis tells the death after the fact, from the last word the dead process
// left: its digest, which its peers keep.

// deathMemory is how near its container's limit a process must have been, at
// its last word or by its growth just after, for vis to call its death most
// likely a kill for memory. The kernel's own count of OOM kills can't say: a
// restarted container starts a new cgroup, and the count with it.
const deathMemory = .95

// killedForMemory reads a dead member's last digest: was it at its memory
// limit, or did its forecast reach the limit within a heartbeat or so of its last word?
func killedForMemory(m *memDigest) bool {
	if m == nil || m.Limit == 0 {
		return false
	}
	return float64(m.Used) >= deathMemory*float64(m.Limit) || m.Kill > 0 && m.Kill <= 5*time.Second
}

// deathWhy is what most likely killed a member, as its death story and the
// fleet say it.
func deathWhy(memory bool) string {
	if memory {
		return "most likely killed for memory"
	}
	return "without a clean stop: a crash, or a kill"
}

// unkept marks the values in lost that vis stopped setting back: kept
// values that killed the process a restart set them back on
// (killedItsProcess), as the former process's kept leases say.
func unkept(lost []Held, last *wireHealth) []Held {
	if last == nil {
		return lost
	}
	for i, l := range lost {
		lost[i].Unkept = slices.ContainsFunc(last.Kept, func(k Held) bool { return heldKey(k) == heldKey(l) && killedItsProcess(k) })
	}
	return lost
}

// noteDeath tells, once, of a member that died: the fleet saw it restarted
// under its name without having left cleanly, or this process read its
// former self's note. n is how many restarts its name has had lately. back is
// set when it was this member's own former process (succeed): the kept values
// set back, by heldKey, with their new leases (0: it failed). A kept value
// that killed the process it was set back on (killedItsProcess) isn't set
// back, and is told apart.
func (in *Instance) noteDeath(name string, r restart, n int, back map[string]uint64) {
	self := back != nil
	in.deathMu.Lock()
	if in.deathsTold == nil {
		in.deathsTold = map[string]bool{}
	}
	told := in.deathsTold[r.id]
	in.deathsTold[r.id] = true
	in.deathMu.Unlock()
	if told {
		return
	}
	attrs := []slog.Attr{slog.String("member", name), slog.Bool("self", self), slog.String("former", r.id),
		slog.Time("died", r.died), slog.Int("restarts", n)}
	if r.last != nil && r.last.Mem != nil {
		m := r.last.Mem
		attrs = append(attrs, slog.Int64("mem_used_mb", int64(m.Used>>20)), slog.Int64("mem_limit_mb", int64(m.Limit>>20)),
			slog.Float64("growth_mb_s", m.Growth/(1<<20)))
		if killedForMemory(m) {
			attrs = append(attrs, slog.String("cause", "memory"))
		}
	}
	var lost, keep []Held
	var earlier []dropped
	if r.last != nil {
		gone := time.Since(time.Unix(0, r.last.At)) // the time since its last word, off each lease
		held := map[string]bool{}
		for _, h := range r.last.Kept {
			if h, ok := leaseLeft(h, gone); ok {
				keep = append(keep, h)
			}
		}
		for _, h := range r.last.Held {
			held[h.Knob] = true
			if h, ok := leaseLeft(h, gone); ok && !slices.ContainsFunc(keep, func(k Held) bool { return heldKey(k) == heldKey(h) }) {
				lost = append(lost, h)
			}
		}
		for _, d := range r.last.Dropped {
			if h, ok := leaseLeft(d.Held, gone); ok && !held[d.Knob] { // what it held is the newer word on that knob
				earlier = append(earlier, dropped{Held: h, Died: d.Died})
			}
		}
	}
	var kept []keptBack
	var unkept []Held
	for _, h := range keep {
		if killedItsProcess(h) {
			unkept = append(unkept, h)
			continue
		}
		if !self { // the restarted member sets it back itself
			kept = append(kept, keptBack{Held: h})
			continue
		}
		lease := back[heldKey(h)]
		if lease == 0 { // it couldn't be set back
			lost = append(lost, h)
			continue
		}
		kept = append(kept, keptBack{h, lease})
	}
	if len(lost) > 0 {
		attrs = append(attrs, slog.Any("lost", lost))
	}
	if len(kept) > 0 {
		attrs = append(attrs, slog.Any("kept", kept))
	}
	if len(unkept) > 0 {
		attrs = append(attrs, slog.Any("unkept", unkept))
	}
	if len(earlier) > 0 {
		attrs = append(attrs, slog.Any("earlier", earlier))
	}
	if self { // carried on, so its next death still names them
		carry := slices.Clone(earlier)
		for _, h := range lost {
			carry = append(carry, dropped{Held: h, Died: r.died.UnixNano()})
		}
		for _, h := range unkept {
			h.Back, h.Age, h.Unkept = false, 0, true
			carry = append(carry, dropped{Held: h, Died: r.died.UnixNano()})
		}
		in.deathMu.Lock()
		in.dropped, in.droppedAt = carry, time.Now()
		in.deathMu.Unlock()
	}
	in.audit(slog.LevelWarn, "vis.death", attrs...)
}

// heldKey names a held value apart from how long it has left.
func heldKey(h Held) string { return h.Knob + "\x00" + h.By + "\x00" + h.Value }

// keptBack is a kept value a restart set back, under a new lease.
type keptBack struct {
	Held
	Lease uint64 `json:"lease,omitempty"` // 0 when another member sets it back
}

// setBackKept sets a kept value back after a restart, as its writer, for what
// is left of its lease, and returns the new lease.
func (in *Instance) setBackKept(h Held) (uint64, error) {
	by, ok := parseWriter(h.By)
	if !ok {
		return 0, fmt.Errorf("vis: no writer %q", h.By)
	}
	info, ok := Lookup(h.Knob)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownKnob, h.Knob)
	}
	v, err := info.Parse(h.Value)
	if err != nil {
		return 0, err
	}
	rec, err := in.Apply(Tx{By: by, For: h.EndsIn, Keep: true, back: true, Reason: "kept across a restart, as its writer asked", Set: []Write{{h.Knob, v}}})
	return rec.Lease, err
}

// dropped is a value an earlier death dropped, on the wire: EndsIn from the
// digest's At, and when the process that held it died.
type dropped struct {
	Held
	Died int64 `json:"died"` // Unix nanoseconds
}

// leaseLeft is h less gone off its lease; false if the lease ran out since.
func leaseLeft(h Held, gone time.Duration) (Held, bool) {
	if h.EndsIn > 0 {
		h.EndsIn = max(h.EndsIn-gone, 0)
		if h.EndsIn == 0 {
			return h, false // nothing to set back
		}
	}
	return h, true
}

// stillDropped is what deaths dropped that nothing has set again (held: what
// it holds now) and whose lease hasn't run out.
func (in *Instance) stillDropped(held []Held) []dropped {
	in.deathMu.Lock()
	defer in.deathMu.Unlock()
	var out []dropped
	for _, d := range in.dropped {
		h, ok := leaseLeft(d.Held, time.Since(in.droppedAt))
		if ok && !slices.ContainsFunc(held, func(g Held) bool { return g.Knob == d.Knob }) {
			out = append(out, dropped{Held: h, Died: d.Died})
		}
	}
	return out
}

// renderDeath tells a member's death: what most likely killed it, what it
// was doing, and what it held that the restart dropped, with how to set it back.
func renderDeath(d *draft, t *teller) {
	f := d.events[len(d.events)-1].Fields
	name := str(f, "member")
	d.Who, d.Alert, d.Mark = "the fleet", true, name+" died"
	why := deathWhy(str(f, "cause") == "memory")
	who := name
	if self, _ := f["self"].(bool); self {
		who = "This member (" + name + ")"
	}
	times := ""
	if len(d.events) > 1 {
		times = fmt.Sprintf(" %d times", len(d.events))
	}
	d.Title = fmt.Sprintf("%s died and restarted%s: %s.", who, times, why)
	var body []string
	if at, ok := f["died"].(time.Time); ok {
		body = append(body, fmt.Sprintf("It went silent at %s.", at.UTC().Format("15:04:05 MST")))
	}
	if n := toFloat(f["restarts"]); n > 1 {
		body = append(body, fmt.Sprintf("That is restart #%d in the last 10 minutes.", int(n)))
	}
	if lim := toFloat(f["mem_limit_mb"]); lim > 0 {
		s := fmt.Sprintf("Its last reading: %.0f of %.0f MiB", toFloat(f["mem_used_mb"]), lim)
		if g := toFloat(f["growth_mb_s"]); g >= .1 {
			s += fmt.Sprintf(", growing %.1f MiB/s", g)
		}
		body = append(body, s+".")
	}
	if lost, _ := f["lost"].([]Held); len(lost) > 0 {
		var held, set []string
		for _, h := range lost {
			held = append(held, heldText(h))
			set = append(set, setBack(name, h))
		}
		body = append(body, "The restart dropped what it held: "+strings.Join(held, "; ")+
			". vis doesn't set it back by itself, since it may be what killed it. To set it back: "+strings.Join(set, "; ")+keepHint)
	}
	if unkept, _ := f["unkept"].([]Held); len(unkept) > 0 {
		for _, h := range unkept { // its age is at the last word, a heartbeat or so before the death
			body = append(body, fmt.Sprintf("vis didn't set back %s, though its writer asked to keep it: the process it was set back on died about %d s after, so it may be what kills it. To set it back anyway: %s.",
				heldText(h), int(h.Age.Round(time.Second)/time.Second), setBack(name, h)))
		}
	}
	if kept, _ := f["kept"].([]keptBack); len(kept) > 0 {
		var held, leases []string
		for _, k := range kept {
			held = append(held, heldText(k.Held))
			if k.Lease != 0 {
				leases = append(leases, fmt.Sprint(k.Lease))
			}
		}
		if len(leases) > 0 {
			body = append(body, fmt.Sprintf("vis set back what its writer asked to keep: %s. If it is what killed it, revert lease %s.",
				strings.Join(held, "; "), strings.Join(leases, ", ")))
		} else {
			body = append(body, fmt.Sprintf("Its writer asked to keep %s: %s sets it back itself.", strings.Join(held, "; "), name))
		}
	}
	if earlier, _ := f["earlier"].([]dropped); len(earlier) > 0 {
		var set []string
		byDeath := map[int64][]string{}
		var deaths []int64
		hint := ""
		for _, e := range earlier {
			if byDeath[e.Died] == nil {
				deaths = append(deaths, e.Died)
			}
			t := heldText(e.Held)
			if e.Unkept {
				t += ", which vis stopped setting back, since it may be what kills it"
			} else {
				hint = keepHint
			}
			byDeath[e.Died] = append(byDeath[e.Died], t)
			set = append(set, setBack(name, e.Held))
		}
		slices.Sort(deaths)
		for _, at := range deaths {
			body = append(body, fmt.Sprintf("An earlier death, at %s UTC, dropped %s, and nothing has set it again since.",
				time.Unix(0, at).UTC().Format("15:04:05"), strings.Join(byDeath[at], "; ")))
		}
		lead := "To set it back: "
		if hint == "" { // every value is one vis stopped setting back: not offered as the fix
			lead = "To set it back anyway: "
		}
		body = append(body, lead+strings.Join(set, "; ")+cmpOrStr(hint, "."))
	}
	d.Body = strings.Join(body, " ")
}

// keepHint ends the set-back calls: how to make a value outlast the next restart.
const keepHint = `. Add "keep":true to set it back after a restart too, once it has held.`

// heldText is a held value as the death story names it.
func heldText(h Held) string {
	left := "no end"
	if h.EndsIn > 0 {
		left = short(h.EndsIn.Round(time.Minute)) + " left"
	}
	return fmt.Sprintf("%s %s (set by %s, %s)", h.Knob, h.Value, h.By, left)
}

// setBack is the set_knob call that sets a dropped value back on member name.
func setBack(name string, h Held) string {
	call := fmt.Sprintf(`set_knob {"instance":%q,"set":{%q:%q}`, name, h.Knob, h.Value)
	if h.EndsIn > 0 {
		call += fmt.Sprintf(`,"for":%q`, short(max(h.EndsIn.Round(time.Minute), time.Minute)))
	}
	return call + `,"reason":"set back after the restart dropped it"}`
}

// kept is every kept lease's value, on any layer: what a restart sets back.
// A kept value under another change is kept too, for when that change ends.
func (in *Instance) kept() []Held {
	in.mu.Lock()
	defer in.mu.Unlock()
	now := time.Now()
	var out []Held
	for _, d := range allDecls() {
		if d.idx >= len(in.knobs) {
			continue
		}
		info, _ := Lookup(d.name)
		for _, s := range in.knobs[d.idx].layers {
			ls := s.lease
			if !s.set || ls == nil || !ls.keep {
				continue
			}
			left := ls.left // frozen: its clock is stopped
			if ls.timer != nil {
				left = max(ls.until.Sub(now), time.Nanosecond)
			}
			out = append(out, Held{Knob: d.name, Value: fmt.Sprint(showValue(info, s.value)), By: s.by.String(), EndsIn: left, Back: ls.back, Age: now.Sub(ls.set)})
		}
	}
	return out
}

// keepHolds is how long a kept value must hold the fresh process a restart
// set it back on: if that process dies sooner, the value may be what kills
// it, and the next restart doesn't set it back. The first death gets the
// benefit of the doubt, since its writer may have set it on a process already
// dying. In the memory evals a 900 MiB cache in a 512 MiB container killed each
// process in about 25 s; a cache that fit held for minutes between a leak's kills.
const keepHolds = time.Minute

// killedItsProcess is whether a kept value h, read from the last word of a
// process that died, failed the breaker (keepHolds).
func killedItsProcess(h Held) bool { return h.Back && h.Age < keepHolds }
