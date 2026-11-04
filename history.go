package vis

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// MaxHistory is how many audit events an instance keeps in memory. The full
// trail goes to the logger; this is the recent part, for agents and the UI.
const MaxHistory = 1000

// Event is one audit record: a knob change, a rule's state change, a freeze,
// or a change vis noticed in its environment, such as the container's CPU limit.
type Event struct {
	At     time.Time
	Kind   string         // change, rule, freeze, unfreeze, environment, onchange_panic
	Fields map[string]any // the record's attributes: knob, actor, old, new, reason, …
}

type historyRing struct {
	mu     sync.Mutex
	events []Event       // oldest first
	wake   chan struct{} // closed, and replaced, when an event is recorded
	last   Event         // the newest change by a person or an agent, or a lease of theirs ending: kept past MaxHistory, which a rule at work fills
}

// lastChange is the newest knob change by a person or an agent (not a rule's
// reaction, not a write that changed nothing), or one of their leases ending,
// said in a line: "compress_level 9 → 1 by agent:oncall", and when.
func (in *Instance) lastChange() (string, time.Time) {
	h := &in.history
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.At.IsZero() {
		return "", time.Time{}
	}
	f := h.last.Fields
	if f["op"] == "expire" {
		return fmt.Sprintf("%v %v → %v: %v's lease ended", f["knob"], f["old"], f["new"], f["holder"]), h.last.At
	}
	return fmt.Sprintf("%v %v → %v by %v", f["knob"], f["old"], f["new"], f["actor"]), h.last.At
}

// peopleOrAgents says an event is a change a person or an agent made, or one
// of their leases ending, that changed what's in effect.
func peopleOrAgents(e Event) bool {
	return e.Kind == "change" && e.Fields["layer"] != RuleLayer.String() && fmt.Sprint(e.Fields["old"]) != fmt.Sprint(e.Fields["new"])
}

// changed is closed when the history next records something: a change, a
// lease ending, a rule acting or stopping, a freeze. Each waiter takes the
// channel current when it asks, so there's no list of subscribers to leak.
func (in *Instance) changed() <-chan struct{} {
	h := &in.history
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.wake == nil {
		h.wake = make(chan struct{})
	}
	return h.wake
}

// History returns the newest n events, oldest first.
func (in *Instance) History(n int) []Event {
	h := &in.history
	h.mu.Lock()
	defer h.mu.Unlock()
	from := max(len(h.events)-n, 0)
	return append([]Event(nil), h.events[from:]...)
}

// audit writes a record to the logger and keeps it in the history.
func (in *Instance) audit(level slog.Level, msg string, attrs ...slog.Attr) {
	in.logger().LogAttrs(context.Background(), level, msg, attrs...)
	e := Event{At: time.Now(), Kind: strings.TrimPrefix(msg, "vis."), Fields: make(map[string]any, len(attrs))}
	for _, a := range attrs {
		e.Fields[a.Key] = a.Value.Any()
	}
	h := &in.history
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
	if peopleOrAgents(e) {
		h.last = e
	}
	if len(h.events) > MaxHistory {
		h.events = h.events[len(h.events)-MaxHistory:]
	}
	if h.wake != nil {
		close(h.wake)
		h.wake = nil
	}
}
