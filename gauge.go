package vis

import (
	"fmt"

	"github.com/tfcace/vis/internal/spec"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// A gauge is a number only the service knows, such as the share of requests
// served from its cache, published to people, agents and rules next to vis's
// own measurements. Declare it like a knob, at package level, and Set it
// wherever the app computes the value. Its value belongs to the process, so
// every instance publishes it.
type Gauge struct {
	name string
	bits atomic.Uint64
	set  atomic.Bool
}

// NewGauge declares a gauge. Unit and About describe it; options that bound a
// knob panic. It panics on a duplicate name, like a knob.
func NewGauge(name string, opts ...Option) *Gauge {
	g := &Gauge{name: name}
	declareGauge(name, opts, func() (float64, bool) { return math.Float64frombits(g.bits.Load()), g.set.Load() })
	return g
}

// Set publishes v as the gauge's value, until the next Set.
func (g *Gauge) Set(v float64) {
	g.bits.Store(math.Float64bits(v))
	g.set.Store(true)
}

// Name is the gauge's name, as people, agents and rules see it.
func (g *Gauge) Name() string { return g.name }

// Measure lets a rule watch the gauge.
func (g *Gauge) Measure() Measure { return gaugeMeasure(g.name, spec.Number, "") }

// Latency is a gauge of a duration, published in milliseconds.
type Latency struct{ g Gauge }

// NewLatency declares a latency gauge. See NewGauge.
func NewLatency(name string, opts ...Option) *Latency {
	l := &Latency{g: Gauge{name: name}}
	declareGauge(name, append([]Option{Unit("ms")}, opts...), func() (float64, bool) {
		return math.Float64frombits(l.g.bits.Load()), l.g.set.Load()
	})
	return l
}

// Set publishes d as the latency's value, until the next Set.
func (l *Latency) Set(d time.Duration) { l.g.Set(msf(d)) }

// Name is the latency's name.
func (l *Latency) Name() string { return l.g.name }

// Measure lets a rule watch the latency, in milliseconds.
func (l *Latency) Measure() Measure { return gaugeMeasure(l.g.name, spec.Latency, "ms") }

// GaugeFunc is a gauge computed each time it is read.
type GaugeFunc struct{ name string }

// NewGaugeFunc declares a gauge whose value is computed each time someone
// reads it, for values that are cheap to compute and always current. Keep
// read cheap: the UI, agents and rules call it.
func NewGaugeFunc(name string, read func() float64, opts ...Option) *GaugeFunc {
	declareGauge(name, opts, func() (float64, bool) { return read(), true })
	return &GaugeFunc{name}
}

// Name is the gauge's name.
func (g *GaugeFunc) Name() string { return g.name }

// Measure lets a rule watch the gauge.
func (g *GaugeFunc) Measure() Measure { return gaugeMeasure(g.name, spec.Number, "") }

type gaugeDef struct {
	name, unit, about string
	read              func() (v float64, ok bool)
}

var gauges struct {
	sync.RWMutex
	defs   []*gaugeDef
	byName map[string]*gaugeDef
}

func declareGauge(name string, opts []Option, read func() (float64, bool)) {
	d := &decl{name: name}
	for _, o := range opts {
		o(d)
	}
	bad := ""
	switch {
	case name == "":
		panic("vis: a gauge needs a name")
	case d.hasRange:
		bad = "Range"
	case d.hasDef:
		bad = "Default"
	case d.pull != 0:
		bad = "Pull"
	case len(d.spends) > 0:
		bad = "Uses or Saves"
	}
	if bad != "" {
		panic(fmt.Sprintf("vis: gauge %q: %s bounds or permits a knob, not a gauge; a gauge takes only Unit and About", name, bad))
	}
	gauges.Lock()
	defer gauges.Unlock()
	if gauges.byName == nil {
		gauges.byName = map[string]*gaugeDef{}
	}
	if gauges.byName[name] != nil {
		panic(fmt.Sprintf("vis: gauge %q is declared twice", name))
	}
	g := &gaugeDef{name: name, unit: d.unit, about: d.about, read: read}
	gauges.defs = append(gauges.defs, g)
	gauges.byName[name] = g
}

func gaugeMeasure(name string, kind spec.Kind, unit string) Measure {
	return Measure{Name: name, Unit: unit, kind: kind, Read: func(*Instance) float64 {
		gauges.RLock()
		g := gauges.byName[name]
		gauges.RUnlock()
		v, _ := g.read()
		return v
	}}
}

// declaredGauges reads every declared gauge, in declaration order.
func declaredGauges() []SignalValue {
	gauges.RLock()
	defs := append([]*gaugeDef(nil), gauges.defs...)
	gauges.RUnlock()
	out := make([]SignalValue, len(defs))
	for i, g := range defs {
		out[i] = SignalValue{Name: g.name, Unit: g.unit, About: g.about}
		func() {
			defer func() {
				if p := recover(); p != nil {
					out[i].Err = fmt.Sprint(p)
				}
			}()
			if v, ok := g.read(); ok {
				out[i].Value = v
			} else {
				out[i].Err = "no value yet: nothing has set it"
			}
		}()
	}
	return out
}

// DeclareGauge declares *g, a gauge variable the app declared without a
// value. It is what vis gen writes for a // vis:signal directive; by hand,
// NewGauge reads better.
func DeclareGauge(g *Gauge, name string, opts ...Option) {
	g.name = name
	declareGauge(name, opts, func() (float64, bool) { return math.Float64frombits(g.bits.Load()), g.set.Load() })
}

// DeclareLatency declares *l, a latency gauge variable. See DeclareGauge.
func DeclareLatency(l *Latency, name string, opts ...Option) {
	l.g.name = name
	declareGauge(name, append([]Option{Unit("ms")}, opts...), func() (float64, bool) {
		return math.Float64frombits(l.g.bits.Load()), l.g.set.Load()
	})
}
