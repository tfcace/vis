package vis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"
)

// ProfileReport is a pprof profile read for a person or an agent: the code
// that owns the cost, biggest first. Owning code is the first frame outside the
// runtime and the sync, syscall and time packages, so a goroutine parked in a
// select is counted where it selects, and lock waits are charged to the code
// that held the lock.
type ProfileReport struct {
	Kind    string  `json:"kind"`
	Seconds float64 `json:"seconds,omitempty" jsonschema:"how long it sampled; goroutine, goroutineleak and heap are a moment"`
	Unit    string  `json:"unit"`
	Total   float64 `json:"total"`
	// Vis is what vis's own goroutines added, left out of Total and Top: in a
	// block or goroutine profile they're parked waiting for work (a dashboard's
	// stream, a rule's next tick, an agent's next call), which costs the service nothing.
	Vis     float64       `json:"vis,omitempty"`
	Top     []ProfileLine `json:"top"`
	Summary string        `json:"summary"`
}

// ProfileLine is one owner of the cost.
type ProfileLine struct {
	Function string   `json:"function"`
	Where    string   `json:"where"`         // file:line in the owning function
	Via      string   `json:"via,omitempty"` // the frame under it where the cost lands, when that is runtime or library code
	Callers  []string `json:"callers,omitempty"`
	Value    float64  `json:"value"`
	Share    float64  `json:"share"`
	Events   int64    `json:"events,omitempty"` // mutex and block: how many waits
	Each     float64  `json:"each_s,omitempty"` // mutex and block: seconds per wait, on average
}

// profiling guards the process's profilers, which are global: one profile at a
// time in the process, whichever Instance takes it.
var profiling sync.Mutex

// noLeakProfile says why a build has no goroutineleak profile and how to get one.
const noLeakProfile = "this build has no goroutineleak profile: Go 1.26 has it only in binaries built with GOEXPERIMENT=goroutineleakprofile"

// ProfileKinds are what Profile can take.
var ProfileKinds = []string{"cpu", "goroutine", "goroutineleak", "heap", "mutex", "block"}

const (
	maxProfile     = 30 * time.Second
	defaultProfile = 10 * time.Second
	profileTop     = 10
)

// Profile takes one profile and reads it. cpu, mutex and block sample for d
// (10 s when zero, 30 s at most); goroutine, goroutineleak and heap are taken
// at once. Mutex and block profiling are switched on only while sampling, so
// block counts the waits that started in the window; the mutex fraction is
// restored afterwards, and the block rate (which can't be read back) is left at 0.
// One profile runs at a time in the process.
func (in *Instance) Profile(ctx context.Context, kind string, d time.Duration) (ProfileReport, error) {
	if !contains(ProfileKinds, kind) {
		return ProfileReport{}, fmt.Errorf("vis: no %q profile: take one of %s", kind, strings.Join(ProfileKinds, ", "))
	}
	if !profiling.TryLock() {
		return ProfileReport{}, fmt.Errorf("vis: a profile is already being taken; try again when it's done")
	}
	defer profiling.Unlock()
	if d <= 0 {
		d = defaultProfile
	}
	d = min(d, maxProfile)
	var (
		p      *profile.Profile
		err    error
		sample string
		r      = ProfileReport{Kind: kind}
	)
	switch kind {
	case "cpu":
		var buf bytes.Buffer
		if err := pprof.StartCPUProfile(&buf); err != nil {
			return r, fmt.Errorf("vis: can't profile CPU: %w", err)
		}
		d = sleepCtx(ctx, d)
		pprof.StopCPUProfile()
		p, err = profile.Parse(&buf)
		sample, r.Unit, r.Seconds = "cpu", "CPU seconds", d.Seconds()
	case "goroutine", "goroutineleak":
		p, err = lookupProfile(kind)
		sample, r.Unit = "goroutine", "goroutines"
	case "heap":
		p, err = lookupProfile(kind)
		sample, r.Unit = "inuse_space", "MiB in use"
	case "mutex", "block":
		if kind == "mutex" {
			prev := runtime.SetMutexProfileFraction(-1)
			runtime.SetMutexProfileFraction(1) // every event: a rare long hold is what matters, and the window is short
			defer runtime.SetMutexProfileFraction(prev)
		} else {
			runtime.SetBlockProfileRate(int(10 * time.Microsecond)) // every block of 10 µs or more, a share of shorter ones
			defer runtime.SetBlockProfileRate(0)
		}
		var before, after *profile.Profile
		if before, err = lookupProfile(kind); err == nil {
			d = sleepCtx(ctx, d)
			if after, err = lookupProfile(kind); err == nil {
				p, err = diffProfiles(after, before)
			}
		}
		sample, r.Unit, r.Seconds = "delay", "seconds waited", d.Seconds()
	}
	if err != nil {
		return r, fmt.Errorf("vis: reading the %s profile: %w", kind, err)
	}
	count := ""
	if kind == "mutex" || kind == "block" {
		count = "contentions"
	}
	r.Total, r.Top, r.Vis = owners(p, sample, count, scaleFor(kind), kind == "block" || kind == "goroutine")
	r.Summary = profileSummary(r)
	if kind == "heap" { // what the container uses besides, which the profile can't see
		if s := heapBesides(in.Runtime(10*time.Second), r.Total); s != "" {
			r.Summary += " " + s
		}
	}
	return r, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) time.Duration {
	start := time.Now()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
	return time.Since(start)
}

func lookupProfile(name string) (*profile.Profile, error) {
	pp := pprof.Lookup(name)
	if pp == nil && name == "goroutineleak" {
		return nil, errors.New(noLeakProfile)
	}
	if pp == nil {
		return nil, fmt.Errorf("this Go runtime has no %s profile", name)
	}
	var buf bytes.Buffer
	if err := pp.WriteTo(&buf, 0); err != nil {
		return nil, err
	}
	return profile.Parse(&buf)
}

// diffProfiles is what happened between two cumulative profiles.
func diffProfiles(after, before *profile.Profile) (*profile.Profile, error) {
	before = before.Copy()
	before.Scale(-1)
	return profile.Merge([]*profile.Profile{after, before})
}

// scaleFor turns a profile's sample values into the report's unit.
func scaleFor(kind string) float64 {
	switch kind {
	case "cpu", "mutex", "block":
		return 1e-9 // nanoseconds to seconds
	case "heap":
		return 1.0 / (1 << 20)
	}
	return 1
}

// Frames come in three tiers. Runtime frames (the runtime, sync, syscall,
// time) are where a cost lands, never who owns it. The standard library is
// charged to its caller too. Owners are the service's own code (its main module)
// and its dependencies, so a cost points at code the service can change, and
// "via" says what that code called.
func isRuntime(fn string) bool {
	switch pkgOf(fn) {
	case "runtime", "sync", "sync/atomic", "syscall", "time":
		return true
	}
	p := pkgOf(fn)
	return strings.HasPrefix(p, "runtime/") || strings.HasPrefix(p, "internal/") || strings.Contains(p, "/internal/")
}

func isStdlib(fn string) bool {
	p := pkgOf(fn)
	first, _, _ := strings.Cut(p, "/")
	if p == "main" || strings.Contains(first, ".") || isMainModule(p) {
		return false
	}
	return true
}

var mainModule = func() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Path
	}
	return ""
}()

func isMainModule(pkg string) bool {
	m := mainModule
	return m != "" && (pkg == m || pkg == m+"_test" || strings.HasPrefix(pkg, m+"/"))
}

// pkgOf is a function's import path: "compress/flate" for compress/flate.(*compressor).deflate.
func pkgOf(fn string) string {
	slash := strings.LastIndex(fn, "/")
	if dot := strings.Index(fn[slash+1:], "."); dot >= 0 {
		return fn[:slash+1+dot]
	}
	return fn
}

// Stacks entirely inside the runtime are named for what the runtime was doing.
var runtimeRoots = []struct{ name, owner string }{
	{"runtime.gcBgMarkWorker", "the garbage collector"}, {"runtime.gcDrain", "the garbage collector"},
	{"runtime.gcStart", "the garbage collector"}, {"runtime.bgsweep", "the garbage collector"},
	{"runtime.bgscavenge", "the garbage collector"}, {"runtime.gcAssistAlloc", "the garbage collector"},
	{"runtime.gcMarkTermination", "the garbage collector"}, {"runtime.stopTheWorld", "the garbage collector"},
	{"runtime.startTheWorld", "the garbage collector"}, {"runtime.forEachP", "the garbage collector"},
	{"runtime.(*mheap).", "the heap, growing or giving memory back"}, {"runtime.(*pageAlloc).", "the heap, growing or giving memory back"},
	{"runtime.sysUsed", "the heap, growing or giving memory back"}, {"runtime.sysUnused", "the heap, growing or giving memory back"},
	{"runtime/pprof.", "the profiler"},
	{"runtime.schedule", "the scheduler, idle or switching"}, {"runtime.findRunnable", "the scheduler, idle or switching"},
	{"runtime.mstart", "the scheduler, idle or switching"},
}

func runtimeOwner(fs []frame) frame {
	for _, f := range fs {
		for _, r := range runtimeRoots {
			if strings.HasPrefix(f.fn, r.name) {
				return frame{r.owner, ""}
			}
		}
	}
	return frame{"the runtime", ""}
}

type frame struct{ fn, where string }

// vis's own waits: a profile's sampling window, an impact call waiting out a change
var selfFrames map[string]bool

func init() { // not a var initializer: Profile itself reaches selfFrames
	selfFrames = map[string]bool{funcName(sleepCtx): true, funcName((*Instance).Profile): true, funcName((*Instance).WaitImpact): true}
}

func funcName(f any) string { return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name() }

// visPkg is vis's own import path, read from one of its functions.
var visPkg = pkgOf(funcName(sleepCtx))

// isVis says whether fn is vis's own code: the package and the ones under it
// (its integrations, its internals), not the service that imports it.
func isVis(fn string) bool {
	p := pkgOf(fn)
	return p == visPkg || strings.HasPrefix(p, visPkg+"/")
}

func waitingInVis(fs []frame) bool {
	for _, f := range fs {
		if selfFrames[f.fn] {
			return true
		}
	}
	return false
}

// frames lists a sample's stack, innermost first, inlined calls included.
func frames(s *profile.Sample) []frame {
	var out []frame
	for _, loc := range s.Location {
		for _, l := range loc.Line {
			if l.Function != nil {
				out = append(out, frame{l.Function.Name, fmt.Sprintf("%s:%d", filepath.Base(l.Function.Filename), l.Line)})
			}
		}
	}
	return out
}

// owner picks who a stack's cost belongs to and what it called to spend it.
func owner(fs []frame) (own frame, via string, callers []string) {
	i := slices.IndexFunc(fs, func(f frame) bool { return !isRuntime(f.fn) && !isStdlib(f.fn) })
	if i < 0 {
		i = slices.IndexFunc(fs, func(f frame) bool { return !isRuntime(f.fn) }) // only libraries: the outermost library frame below the runtime
	}
	if i < 0 {
		return runtimeOwner(fs), "", nil
	}
	if i > 0 && !strings.HasPrefix(fs[i-1].fn, "runtime.asyncPreempt") && !strings.HasPrefix(fs[i-1].fn, "runtime.morestack") {
		via = fs[i-1].fn // what the owner called: runtime.chanrecv1 rather than runtime.gopark, hash/crc32.ChecksumIEEE
	}
	for _, f := range fs[i+1:] {
		if len(callers) == 2 {
			break
		}
		if !isRuntime(f.fn) && !isStdlib(f.fn) && !strings.Contains(f.fn, ".gowrap") {
			callers = append(callers, f.fn)
		}
	}
	return fs[i], via, callers
}

// owners sums a profile's chosen sample by the code that owns each stack.
// count, when set, names the sample counting events, for Events and Each.
// leaveOutVis drops the stacks vis's own code owns, and returns what they added.
func owners(p *profile.Profile, sample, count string, scale float64, leaveOutVis bool) (float64, []ProfileLine, float64) {
	idx, cidx := -1, -1
	for i, st := range p.SampleType {
		switch st.Type {
		case sample:
			idx = i
		case count:
			cidx = i
		}
	}
	if idx < 0 {
		return 0, nil, 0
	}
	type acc struct {
		ProfileLine
		lines map[string]float64
	}
	byOwner := map[string]*acc{}
	var total, left float64
	for _, s := range p.Sample {
		v := float64(s.Value[idx]) * scale
		fs := frames(s)
		if v <= 0 || len(fs) == 0 || waitingInVis(fs) {
			continue
		}
		own, via, callers := owner(fs)
		if leaveOutVis && isVis(own.fn) {
			left += v
			continue
		}
		total += v
		key := own.fn + "\x00" + via
		l := byOwner[key]
		if l == nil {
			l = &acc{ProfileLine: ProfileLine{Function: own.fn, Via: via, Callers: callers}, lines: map[string]float64{}}
			byOwner[key] = l
		}
		l.Value += v
		l.lines[own.where] += v
		if cidx >= 0 && count != "" {
			l.Events += max(s.Value[cidx], 0)
		}
	}
	var top []ProfileLine
	for _, l := range byOwner {
		if total > 0 {
			l.Share = l.Value / total
		}
		if l.Events > 0 {
			l.Each = l.Value / float64(l.Events)
		}
		for where, v := range l.lines { // the owner's hottest line
			if v > l.lines[l.Where] || l.Where == "" || (v == l.lines[l.Where] && where < l.Where) {
				l.Where = where
			}
		}
		top = append(top, l.ProfileLine)
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Value != top[j].Value {
			return top[i].Value > top[j].Value
		}
		return top[i].Function < top[j].Function
	})
	if len(top) > profileTop {
		if i := longest(top); i >= profileTop { // keep the longest waits in view: a convoy behind one long hold hides it by total
			top[profileTop-1] = top[i]
		}
		top = top[:profileTop]
	}
	return total, top, left
}

// longest is the line whose waits are longest each time, or -1.
func longest(top []ProfileLine) int {
	best := -1
	for i, l := range top {
		if l.Events > 0 && (best < 0 || l.Each > top[best].Each) {
			best = i
		}
	}
	return best
}

func window(seconds float64) string {
	if seconds < 1 {
		return num(math.Round(seconds*10)/10) + "s"
	}
	return short(time.Duration(seconds * float64(time.Second)))
}

func profileSummary(r ProfileReport) string {
	var head, verb string
	switch r.Kind {
	case "cpu":
		head, verb = fmt.Sprintf("%s cores of CPU over %s", num(round2(r.Total/max(r.Seconds, 1e-9))), window(r.Seconds)), "in "
	case "goroutine":
		head, verb = fmt.Sprintf("%.0f goroutines", r.Total), "in "
	case "goroutineleak":
		head, verb = fmt.Sprintf("%.0f goroutines leaked, blocked on something nothing can reach", r.Total), "in "
	case "heap":
		head, verb = fmt.Sprintf("%s MiB in use", num(round2(r.Total))), "allocated in "
	case "mutex":
		head, verb = fmt.Sprintf("Goroutines waited %s s for locks over %s", num(round2(r.Total)), window(r.Seconds)), "held by "
	case "block":
		head, verb = fmt.Sprintf("Goroutines spent %s s blocked over %s", num(round2(r.Total)), window(r.Seconds)), "waiting in "
	}
	// Say what was left out, so a number that looks low isn't a mystery.
	left := ""
	if r.Vis > 0 {
		left = " vis's own goroutines, waiting for work (a dashboard's stream, a rule's next tick), are left out."
	}
	if len(r.Top) == 0 {
		return head + "." + left
	}
	var parts []string
	for _, l := range r.Top[:min(3, len(r.Top))] {
		if l.Share < .05 && len(parts) > 0 {
			break
		}
		s := fmt.Sprintf("%.0f%% %s%s", l.Share*100, verb, l.Function)
		if l.Where != "" {
			s += " (" + l.Where + ")"
		}
		if l.Via != "" {
			s += " via " + l.Via
		}
		if len(l.Callers) > 0 {
			s += ", called from " + l.Callers[0]
		}
		if l.Events > 0 {
			s += ", " + roughMs(l.Each*1000) + " ms a time"
		}
		parts = append(parts, s)
	}
	out := head + ": " + strings.Join(parts, "; ") + "."
	// Go charges a lock wait to the unlock that ended it, so the goroutines queued behind
	// one long hold are charged to each other's short ones: say where the long waits end.
	if i := longest(r.Top); i >= len(parts) && r.Top[i].Each > 3*r.Top[0].Each {
		l := r.Top[i]
		out += fmt.Sprintf(" The longest waits, %s ms a time, end at %s (%s).", roughMs(l.Each*1000), l.Function, l.Where)
	}
	if i := slices.IndexFunc(r.Top, func(l ProfileLine) bool { return !isStdlib(l.Function) && strings.Contains(l.Function, ".") }); i >= len(parts) && i >= 0 && r.Top[i].Share >= .05 {
		l := r.Top[i] // the first of the service's own code, when libraries lead
		out += fmt.Sprintf(" The service's own code first appears at %.0f%%: %s (%s).", l.Share*100, l.Function, l.Where)
	}
	return out + left
}
