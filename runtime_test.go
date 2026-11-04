package vis_test

import (
	"runtime"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

func TestRuntimeSignalsAreSane(t *testing.T) {
	in := vis.NewInstance()
	in.Runtime(time.Second) // first sample
	busy := make(chan struct{})
	for range 4 {
		go func() {
			for {
				select {
				case <-busy:
					return
				default:
					runtime.Gosched()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(busy)
	_ = make([]byte, 8<<20) // some garbage
	runtime.GC()
	r := in.Runtime(time.Second)
	if r.GOMAXPROCS != runtime.GOMAXPROCS(0) {
		t.Fatalf("GOMAXPROCS = %d, want %d", r.GOMAXPROCS, runtime.GOMAXPROCS(0))
	}
	if r.Goroutines < 1 || r.SchedP99 < 0 || r.SchedP99 < r.SchedP50 || r.GCCPU < 0 || r.GCCPU > 1 {
		t.Fatalf("runtime = %+v", r)
	}
	if r.Over <= 0 || r.Over > 2*time.Second {
		t.Fatalf("window covered %v, want the time since the first sample", r.Over)
	}
	if r.HeapGoal == 0 {
		t.Fatalf("heap goal = 0")
	}
}
