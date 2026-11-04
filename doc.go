// Package vis gives a Go service runtime knobs: typed, bounded settings that
// people, AI agents and the service's own rules can change while it runs.
// Each change carries a reason, lasts for an agreed duration, and is audited.
// vis also measures the service: each route's latency against its objective,
// the Go runtime, and what every change did. People see it all in a live
// control UI; agents reach it over MCP.
//
// # Declaring with directives
//
// The easiest way to declare is to annotate: put // vis: comments above your
// own declarations, and go generate writes the vis calls into vis_gen.go, an
// ordinary Go file you check in and can read.
//
//	//go:generate go tool vis gen
//
//	// vis:knob range=1..16384 default=100 unit=MB uses=memory pull=agent
//	var CacheSize vis.Int // Memory for cached images.
//
//	// vis:knob range=1..100 default=95 uses=cpu pull=agent,rule
//	// vis:rule name=quality_backoff when="slo.p99 > 400ms" for=30s do=step(-5) cooldown=30s lease=10m
//	var JPEGQuality vis.Int // JPEG quality of resized images.
//
//	// vis:knob range=100ms..30s default=5s
//	var FetchTimeout vis.Duration // How long to wait for an origin.
//
//	// vis:knob pull=agent
//	var Logging vis.LogLevel // What the service logs.
//
//	// vis:signal unit=images/s
//	var OriginFetches vis.Gauge // Images fetched from origin servers per second.
//
//	// vis:slo p99=500ms
//	func (p *Proxy) serveImage(w http.ResponseWriter, r *http.Request) { ... }
//
// Add the tool to your module once with go get -tool github.com/tfcace/vis/cmd/vis,
// then run go generate.
// A knob's name comes from its variable (CacheSize is cache_size; name=
// overrides it) and its description from its comment; env=MAX_LOADED lets
// that environment variable set its default, as configuration read from the
// environment did before (a bad value keeps the default, with a warning). An
// slo takes its route from where the package registers the handler, with
// HandleFunc or Handle, or as a gin or echo route; give route="GET /path"
// when vis gen can't tell, and it says so. A
// service that routes on its own, with no ServeMux, gives route= on every
// slo and names its routes with RouteName (see Routes); writing those calls
// by hand is as easy there.
// A vis.LogLevel knob needs neither range= nor default= (it runs DEBUG..ERROR
// from INFO), takes levels by name (range=trace..error default=warn,
// do=set(debug); TRACE is DEBUG-4, which slog leaves unnamed), and is a slog.Leveler: give it to your handler,
// slog.HandlerOptions{Level: Logging}, and changing the knob changes what
// the service logs, with no LevelVar in between. Build that handler in main,
// not in a package-level variable, which vis gen's init hasn't declared yet.
// Mistakes are reported at their line, with how to fix them. The keys:
//
//	knob    (above a vis.Int, vis.Duration or vis.LogLevel var)
//	        range=lo..hi default= special=v:"meaning" unit= pull=agent,rule uses= saves= env= name= about=
//	signal  (above a vis.Gauge or vis.Latency var) unit= name= about=
//	slo     (above a handler func) p99= route=
//	rule    (below a knob's directive) when="<measure> > <threshold>" do=step(N)|step(P%)|set(V)|reset
//	        for= cooldown= lease= deadband= max_drift= stall= mode=shadow every= name=
//
// A rule watches sched.p99, gc.cpu, cpu.throttled, cpu.used, goroutines,
// slo.p99 (the package's one objective), a route by name, as in
// slo(POST /api/generate).p99, which may be served by another package, or one
// of the package's signals; its threshold is a
// duration, a number, or one of the package's knobs. Its action steps the knob (by a duration, for a
// Duration knob: step(-1ms)), sets it to one value, or resets it, ending
// what rules set on it so it falls back to the value below them: one rule can
// undo another's. Each value must fit the knob's range. Costs (uses, saves)
// are cpu, memory, or a signal. vis gen registers everything in an init function, so don't copy a
// knob into another package-level variable: read it where you use it.
//
// Check it, in CI or before a commit, with go tool vis vet ./...: it
// reports the mistakes that compile and run but are wrong, each with how to
// fix it. A directive vis gen can't read; a vis_gen.go out of date with the
// directives; a knob read during package initialization, or copied before vis
// gen's init declares it; a rule whose knob doesn't let rules change it,
// whose threshold is in the wrong unit for what it watches, or whose action
// doesn't fit its knob's range; and an SLO naming
// a route no handler in its package is registered under.
//
// To review a change to what the service declares, run go tool vis diff
// [ref]: it lists the knobs, signals, objectives and rules added, removed or
// changed since a git revision (HEAD by default), marking with ! each change
// that gives automation more room or loosens an objective: a new writer, a
// wider range, a new rule, a longer lease, a slower p99, a removed objective.
// It reads hand-written calls and directives alike. With -check it exits 1
// when there is such a change, so CI can ask for a review.
//
// The steps below show the calls the directives generate, which you can also
// write by hand where you can't generate.
//
// # Adopting vis in four steps
//
// 1. Declare knobs as package-level variables, next to the code they tune.
// A declaration is reviewed code: its range, default and unit can't change
// at runtime, only its value.
//
//	var (
//		CacheSize = vis.NewInt("cache_size", vis.Range(1, 16384), vis.Default(100), vis.Unit("MB"),
//			vis.Uses(vis.Memory), vis.Pull(vis.Agent),
//			vis.About("Memory for cached images."))
//		JPEGQuality = vis.NewInt("jpeg_quality", vis.Range(1, 100), vis.Default(95),
//			vis.Uses(vis.CPU), vis.Pull(vis.Agent, vis.Rule),
//			vis.About("JPEG quality of resized images."))
//		FetchTimeout = vis.NewDuration("fetch_timeout",
//			vis.Range(100*time.Millisecond, 30*time.Second), vis.Default(5*time.Second))
//	)
//
// About is what people and agents read about the knob. Pull says which
// automated writers may change it, agents and rules; people always may
// (pull=people,agent in a directive says so, for whoever reads it).
// Uses and Saves are optional: they say what turning the knob up costs and
// what it saves, each either CPU or Memory, which vis measures itself, or one
// of your own gauges (step 4): bytes sent, say, as a bytes_out_per_s gauge. When the service runs short of CPU or memory,
// the UI and agents are told which knobs use it and how far up each one is,
// so whoever is on call knows what to turn down without reading your code.
//
// 2. Read them where the work happens. In a request, read the knob from the
// request's context: every knob a request reads comes from one snapshot,
// taken when the request arrived, so a request never sees half of a change.
//
//	q := JPEGQuality.From(r.Context())
//
// For something you build from a knob, such as a cache whose size is a knob,
// use Bind. It calls your function once right away with the knob's current
// value, then again each time someone changes the knob, so the cache is
// created at today's size and resized the moment anyone changes it:
//
//	CacheSize.Bind(func(mb int) { cache.Resize(mb * 1_000_000) }) // your cache's own resize
//
// Anywhere else, such as a background worker, FetchTimeout.Now() is the
// value right now.
//
// 3. Declare objectives beside your handlers, then start vis. An objective
// (SLO) says how fast a route must be: here, 99% of requests to it finish
// within 50 ms, measured inside the service. The route is named as
// http.ServeMux registered it, so share one constant:
//
//	const keyRoute = "GET /k/{key}"
//
//	var KeySLO = vis.NewSLO(keyRoute, vis.P99(50*time.Millisecond))
//
//	mux.HandleFunc(keyRoute, serveKey)
//
// The UI's headline, agents, impact reports and rules judge the service by
// its first declared objective. (The SLO option gives a single instance an
// objective of its own, as tests do; a service declares its objectives with
// NewSLO.) Then, in main:
//
//	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	if err := vis.Start(ctx, vis.Service("imageproxy")); err != nil {
//		log.Fatal(err)
//	}
//	defer vis.Stop()
//	srv := &http.Server{Addr: *addr, Handler: vis.HTTP(mux)}
//	go func() {
//		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
//			log.Fatal(err)
//		}
//	}()
//	<-ctx.Done()                       // Ctrl-C, or SIGTERM from docker stop or Kubernetes
//	srv.Shutdown(context.Background()) // the requests in flight finish, then Stop runs
//
// Keep the last two lines. Catching a signal turns off Go's own exit on it,
// so a main that serves until it is killed would ignore Ctrl-C and SIGTERM,
// and a deploy would wait on it until it is killed. Ending on ctx is also
// what lets vis tell a fleet this instance is leaving (see Fleets).
//
// Start serves the control UI on localhost:7071 and MCP on localhost:7070,
// and runs the rules you declared. Bind, Now and SetDefault may run before
// Start: they use the same default instance, and Start's options still apply. HTTP times every request under its route.
// Open http://localhost:7071 to see and turn the knobs. That is a working
// adoption; the rest is optional.
//
// 4. Publish what only your service knows, and teach it a reflex. A gauge is
// a number your service computes that people, agents and rules should see.
// Declare it like a knob and Set it wherever your code already computes it:
//
//	var HitShare = vis.NewGauge("cache_hit_share",
//		vis.About("Share of images served from the cache over the last 10 s, 0 to 1."))
//
//	for range time.Tick(10 * time.Second) { // the stats loop that already counts hits
//		served, hits := p.takeCounts()
//		HitShare.Set(float64(hits) / float64(max(served, 1)))
//	}
//
// NewLatency is a gauge of a time.Duration, which a rule watches with a
// latency threshold; NewGaugeFunc computes its value each time it is read.
//
// A rule is a reflex compiled into the service: when what it watches stays
// over its threshold, it steps a knob, each step lasting an agreed duration.
//
//	var QualityBackoff = vis.NewRule(vis.RuleSpec{
//		Name:  "quality_backoff",
//		Knob:  JPEGQuality, Step: -5, // in the knob's own unit
//		Watch: vis.RouteP99("GET /{options}/{remote}"),
//		Over:  vis.Fixed(400 * time.Millisecond), // or a Duration knob, so people can tune it
//		For:   30 * time.Second, Cooldown: 30 * time.Second, Lease: 10 * time.Minute,
//	})
//
// It lowers the quality by 5 every 30 s while the route's p99 is over 400 ms,
// each step lasting 10 minutes. A rule moves its knob at most MaxDrift from
// the value below it, by default four steps, so this one never takes the
// quality below 75; it says so when it starts, in the UI and over MCP. It
// stops by itself if its steps don't move what it watches. If they do but the
// objective gets worse, or stays broken without coming closer (by a quarter
// of the way to its target, from where the step before left it) for Stall
// steps in a row, vis undoes its change and stops it, saying which. A step
// after which 10% more of the objective's requests fail (5xx) is stopped the
// same way, however much p99 improved: a request that fails fast is no win.
// A refused request (429) is counted apart and reported beside p99, in impact
// reports too, but doesn't stop a rule, since shedding load can be the point.
// NewRule
// checks the rule when it is declared and panics saying what to fix: a knob
// without Pull(Rule), a threshold in the wrong unit for what it watches, no
// step.
//
// A quality steps by a fixed amount. A size, a count or a concurrency limit
// responds to ratios, so step it by a share of its value instead: Do:
// vis.StepPercent(-25) (do=step(-25%)) takes a batch of 64 to 48, 36, 27 and
// 20, where a fixed step of -16 would take 16 to 1 in one step. vis vet points
// out a fixed step that cuts its knob by more than half.
//
// A Duration knob's Step and MaxDrift count nanoseconds, so write them as
// durations: Step: int64(-time.Millisecond). Instead of a Step, Do takes
// vis.StepPercent(p), vis.SetTo(v) or vis.Reset().
//
// To time part of a request, wrap it in a probe: defer vis.Probe(ctx, "origin")().
//
// # The words vis uses
//
//   - Knob: a declared setting with a range, a default and a unit.
//   - Writer: who changes a knob: a person, an agent, a rule, or a fleet rollout.
//   - Layer: each kind of writer has its own slot on every knob, and the
//     highest one that is set wins: a person's pin over a rule, over an agent,
//     over a fleet rollout, over the default. When a value ends, the knob
//     falls back to the next one down. Instance.Stack lists them.
//   - Lease: the agreed duration an automated writer's value lasts. Agents
//     and rules must give one, so nothing they set outlives its reason.
//   - Change: one or more knobs set together, with a reason. Every change is
//     audited and measured: 30 s later vis reports what moved.
//   - Snapshot: every knob's value at one moment. A request reads one
//     snapshot from start to end.
//   - SLO: a route's objective, such as p99 under 50 ms.
//   - Gauge (signal): a number only your service knows, published to people,
//     agents and rules.
//   - Rule: a compiled-in reflex that steps a knob while what it watches is too high.
//   - Cost (Uses, Saves): what turning a knob up spends or saves: CPU,
//     Memory, or one of your gauges.
//
// # Routes
//
// vis times every request under its route: the http.ServeMux pattern that
// served it, such as "GET /k/{key}". A handler that routes on its own leaves
// the pattern empty, and vis counts its requests as "unmatched". Name the
// routes yourself with RouteName, and give the SLO the same name:
//
//	vis.HTTP(proxy, vis.RouteName(func(r *http.Request) string {
//		if r.URL.Path == "/health-check" {
//			return "GET /health-check"
//		}
//		return imageRoute
//	}))
//
// With directives, put the slo above the func that does that route's work,
// even though nothing registers it, and give the route as RouteName returns
// it (route= is a string, so it repeats the constant):
//
//	// vis:slo p99=500ms route="GET /{options}/{remote}"
//	func (p *Proxy) serveImage(w http.ResponseWriter, r *http.Request) { ... }
//
// A rule in the same package reads it as slo.p99; from another package, such
// as main, name the route: when="slo(GET /{options}/{remote}).p99 > 500ms".
// What vis times is what RouteName returns, so a route= that doesn't match it
// is an objective no request counts toward: vis warns about it once requests
// come in.
//
// With gin or echo, use their middleware instead of vis.HTTP: it names each
// route as the router does, "POST /api/generate" or "GET /k/:key", and a
// // vis:slo above a handler the router registers finds that route itself.
// Each is its own module, so vis doesn't depend on either:
//
//	r.Use(visgin.Middleware())       // github.com/tfcace/vis/integrations/visgin
//	e.Use(visecho.Middleware())      // github.com/tfcace/vis/integrations/visecho
//
// If an SLO's route never serves while others do, vis warns in the UI and
// over MCP, naming the routes that do.
//
// # Reading knobs below a cache, or outside a request
//
// From reads the snapshot HTTP puts in the request's context. Without one, in
// code vis.HTTP didn't serve, it reads the knob's value now in the default
// instance, so nothing panics; but that request loses its consistent
// snapshot, and probably its timing, so vis reports the reads, per knob, with
// how to fix them: in the UI, over MCP's describe, and once in the log. A test
// that calls a handler directly works the same way; to read a test's own
// instance, wrap the handler with in.HTTP.
//
// Code the request context doesn't reach (a RoundTripper, a worker, a cache)
// should get the value from the request that started the work: read it once
// in the handler and pass it down. If the value changes what you cache, as an
// image's quality does, make it part of the cache key, or a change will keep
// serving what was cached before it.
//
// Keep vis in your service, not in libraries it imports: declare knobs,
// objectives, gauges and rules in the service, and pass values to a library
// through its own options or hooks, as you would any setting.
//
// # Declaring knobs from flags
//
// Declare knobs at package level. To keep an existing flag as a knob's
// default, call SetDefault after flag.Parse and before Start, Bind or Now:
//
//	flag.Parse()
//	if err := CacheSize.SetDefault(*cacheMB); err != nil {
//		log.Fatal(err) // outside the knob's range, which the error names
//	}
//
// Range returns a knob's declared range, so a flag's own check needn't repeat it.
//
// The knob's env= variable (vis.Env) beats SetDefault: what the operator set
// for this process wins over the code's default. A bad value in it falls back
// to the default SetDefault gave, and vis says so.
//
// When a setting's 0 means "work it out", as "three per GPU" does, don't make
// 0 a knob value: people, agents and rules would read 0 and a rule would step
// from it, while the service runs with three. Work the value out at startup,
// start the range at 1, and pass the value to SetDefault; default= in the
// directive is the fallback for when it can't be worked out:
//
//	// vis:knob range=1..64 default=3 env=MAX_LOADED pull=agent,rule
//	var MaxLoaded vis.Int // Models kept loaded at once. Starts at three per GPU.
//
//	gpus := discoverGPUs()
//	if err := MaxLoaded.SetDefault(3 * max(len(gpus), 1)); err != nil {
//		log.Fatal(err)
//	}
//	vis.Start(ctx) // after the code that knows the value
//
// Every value is then what the service runs with: the UI and agents see 3,
// and a rule's step(1) goes to 4.
//
// When a setting's 0 means none instead, as imageproxy's -timeout 0 means no
// limit, keep it: declare 0 as the knob's special value, a value outside the
// range that the service gives its own meaning, and start the range past it:
//
//	// vis:knob range=1s..5m default=0s special=0s:"no limit" pull=agent,rule
//	// vis:rule name=fail_fast when="slo.p99 > 500ms" do=step(-1s) lease=10m mode=shadow
//	var RequestTimeout vis.Duration // Time limit for a request.
//
//	if err := RequestTimeout.SetDefault(*timeout); err != nil { // -timeout 0 is still no limit
//		log.Fatal(err)
//	}
//
// The code reads it as before (From returns 0, so if timeout > 0 still
// works), and people and agents see it, and set it, by name: "no limit".
// vis never calculates with it, so it needn't know which side of the range
// "no limit" is on. A rule never moves the knob into it or out of it: from
// 3s, fail_fast steps down to 1s and stops there; at no limit it stands
// aside and says why. (Special(time.Duration(0), "no limit") without
// directives.) Don't put the 0 inside the range instead: a rule stepping
// down from 1s would land on it and turn the limit off.
//
// # Units
//
// An Int knob's unit is a label (Unit("MB")); its range, default and a rule's
// steps count in it. A Duration knob's range and default are time.Durations,
// so a bare 10 can't mean 10 ns. A LogLevel knob's are slog.Levels, which
// people and agents see by name: TRACE, DEBUG, INFO, WARN, ERROR. A rule's threshold and dead band are Levels:
// vis.Fixed(400*time.Millisecond) or a Duration knob against a latency,
// vis.Fixed(0.9) against a gauge. NewRule checks they match.
//
// # The control UI and its JSON API
//
// The UI is a page at / and a JSON API under /api/, which scripts can use too.
// It acts as one person (VIS_UI_USER, by default the OS user), so keep it on
// localhost.
//
//	curl localhost:7071/api/state   # what the UI shows: knobs, SLOs, gauges, rules, stories
//	curl -X POST localhost:7071/api/apply \
//	  -d '{"set": {"jpeg_quality": 40}, "for": "10m", "reason": "testing quality"}'
//	curl -X POST localhost:7071/api/clear -d '{"knob": "jpeg_quality", "reason": "done"}'
//
// "for" is a Go duration; without it, a person's value has no end. A
// person's value is on the pin layer either way. In /api/state, "slo" is the
// main objective, the one the headline judges, and "slos" lists them all.
//
// # MCP for agents
//
// MCP is served at / on localhost:7070. Without VIS_TOKENS, anyone who can
// reach it may read, and no one may act. With it, every request needs one of
// its tokens, and only tokens granted act may act:
// VIS_TOKENS=token=agent:oncall:act. Responses are MCP's event streams, so use
// an MCP client rather than plain curl. An agent's client config:
//
//	{"mcpServers": {"vis": {"type": "http", "url": "http://127.0.0.1:7070",
//	  "headers": {"Authorization": "Bearer <token>"}}}}
//
// # Fleets
//
// Instances of one service that share a cluster key form a fleet: give each
// the same VIS_CLUSTER_KEY and a seed to join through, VIS_SEEDS=cp-1:7946 (a
// host:port, several, or a DNS name that resolves to them; an instance may
// list itself). They find each other by gossip on a port of their own,
// VIS_PEER_LISTEN (:7946), which the other instances must reach; every
// message is signed with the key, and instances of other services or keys
// are refused, with the reason in Warnings. Nothing else is configured:
// members, their builds and whether they're alive come from the fleet itself.
//
// Any instance then answers for all of them. MCP's fleet tool compares the
// members (each objective's p99 across the fleet from merged histograms, CPU
// and throttling, the members that stand out and what's different about
// them: a CPU limit, a build, a knob), and every other tool takes instance,
// a member's name, and runs there under the caller's own token, which that
// member checks. A change for every member is a rollout: one member first,
// then, if it got no worse than the rest over the same windows, the others;
// a first stage that hurt is undone, and one it can't judge is kept and the
// rollout stops. It answers within the minute an MCP client waits: with its
// result and how to take it back (undo), or that it is still running, with
// a member's full patience, when members are slow. Fleet, asked of any
// member, shows the fleet's last rollout (last_rollout: how it ended, its
// undo, then each member from its own change and the fleet before and
// after). A rollout is refused while another runs, and while frozen.
// A freeze on one member holds them all. The UI judges the
// fleet: its headline names a member over the objective the rest meet,
// wherever the page is open, and any member can be opened and changed from
// it. Its API takes ?instance=cp-2 the same way: inside a fleet, members
// trust each other's UI to say which person is acting, so keep every
// member's UI on localhost, as one member's already is.
//
// A member that stops cleanly says so and leaves at once. The main in step 3
// of the adoption makes a stop clean: Start's context ends on SIGTERM as well
// as Ctrl-C, which is what docker stop and Kubernetes send; the server shuts
// down when it does; and the deferred Stop tells the fleet and returns once
// it has. Without SIGTERM in NotifyContext, Go ends the process on it before vis can say anything,
// and the fleet can't tell the stop from a crash. A member that goes silent may still be taking traffic,
// so it stays listed, dead, for 10 minutes or until an instance restarts
// under its name, and the fleet's objective can't read as met while it is
// unaccounted for. Where names don't come back (a Kubernetes Deployment gives
// a new pod a new name), a person forgets it: POST /api/forget on any
// member's UI. The fleet also shows what each member holds above its
// defaults and when each value ends, and how often a member restarted, with
// what the restart dropped: runtime values live in the process, so a
// restart, a deploy included, undoes them. And it says what every member
// shares, which a comparison can't: a pressure, a change in their
// environment such as a CPU limit cut on all of them, a value they all hold
// and when it ends. A member too busy
// to answer within VIS_FLEET_WAIT (2 s) is shown from what its gossip last
// carried; raise it where members answer slowly by design. However high it
// is, a call forwarded to a member waits at most 20 s for it to start
// answering, and 55 s in all, so that it answers within the minute an MCP
// client waits.
//
// # Configuration
//
// Each setting comes from the environment or from an option given to Start;
// the option wins:
//
//	VIS_LISTEN     MCPAt(addr)                  MCP's address: 127.0.0.1:7070; "" turns it off
//	VIS_UI_LISTEN  UIAt(addr)                   the UI's address: 127.0.0.1:7071; "" turns it off
//	VIS_TOKENS     MCPAt(addr, Tokens(...))     who may use MCP, and who may act: no one, so read-only
//	VIS_UI_USER    UIAt(addr, UIUser(id))       the person the UI acts as: the OS user
//	VIS_UI_HOSTS   UIAt(addr, UIHosts(...))     host names the UI answers to beyond localhost
//	VIS_STATE_DIR  StateDir(dir)                where it keeps its last word: the temp dir
//
// A process that dies without stopping cleanly (an OOM kill, a crash) can't
// say why, so the one that replaces it does: "most likely killed for memory"
// when its container was at its memory limit, with what the death dropped
// (values people, agents and rules had set) and how to set each back. vis
// doesn't set them back itself, since one may be what killed it, unless its
// writer asked: a change with Tx.Keep (set_knob's keep) is set back after a
// restart, clean or not, as its writer, for what is left of its lease, and
// the death story says so and which lease to revert. If the fresh process it
// was set back on dies within a minute, it may be what kills it: vis stops
// setting it back, and the story says so and how to set it back anyway. In a
// crash loop each death also names what earlier ones dropped, until someone
// sets the knob again or its lease ends. The restarted process numbers its changes
// and leases on from the former one's, and set_knob warns of a change on a
// process seconds from its memory limit, which a kill would take with it.
// describe and impact reports say when the container reaches its memory limit
// at the rate since the last change, however far ahead: how long a fix buys.
// The former process's last word comes from its peers in a fleet (its last
// digest if it died, what it said as it left if it stopped), and alone from a
// note in VIS_STATE_DIR, one per process (by PID), written every couple of
// seconds and on each change. A note whose process still runs isn't a former
// one's: two processes under one name on a host is a warning.
// Docker keeps the temp dir across a restart; on Kubernetes, mount an
// emptyDir volume there. The note is optional: where it can't be written, vis
// says so in its warnings and goes on. The guide's main stops through Stop,
// which marks the note clean: no death to tell.
//
// and for a fleet (see Fleets):
//
//	VIS_CLUSTER_KEY     ClusterKey(key)         the secret every member shares; without it, no fleet
//	VIS_SEEDS           Seeds(addrs...)         members to join through, comma-separated
//	VIS_PEER_LISTEN     PeerAt(addr)            where peers reach this one: :7946
//	VIS_PEER_ADVERTISE  PeerAdvertise(addr)     the address peers should use, when the one it listens on isn't
//	VIS_INSTANCE_NAME   InstanceName(name)      its name in the fleet, stable across restarts: the hostname
//	VIS_FLEET_WAIT      FleetWait(d)            how long a fleet question waits for each member: 2s
//
// # The default instance
//
// The package-level functions (Start, HTTP, and a knob's Bind and Now) work
// on one instance per process, the default instance, the way slog.Default and
// http.DefaultServeMux do. That is global state, with consequences you should
// know:
//
//   - Everything declared (knobs, SLOs, gauges, rules) belongs to the process,
//     and Start runs once; a second call returns ErrStarted.
//   - Tests that use the default instance share it, so they can't run in
//     parallel. A test that needs its own state takes its own instance.
//
// # Tests and several instances
//
// Tests, and programs that need more than one instance, create instances with
// NewInstance and use its methods: in.HTTP, in.Serve, in.Apply, in.RunRules,
// and a knob's BindIn and Get(in.Snapshot()). vistest.Start(t) gives a test
// its own instance, so tests can run in parallel, and vistest.Ring(t, n) a
// joined fleet of n, over an in-memory network that testing/synctest runs in
// no time.
package vis
