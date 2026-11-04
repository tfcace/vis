// Package app is a small service declared with directives, for vis gen's tests.
package app

import (
	"net/http"

	"github.com/tfcace/vis"
)

// vis:knob range=1..16384 default=100 unit=MB uses=memory saves=origin_fetches pull=agent
var CacheSize vis.Int // Memory for cached images.

// vis:knob range=1..100 default=95 uses=cpu pull=agent,rule
// vis:rule name=quality_backoff when="slo.p99 > 400ms" for=30s do=step(-5) cooldown=30s lease=10m
var JPEGQuality vis.Int // JPEG quality of resized images.

// FetchTimeout is how long to wait for an origin.
//
// vis:knob range=100ms..30s default=5s
var FetchTimeout vis.Duration

// vis:signal unit=images/s
var OriginFetches vis.Gauge // Images fetched from origin servers per second.

// vis:signal
var OriginWait vis.Latency // How long origin fetches take, p99.

const imageRoute = "GET /i/{name}"

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(imageRoute, serveImage)
	return vis.HTTP(mux)
}

// vis:slo p99=500ms
func serveImage(w http.ResponseWriter, r *http.Request) {}

// Rules on every kind of measure and action, so the generated init is run
// against each: vis.NewRule checks them when the test package starts.

// vis:knob range=1..512 default=64 pull=rule
// vis:rule name=on_wait when="origin_wait > 2s" do=reset lease=1m
// vis:rule name=on_fetches when="origin_fetches > 1000" do=step(-16) lease=1m max_drift=48
// vis:rule name=on_gc when="gc.cpu > 0.25" do=set(8) lease=1m
// vis:rule name=on_cpu when="cpu.used > 1.5" do=step(-25%) lease=1m
var Batch vis.Int // Most misses fetched in one origin request.

// vis:knob range=1ms..50ms default=5ms pull=agent,rule
// vis:rule name=ease when="sched.p99 > SchedLimit" do=step(-1ms) lease=1m max_drift=3ms deadband=500us
var Target vis.Duration // A latency target rules can tune.

// vis:knob range=1ms..100ms default=20ms pull=agent
var SchedLimit vis.Duration // The scheduler latency above which ease acts.
