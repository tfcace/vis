package vis

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Config is one immutable snapshot of every knob's effective value. Taking a
// snapshot is one atomic load, and reading a knob is a slice index, so a
// handler that takes one at request start sees consistent values throughout.
type Config struct {
	vals []int64
}

func (c *Config) get(d *decl) int64 {
	if d.idx < len(c.vals) {
		return c.vals[d.idx]
	}
	return d.def // declared after this instance was created
}

// Entry is one layer of a knob's stack.
type Entry struct {
	Layer  Layer
	Value  int64
	By     Writer
	Change uint64        // the change that wrote it
	Lease  uint64        // the lease group it belongs to; 0 means no expiry
	Left   time.Duration // time until the lease ends; 0 means no expiry
	Then   int64         // what the knob falls back to when this layer ends
	Kept   bool          // its lease is kept across a restart (Tx.Keep)
}

type slot struct {
	set    bool
	value  int64
	by     Writer
	change uint64
	lease  *lease // nil means no expiry
}

type knobState struct {
	layers [numLayers]slot
}

// Instance holds knob values. Declarations are shared by the whole process;
// values are per instance, so tests that each start their own can run in parallel.
type Instance struct {
	cfg     atomic.Pointer[Config] // the published snapshot; readers never lock
	started time.Time              // when it was made: the first minute after, the page judges no SLO

	deathMu    sync.Mutex
	deathsTold map[string]bool        // former processes whose death it told, by ID: each is told once
	succeeded  map[string]bool        // its own former processes it took over from, by ID (succeed): each once
	dropped    []dropped              // what deaths of its former processes dropped, carried until set again or its lease ends
	droppedAt  time.Time              // when dropped's leases were read
	noteErr    atomic.Pointer[string] // why its last word can't be written, if it can't (lastword.go)
	noteDir    atomic.Pointer[string]
	former     atomic.Pointer[formerProcess] // the process this one replaced, from its last word
	ownDeaths  []int64                       // its name's deaths its note kept, Unix nanoseconds; under deathMu
	memShort   atomic.Bool                   // memory's trend raised pressure when last asked: it holds to pressureHold
	noteTwin   atomic.Int64                  // another live process's PID under this service and name, from its note: a warning

	mu     sync.Mutex // guards everything below until nmu
	decls  []*decl
	knobs  []knobState
	change uint64 // last change ID
	leaseN uint64 // last lease ID
	// when the last change was made, for windows measured since it (memoryOutlook)
	changedAt time.Time
	leases    map[uint64]*lease
	freeze    FreezeState
	watch     map[int][]func(old, new int64)
	log       *slog.Logger // set by WithLogger; nil means slog.Default when logging
	build     string
	service   string
	id        string // this process's, new at every start
	name      string // stable across restarts; see InstanceName
	slos      []sloDef

	routes      routeTable                    // request timing; locks of its own
	probes      probeTable                    // per route: time in each probe, and requests by the probes they ran
	profiling   sync.Mutex                    // one profile at a time
	runtime     runtimeRing                   // runtime counter samples; locks of its own
	rules       ruleTable                     // running rules' states; locks of its own
	history     historyRing                   // recent audit events; locks of its own
	signals     signalTable                   // app-published signals; locks of its own
	impacts     impactTable                   // what each change did; locks of its own
	ring        atomic.Pointer[ring]          // the fleet, once Serve joins it; nil alone
	lastRollout atomic.Pointer[rolloutReport] // the last rollout this instance coordinated; fleet shows the fleet's newest, from gossip too
	rolling     atomic.Bool                   // a rollout from here hasn't published how it ended yet
	rolloutSeq  atomic.Int64                  // orders this instance's rollout reports

	stallSummary atomic.Int64 // tests: how long to sit on a fleet summary call, as a starved instance would

	stopMu     sync.Mutex
	stops      []stopper    // what each Serve started, for Stop
	skipLeave  atomic.Bool  // tests: stop without telling peers, as a crash would
	stallPeers atomic.Int64 // tests: how long to sit on every peer request, as a paused process would
	stallCalls atomic.Int64 // tests: how long to sit on forwarded tool calls only, as a starved process (whose gossip still gets through) would

	// Bind callbacks run outside mu, in publish order, so a watcher may change knobs itself.
	nmu      sync.Mutex
	pending  []func()
	draining bool
}

// InstanceOption configures an Instance.
type InstanceOption func(*Instance)

// WithLogger sends audit records to l instead of slog.Default.
func WithLogger(l *slog.Logger) InstanceOption { return func(in *Instance) { in.log = l } }

// logger is WithLogger's logger, or the slog default in effect now: an app
// may install its own with slog.SetDefault after the instance exists.
func (in *Instance) logger() *slog.Logger {
	if in.log != nil {
		return in.log
	}
	return slog.Default()
}

// NewInstance starts an instance with every declared knob at its default.
func NewInstance(opts ...InstanceOption) *Instance {
	in := &Instance{started: time.Now(), decls: allDecls(), service: "service", build: buildID(), id: newID(), name: defaultName(), watch: map[int][]func(old, new int64){}, leases: map[uint64]*lease{}}
	for _, o := range opts {
		o(in)
	}
	in.knobs = make([]knobState, len(in.decls))
	for i, d := range in.decls {
		in.knobs[i].layers[DefaultLayer] = slot{set: true, value: d.def}
	}
	in.publish()
	return in
}

// Snapshot returns the current values. It never blocks.
func (in *Instance) Snapshot() *Config { return in.cfg.Load() }

// Stack returns a knob's layers, top first, so a caller can see why it has its value.
func (in *Instance) Stack(knob string) ([]Entry, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	d, err := in.decl(knob)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []Entry
	for l := numLayers - 1; l >= 0; l-- {
		s := in.knobs[d.idx].layers[l]
		if !s.set {
			continue
		}
		e := Entry{Layer: Layer(l), Value: s.value, By: s.by, Change: s.change}
		if ls := s.lease; ls != nil {
			e.Lease, e.Left, e.Kept = ls.id, ls.until.Sub(now), ls.keep
			if in.freeze.Frozen {
				e.Left = ls.left
			}
		}
		out = append(out, e)
	}
	for i := range out {
		out[i].Then = out[min(i+1, len(out)-1)].Value // the default has nothing below it
	}
	return out, nil
}

// notify runs queued Bind callbacks in order. Only one goroutine drains at a
// time; a call that changes a knob just queues more work for the same drain.
func (in *Instance) notify() {
	in.nmu.Lock()
	if in.draining {
		in.nmu.Unlock()
		return
	}
	in.draining = true
	for len(in.pending) > 0 {
		fn := in.pending[0]
		in.pending[0] = nil // let the closure go once it has run
		in.pending = in.pending[1:]
		in.nmu.Unlock()
		fn()
		in.nmu.Lock()
	}
	in.draining = false
	in.nmu.Unlock()
}

// safely runs a watcher. A panic is the app's bug: it is audited and the drain
// goes on, so one bad watcher can't silence every later notification.
func (in *Instance) safely(knob string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			in.audit(slog.LevelError, "vis.onchange_panic",
				slog.String("knob", knob), slog.Any("panic", p), slog.String("stack", string(debug.Stack())))
		}
	}()
	fn()
}

// setDefault replaces a knob's default layer, publishing and notifying when
// that changes the knob's value, as a change would.
func (in *Instance) setDefault(d *decl, v int64) {
	in.mu.Lock()
	in.adopt(d)
	old := in.effect(d.idx)
	in.knobs[d.idx].layers[DefaultLayer] = slot{set: true, value: v}
	now := in.effect(d.idx)
	if now.Value != old.Value {
		in.publish()
		in.nmu.Lock()
		for _, fn := range in.watch[d.idx] {
			o, n := old.Value, now.Value
			in.pending = append(in.pending, func() { in.safely(d.name, func() { fn(o, n) }) })
		}
		in.nmu.Unlock()
	}
	in.mu.Unlock()
	in.notify()
}

// bind registers fn as a watcher and queues its first call, with the current
// value, ahead of any change published after it: both happen under mu, where
// changes queue theirs. It returns once that first call has run.
func (in *Instance) bind(d *decl, fn func(v int64)) {
	done := make(chan struct{})
	in.mu.Lock()
	in.adopt(d)
	in.watch[d.idx] = append(in.watch[d.idx], func(_, n int64) { fn(n) })
	v := in.effect(d.idx).Value
	in.nmu.Lock()
	in.pending = append(in.pending, func() {
		defer close(done)
		in.safely(d.name, func() { fn(v) })
	})
	in.nmu.Unlock()
	in.mu.Unlock()
	in.notify()
	<-done // run here, or by whoever is delivering changes right now
}

func buildID() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return bi.Main.Version
}

// decl resolves a knob by name. Callers hold in.mu.
func (in *Instance) decl(name string) (*decl, error) {
	d := lookup(name)
	if d == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKnob, name)
	}
	in.adopt(d)
	return d, nil
}

// adopt takes in knobs declared after the instance started, at their defaults.
// Declarations are append-only, so existing indexes stay put. Callers hold in.mu.
func (in *Instance) adopt(d *decl) {
	if d.idx < len(in.knobs) {
		return
	}
	for _, nd := range allDecls()[len(in.decls):] {
		in.decls = append(in.decls, nd)
		in.knobs = append(in.knobs, knobState{})
		in.knobs[len(in.knobs)-1].layers[DefaultLayer] = slot{set: true, value: nd.def}
	}
	in.publish()
}

// effect is the top set layer of knob i. Callers hold in.mu.
func (in *Instance) effect(i int) Effect { return effectOf(&in.knobs[i].layers) }

// effectOf is the top set layer of a stack.
func effectOf(ls *[numLayers]slot) Effect {
	for l := numLayers - 1; l >= 0; l-- {
		if ls[l].set {
			return Effect{Value: ls[l].value, Layer: Layer(l)}
		}
	}
	panic("vis: knob without a default layer")
}

// publish rebuilds the Config from every knob and swaps it in. Changes patch
// the current Config instead (see finish); this is for start-up and adoption.
// Callers hold in.mu (or own in).
func (in *Instance) publish() {
	c := &Config{vals: make([]int64, len(in.knobs))}
	for i := range in.knobs {
		c.vals[i] = in.effect(i).Value
	}
	in.cfg.Store(c)
}
