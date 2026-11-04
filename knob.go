package vis

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/tfcace/vis/internal/spec"
)

// Layer is one writer's slot in a knob's stack. The effective value is the
// highest layer that hasn't expired: Default < Fleet < Agent < Rule < Pin.
type Layer int

const (
	DefaultLayer Layer = iota // the reviewed baseline, set in code
	FleetLayer                // a staged rollout
	AgentLayer                // vis.set_knob or an approved proposal
	RuleLayer                 // a compiled-in rule
	PinLayer                  // a person; overrides everything until it expires or is removed

	numLayers = int(PinLayer) + 1
)

var layerNames = [numLayers]string{"default", "fleet", "agent", "rule", "pin"}

func (l Layer) String() string {
	if l < 0 || int(l) >= numLayers {
		return fmt.Sprintf("Layer(%d)", int(l))
	}
	return layerNames[l]
}

// Kind is the kind of writer behind a change. The core derives the layer from
// it, so no caller can claim a layer it doesn't own.
type Kind int

const (
	Human Kind = iota + 1 // a person, in the UI or over MCP; writes the pin layer
	Agent                 // an agent through vis.set_knob or an approved proposal
	Rule                  // a compiled-in rule
	Fleet                 // a staged rollout
)

var kindNames = map[Kind]string{Human: "human", Agent: "agent", Rule: "rule", Fleet: "fleet"}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// layer is the one layer a kind of writer may write.
func (k Kind) layer() Layer {
	return map[Kind]Layer{Human: PinLayer, Agent: AgentLayer, Rule: RuleLayer, Fleet: FleetLayer}[k]
}

// Writer is who makes a change. The ID comes from authentication (an OAuth
// subject, a static token's name, a rule's name), never from text the caller types.
type Writer struct {
	Kind Kind
	ID   string
}

func (w Writer) String() string { return w.Kind.String() + ":" + w.ID }

func (w Writer) valid() bool { return w.ID != "" && w.Kind.layer() != DefaultLayer }

var (
	ErrUnknownKnob   = errors.New("vis: unknown knob")
	ErrOutOfRange    = errors.New("vis: value out of range")
	ErrDefaultLayer  = errors.New("vis: the default layer is set in code")
	ErrLeaseRequired = errors.New("vis: fleet, agent and rule values need a lease")
	ErrActorRequired = errors.New("vis: a change needs an actor")
	ErrFrozen        = errors.New("vis: automated changes are frozen")
	ErrNotAllowed    = errors.New("vis: writer not allowed")
	ErrConflict      = errors.New("vis: conflicting change")
	ErrEmptyChange   = errors.New("vis: a change needs at least one knob")
	ErrUnknownLease  = errors.New("vis: unknown or ended lease")
	ErrUnknownRule   = errors.New("vis: unknown rule")
)

// Option configures a knob declaration.
type Option func(*decl)

// Range bounds a numeric knob. Every runtime value must fall inside it. Its
// type must match the knob's: durations for a Duration knob, so a bare 10
// can't silently mean 10 nanoseconds.
func Range[T ~int | ~int64](min, max T) Option {
	return func(d *decl) {
		d.min, d.max, d.hasRange, d.rangeT = int64(min), int64(max), true, reflect.TypeFor[T]()
	}
}

// Default is the knob's reviewed baseline value. Its type must match the knob's.
func Default[T ~int | ~int64](v T) Option {
	return func(d *decl) { d.def, d.hasDef, d.defT = int64(v), true, reflect.TypeFor[T]() }
}

// Special is one value outside the range that the service gives its own
// meaning, named for people and agents: Special(time.Duration(0), "no limit")
// for a time limit where 0 means none, Special(0, "unlimited") for a pool
// size. The code reads it as the value it is, so `if timeout > 0` keeps
// working. vis never calculates with it: people and agents choose it by its
// name, a default, a flag (SetDefault) or Env may start the knob at it, and a
// rule never steps into it or out of it. Its type must match the knob's.
func Special[T ~int | ~int64](v T, name string) Option {
	return func(d *decl) {
		d.special, d.hasSpecial, d.specialName, d.specialT = int64(v), true, name, reflect.TypeFor[T]()
	}
}

// Unit labels the knob's value for people and agents, for example "MiB".
func Unit(u string) Option { return func(d *decl) { d.unit = u } }

// About says what the knob does and what turning it costs, for the people and
// agents deciding whether to: "Compression level. 9 costs about 7x the CPU of 6."
func About(s string) Option { return func(d *decl) { d.about = s } }

// Env takes the knob's default from the environment variable key when it is
// set: a whole number for an Int knob, a duration such as 300ms for a
// Duration knob, a level such as debug for a LogLevel knob. A value that doesn't parse or is outside the range keeps the
// code's Default, and the instance's Warnings say why. A service configured
// by environment variables keeps them working this way, with vis's range
// around them.
func Env(key string) Option { return func(d *decl) { d.env = key } }

// applyEnv sets the default from the knob's variable, if it holds a good value.
func (d *decl) applyEnv() {
	raw, ok := os.LookupEnv(d.env)
	if d.env == "" || !ok || raw == "" {
		return
	}
	var v int64
	if d.isLevel {
		l, err := spec.ParseLevel(raw)
		if err != nil {
			d.envWarn = fmt.Sprintf("%s=%q isn't a log level such as trace, debug, info, warn or error", d.env, raw)
			return
		}
		v = int64(l)
	} else if d.isDuration {
		dur, err := time.ParseDuration(raw)
		if err != nil {
			example := "300ms"
			if _, err := strconv.ParseFloat(raw, 64); err == nil {
				example = raw + "s" // a bare number: say the unit it needs
			}
			d.envWarn = fmt.Sprintf("%s=%q isn't a duration such as %s", d.env, raw, example)
			return
		}
		v = int64(dur)
	} else {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			d.envWarn = fmt.Sprintf("%s=%q isn't a whole number", d.env, raw)
			return
		}
		v = n
	}
	if !d.allowed(v) {
		d.envWarn = fmt.Sprintf("%s=%q is outside %s's range %s", d.env, raw, d.name, d.rangeText())
		return
	}
	d.def, d.fromEnv = v, true
}

// Pull lists the automated writers that may change the knob directly. People
// may always change any knob; a knob without Pull is theirs alone.
func Pull(kinds ...Kind) Option {
	return func(d *decl) {
		for _, k := range kinds {
			d.pull |= 1 << k
		}
	}
}

// decl is a knob declaration. Declarations are package-level and shared by
// every Instance; values live in an Instance.
type decl struct {
	name             string
	idx              int
	min, max, def    int64
	hasRange, hasDef bool
	unit             string
	about            string
	spends           []spend
	pull             uint8 // bit per Kind that may write directly
	isDuration       bool
	isLevel          bool
	rangeT, defT     reflect.Type // the types Range and Default were given
	env              string       // the variable that may set the default (Env)
	fromEnv          bool         // it did
	envWarn          string       // why it didn't, when it was set; Warnings adds the default that stands
	special          int64        // the value outside the range the service gives its own meaning (Special)
	hasSpecial       bool
	specialName      string       // what it means, as people and agents read and write it: "no limit"
	specialT         reflect.Type // the type Special was given
}

// isSpecial reports whether v is the knob's special value.
func (d *decl) isSpecial(v int64) bool { return d.hasSpecial && v == d.special }

// allowed reports whether a knob may hold v: inside its range, or its special value.
func (d *decl) allowed(v int64) bool { return v >= d.min && v <= d.max || d.isSpecial(v) }

// rangeText is what a knob may hold, as an error says it: 1s..5m0s, or 1s..5m0s or "no limit".
func (d *decl) rangeText() string {
	s := d.format(d.min) + ".." + d.format(d.max)
	if d.hasSpecial {
		s += fmt.Sprintf(" or %q", d.specialName)
	}
	return s
}

var (
	durationType = reflect.TypeFor[time.Duration]()
	levelType    = reflect.TypeFor[slog.Level]()
)

// fits reports whether an option's type suits the knob: time.Duration for a
// Duration knob, slog.Level for a LogLevel, a plain integer (not a duration)
// for an Int.
func (d *decl) fits(t reflect.Type) bool {
	if d.isLevel {
		return t == levelType
	}
	if d.isDuration {
		return t == durationType
	}
	return t != durationType
}

// format writes a value in the knob's own terms, "1m0s" rather than 60000000000.
func (d *decl) format(v int64) string {
	if d.isSpecial(v) {
		return d.specialName
	}
	if d.isLevel {
		return spec.LevelName(slog.Level(v))
	}
	if d.isDuration {
		return time.Duration(v).String()
	}
	return strconv.FormatInt(v, 10)
}

// attr is an audit attribute in the knob's own terms: numbers stay numbers, durations read as durations.
func (d *decl) attr(key string, v int64) slog.Attr {
	if d.isLevel || d.isDuration || d.isSpecial(v) {
		return slog.String(key, d.format(v))
	}
	return slog.Int64(key, v)
}

// formatPlain writes v as a number of the knob's type, even when it is the special value.
func (d *decl) formatPlain(v int64) string {
	c := *d
	c.hasSpecial = false
	return c.format(v)
}

// unitStep is a small step in the knob's own unit, for a suggested range: 1s for a duration, 1 otherwise.
func (d *decl) unitStep() int64 {
	if d.isDuration {
		return int64(time.Second)
	}
	return 1
}

// readsAsValue reports whether s parses as one of the knob's values, so it can't name the special one.
func (d *decl) readsAsValue(s string) bool {
	c := *d
	c.hasSpecial = false
	_, err := c.parse(s)
	return err == nil
}

func (d *decl) pulls(k Kind) bool { return k == Human || d.pull&(1<<k) != 0 }

// registry holds every declaration in the process, in declaration order.
var registry struct {
	sync.RWMutex
	decls  []*decl
	byName map[string]*decl
}

// knobType is what a knob's int64 value means.
type knobType uint8

const (
	intKnob knobType = iota
	durationKnob
	levelKnob
)

func declare(name string, typ knobType, opts []Option) *decl {
	d := &decl{name: name, isDuration: typ == durationKnob, isLevel: typ == levelKnob}
	for _, o := range opts {
		o(d)
	}
	want := "an integer, for example vis.Range(1, 100)"
	switch typ {
	case durationKnob:
		want = "a time.Duration, for example vis.Range(10*time.Millisecond, 5*time.Second)"
	case levelKnob:
		want = "a slog.Level, for example vis.Range(slog.LevelDebug, slog.LevelError)"
		if !d.hasRange { // a service's log level is almost always one of these
			d.min, d.max, d.hasRange, d.rangeT = int64(slog.LevelDebug), int64(slog.LevelError), true, levelType
		}
		if !d.hasDef {
			d.def, d.hasDef, d.defT = int64(slog.LevelInfo), true, levelType
		}
	}
	switch {
	case d.hasRange && !d.fits(d.rangeT):
		panic(fmt.Sprintf("vis: knob %q: Range was given %v; it needs %s", name, d.rangeT, want))
	case d.hasDef && !d.fits(d.defT):
		panic(fmt.Sprintf("vis: knob %q: Default was given %v; it needs %s", name, d.defT, want))
	case name == "":
		panic("vis: a knob needs a name")
	case !d.hasRange:
		panic(fmt.Sprintf("vis: knob %q needs a Range", name))
	case !d.hasDef:
		panic(fmt.Sprintf("vis: knob %q needs a Default", name))
	case d.min > d.max:
		panic(fmt.Sprintf("vis: knob %q has an empty range %s..%s", name, d.format(d.min), d.format(d.max)))
	}
	if d.hasSpecial {
		switch {
		case !d.fits(d.specialT):
			panic(fmt.Sprintf("vis: knob %q: Special was given %v; it needs %s", name, d.specialT, want))
		case d.special >= d.min && d.special <= d.max:
			panic(fmt.Sprintf("vis: knob %q: its special value %s is inside its range %s..%s; a special value means something the range doesn't, so start the range past it, such as %s..%s",
				name, d.formatPlain(d.special), d.format(d.min), d.format(d.max), d.formatPlain(d.special+d.unitStep()), d.format(d.max)))
		case d.specialName == "":
			panic(fmt.Sprintf("vis: knob %q: its special value %s needs a name saying what it means, such as \"no limit\"", name, d.formatPlain(d.special)))
		case d.readsAsValue(d.specialName):
			panic(fmt.Sprintf("vis: knob %q: the special value's name %q reads as a value; name what it means, such as \"no limit\"", name, d.specialName))
		}
	}
	if !d.allowed(d.def) {
		panic(fmt.Sprintf("vis: knob %q default %s is outside %s", name, d.format(d.def), d.rangeText()))
	}
	d.applyEnv()
	registry.Lock()
	defer registry.Unlock()
	if registry.byName == nil {
		registry.byName = map[string]*decl{}
	}
	if _, dup := registry.byName[name]; dup {
		panic(fmt.Sprintf("vis: knob %q is declared twice", name))
	}
	d.idx = len(registry.decls)
	registry.decls = append(registry.decls, d)
	registry.byName[name] = d
	return d
}

func (d *decl) setDefault(v int64) error {
	if !d.allowed(v) {
		return fmt.Errorf("vis: knob %s: %s is outside its range %s", d.name, d.format(v), d.rangeText())
	}
	if defaultStarted.Load() {
		panic(fmt.Sprintf("vis: knob %q: SetDefault after vis.Start: a running service's defaults are what it was reviewed with; call it after flag.Parse, before Start", d.name))
	}
	if d.fromEnv {
		return nil // the operator's value for this process beats the code's
	}
	registry.Lock()
	d.def = v
	registry.Unlock()
	if defaultCreated.Load() { // Bind, Now or HTTP made it already: it follows
		DefaultInstance().setDefault(d, v)
	}
	return nil
}

func lookup(name string) *decl {
	registry.RLock()
	defer registry.RUnlock()
	return registry.byName[name]
}

func allDecls() []*decl {
	registry.RLock()
	defer registry.RUnlock()
	return append([]*decl(nil), registry.decls...)
}

// Int is a bounded integer knob.
type Int struct{ d *decl }

// NewInt declares an integer knob. It panics on an invalid declaration, since
// declarations are reviewed code, not runtime input.
func NewInt(name string, opts ...Option) Int { return Int{declare(name, intKnob, opts)} }

// DeclareInt declares *k, a knob variable the app declared without a value.
// It is what vis gen writes for a // vis:knob directive; by hand, NewInt reads better.
func DeclareInt(k *Int, name string, opts ...Option) { *k = NewInt(name, opts...) }

// DeclareDuration declares *k, a Duration knob variable. See DeclareInt.
func DeclareDuration(k *Duration, name string, opts ...Option) { *k = NewDuration(name, opts...) }

// DeclareLogLevel declares *k, a LogLevel knob variable. See DeclareInt.
func DeclareLogLevel(k *LogLevel, name string, opts ...Option) { *k = NewLogLevel(name, opts...) }

// Name is the knob's name, as agents and the manifest see it.
func (k Int) Name() string { return k.decl().name }

// Get reads the knob from a snapshot.
func (k Int) Get(c *Config) int { return int(c.get(k.decl())) }

// SetDefault replaces the knob's declared default, for a default that comes
// from a flag: call it after flag.Parse and before Start. A value outside the
// range is an error, saying the range, since a flag is user input. The default
// instance follows, and anything bound to the knob sees the new value; other
// instances created earlier keep theirs. It panics after Start. A value the
// knob's env= variable set wins: SetDefault then only checks v's range. Use it
// too for a default worked out at startup, such as one per CPU.
func (k Int) SetDefault(v int) error { return k.decl().setDefault(int64(v)) }

// Range is the knob's declared range.
func (k Int) Range() (lo, hi int) { return int(k.decl().min), int(k.decl().max) }

// Bind keeps state built from the knob in step with it, in the default
// instance, such as a cache sized by it: fn runs with the current value before Bind returns, then with every
// new value, in the order changes were published, possibly on another
// goroutine. No change can slip in between. fn may change knobs itself. A
// panic in fn is recovered and audited. Bind while setting up, not from
// inside another knob's callback: there it would wait on itself.
func (k Int) Bind(fn func(n int)) { k.BindIn(DefaultInstance(), fn) }

// BindIn is Bind on a given instance.
func (k Int) BindIn(in *Instance, fn func(n int)) {
	in.bind(k.decl(), func(v int64) { fn(int(v)) })
}

// Now is the knob's value in the default instance right now. In a request,
// read From instead, so the whole request sees one snapshot.
func (k Int) Now() int { return k.Get(DefaultInstance().Snapshot()) }

// Duration is a bounded time.Duration knob.
type Duration struct{ d *decl }

// NewDuration declares a duration knob. It panics on an invalid declaration.
func NewDuration(name string, opts ...Option) Duration {
	return Duration{declare(name, durationKnob, opts)}
}

// Name is the knob's name, as agents and the manifest see it.
func (k Duration) Name() string { return k.decl().name }

// Get reads the knob from a snapshot.
func (k Duration) Get(c *Config) time.Duration { return time.Duration(c.get(k.decl())) }

// SetDefault replaces the knob's declared default. See Int.SetDefault.
func (k Duration) SetDefault(v time.Duration) error { return k.decl().setDefault(int64(v)) }

// Range is the knob's declared range.
func (k Duration) Range() (lo, hi time.Duration) {
	return time.Duration(k.decl().min), time.Duration(k.decl().max)
}

// Bind keeps state built from the knob in step with it. See Int.Bind.
func (k Duration) Bind(fn func(d time.Duration)) { k.BindIn(DefaultInstance(), fn) }

// BindIn is Bind on a given instance.
func (k Duration) BindIn(in *Instance, fn func(d time.Duration)) {
	in.bind(k.decl(), func(v int64) { fn(time.Duration(v)) })
}

// Now is the knob's value in the default instance right now. See Int.Now.
func (k Duration) Now() time.Duration { return k.Get(DefaultInstance().Snapshot()) }

// LogLevel is a log level knob, so a service can log more while someone
// looks into it and less again after. It is a slog.Leveler, reading the
// default instance, so a handler takes it as its level directly:
//
//	slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: LogLevel}))
//
// People and agents see and set it by name: DEBUG, INFO, WARN, ERROR.
type LogLevel struct{ d *decl }

// NewLogLevel declares a log level knob. Without Range it runs from
// slog.LevelDebug to slog.LevelError, and without Default it starts at
// slog.LevelInfo; both take slog.Level values. It panics on an invalid
// declaration.
func NewLogLevel(name string, opts ...Option) LogLevel {
	return LogLevel{declare(name, levelKnob, opts)}
}

// Name is the knob's name, as agents and the manifest see it.
func (k LogLevel) Name() string { return k.decl().name }

// Get reads the knob from a snapshot.
func (k LogLevel) Get(c *Config) slog.Level { return slog.Level(c.get(k.decl())) }

// Level is the knob's value in the default instance, which makes the knob a
// slog.Leveler. It costs one atomic load, so a handler can ask on every record.
func (k LogLevel) Level() slog.Level { return k.Now() }

// Now is the knob's value in the default instance right now. See Int.Now.
func (k LogLevel) Now() slog.Level { return k.Get(DefaultInstance().Snapshot()) }

// SetDefault replaces the knob's declared default. See Int.SetDefault.
func (k LogLevel) SetDefault(v slog.Level) error { return k.decl().setDefault(int64(v)) }

// Range is the knob's declared range.
func (k LogLevel) Range() (lo, hi slog.Level) {
	return slog.Level(k.decl().min), slog.Level(k.decl().max)
}

// Bind keeps state built from the knob in step with it. See Int.Bind. A
// handler needs no Bind: give it the knob as its Level.
func (k LogLevel) Bind(fn func(l slog.Level)) { k.BindIn(DefaultInstance(), fn) }

// BindIn is Bind on a given instance.
func (k LogLevel) BindIn(in *Instance, fn func(l slog.Level)) {
	in.bind(k.decl(), func(v int64) { fn(slog.Level(v)) })
}
