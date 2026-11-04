package vis

import (
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"
)

// FreezeState says whether automated changes are frozen, and who changed that, when and why.
type FreezeState struct {
	Frozen bool
	By     Writer
	Reason string
	Since  time.Time
	Epoch  uint64 // rises with every freeze and unfreeze; across a fleet the highest wins
	On     string // the member it was set on, by name; a freeze reaches the whole fleet
	origin string // that member's ID, which breaks a tie between two freezes of one epoch
}

// MaxFreezeReason caps a freeze reason so the record stays one gossip message.
const MaxFreezeReason = 140

// Freeze stops automated changes (fleet, agent and rule layers) and holds
// every knob where it is: no layer is masked, and every lease clock stops,
// a pin's expiry included. A person can still pin or clear any knob.
// Any writer may freeze. Freezing an already frozen instance keeps the first
// freeze but raises its epoch: in a fleet, an unfreeze set elsewhere a moment
// earlier, not yet heard here, mustn't lift a freeze made after it.
func (in *Instance) Freeze(by Writer, reason string) error {
	if !by.valid() {
		return ErrActorRequired
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.freeze.Frozen { // the first freeze stands, but the second person's reason is still on record
		in.audit(slog.LevelInfo, "vis.freeze", slog.String("actor", by.String()), slog.String("reason", capReason(reason)),
			slog.Uint64("epoch", in.freeze.Epoch+1), slog.String("already_frozen_by", in.freeze.By.String()), slog.String("build", in.build))
		in.freeze.Epoch++
		in.freeze.Since = time.Now() // reasserted: the merge must see it as set now, after any unfreeze it hasn't heard
		return nil
	}
	in.holdLocked(FreezeState{Frozen: true, By: by, Reason: capReason(reason), Since: time.Now(), Epoch: in.freeze.Epoch + 1, On: in.name, origin: in.id})
	return nil
}

// holdLocked freezes: every lease clock stops. Callers hold in.mu.
func (in *Instance) holdLocked(st FreezeState) {
	now := time.Now()
	in.freeze = st
	in.audit(slog.LevelInfo, "vis.freeze", slog.String("actor", st.By.String()), slog.String("reason", st.Reason),
		slog.Uint64("epoch", st.Epoch), slog.String("on", st.On), slog.String("build", in.build))
	for _, ls := range in.leases {
		if ls.timer != nil {
			ls.timer.Stop()
			ls.timer, ls.left = nil, ls.until.Sub(now)
		}
		if ls.warn != nil {
			ls.warn.Stop()
			ls.warn = nil
		}
	}
}

// Unfreeze resumes automated changes. Each lease picks up with the time it had
// left. Only a person may unfreeze; an agent that could would make the freeze meaningless.
func (in *Instance) Unfreeze(by Writer, reason string) error {
	if !by.valid() {
		return ErrActorRequired
	}
	if by.Kind != Human {
		return fmt.Errorf("%w: only a person can unfreeze (%v)", ErrNotAllowed, by)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.freeze.Frozen {
		return nil
	}
	in.releaseLocked(FreezeState{By: by, Reason: capReason(reason), Since: time.Now(), Epoch: in.freeze.Epoch + 1, On: in.name, origin: in.id})
	return nil
}

// releaseLocked unfreezes: each lease picks up with the time it had left. Callers hold in.mu.
func (in *Instance) releaseLocked(st FreezeState) {
	now := time.Now()
	in.freeze = st
	in.audit(slog.LevelInfo, "vis.unfreeze", slog.String("actor", st.By.String()), slog.String("reason", st.Reason),
		slog.Uint64("epoch", st.Epoch), slog.String("on", st.On), slog.String("build", in.build))
	for _, ls := range in.leases {
		ls.until = now.Add(ls.left)
		ls.timer = in.leaseTimer(ls, max(ls.left, 0)) // a lease that ran out as the freeze began ends now
		in.scheduleWarning(ls, ls.left)
	}
}

// adoptFreeze takes a member's freeze or unfreeze when it is newer than this
// instance's: a higher epoch, or the same one set on a member with a higher
// ID. The member it came from checked who may (only a person unfreezes).
//
// A freeze is the safety switch, so where two sides of a partition decided
// differently it wins: a freeze set after the unfreeze that would lift it
// stands (and takes an epoch above both, so it spreads), and an equal epoch
// goes to frozen.
func (in *Instance) adoptFreeze(st FreezeState) {
	in.mu.Lock()
	defer in.mu.Unlock()
	cur := in.freeze
	if st.Epoch > cur.Epoch+1<<20 { // no fleet freezes a million times: a forged or broken epoch would pin freezes for good
		return
	}
	switch {
	case cur.Frozen && !st.Frozen && cur.Since.After(st.Since): // this side froze after that unfreeze
		if st.Epoch >= cur.Epoch {
			in.freeze.Epoch = st.Epoch + 1 // so this freeze outranks it wherever it goes
		}
		return
	case !cur.Frozen && st.Frozen && st.Since.After(cur.Since) && st.Epoch <= cur.Epoch: // a freeze set after this side's unfreeze
		st.Epoch = cur.Epoch + 1
		in.holdLocked(st)
		return
	}
	if !newerFreeze(st, cur) {
		return
	}
	switch {
	case st.Frozen && !cur.Frozen:
		in.holdLocked(st)
	case !st.Frozen && cur.Frozen:
		in.releaseLocked(st)
	default:
		in.freeze = st
	}
}

func newerFreeze(a, b FreezeState) bool {
	if a.Epoch != b.Epoch {
		return a.Epoch > b.Epoch
	}
	if a.Frozen != b.Frozen {
		return a.Frozen // the same count either way: frozen
	}
	return a.origin > b.origin
}

// FreezeState reports whether automated changes are frozen, and who changed that, when and why.
func (in *Instance) FreezeState() FreezeState {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.freeze
}

func capReason(r string) string {
	if len(r) <= MaxFreezeReason {
		return r
	}
	// Step back to the start of the rune the cut lands in, if it lands inside one.
	// Only the cut matters: invalid bytes elsewhere are kept as they came.
	cut := MaxFreezeReason
	for i := cut; i > cut-utf8.UTFMax && i > 0; i-- {
		if utf8.RuneStart(r[i]) {
			if _, size := utf8.DecodeRuneInString(r[i:]); i+size > cut && size > 1 {
				cut = i
			}
			break
		}
	}
	return r[:cut]
}
