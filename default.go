package vis

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
)

// The default instance: one per process, the way slog.Default and
// http.DefaultServeMux are. vis.Start configures and serves it; vis.HTTP and
// a knob's Bind and Now reach it. It is global state: everything declared
// belongs to the process, Start runs once, and tests that use it share it.
// A test that needs its own state uses NewInstance (or vistest.Start).
var (
	defaultOnce    sync.Once
	defaultIn      *Instance
	defaultStarted atomic.Bool
	defaultCreated atomic.Bool
)

// DefaultInstance returns the process's default instance, creating it on first use.
func DefaultInstance() *Instance {
	defaultOnce.Do(func() {
		defaultCreated.Store(true)
		defaultIn = NewInstance()
	})
	return defaultIn
}

// StartOption is an option for Start: an InstanceOption, such as Service or
// SLO, or a ServeOption, such as UIAt or MCPAt.
type StartOption interface{ startOption() }

func (InstanceOption) startOption() {}
func (ServeOption) startOption()    {}

// ErrStarted is returned by a second call to Start.
var ErrStarted = errors.New("vis: Start already ran: the default instance starts once per process")

// Start sets up the default instance with opts and serves it until ctx ends:
// the control UI on localhost:7071 and MCP on localhost:7070, unless options
// or the environment say otherwise (see Instance.Serve), and runs the rules
// declared with NewRule. It returns once both are listening, so a bad address fails here. Call it once, in main, before
// serving requests; a second call returns ErrStarted.
func Start(ctx context.Context, opts ...StartOption) error {
	if !defaultStarted.CompareAndSwap(false, true) {
		return ErrStarted
	}
	in := DefaultInstance()
	var serve []ServeOption
	in.mu.Lock()
	for _, o := range opts {
		switch o := o.(type) {
		case InstanceOption:
			o(in)
		case ServeOption:
			serve = append(serve, o)
		}
	}
	in.mu.Unlock()
	if os.Getenv("VIS_STATE_DIR") == "" { // a service's last word, so its next process can say why it died; an option still wins
		serve = append([]ServeOption{StateDir(os.TempDir())}, serve...)
	}
	if _, err := in.Serve(ctx, serve...); err != nil {
		defaultStarted.Store(false) // nothing is serving: Start may be tried again
		return err
	}
	in.RunRules(ctx)
	return nil
}

// Stop ends what Start started and tells the fleet this instance is leaving,
// returning once it has. Defer it in main after Start. See Instance.Stop.
func Stop() { DefaultInstance().Stop() }

// HTTP wraps h with the default instance. See Instance.HTTP.
func HTTP(h http.Handler, opts ...HTTPOption) http.Handler { return DefaultInstance().HTTP(h, opts...) }
