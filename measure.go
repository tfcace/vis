package vis

import (
	"time"

	"github.com/tfcace/vis/internal/spec"
)

// Knob is any declared knob: Int, Duration. A rule takes the knob itself, not
// its name, so a typo is a compile error.
type Knob interface{ decl() *decl }

func (k Int) decl() *decl      { return declared(k.d, "Int") }
func (k Duration) decl() *decl { return declared(k.d, "Duration") }
func (k LogLevel) decl() *decl { return declared(k.d, "LogLevel") }

// declared is d, or a panic saying how to declare the knob: a knob variable
// is its zero value until vis_gen.go (or a Declare call) declares it, and the
// stack points at the read.
func declared(d *decl, kind string) *decl {
	if d == nil {
		panic("vis: a knob was used but not declared: run go generate (vis gen, with //go:generate go tool vis gen in the package), or declare it with vis.Declare" + kind)
	}
	return d
}

// Measure is a value a rule can watch: what it is, its unit, and how to read it.
type Measure struct {
	Name     string   // as people and agents read it: "sched.p99"
	Unit     string   // "ms"
	Resource Resource // what it measures the shortage of, if anything: CPU for scheduler latency
	Read     func(*Instance) float64

	since func(*Instance, time.Time) (float64, bool) // the value over requests since a time, for measures of routes
	kind  spec.Kind                                  // a latency or a number; unset, a unit of "ms" means a latency
}

// kindOf is whether the measure is a latency or a plain number.
func (m Measure) kindOf() spec.Kind {
	switch {
	case m.kind != spec.Unknown:
		return m.kind
	case m.Unit == "ms":
		return spec.Latency
	}
	return spec.Number
}

// Ready-made measures from vis's own signals.
var (
	// SchedP99 is the Go runtime's scheduler latency p99 over the last second, in ms:
	// how long runnable goroutines wait for a CPU.
	SchedP99 = Measure{Name: "sched.p99", Unit: "ms", Resource: CPU, kind: spec.Latency, Read: func(in *Instance) float64 { return msf(in.Runtime(time.Second).SchedP99) }}
	// GCCPU is the share of CPU spent on garbage collection over the last 10 seconds, 0 to 1.
	GCCPU = Measure{Name: "gc.cpu", Resource: CPU, kind: spec.Number, Read: func(in *Instance) float64 { return in.Runtime(10 * time.Second).GCCPU }}
	// CPUThrottled is the share of scheduling periods in which the container hit its CPU limit, over the last 10 seconds.
	CPUThrottled = Measure{Name: "cpu.throttled", Resource: CPU, kind: spec.Number, Read: func(in *Instance) float64 { return in.Runtime(10 * time.Second).Throttled }}
	// CPUUsed is the CPU the process used over the last 10 seconds, in cores.
	CPUUsed = Measure{Name: "cpu.used", Unit: "cores", Resource: CPU, kind: spec.Number, Read: func(in *Instance) float64 { return in.Runtime(10 * time.Second).CPUUsed }}
	// Goroutines is how many goroutines are alive now.
	Goroutines = Measure{Name: "goroutines", kind: spec.Number, Read: func(in *Instance) float64 { return float64(in.Runtime(time.Second).Goroutines) }}
)

// RouteP99 is a route's request p99 over the last minute, in ms, measured inside the service.
// A rule judging its step counts only the requests served since the step.
func RouteP99(route string) Measure {
	return Measure{Name: route + " p99", Unit: "ms", kind: spec.RouteP99,
		Read: func(in *Instance) float64 { return msf(in.Route(route, time.Minute).P99) },
		since: func(in *Instance, t time.Time) (float64, bool) {
			r := in.routes.since(route, t, time.Now())
			return msf(r.P99), r.Requests > 0
		}}
}
