package vis_test

import (
	"context"
	"hash/crc32"
	"net/http"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

//go:noinline
func burnCPU(stop <-chan struct{}) {
	x := 0
	for {
		select {
		case <-stop:
			return
		default:
			for i := range 100000 {
				x += i * i
			}
		}
	}
}

//go:noinline
func checksumLoop(stop <-chan struct{}) {
	buf := make([]byte, 1<<20)
	for {
		select {
		case <-stop:
			return
		default:
			crc32.ChecksumIEEE(buf)
		}
	}
}

var keep [][]byte

//go:noinline
func allocate(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
			for range 1000 {
				keep = append(keep[:0], make([]byte, 64<<10))
			}
		}
	}
}

//go:noinline
func parkForever(c <-chan struct{}) { <-c }

//go:noinline
func holdLock(mu *sync.Mutex, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
			mu.Lock()
			time.Sleep(5 * time.Millisecond)
			mu.Unlock()
		}
	}
}

//go:noinline
func waitOnChannel(c <-chan struct{}) { <-c }

func topOf(t *testing.T, r vis.ProfileReport) vis.ProfileLine {
	t.Helper()
	t.Log(r.Summary)
	if len(r.Top) == 0 {
		t.Fatalf("%s profile is empty: %+v", r.Kind, r)
	}
	return r.Top[0]
}

// A CPU profile names the function burning the CPU, not the runtime under it.
func TestProfileCPUNamesTheBurner(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	go burnCPU(stop)
	r, err := vis.NewInstance().Profile(context.Background(), "cpu", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	top := topOf(t, r)
	if !strings.HasSuffix(top.Function, "burnCPU") || top.Share < .5 {
		t.Fatalf("top = %+v, want burnCPU with most of the CPU; summary %q", top, r.Summary)
	}
	if !strings.Contains(r.Summary, "burnCPU") || !strings.Contains(top.Where, "profile_test.go:") {
		t.Fatalf("summary %q, where %q", r.Summary, top.Where)
	}
}

// CPU spent in the standard library is charged to the service code that called it.
func TestProfileChargesTheStandardLibraryToItsCaller(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	go checksumLoop(stop)
	r, err := vis.NewInstance().Profile(context.Background(), "cpu", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if top := topOf(t, r); !strings.HasSuffix(top.Function, "checksumLoop") || !strings.HasPrefix(top.Via, "hash/crc32.") {
		t.Fatalf("top = %+v, want checksumLoop via hash/crc32", top)
	}
}

// Work the runtime does on its own is named for what it was: the GC, not runtime.madvise.
func TestProfileNamesTheGarbageCollector(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	go allocate(stop)
	r, err := vis.NewInstance().Profile(context.Background(), "cpu", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(r.Summary)
	if !slices.ContainsFunc(r.Top, func(l vis.ProfileLine) bool { return l.Function == "the garbage collector" }) {
		t.Fatalf("top = %+v, want the garbage collector among them", r.Top)
	}
	for _, l := range r.Top {
		if strings.HasPrefix(l.Function, "runtime.") && l.Share > .05 {
			t.Fatalf("%+v: a runtime frame owns %0.f%%", l, l.Share*100)
		}
	}
}

// A goroutine profile counts goroutines by the code that parked them.
func TestProfileGoroutinesCountsWhereTheyWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := make(chan struct{})
		defer close(c)
		for range 500 {
			go parkForever(c)
		}
		synctest.Wait() // all parked
		r, err := vis.NewInstance().Profile(context.Background(), "goroutine", 0)
		if err != nil {
			t.Fatal(err)
		}
		top := topOf(t, r)
		if !strings.HasSuffix(top.Function, "parkForever") || top.Value < 500 || !strings.Contains(top.Via, "chanrecv") {
			t.Fatalf("top = %+v, want 500 goroutines parked in parkForever on a channel", top)
		}
	})
}

// A mutex profile names the code holding the lock others wait for.
// vis's own goroutines (here the dashboard's event stream, waiting for the
// next change) cost the service nothing while parked: a profile of where
// goroutines wait leaves them out, and says it did.
func TestProfileLeavesOutVisOwnWaits(t *testing.T) {
	c := make(chan struct{})
	defer close(c)
	for range 50 {
		go parkForever(c)
	}
	in := vis.NewInstance()
	srv := uiServer(t, in)
	res, err := http.Get(srv.URL + "/api/events") // a dashboard, open
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	time.Sleep(200 * time.Millisecond) // its stream is parked, waiting for news
	r, err := in.Profile(context.Background(), "goroutine", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range r.Top {
		if strings.HasPrefix(l.Function, "github.com/tfcace/vis.") {
			t.Fatalf("top has vis's own %s: %+v", l.Function, r.Top)
		}
	}
	if !strings.HasSuffix(topOf(t, r).Function, "parkForever") || !strings.Contains(r.Summary, "vis's own") {
		t.Fatalf("summary = %q, want parkForever first and a word that vis's own goroutines were left out", r.Summary)
	}
}

func TestProfileMutexNamesTheHolder(t *testing.T) {
	var mu sync.Mutex
	stop := make(chan struct{})
	defer close(stop)
	go holdLock(&mu, stop)
	for range 4 {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					mu.Lock()
					mu.Unlock() //nolint:staticcheck // contention is the point
				}
			}
		}()
	}
	r, err := vis.NewInstance().Profile(context.Background(), "mutex", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	top := topOf(t, r)
	if !strings.HasSuffix(top.Function, "holdLock") {
		t.Fatalf("top = %+v, want holdLock, which holds the lock; %q", top, r.Summary)
	}
	// a hold of 5 ms makes each wait ending at it a few ms: the rare long hold stands out by this
	if top.Events == 0 || top.Each < .001 || !strings.Contains(r.Summary, "ms a time") {
		t.Fatalf("top = %+v, summary %q; want the waits counted and their length each time", top, r.Summary)
	}
}

// A block profile names the code that waited, and how long in all.
func TestProfileBlockNamesTheWaiter(t *testing.T) {
	c := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond) // block counts the waits that start while it samples
		for range 10 {
			go waitOnChannel(c)
		}
		time.Sleep(200 * time.Millisecond)
		close(c)
	}()
	r, err := vis.NewInstance().Profile(context.Background(), "block", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if top := topOf(t, r); !strings.HasSuffix(top.Function, "waitOnChannel") {
		t.Fatalf("top = %+v, want waitOnChannel; %q", top, r.Summary)
	}
}

func TestProfileRefusesWhatItCannotDo(t *testing.T) {
	in := vis.NewInstance()
	if _, err := in.Profile(context.Background(), "everything", time.Second); err == nil || !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("unknown kind: %v, want an error naming the kinds", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		in.Profile(context.Background(), "cpu", 500*time.Millisecond)
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := in.Profile(context.Background(), "cpu", time.Second); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("a second profile at once: %v, want it refused", err)
	}
	<-done
}

// Go 1.26 has the goroutineleak profile only behind GOEXPERIMENT, so where
// the build lacks it, the refusal and the tool's description say how to get
// it instead of leaving an agent to try it and wonder.
func TestProfileSaysHowToGetTheLeakProfile(t *testing.T) {
	if pprof.Lookup("goroutineleak") != nil {
		t.Skip("this runtime has the goroutineleak profile")
	}
	in := vis.NewInstance()
	if _, err := in.Profile(context.Background(), "goroutineleak", 0); err == nil || !strings.Contains(err.Error(), "GOEXPERIMENT=goroutineleakprofile") {
		t.Fatalf("goroutineleak on this runtime: %v, want it to say how to get the profile", err)
	}
	tools, err := connect(t, in, "").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "profile" && !strings.Contains(tool.Description, "GOEXPERIMENT=goroutineleakprofile") {
			t.Fatalf("profile's description doesn't say this build lacks goroutineleak:\n%s", tool.Description)
		}
	}
}
