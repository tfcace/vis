package vis

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reads of a knob outside a request that vis served: From found no snapshot
// in its context. Each read uses the knob's value now, in the default
// instance, so nothing panics; but the request loses its one consistent
// snapshot, and likely its timing too, so vis counts them and says so in the
// UI, over MCP and once in the log.
var unwrapped struct {
	sync.Mutex
	reads map[string]uint64
}

func snapshotFor(ctx context.Context, knob string) *Config {
	if c, ok := SnapshotFrom(ctx); ok {
		return c
	}
	unwrapped.Lock()
	if unwrapped.reads == nil {
		unwrapped.reads = map[string]uint64{}
	}
	unwrapped.reads[knob]++
	first := unwrapped.reads[knob] == 1
	unwrapped.Unlock()
	in := DefaultInstance()
	if first {
		in.logger().Warn("vis: "+knob+" was read outside a request vis.HTTP served, so it reads the value now, not the request's snapshot. Wrap the handler with vis.HTTP (in.HTTP in a test), or carry a snapshot with vis.WithSnapshot.", "knob", knob)
	}
	return in.Snapshot()
}

// UnwrappedReads counts, per knob, the reads From made outside a request vis
// served, since the process started.
func UnwrappedReads() map[string]uint64 {
	unwrapped.Lock()
	defer unwrapped.Unlock()
	out := make(map[string]uint64, len(unwrapped.reads))
	for k, n := range unwrapped.reads {
		out[k] = n
	}
	return out
}

// Warnings are reasons to doubt the service is set up right, each saying how
// to fix it: knobs read outside requests vis served, and objectives whose
// routes never serve.
func (in *Instance) Warnings() []string {
	var out []string
	reads := UnwrappedReads()
	if in != defaultIn { // the reads used the default instance's values, not this one's
		reads = nil
	}
	knobs := make([]string, 0, len(reads))
	for k := range reads {
		knobs = append(knobs, k)
	}
	sort.Strings(knobs)
	if len(knobs) > 0 {
		var parts []string
		for _, k := range knobs {
			parts = append(parts, fmt.Sprintf("%s %s", k, times(reads[k])))
		}
		out = append(out, fmt.Sprintf("Read outside a request vis.HTTP served: %s. Those reads used each knob's value now instead of the request's snapshot, and such requests likely aren't timed. Wrap the handler with vis.HTTP (in.HTTP in a test).",
			strings.Join(parts, ", ")))
	}
	out = append(out, in.envWarnings()...)
	if w := memLimitWarning(in.Runtime(10 * time.Second)); w != "" {
		out = append(out, w)
	}
	if w := in.twinWarning(); w != "" {
		out = append(out, w)
	}
	if w := in.noteWarning(); w != "" {
		out = append(out, w)
	}
	if r := in.ring.Load(); r != nil {
		if why := r.alone(); why != "" {
			out = append(out, fmt.Sprintf("No seed answered (%s): this instance is alone, and the others can't find it until one does. List several seeds in VIS_SEEDS, or a DNS name that resolves to every member.", why))
		}
		for name, addrs := range in.sharedNames() {
			out = append(out, fmt.Sprintf("More than one instance answers as %s, at %s: a change meant for one may land on the other, and the fleet can't tell them apart. Give each its own VIS_INSTANCE_NAME (the default is the hostname).", name, strings.Join(addrs, " and ")))
		}
		if why := r.refusals(); len(why) > 0 {
			out = append(out, fmt.Sprintf("Refused messages from would-be peers: %s. Every instance of %s needs the same VIS_CLUSTER_KEY and service name.", strings.Join(why, "; "), in.service))
		}
	}
	for _, s := range in.SLOs() {
		if s.Warning != "" {
			out = append(out, s.Warning)
		}
	}
	return out
}

func times(n uint64) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// envWarnings says which knobs' environment variables vis didn't take, and why.
func (in *Instance) envWarnings() []string {
	in.mu.Lock()
	decls := in.decls // adopt appends to it under in.mu
	in.mu.Unlock()
	var out []string
	for _, d := range decls {
		if d.envWarn != "" {
			registry.RLock()
			def := d.def // SetDefault may have replaced the declared one
			registry.RUnlock()
			out = append(out, fmt.Sprintf("%s, so %s keeps its default %s.", d.envWarn, d.name, d.format(def)))
		}
	}
	return out
}
