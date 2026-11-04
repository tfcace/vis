// Command cacheproxy is a test vehicle for vis: an HTTP cache in front of a
// slow, simulated origin, with knobs a person, an agent or a reflex can turn.
// It is the service from the UI mockup, running for real.
//
//	go run ./examples/cacheproxy                # the proxy on :8790, MCP on :7070, the control UI on :7071
//	go run ./examples/cacheproxy/loadgen        # load, from outside the process
//	curl localhost:7071/api/state               # what the UI shows, for scripts too
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/tfcace/vis"
)

//go:generate go tool vis gen

// The knobs, as reviewed code: vis gen declares them from these directives
// (vis_gen.go). Values live in the instance.
//
// The descriptions say what a knob does, the way its developer would, and not
// what it costs or when to turn it: the evals measure whether vis helps an
// on-call agent find that out, so the answer can't be in the text.
var (
	// vis:knob range=1..10000 default=1000 unit=MiB pull=agent,rule uses=memory
	CacheSize vis.Int // Memory for cached values.

	// vis:knob range=1..512 default=64 unit=items pull=agent,rule
	// vis:rule name=batch_backoff when="sched.p99 > SchedTarget" for=3s do=step(-25%) cooldown=10s lease=60s deadband=500us
	BatchSize vis.Int // Most cache misses fetched from the origin in one request.

	// vis:knob range=1ms..50ms default=5ms pull=agent
	SchedTarget vis.Duration // Threshold for the batch_backoff rule: Go scheduler latency p99 above this makes the rule lower batch_size.

	// vis:knob range=-4..8 default=0 saves=cpu
	LogLevel vis.Int // slog level: -4 debug, 0 info, 4 warn, 8 error.

	// vis:knob range=0..10000 default=10000 unit=rps pull=agent uses=cpu,origin_keys_per_s
	ScanLimit vis.Int // Requests per second admitted from the scan tenant (X-Tenant: scan); the rest get 429.

	// vis:knob range=1..9 default=9 pull=agent uses=cpu saves=response_kb
	CompressLevel vis.Int // Response compression level (flate), 1 to 9: higher compresses smaller.
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8790", "the proxy's listen address")
	mcpAddr := flag.String("mcp", "127.0.0.1:7070", "MCP listen address, or VIS_LISTEN; act tools need VIS_TOKENS (token=kind:id[:act],...)")
	uiAddr := flag.String("ui", "127.0.0.1:7071", "control UI listen address, or VIS_UI_LISTEN: keep it on localhost and reach it through a port-forward")
	faultsAddr := flag.String("faults", "", "fault-injection listen address for experiments; empty means none")
	faultsOn := flag.String("faults-on", "", "faults to start at boot, comma-separated, as a bug in the code would be on after a restart; needs -faults")
	realValues := flag.Bool("real-values", false, "cache entries hold real memory, 128 KiB each, so cache_size's MiB are used; for memory experiments")
	flag.Parse()

	level := new(slog.LevelVar)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// SIGTERM too: docker stop and Kubernetes send it, and ending ctx is how vis
	// tells the fleet this replica is leaving rather than gone wrong.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The default instance: MCP for agents and the control UI for people, with
	// VIS_TOKENS, VIS_UI_USER and VIS_UI_HOSTS from the environment.
	opts := []vis.StartOption{vis.WithLogger(logger), vis.Service("cacheproxy")}
	flag.Visit(func(f *flag.Flag) { // an address given on the command line wins; otherwise the environment, then vis's defaults
		switch f.Name {
		case "mcp":
			opts = append(opts, vis.MCPAt(*mcpAddr))
		case "ui":
			opts = append(opts, vis.UIAt(*uiAddr))
		}
	})
	if err := vis.Start(ctx, opts...); err != nil {
		log.Fatal(err)
	}
	defer vis.Stop() // tells the fleet this replica is leaving before the process exits
	LogLevel.Bind(func(n int) { level.Set(slog.Level(n)) })
	app := newApp(vis.DefaultInstance())
	app.cache.real = *realValues
	if *faultsAddr != "" {
		ad := newAdmin(app)
		if err := ad.serve(ctx, *faultsAddr); err != nil {
			log.Fatal(err)
		}
		for _, name := range strings.Split(*faultsOn, ",") {
			if slices.Contains(faultNames, name) {
				ad.start(name)
			}
		}
	}
	go app.run(ctx)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: app.routes()}
	go func() { <-ctx.Done(); srv.Close() }()
	logger.Info("cacheproxy listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// app wires the proxy and the reflex to one vis instance.
type app struct {
	in     *vis.Instance
	cache  *lru
	origin *origin
	batch  *batcher
	stats  *stats
	ten    *tenants

	// background workers' state
	watchClients atomic.Bool
	quit         chan struct{}
	ringHead     atomic.Uint64
}

// newApp builds the proxy on in: the default instance in main, one of their own in tests.
func newApp(in *vis.Instance) *app {
	a := &app{in: in, origin: newOrigin(), stats: newStats(), quit: make(chan struct{})}
	a.cache = newLRU(0)
	CacheSize.BindIn(in, func(n int) { a.cache.resize(entriesFor(n)) })
	a.batch = newBatcher(in, a.origin, a.stats)
	a.ten = newTenants(in)
	return a
}

// entriesFor turns MiB into cache entries; the simulated values are 128 KiB each.
func entriesFor(mib int) int { return mib * 8 }

func (a *app) run(ctx context.Context) {
	go a.batch.run(ctx)
	a.in.RunRules(ctx) // vis.Start already ran them on the default instance; this is for tests' own
	a.stats.run(ctx)
}

// routes is the whole HTTP surface, wrapped once by vis: every request carries
// a snapshot and is timed under its ServeMux pattern.
func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k/{key}", a.serveKey)
	return a.in.HTTP(mux)
}

// keyRoute is the proxy's route as vis names it.
const keyRoute = "GET /k/{key}"
