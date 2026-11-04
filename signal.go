package vis

import (
	"fmt"
	"sync"
)

// SignalValue is one app-published signal, read now.
type SignalValue struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
	About string  `json:"about,omitempty"`
	Err   string  `json:"error,omitempty"` // the read failed; Value is meaningless
}

type signalTable struct {
	mu      sync.Mutex
	signals []signalDef
}

type signalDef struct {
	name, unit, about string
	read              func() float64
}

// Signal publishes a value this instance computes when asked, for signals
// that appear while the service runs, such as one per tenant as tenants show
// up. A signal known when the code is written is a gauge: declare it at
// package level with NewGauge, NewLatency or NewGaugeFunc instead. read is
// called whenever someone asks, so keep it cheap. It returns the signal as a
// Measure, so a rule can watch it.
func (in *Instance) Signal(name, unit, about string, read func() float64) Measure {
	t := &in.signals
	t.mu.Lock()
	defer t.mu.Unlock()
	t.signals = append(t.signals, signalDef{name, unit, about, read})
	return Measure{Name: name, Unit: unit, Read: func(*Instance) float64 { return read() }}
}

// Signals reads every published signal: the declared gauges, then those this
// instance publishes with Signal, each in the order they were declared.
func (in *Instance) Signals() []SignalValue {
	t := &in.signals
	t.mu.Lock()
	defs := append([]signalDef(nil), t.signals...)
	t.mu.Unlock()
	declared := declaredGauges()
	out := make([]SignalValue, len(defs))
	for i, d := range defs {
		out[i] = SignalValue{Name: d.name, Unit: d.unit, About: d.about}
		func() {
			defer func() {
				if p := recover(); p != nil {
					out[i].Err = fmt.Sprint(p)
				}
			}()
			out[i].Value = d.read()
		}()
	}
	return append(declared, out...)
}
