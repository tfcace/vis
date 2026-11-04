package vis

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A rollout changes knobs across the fleet in stages: one member first, then,
// if it got no worse than the rest over the same windows, all the others.
// Every write is the caller's own, on that member, under the caller's token,
// lease and reason, forwarded as instance= calls are, so each member's audit
// trail names who changed it. The order is a hash of each member's name and
// the knob, as the RFC has it, so it stays the same across restarts.
//
// The gate looks for harm, not benefit: the first member's p99 rising by more
// than about one histogram bucket (12%) beyond what the rest did, or its
// objective going from met to unmet, undoes it and stops. At three instances
// this is one member against two over one window: a comparison, and the
// result says so.

type (
	rolloutIn struct {
		Set    map[string]any `json:"set" jsonschema:"knob name to value, in the knob's own terms, as set_knob takes them"`
		For    string         `json:"for" jsonschema:"the lease on every member, like 30m"`
		Reason string         `json:"reason" jsonschema:"why, in a sentence; it goes to every member's audit trail"`
		by     string         // who asked, for its report
	}
	stageOut struct {
		Members  []string    `json:"members" jsonschema:"the members this stage changed"`
		Changes  []changeOut `json:"changes" jsonschema:"each member's change: its change and lease numbers are that member's own"`
		Masked   []string    `json:"masked,omitempty" jsonschema:"members where a higher layer holds the knob, so the change doesn't take effect there"`
		Evidence string      `json:"evidence,omitempty" jsonschema:"what the first member did against the rest, which the gate judged"`
	}
	rolloutOut struct {
		Outcome    string     `json:"outcome" jsonschema:"running (still, past the minute an answer can wait: fleet's last_rollout says how it ends); writing (stage 1 passed, stage 2 still being written: likewise); done; stopped (the rest left alone, and the first stage undone if it did harm, or kept if it couldn't be judged: why says which, undo how to take back what is left); or failed"`
		Why        string     `json:"why,omitempty"`
		Stages     []stageOut `json:"stages"`
		NotReach   []string   `json:"not_reached,omitempty" jsonschema:"members it didn't change: not alive, or not reached"`
		MeasuredAt string     `json:"measured_at,omitempty" jsonschema:"once done, when stage 2 will have been measured: fleet's last_rollout then has each member's before and after and the fleet's"`
		Undo       string     `json:"undo,omitempty" jsonschema:"how to take back whatever the rollout left, on any outcome: one revert per member, and where the change replaced a value in your layer, setting it back while it would have lasted"`
		Note       string     `json:"note"`
	}
)

// minJudged is the fewest requests in each window the gate judges a p99 by.
const minJudged = 20

// stageOrder sorts members by a hash of their name and the knob: the first is stage 1.
func stageOrder(names []string, knob string) []string {
	h := func(n string) uint64 {
		f := fnv.New64a()
		f.Write([]byte(n + "\x00" + knob))
		return f.Sum64()
	}
	out := slices.Clone(names)
	sort.Slice(out, func(i, j int) bool { return h(out[i]) < h(out[j]) })
	return out
}

// answerWithin is how long the rollout tool takes to answer at most: an MCP
// client gives up on a call at a minute. A rollout still running by then
// goes on, and fleet's last_rollout says how it ends.
const answerWithin = 52 * time.Second

// rollout runs the rollout as a job of the ring's, with a member's full
// patience everywhere (the starved member is the one that needs the change),
// and answers with its result, or, past answerWithin, that it is running.
func (t *mcpTools) rollout(ctx context.Context, req *mcp.CallToolRequest, in rolloutIn) (rolloutOut, error) {
	answerBy := time.Now().Add(answerWithin)
	if w, err := t.actor(req); err == nil {
		in.by = w.String()
	}
	if fz := t.in.FreezeState(); fz.Frozen { // a freeze stops rollouts, every writer's: a person can still change members one by one
		return rolloutOut{}, fmt.Errorf("vis: the fleet is frozen (by %s: %s): a rollout waits for the unfreeze; a person can still change members one at a time with set_knob and instance", fz.By, fz.Reason)
	}
	r := t.in.ring.Load()
	if r == nil {
		return t.runRollout(ctx, req, in, answerBy, time.Now().UTC().Format(time.RFC3339))
	}
	r.fleetMu.Lock()
	if r.fleetShut {
		r.fleetMu.Unlock()
		return rolloutOut{}, fmt.Errorf("vis: this instance is stopping")
	}
	if other := r.runningElsewhere(t.in.id); other != nil {
		r.fleetMu.Unlock()
		return rolloutOut{}, fmt.Errorf("vis: a rollout from %s (%s) is still running: fleet's last_rollout says how it ends; roll out again once it has", other.Coordinator, other.Set)
	}
	if !t.in.rolling.CompareAndSwap(false, true) {
		r.fleetMu.Unlock()
		return rolloutOut{}, fmt.Errorf("vis: a rollout from %s is still running: fleet's last_rollout says how it ends; roll out again once it has", t.in.name)
	}
	if err := t.preflight(in); err != nil {
		t.in.rolling.Store(false)
		r.fleetMu.Unlock()
		return rolloutOut{}, err
	}
	r.bg.Add(1)
	r.fleetMu.Unlock()
	startAt := time.Now().UTC().Format(time.RFC3339) // one rollout's reports share it; seq orders them
	prev := t.in.lastRollout.Load()
	t.in.publishRollout(nil, &rolloutReport{Set: setText(in.Set), By: in.by, At: startAt, State: "running", Outcome: "running"})
	type result struct {
		out rolloutOut
		err error
	}
	done := make(chan result, 1)
	var gone atomic.Bool                                                                                     // the call answered before the rollout ended, so its end is published even if it left nothing
	bctx, cancel := context.WithCancel(context.WithValue(context.WithoutCancel(ctx), noClientWaits{}, true)) // past the call: its forwards take their full time
	stop := context.AfterFunc(r.ctx, cancel)
	go func() {
		defer r.bg.Done()
		defer cancel()
		defer stop()
		out, err := t.runRollout(bctx, req, in, answerBy, startAt)
		done <- result{out, err}
		if err != nil || out.Outcome != "done" && out.Outcome != "writing" { // those publish their own report, and release rolling
			end := &rolloutReport{Set: setText(in.Set), By: in.by, At: startAt, State: "ended", Outcome: out.Outcome, Why: out.Why, NotReached: out.NotReach, Undo: out.Undo}
			if err != nil {
				end.Outcome, end.Why = "refused", err.Error()
			}
			if !gone.Load() && end.Undo == "" && prev != nil { // answered, and left nothing: the one before stays the last
				c := *prev
				end = &c
			}
			t.in.publishRollout(nil, end)
			t.in.rolling.Store(false)
		}
	}()
	wait := time.NewTimer(time.Until(answerBy))
	defer wait.Stop()
	select {
	case x := <-done:
		return x.out, x.err
	case <-wait.C:
	case <-ctx.Done():
	}
	gone.Store(true)
	select { // it may have ended just now
	case x := <-done:
		return x.out, x.err
	default:
	}
	return rolloutOut{Stages: []stageOut{}, Outcome: "running",
		Why:  "still running, a member slow to answer: fleet, asked of any member, shows how it ends as last_rollout, with how to take back what it left",
		Note: "Nothing is undone because the answer came first: the rollout goes on as it would have."}, nil
}

// preflight refuses a rollout whose arguments or fleet it can't run with, before it is published.
func (t *mcpTools) preflight(in rolloutIn) error {
	if t.in.ring.Load() == nil {
		return fmt.Errorf("vis: this instance has no fleet: use set_knob")
	}
	if len(in.Set) == 0 {
		return fmt.Errorf("vis: set names no knob")
	}
	if d, err := time.ParseDuration(in.For); err == nil && d < settleFor {
		return fmt.Errorf("vis: for %s is shorter than the %s the first stage takes to measure: it would be judged after it ended", d, short(settleFor))
	}
	if shared := t.in.sharedNames(); len(shared) > 0 {
		var names []string
		for n, a := range shared {
			names = append(names, fmt.Sprintf("%s (at %s)", n, strings.Join(a, " and ")))
		}
		slices.Sort(names)
		return fmt.Errorf("vis: instances share a name, %s, so the rollout can't tell them apart: give each its own VIS_INSTANCE_NAME", strings.Join(names, "; "))
	}
	return nil
}

func (t *mcpTools) runRollout(ctx context.Context, req *mcp.CallToolRequest, in rolloutIn, answerBy time.Time, startAt string) (out rolloutOut, err error) {
	undone := map[string]bool{} // "member lease" the rollout took back itself
	var unrestored []string     // what it couldn't put back after taking a change back
	defer func() {              // on every path: how to take back what it left, and what it couldn't put back
		out.Undo = undoLine(out.Stages, undone)
		if len(unrestored) > 0 {
			out.Why += "; it couldn't put back " + strings.Join(unrestored, ", ")
		}
	}()
	out = rolloutOut{Stages: []stageOut{}, Note: "Stage 1 is judged against the rest; undo says how to take back whatever the rollout left."}
	if err := t.preflight(in); err != nil {
		return out, err
	}
	knobs := make([]string, 0, len(in.Set))
	for k := range in.Set {
		knobs = append(knobs, k)
	}
	sort.Strings(knobs)
	var alive []string
	all := t.in.Members()
	for _, m := range liveOnce(all) { // a member restarted moments ago once, not twice
		alive = append(alive, m.Name)
	}
	for _, m := range all {
		if m.State != "alive" && !slices.Contains(alive, m.Name) {
			out.NotReach = append(out.NotReach, fmt.Sprintf("%s (%s)", m.Name, m.State))
		}
	}
	order := stageOrder(alive, knobs[0])
	before := t.in.Fleet(ctx) // the fleet before, to say what the whole rollout did

	ids := map[string]string{} // the process each member was when changed: change and lease numbers are per process
	var idsMu sync.Mutex
	// setOn writes the change on a member under sctx; stage 1's handshakes are
	// bounded so the canary can still be measured in time, stage 2's aren't
	// (it may finish in the background).
	setOn := func(sctx context.Context, member string) (changeOut, error) {
		if m, err := t.in.member(member); err == nil {
			idsMu.Lock()
			ids[member] = m.ID
			idsMu.Unlock()
		}
		var c changeOut
		err := t.forward(sctx, req, member, "set_knob", setIn{on: on{Instance: member}, Set: in.Set, For: in.For, Reason: in.Reason + " (rollout)"}, &c)
		return c, err
	}
	set := func(member string) (changeOut, error) {
		return setOn(ctx, member)
	}
	// undo ends the leases a stopped rollout left, under a context the caller
	// hanging up can't cancel, and says where it couldn't.
	undo := func(cs []changeOut, why string) []string {
		var failed []string
		uctx := context.WithoutCancel(ctx)
		for _, c := range cs {
			if c.Lease == 0 {
				continue
			}
			var u changeOut
			if err := t.forward(uctx, req, c.Instance, "revert", revertIn{on: on{Instance: c.Instance}, Lease: c.Lease, Reason: why, Process: c.Process}, &u); err != nil {
				failed = append(failed, fmt.Sprintf("%s (%v)", c.Instance, err))
				continue
			}
			undone[fmt.Sprint(c.Instance, " ", c.Lease)] = true
			// Reverting clears the layer; what the rollout replaced there goes back, for the time it had left.
			byLeft := map[string]map[string]any{}
			for _, r := range c.Replaced {
				left := r.Left
				if left == "" { // a person's pin, with no end: the caller is that layer's writer, and for=0s writes it so again
					left = "0s"
				}
				if until, err := time.Parse(time.RFC3339, r.Until); err == nil { // what it has left now, not when replaced
					d := time.Until(until).Round(time.Second)
					if d <= 0 {
						continue // it would have ended by now
					}
					left = d.String()
				}
				r.Left = left
				if byLeft[r.Left] == nil {
					byLeft[r.Left] = map[string]any{}
				}
				byLeft[r.Left][r.Knob] = r.Value
			}
			for left, set := range byLeft {
				var back changeOut
				if err := t.forward(uctx, req, c.Instance, "set_knob", setIn{on: on{Instance: c.Instance}, Set: set, For: left, Reason: why + ": putting back what it replaced"}, &back); err != nil {
					unrestored = append(unrestored, fmt.Sprintf("set_knob instance=%s %s for %s (%v)", c.Instance, setText(set), left, err)) // the revert held: only this is missing
				}
			}
		}
		return failed
	}
	// measure reads a member's impact report: a read, so one whose handshake
	// failed (never sent) is tried once more, as a starved member drops some.
	measure := func(member string, change uint64) (Impact, error) {
		var im Impact
		err := t.forward(ctx, req, member, "impact", impactIn{on: on{Instance: member}, Change: change}, &im)
		if err != nil && neverStarted(err) && ctx.Err() == nil {
			im = Impact{}
			err = t.forward(ctx, req, member, "impact", impactIn{on: on{Instance: member}, Change: change}, &im)
		}
		return im, err
	}
	masked := func(c changeOut) bool {
		for _, e := range c.Effects {
			if e.Layer != layerOf(req) {
				return true
			}
		}
		return false
	}

	// Stage 1: the first member where the change takes effect.
	var first string
	var firstChange changeOut
	var held []changeOut // masked members' leases: they'd take effect, unmeasured, when the higher layer lets go
	st := stageOut{}
	var unreached []string    // couldn't be reached for stage 1: tried again in stage 2
	var already []string      // already held the values
	var alreadyCs []changeOut // their changes, under the rollout's lease now: taken back on harm, which puts their own back
	alreadyLease := map[string]uint64{}
	for i, m := range order {
		c, err := set(m)
		if err != nil && neverStarted(err) { // a starved or cut-off member: the next one can go first
			unreached = append(unreached, m)
			continue
		}
		if err != nil {
			out.Outcome, out.Why = "failed", err.Error()
			if !declined(err) { // the call reached it: the answer may be what was lost
				out.Why += fmt.Sprintf("; %s may have taken the change before its answer was lost: see its history (history with instance=%q) and revert there if it did", m, m)
			}
			if f := undo(held, "rollout failed"); len(f) > 0 {
				out.Why += "; and couldn't undo it on " + strings.Join(f, ", ")
			}
			out.Stages = append(out.Stages, st)
			return out, nil
		}
		st.Changes = append(st.Changes, c)
		if masked(c) {
			st.Masked = append(st.Masked, m)
			held = append(held, c)
			continue
		}
		if c.MeasuredAt == "" { // it already held these values: nothing to measure here
			already = append(already, m) // kept as it is, under the rollout's lease now
			alreadyLease[m] = c.Lease
			alreadyCs = append(alreadyCs, c)
			continue
		}
		first, firstChange, order = m, c, append(slices.Delete(slices.Clone(order), 0, i+1), unreached...)
		break
	}
	if first == "" {
		undo(held, "rollout stopped: it takes effect nowhere")
		var why []string
		if len(st.Masked) > 0 {
			why = append(why, "a higher layer holds the knob on "+list(st.Masked))
		}
		if len(already) > 0 {
			var ls []string
			for _, m := range already {
				ls = append(ls, fmt.Sprintf("%s lease %d", m, alreadyLease[m]))
			}
			why = append(why, list(already)+" already held the values, now under the rollout's lease ("+strings.Join(ls, ", ")+")")
		}
		out.Outcome = "stopped"
		if len(unreached) > 0 {
			out.Outcome = "failed"
			out.NotReach = append(out.NotReach, unreached...)
			why = append(why, "couldn't reach "+list(unreached)+", which wasn't changed")
		}
		out.Why = "no member it reached would take the change: " + strings.Join(why, "; ")
		out.Stages = append(out.Stages, st)
		return out, nil
	}
	st.Members = []string{first}

	// The gate: what the first member did against the rest, over the same windows.
	im, err := measure(first, firstChange.Change)
	if err == nil && !im.Done { // late past what impact waits: not a measurement
		err = fmt.Errorf("its impact report is still pending, past %s after the change", short(settleFor+lateMeasure))
	}
	restarted := func() bool { // a member that answers under its name as another process (dead isn't restarted: it may come back holding the change)
		m, merr := t.in.member(first)
		return merr == nil && m.ID != ids[first]
	}
	if err != nil { // a member that dropped the call may be restarting: give the fleet time to see which
		for wait := time.Now().Add(deadAfter); !restarted() && ctx.Err() == nil && time.Now().Before(wait); {
			time.Sleep(time.Second)
		}
	}
	if restarted() { // it restarted: the change may have crashed it, and went with the process
		undo(held, "rollout stopped: the first member restarted")
		undone[fmt.Sprint(first, " ", firstChange.Lease)] = true // gone with the old process: nothing to revert, and its number may be another's now
		out.Outcome = "stopped"
		out.Why = fmt.Sprintf("%s restarted while the first stage was measured (the change may have crashed it); a restart drops runtime values, so it no longer holds the change and its lease %d is gone with the old process: there is nothing to revert there; the rest were left alone", first, firstChange.Lease)
		out.Stages = append(out.Stages, st)
		return out, nil
	}
	if err != nil {
		// Can't measure is can't judge: stop, and keep the change, as for any first stage it can't judge.
		undo(held, "rollout stopped: couldn't measure the first stage")
		out.Outcome = "stopped"
		out.Why = fmt.Sprintf("couldn't measure %s (%v), so it can't be judged; %s keeps the change under its lease (%s lease %d): to take it back, follow undo; the rest were left alone",
			first, err, first, first, firstChange.Lease)
		out.Stages = append(out.Stages, st)
		return out, nil
	}
	harm, unjudged, evidence := gate(im, t.in.objectives(), first)
	st.Evidence = evidence
	out.Stages = append(out.Stages, st)
	if unjudged != "" { // stop, but keep what it couldn't judge: undoing it threw away fixes that worked
		undo(held, "rollout stopped: "+unjudged)
		out.Outcome = "stopped"
		out.Why = fmt.Sprintf("%s; no harm seen, so %s keeps the change under its lease (%s lease %d): to take it back, follow undo; the rest were left alone",
			unjudged, first, first, firstChange.Lease)
		return out, nil
	}
	if harm != "" {
		out.Outcome, out.Why = "stopped", harm+fmt.Sprintf("; undone on %s, and the rest were left alone", first)
		if f := undo(append(append(held, alreadyCs...), firstChange), "rollout stopped: "+harm); len(f) > 0 {
			out.Outcome, out.Why = "failed", harm+"; the undo failed on "+strings.Join(f, ", ")+": it holds the change until its lease ends"
			if fz := t.in.FreezeState(); fz.Frozen { // a freeze refuses an agent's revert, and stops the lease's clock
				out.Why = harm + fmt.Sprintf("; the fleet was frozen meanwhile (by %s), which refused the undo on %s and stops its lease's clock: it holds the change until a person takes it back (undo, with a person's token), or the freeze lifts and the lease runs out",
					fz.By, strings.Join(f, ", "))
			}
		}
		return out, nil
	}

	// Stage 2: the rest, at once. Its writes run past the answer when members
	// are slow to answer (an MCP client gives up on a call at a minute, and a
	// starved member is the one that needs the change): the background job
	// finishes them, and fleet's last_rollout says how they ended.
	r := t.in.ring.Load()
	r.fleetMu.Lock() // a background job the ring's shutdown waits for, as the page's fleet refresh
	if r.fleetShut {
		r.fleetMu.Unlock()
		out.Outcome, out.Why = "failed", "this instance is stopping: stage 2 wasn't written; the first stage keeps the change (follow undo to take it back)"
		return out, nil
	}
	r.bg.Add(1)
	r.fleetMu.Unlock()
	sctx, scancel := context.WithCancel(context.WithoutCancel(ctx)) // past the call, for the ring's life
	stopWith := context.AfterFunc(r.ctx, scancel)
	written := make(chan stage2Writes, 1)
	stage1 := slices.Clone(out.Stages) // the job's own: the answer goes on being written here
	prevRep := t.in.lastRollout.Load() // what fleet shows until this one's stage 2 is written
	notReach := slices.Clone(out.NotReach)
	go func() {
		defer r.bg.Done()
		defer scancel()
		defer stopWith()
		w := writeStage2(sctx, order, unreached, masked, setOn)
		written <- w
		t.finishStage2(r.ctx, req, in, stage1, undone, first, im, before, w, masked, notReach, startAt)
	}()
	select {
	case w := <-written:
		out.Stages = append(out.Stages, w.st)
		out.Outcome = "done"
		out.MeasuredAt = w.at.Add(settleFor + bucketWidth).UTC().Format(time.RFC3339)
		out.Note = "Stage 2 is being measured: fleet, asked of any member, shows this rollout's result as last_rollout once it is, about measured_at " +
			"(state says when): each member from its own change, read as before and after only since no member is left to compare with, and the fleet before and after."
		if len(w.failed) > 0 {
			out.Outcome, out.Why = "failed", w.why()
			out.NotReach = append(out.NotReach, w.failed...)
		}
	case <-time.After(time.Until(answerBy.Add(-time.Second))):
		out.Outcome = "writing"
		out.Why = fmt.Sprintf("stage 1 passed; stage 2 is still being written to %s, some slow to answer: fleet, asked of any member, says in last_rollout who it reached and how to take it all back, once it has", list(order))
		out.Note = "undo covers stage 1 only, until last_rollout has stage 2."
		// Until stage 2 is written, fleet shows this rollout as writing, not the
		// one before it; unless stage 2 got there just now.
		t.in.publishRollout(prevRep, &rolloutReport{Set: setText(in.Set), By: in.by, At: startAt, State: "writing stage 2", Outcome: "writing",
			Why: out.Why, NotReached: slices.Clone(out.NotReach), Undo: undoLine(out.Stages, undone)})
	}
	return out, nil
}

// stage2Writes is how a rollout's stage 2 writes ended.
type stage2Writes struct {
	st     stageOut
	failed []string         // "member: error"
	errOf  map[string]error // each member's own error: what it is decides a retry, never its text
	at     time.Time        // when the last write returned
}

// why says which members stage 2 didn't reach, and which may hold the change anyway.
func (w stage2Writes) why() string {
	s := "stage 2 didn't reach " + strings.Join(w.failed, "; ")
	var maybe []string
	for m, err := range w.errOf {
		if !declined(err) && !neverStarted(err) && !slices.Contains(w.st.Members, m) {
			maybe = append(maybe, m)
		}
	}
	if slices.Sort(maybe); len(maybe) > 0 {
		s += fmt.Sprintf("; %s may have taken the change before the answer was lost: see their history and revert there if they did", list(maybe))
	}
	return s
}

// writeStage2 writes the change on the rest at once, and once more where the
// first try never started: a member starved of CPU answers slowly, and the
// change may be what it needs. Not twice for one already tried in stage 1.
func writeStage2(ctx context.Context, order, unreached []string, masked func(changeOut) bool, setOn func(context.Context, string) (changeOut, error)) stage2Writes {
	w := stage2Writes{errOf: map[string]error{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	write := func(members []string) {
		for _, m := range members {
			wg.Go(func() {
				c, err := setOn(ctx, m)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					w.failed = append(w.failed, fmt.Sprintf("%s: %v", m, err))
					w.errOf[m] = err
					return
				}
				delete(w.errOf, m)
				w.st.Members = append(w.st.Members, m)
				w.st.Changes = append(w.st.Changes, c)
				if masked(c) {
					w.st.Masked = append(w.st.Masked, m)
				}
			})
		}
		wg.Wait()
	}
	write(order)
	var retry []string
	kept := w.failed[:0:0]
	for _, f := range w.failed { // sorted out before any retry runs: the retries write failed and errOf
		m, _, _ := strings.Cut(f, ": ")
		if !neverStarted(w.errOf[m]) || slices.Contains(unreached, m) || ctx.Err() != nil {
			kept = append(kept, f)
			continue
		}
		retry = append(retry, m)
	}
	w.failed = kept
	write(retry)
	slices.Sort(w.st.Members)
	w.at = time.Now()
	return w
}

// finishStage2 publishes the rollout's report once stage 2 is written (who
// it reached, the undo for all of it), then measures stage 2.
func (t *mcpTools) finishStage2(ctx context.Context, req *mcp.CallToolRequest, in rolloutIn, stage1 []stageOut, undone map[string]bool, first string, im Impact, before FleetReport, w stage2Writes, masked func(changeOut) bool, notReach []string, startAt string) {
	stages := append(stage1, w.st)
	outcome, why := "done", ""
	if len(w.failed) > 0 {
		outcome, why = "failed", w.why()
	}
	ready := w.at.Add(settleFor + bucketWidth)
	var toMeasure []changeOut
	for _, c := range w.st.Changes {
		if c.MeasuredAt != "" && !masked(c) {
			toMeasure = append(toMeasure, c)
		}
	}
	rep := &rolloutReport{Set: setText(in.Set), By: in.by, At: startAt, MeasuredAt: ready.UTC().Format(time.RFC3339), State: "measuring stage 2",
		Outcome: outcome, Why: why, NotReached: append(notReach, w.failed...), Reached: w.st.Members, Undo: undoLine(stages, undone), Result: []string{memberRow(first, im, t.in.objectives())}}
	t.in.publishRollout(nil, rep) // published whole: fleet may read it from now on, so nothing writes it after
	t.in.rolling.Store(false)
	t.measureStage2(ctx, req, rep, first, im, before, toMeasure, ready)
}

// neverStarted says a forwarded call failed before the member saw it: it
// couldn't be dialled, or never answered the handshake. It goes by the error's
// type, never its text, which holds member names, addresses and knob names.
// (Not the SDK's ErrRejected: it marks any failed request, one lost after the
// member applied it included.)
func neverStarted(err error) bool {
	var op *net.OpError
	var u unsent
	return errors.Is(err, errNoAnswer) || errors.As(err, &op) && op.Op == "dial" || errors.As(err, &u)
}

// declined says a forwarded call was answered with a refusal (out of range,
// frozen, not this writer's): the member changed nothing.
func declined(err error) bool {
	var te toolError
	var u unsent
	return errors.As(err, &te) || errors.As(err, &u)
}

// rolloutReport is the last rollout as the fleet tool shows it: its answer
// came once stage 2 was written, and what stage 2 did is filled in here once
// measured.
type rolloutReport struct {
	Coordinator string   `json:"coordinator" jsonschema:"the member that ran it, which keeps it"`
	Process     string   `json:"process"` // the coordinator's instance ID: a restart is another process
	Seq         int64    `json:"seq"`     // orders one coordinator's reports: a relayed old one doesn't win
	Set         string   `json:"set" jsonschema:"what it set: compress_level=1"`
	By          string   `json:"by,omitempty" jsonschema:"who asked for it"`
	At          string   `json:"at"`
	Outcome     string   `json:"outcome"`
	Why         string   `json:"why,omitempty"`
	NotReached  []string `json:"not_reached,omitempty"`
	Reached     []string `json:"stage_2,omitempty" jsonschema:"the members stage 2 changed"`
	State       string   `json:"state" jsonschema:"running; writing stage 2; measuring stage 2; measured; ended (it stopped, failed or was refused); or unknown (its coordinator stopped answering or restarted before it ended)"`
	MeasuredAt  string   `json:"measured_at"`
	Result      []string `json:"result" jsonschema:"each member's own before and after, each measured from its change"`
	Fleet       string   `json:"fleet,omitempty" jsonschema:"each objective across the fleet before the rollout and after it, and whether it meets it"`
	Undo        string   `json:"undo,omitempty"`
}

// publishRollout publishes a version of this instance's rollout report
// (stamped with who coordinates it and its order), replacing old if old is
// what fleet shows (nil: whatever it shows).
func (in *Instance) publishRollout(old, rep *rolloutReport) bool {
	rep.Coordinator, rep.Process, rep.Seq = in.name, in.id, in.rolloutSeq.Add(1)
	if old == nil {
		in.lastRollout.Store(rep)
		return true
	}
	return in.lastRollout.CompareAndSwap(old, rep)
}

// inProgress says a report is of a rollout that hasn't ended.
func (r *rolloutReport) inProgress() bool {
	return r.State == "running" || r.State == "writing stage 2"
}

// noteRollout keeps the newest report gossip carries from each coordinator,
// so the fleet shows its last rollout wherever it is asked, and after its
// coordinator restarts. r.mu is held.
func (r *ring) noteRollout(rep *rolloutReport) {
	if rep == nil || rep.Process == "" || rep.Process == r.self.ID {
		return
	}
	if r.rollouts == nil {
		r.rollouts = map[string]*rolloutReport{}
	}
	if old := r.rollouts[rep.Process]; old == nil || rep.Seq > old.Seq {
		r.rollouts[rep.Process] = rep
	}
}

// runningElsewhere is a rollout another live member coordinates that hasn't
// ended, if any: two at once would re-lease each other's members.
func (r *ring) runningElsewhere(self string) *rolloutReport {
	alive := map[string]bool{} // not dead: a coordinator slow to gossip is still running it
	for _, m := range r.in.Members() {
		if m.State != "dead" {
			alive[m.ID] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, rep := range r.rollouts {
		if id != self && alive[id] && rep.inProgress() {
			if at, err := time.Parse(time.RFC3339, rep.At); err == nil && time.Since(at) < 10*time.Minute {
				return rep
			}
		}
	}
	return nil
}

// lastRolloutAnywhere is the newest rollout report in the fleet: this
// instance's, or one gossip carried from another coordinator. One that hadn't
// ended when its coordinator stopped answering, or restarted, says so.
func (in *Instance) lastRolloutAnywhere() *rolloutReport {
	best := in.lastRollout.Load()
	r := in.ring.Load()
	if r != nil {
		alive := map[string]bool{} // not dead: only a coordinator gone quiet past deadAfter hasn't got there
		for _, m := range in.Members() {
			if m.State != "dead" {
				alive[m.ID] = true
			}
		}
		r.mu.Lock()
		for _, rep := range r.rollouts {
			if best != nil && rep.At <= best.At { // RFC3339 in UTC: later sorts later
				continue
			}
			c := *rep
			if c.inProgress() && !alive[c.Process] {
				c.State = fmt.Sprintf("unknown: %s, which ran it, stopped answering or restarted before it ended; each member's history (history with instance) says what it holds", c.Coordinator)
			}
			best = &c
		}
		r.mu.Unlock()
	}
	if best == nil {
		return nil
	}
	if at, err := time.Parse(time.RFC3339, best.At); err != nil || time.Since(at) >= 30*time.Minute {
		return nil
	}
	return best
}

// measureStage2 measures each stage-2 member from its own change, then the
// fleet once its window is past their first seconds, and publishes the
// finished report. It runs past the call that started it, for the ring's life.
func (t *mcpTools) measureStage2(ctx context.Context, req *mcp.CallToolRequest, rep *rolloutReport, first string, firstIm Impact, before FleetReport, cs []changeOut, ready time.Time) {
	rows := map[string]string{first: memberRow(first, firstIm, t.in.objectives())}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Go(func() {
			var mi Impact
			err := t.forward(ctx, req, c.Instance, "impact", impactIn{on: on{Instance: c.Instance}, Change: c.Change}, &mi)
			if err != nil && neverStarted(err) && ctx.Err() == nil { // never sent: once more
				mi = Impact{}
				err = t.forward(ctx, req, c.Instance, "impact", impactIn{on: on{Instance: c.Instance}, Change: c.Change}, &mi)
			}
			row := memberRow(c.Instance, mi, t.in.objectives())
			if err != nil {
				row = fmt.Sprintf("%s: not measured (%v)", c.Instance, err)
			}
			mu.Lock()
			rows[c.Instance] = row
			mu.Unlock()
		})
	}
	wg.Wait()
	select { // the fleet's window reaches back to the start of every bucket it touches: past stage 2's first seconds
	case <-time.After(time.Until(ready)):
	case <-ctx.Done():
		return
	}
	done := *rep
	done.Result = nil
	for _, m := range slices.Sorted(maps.Keys(rows)) {
		done.Result = append(done.Result, rows[m])
	}
	done.Fleet, done.State = fleetRow(before, t.in.Fleet(ctx)), "measured"
	t.in.publishRollout(rep, &done) // unless a newer rollout has started
}

// setText writes a set as the undo line does: "compress_level=3 batch=64".
func setText(set map[string]any) string {
	var s []string
	for _, k := range slices.Sorted(maps.Keys(set)) {
		s = append(s, fmt.Sprintf("%s=%v", k, set[k]))
	}
	return strings.Join(s, " ")
}

// undoLine is how to take back what a rollout left on its members: per
// change it didn't take back itself, its revert, and since a revert empties
// the layer, setting back what the change replaced there while that would
// have lasted.
func undoLine(stages []stageOut, undone map[string]bool) string {
	var undoes []string
	for _, st := range stages {
		for _, c := range st.Changes {
			if c.Lease == 0 || undone[fmt.Sprint(c.Instance, " ", c.Lease)] {
				continue
			}
			u := fmt.Sprintf("revert instance=%s lease=%d process=%s", c.Instance, c.Lease, c.Process)
			for _, r := range c.Replaced {
				switch {
				case r.Until != "":
					u += fmt.Sprintf(", then set_knob instance=%s %s=%v for the time left until %s (what the rollout replaced, %s's: past then it would have ended, so skip it)", c.Instance, r.Knob, r.Value, r.Until, r.By)
				default: // a person's pin, with no end
					u += fmt.Sprintf(", then set_knob instance=%s %s=%v for 0s (%s's pin, which the rollout replaced: for 0s pins it again, with no end)", c.Instance, r.Knob, r.Value, r.By)
				}
			}
			undoes = append(undoes, u)
		}
	}
	slices.Sort(undoes)
	return strings.Join(undoes, "; ")
}

// memberRow is one member's before and after, measured from its own change:
// "cp-1: GET /k p99 2.3 s → 18 ms; CPU 0.25 → 0.06 cores, throttled 100% → 0%."
func memberRow(name string, im Impact, objs []sloDef) string {
	if !im.Done {
		return fmt.Sprintf("%s: still measuring; impact with instance=%q change=%d says what it did once done", name, name, im.Change)
	}
	var s []string
	for _, o := range objs {
		b, okb := im.Before[o.route+" p99"]
		a, oka := im.After[o.route+" p99"]
		if okb && oka {
			s = append(s, fmt.Sprintf("%s p99 %s → %s ms", o.route, roughMs(b), roughMs(a)))
		}
	}
	row := name + ": " + strings.Join(s, ", ")
	if c := cpuEvidence(im); c != "" {
		row += "; " + c
	}
	if len(im.Confounded) > 0 {
		row += " (while " + strings.Join(im.Confounded, "; ") + ")"
	}
	return row
}

// fleetRow is each objective across the whole fleet before the rollout and
// after it: "GET /k p99 2.3 s → 18 ms across 3 of 3 members: meets its 40 ms objective".
func fleetRow(before, after FleetReport) string {
	var s []string
	for _, a := range after.SLOs {
		from := "?"
		for _, b := range before.SLOs {
			if b.Route == a.Route && b.Requests > 0 {
				from = latency(b.P99)
			}
		}
		covers := after.Covers
		if before.Covers != "" && before.Covers != after.Covers { // unlike sets: say so
			covers = fmt.Sprintf("%s (before: %s)", after.Covers, before.Covers)
		}
		if a.Requests == 0 {
			s = append(s, fmt.Sprintf("%s p99 %s → no requests after, across %s: nothing to judge it by", a.Route, from, covers))
			continue
		}
		met := "meets its " + latency(a.Objective) + " objective"
		if !a.Met {
			met = "still over its " + latency(a.Objective) + " objective"
		}
		s = append(s, fmt.Sprintf("%s p99 %s → %s across %s: %s", a.Route, from, latency(a.P99), covers, met))
	}
	return strings.Join(s, "; ")
}

// quiet says a route's request rate fell to under a quarter of what it was,
// from enough requests before to tell. The two windows differ in length, so
// it compares rates; and a ratio, not a count against minJudged, so that a
// quiet route's noise isn't a stop, nor a busy route fallen to a trickle of
// stragglers a lull.
func quiet(im Impact, route string) bool {
	nb := im.Before[route+" requests"]
	if nb < minJudged {
		return false
	}
	nEq := im.After[route+" requests"] // the requests after, at the before window's length
	if rb := im.Before[route+" rps"]; rb > 0 {
		nEq = nb * im.After[route+" rps"] / rb
	}
	return nEq < nb/4
}

// keptLoad says the rest served a route about as much after as before: enough
// requests to tell, and not quiet themselves (their windows are the same
// length, so their counts compare).
func keptLoad(r *Rest, route string) bool {
	return r != nil && r.After[route+" requests"] >= minJudged && !quiet(Impact{Before: r.Before, After: r.After}, route)
}

// rates says a route's requests before and after, as rates where it has them
// (the windows differ in length): "20.0 requests a second before, 1.5 after
// (600 and 15 requests)".
func rates(im Impact, route string) string {
	nb, na := im.Before[route+" requests"], im.After[route+" requests"]
	if rb, ok := im.Before[route+" rps"]; ok {
		return fmt.Sprintf("%.1f requests a second before, %.1f after (%.0f and %.0f requests)", rb, im.After[route+" rps"], nb, na)
	}
	return fmt.Sprintf("%.0f requests in the window before, %.0f after", nb, na)
}

// cpuEvidence is what the change did to the member's CPU, which often says
// more than a p99 does under a CPU squeeze: "CPU 0.25 → 0.06 cores, throttled
// 100% → 0%".
func cpuEvidence(im Impact) string {
	var s []string
	if b, ok := im.Before["cpu_used"]; ok {
		if a, ok := im.After["cpu_used"]; ok && max(a, b) >= .005 { // nothing to say of an idle CPU
			s = append(s, fmt.Sprintf("CPU %.2f → %.2f cores", b, a))
		}
	}
	if b, ok := im.Before["throttled"]; ok {
		if a, ok := im.After["throttled"]; ok {
			s = append(s, fmt.Sprintf("throttled %.0f%% → %.0f%%", b*100, a*100))
		}
	}
	if len(s) == 0 {
		return ""
	}
	return strings.Join(s, ", ") + "."
}

// verdict says in a word what a first stage the gate passed did: helped
// (every judged route's p99 fell, by a fifth or more beyond what the rest's
// did over the same windows, or into its objective while the rest didn't
// get as much better), no clear change (saying when the rest moved as much
// or more), or held while the rest got worse.
func verdict(im Impact, objs []sloDef) string {
	helped, judged, restToo, held := 0, 0, false, false
	for _, o := range objs {
		b, okb := im.Before[o.route+" p99"]
		a, oka := im.After[o.route+" p99"]
		if !okb || !oka || b <= 0 || min(im.Before[o.route+" requests"], im.After[o.route+" requests"]) < minJudged {
			continue
		}
		judged++
		ratio := 1.0 // what the rest did over the same windows, when they compare
		if r := im.Rest; r != nil && len(r.Changed) == 0 && r.AfterOf == nil && min(r.Before[o.route+" requests"], r.After[o.route+" requests"]) >= minJudged {
			if rb, ra := r.Before[o.route+" p99"], r.After[o.route+" p99"]; rb > 0 && ra > 0 {
				ratio = ra / rb
			}
		}
		obj := msf(o.p99)
		switch {
		case a <= .8*b*ratio && a < .95*b: // better, and by more than the rest got on their own
			helped++
		case a <= .8*b, b > obj && a <= obj && ratio < .8: // better, but no more than the rest got
			restToo = true
		case b > obj && a <= obj:
			helped++
		case a < 1.15*b && ratio >= 1.25: // the rest got worse; it about held
			held = true
		}
	}
	switch {
	case judged > 0 && helped == judged:
		return "helped"
	case restToo:
		return "no clear change (the rest moved as much or more)"
	case held:
		return "held while the rest got worse"
	}
	return "no clear change"
}

// layerOf is the layer the caller's token writes.
func layerOf(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		if g, ok := req.Extra.TokenInfo.Extra["grant"].(Grant); ok {
			return g.Writer.Kind.layer().String()
		}
	}
	return AgentLayer.String()
}

// gate judges the first stage. harm is what it saw go wrong, and means undo;
// unjudged is why it couldn't tell (too few requests, or someone else changed
// something on the member meanwhile), and means stop but leave the change,
// since undoing what it couldn't judge throws away fixes that work. Harm seen
// counts even in a confounded window. What the member's own rules did in the
// window (a step, a lease ending, a revert) is the system at work, and only noted.
func gate(im Impact, objs []sloDef, member string) (harm, unjudged, evidence string) {
	if len(objs) == 0 {
		return "", "", fmt.Sprintf("%s: %s There is no objective to judge it by, so the rollout went on.", member, im.Summary)
	}
	var rules, others []string
	for _, c := range im.ConfoundedBy {
		switch {
		case c.Kind == "freeze" || c.Kind == "unfreeze": // no value, no traffic changed
		case c.Kind == "change" && c.Layer == RuleLayer.String():
			rules = append(rules, c.What)
		default:
			others = append(others, c.What)
		}
	}
	if im.Warming { // in its first minute, its own before window is the warm-up
		others = append(others, member+" had started under a minute before, while its caches still filled")
	}
	var seen, idle []string
	for _, o := range objs { // every objective: a change can help one route and hurt another
		h, ev := gateRoute(im, o, member)
		switch {
		case ev == "": // too few requests on it to judge: noted, not a reason to stop
			r := o.route
			if quiet(im, o.route) && !keptLoad(im.Rest, o.route) { // gone quiet, with none that kept its load to compare with: say so
				r += fmt.Sprintf(" (%s: a lull, or the route stopped answering; with no other member that kept its load to compare with they look alike, so check it)", rates(im, o.route))
			}
			idle = append(idle, r)
			continue
		case h != "" && harm == "":
			harm = h
		}
		seen = append(seen, ev)
	}
	evidence = strings.Join(seen, "; ")
	if len(idle) > 0 {
		evidence += fmt.Sprintf(" (too few requests on %s to judge)", list(idle))
	}
	if n := len(im.Rest.membersOrNil()); n > 0 && len(seen) > 0 {
		evidence += fmt.Sprintf(". One member against %d, one window each: a comparison, not an inference.", n)
	} else if len(seen) > 0 {
		evidence += ". No other member answered, so nothing to compare it with: judged on its own before and after."
	}
	if len(rules) > 0 {
		evidence += " Its own rules moved meanwhile: " + strings.Join(rules, "; ") + "."
	}
	if c := cpuEvidence(im); c != "" {
		evidence += " " + c
	}
	switch {
	case harm != "":
		evidence = "harm: " + evidence
	case len(seen) > 0 && len(others) == 0:
		evidence = verdict(im, objs) + ": " + evidence
	}
	switch {
	case harm != "":
		if len(others) > 0 {
			harm += " (in a window where " + strings.Join(others, "; ") + ")"
		}
		return harm, "", evidence
	case len(others) > 0:
		return "", fmt.Sprintf("the first stage on %s is confounded (%s), so it can't be judged", member, strings.Join(others, "; ")), evidence
	case len(seen) == 0:
		return "", fmt.Sprintf("too few requests on %s to judge the first stage on %s", list(idle), member), evidence
	}
	return "", "", evidence
}

// gateRoute judges one objective: errors first (a request that fails fast is
// a short one), then p99. Harm relative to the rest is a rise of more than
// about a histogram bucket (12%) beyond what they did; harm whatever the rest
// did is errors or p99 doubling, p99 to over the objective; and going from
// met to unmet is harm too.
func gateRoute(im Impact, o sloDef, member string) (harm, evidence string) {
	key, obj := o.route+" p99", msf(o.p99)
	b, okb := im.Before[key]
	a, oka := im.After[key]
	nb, hasN := im.Before[o.route+" requests"]
	na := im.After[o.route+" requests"]
	if hasN && quiet(im, o.route) { // it stopped answering, unless everyone's load went
		restKept := keptLoad(im.Rest, o.route) // with none that kept its load to compare with, a lull and harm look alike
		if restKept {
			return fmt.Sprintf("%s got worse: %s answered %s, while the rest kept theirs", member, o.route, rates(im, o.route)),
				fmt.Sprintf("%s on %s: %s", o.route, member, rates(im, o.route))
		}
	}
	if !okb || !oka || b <= 0 || hasN && min(nb, na) < minJudged { // a p99 of a handful of requests is their slowest one
		return "", ""
	}
	evidence = fmt.Sprintf("%s on %s: p99 %s → %s ms", o.route, member, roughMs(b), roughMs(a))
	restRatio := 1.0
	if r := im.Rest; r != nil {
		rnb, rna := r.Before[o.route+" requests"], r.After[o.route+" requests"]
		switch rb, ra := r.Before[key], r.After[key]; {
		case len(r.Changed) > 0:
			evidence += fmt.Sprintf(", judged on its own before and after: the rest were changed too (%s)", strings.Join(r.Changed, "; "))
		case r.AfterOf != nil:
			evidence += fmt.Sprintf(", judged on its own before and after: only %s of the rest answered after", list(r.AfterOf))
		case rb > 0 && ra > 0 && min(rnb, rna) >= minJudged:
			restRatio = ra / rb
			evidence += fmt.Sprintf(", while the rest of the fleet (%s) went %s → %s ms", list(r.Members), roughMs(rb), roughMs(ra))
		}
	}
	eb, ea := im.Before[o.route+" errors"], im.After[o.route+" errors"]
	restErr := eb
	if im.Rest != nil {
		if re, ok := im.Rest.After[o.route+" errors"]; ok {
			restErr = max(restErr, re)
		}
	}
	switch {
	case ea > restErr+.01:
		return fmt.Sprintf("%s got worse: %s errors %.1f%% → %.1f%%, where the rest had %.1f%%", member, o.route, eb*100, ea*100, restErr*100), evidence
	case ea >= max(2*eb, eb+.05):
		return fmt.Sprintf("%s got worse: %s errors %.1f%% → %.1f%%", member, o.route, eb*100, ea*100), evidence
	case a/b > restRatio*1.12 && a-b > 0.1: // 0.1 ms: below that, a histogram bucket is the noise
		return fmt.Sprintf("%s got worse: %s p99 %s → %s ms, %+.0f%% beyond what the rest did", member, o.route, roughMs(b), roughMs(a), (a/b/restRatio-1)*100), evidence
	case a >= 2*b && a > obj: // whatever the rest did: they may be getting worse for their own reasons
		return fmt.Sprintf("%s got worse: %s p99 %s → %s ms, over its %s ms objective", member, o.route, roughMs(b), roughMs(a), roughMs(obj)), evidence
	case b <= obj && a > obj:
		return fmt.Sprintf("%s went over its objective: %s p99 %s → %s ms against %s ms", member, o.route, roughMs(b), roughMs(a), roughMs(obj)), evidence
	}
	return "", evidence
}

func (r *Rest) membersOrNil() []string {
	if r == nil {
		return nil
	}
	return r.Members
}
