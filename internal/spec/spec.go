// Package spec is what a vis declaration means, in one place: which measures
// are latencies, and which vis types hold latencies. The library's checks,
// vis gen and vis vet all read it, so they can't disagree; a test in vis ties
// the library's own measures to it.
package spec

import "time"

// Kind is what a measure or a threshold is: a latency (milliseconds, written
// as a time.Duration) or a plain number.
type Kind int

const (
	Unknown Kind = iota
	Latency
	Number
)

func (k Kind) String() string {
	switch k {
	case Latency:
		return "a latency"
	case Number:
		return "a plain number"
	}
	return "unknown"
}

// Measure is one of vis's ready-made measures.
type Measure struct {
	Name string // as a rule's when= names it: sched.p99
	Var  string // the vis variable: SchedP99
	Kind Kind
}

// Measures are vis's ready-made measures.
var Measures = []Measure{
	{"sched.p99", "SchedP99", Latency},
	{"gc.cpu", "GCCPU", Number},
	{"cpu.throttled", "CPUThrottled", Number},
	{"cpu.used", "CPUUsed", Number},
	{"goroutines", "Goroutines", Number},
}

// MeasureNamed is the ready-made measure a rule's when= names, if any.
func MeasureNamed(name string) (Measure, bool) {
	for _, m := range Measures {
		if m.Name == name {
			return m, true
		}
	}
	return Measure{}, false
}

// MeasureVar is the ready-made measure a vis variable holds, if any.
func MeasureVar(v string) (Measure, bool) {
	for _, m := range Measures {
		if m.Var == v {
			return m, true
		}
	}
	return Measure{}, false
}

// RouteP99 is a route's p99, vis.RouteP99: a latency.
const RouteP99 = Latency

// TypeKind is what a vis type's values are, as a measure or a threshold:
// a Latency gauge and a Duration knob are latencies; a Gauge and an Int knob
// are numbers.
func TypeKind(visType string) Kind {
	switch visType {
	case "Latency", "Duration":
		return Latency
	case "Gauge", "GaugeFunc", "Int":
		return Number
	}
	return Unknown
}

// MinDurationStep is the smallest step or drift that reads as meant on a
// Duration knob: below it, a bare number was almost surely meant in another
// unit, since a Duration knob counts nanoseconds.
const MinDurationStep = time.Microsecond

// Lurch is the first step of a rule's fixed step that cuts its knob by more
// than half: the value before and after it, as the rule steps from the knob's
// default def toward the end of its reach (drift, by default four steps,
// and no lower than lo). Sizes, counts and concurrency respond to ratios, so a fixed
// step gets harsher near the bottom of the range: -16 takes 64 to 48 but 16
// to 1. A step that grows the knob, or a knob whose range reaches below zero,
// where a value isn't a size, never lurches.
func Lurch(def, lo, step, drift int64) (from, to int64, ok bool) {
	if step >= 0 || lo < 0 {
		return 0, 0, false
	}
	if drift <= 0 {
		drift = 4 * -step
	}
	floor := max(def-drift, lo)
	for v := def; v > floor && v > 0; {
		next := max(v+step, floor)
		if (v-next)*2 > v {
			return v, next, true
		}
		v = next
	}
	return 0, 0, false
}

// Cut is how much a step from from to to cuts, in whole percent.
func Cut(from, to int64) int64 { return ((from-to)*100 + from/2) / from }
