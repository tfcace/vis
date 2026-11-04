package vis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Grant is what a bearer token gives its holder: who they write as, and
// whether they may act or only read. The writer comes from the token, never
// from a tool's input, so an agent can't claim to be a person.
type Grant struct {
	Writer Writer
	Act    bool
}

// MCPOption configures Instance.MCP.
type MCPOption func(*mcpOptions)

type mcpOptions struct {
	tokens map[string]Grant
}

// Tokens configures static bearer tokens. Without tokens an instance serves
// the read tools only; with them, every request needs a valid token.
// (OAuth, as the MCP authorization spec describes, comes later.)
func Tokens(t map[string]Grant) MCPOption { return func(o *mcpOptions) { o.tokens = t } }

// explainMissingToken answers a request without a token by saying what it needs.
func explainMissingToken(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "vis: this MCP server takes tokens (VIS_TOKENS): send Authorization: Bearer <token>", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// MCP serves the instance's tools over MCP's streamable HTTP transport,
// stateless, so any instance can answer and no load balancer needs sticky sessions.
func (in *Instance) MCP(opts ...MCPOption) http.Handler {
	o := mcpOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	s, t := in.mcpServer(o)
	h := http.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true}))
	if o.tokens != nil {
		h = auth.RequireBearerToken(t.verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)
		h = explainMissingToken(h)
	}
	return in.longCalls(h)
}

// longestResponse is how long a response may take to write: the longest
// call, or a rollout, which measures its first stage and then its second
// (the longest call each), reads the fleet before and after, and waits for
// starved members around them: each member passed over in stage 1, the
// measurement and its retry, stage 2 and its retry, and the retry of stage 2's
// measurements; and the fleet read after, past the window's first bucket.
func (in *Instance) longestResponse() time.Duration {
	r := in.ring.Load()
	if r == nil {
		return longestCall
	}
	return 2*longestCall + 2*r.patience(false) + bucketWidth + time.Duration(len(in.Members())+5)*r.patience(true)
}

// longestCall is the most a tool call takes: impact waits out a change's
// settle time and then some, a profile samples for up to 30 s.
const longestCall = settleFor + lateMeasure + 10*time.Second

// longCalls gives every MCP response longestCall to be written, past a shorter
// WriteTimeout on the server, which would cut a waiting impact or a profile off
// mid-answer. (Responses are a single JSON answer: stateless, no streams.)
func (in *Instance) longCalls(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(in.longestResponse()))
		next.ServeHTTP(w, r)
	})
}

func (in *Instance) mcpServer(o mcpOptions) (*mcp.Server, *mcpTools) {
	s := mcp.NewServer(&mcp.Implementation{Name: in.service, Title: in.service + " (vis)", Version: in.build}, &mcp.ServerOptions{
		Instructions: "Knobs are typed and bounded; every change is leased and audited. Look before acting: " +
			"metrics and history show what is happening and what changed; knobs says what each knob does and costs. " +
			"Changes need a lease and a reason, and set_knob reports what actually took effect. " +
			"After a change, impact waits until vis has measured it and says what it did.",
	})
	t := &mcpTools{in: in, opts: o}
	t.register(s)
	return s, t
}

type mcpTools struct {
	in   *Instance
	opts mcpOptions
}

func (t *mcpTools) verify(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	g, ok := t.opts.tokens[token]
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	scopes := []string{"vis:read"}
	if g.Act {
		scopes = append(scopes, "vis:act")
	}
	return &auth.TokenInfo{UserID: g.Writer.String(), Scopes: scopes, Extra: map[string]any{"grant": g}}, nil
}

// actor is the writer behind a call, if it may act.
func (t *mcpTools) actor(req *mcp.CallToolRequest) (Writer, error) {
	if t.opts.tokens == nil {
		return Writer{}, errors.New("this service lets agents read but not act. Whoever runs it can let an agent act: " +
			"start it with VIS_TOKENS=<token>=agent:<name>:act (vis.Tokens in code), " +
			"and have the agent's MCP client send the header Authorization: Bearer <token>")
	}
	ti := req.Extra.TokenInfo
	if ti == nil {
		return Writer{}, errors.New("no token")
	}
	g, _ := ti.Extra["grant"].(Grant)
	if !g.Act {
		return Writer{}, fmt.Errorf("%v's token can read but not act. To let it act, end its grant with :act: VIS_TOKENS=<token>=%v:act", g.Writer, g.Writer)
	}
	return g.Writer, nil
}

// reader is who is calling a read tool: the token's writer, or "agent" for a
// caller with no token (a service that lets agents read without one).
func (t *mcpTools) reader(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		if g, ok := req.Extra.TokenInfo.Extra["grant"].(Grant); ok {
			return g.Writer.String()
		}
	}
	return "agent"
}

// knob values in their own terms: numbers stay numbers, durations read as "5ms".
func showValue(i Info, v int64) any {
	if i.Type == "int" && !(i.HasSpecial && v == i.Special) {
		return v
	}
	return i.Format(v)
}

type (
	none struct{}
	// on names the member a call runs on; every tool but fleet, freeze and unfreeze takes it.
	on struct {
		Instance string `json:"instance,omitempty" jsonschema:"a member of the fleet by name, as fleet lists them: the call runs on that instance instead of this one"`
	}

	knobOut struct {
		Name        string     `json:"name"`
		Type        string     `json:"type"`
		Unit        string     `json:"unit,omitempty"`
		About       string     `json:"about,omitempty"`
		Uses        []Resource `json:"uses,omitempty" jsonschema:"what turning this knob up uses more of: cpu, memory, or one of the service's signals"`
		Saves       []Resource `json:"saves,omitempty" jsonschema:"what turning this knob up saves, so turning it down spends more of"`
		Min         any        `json:"min"`
		Max         any        `json:"max"`
		Default     any        `json:"default"`
		Special     string     `json:"special,omitempty" jsonschema:"a value outside min..max the service gives its own meaning, by name: set it by that name; rules never move the knob into or out of it"`
		DefaultFrom string     `json:"default_from,omitempty" jsonschema:"the environment variable the default came from, when one set it"`
		Pull        []string   `json:"pull" jsonschema:"writers that may change it directly; people always may"`
		Value       any        `json:"value"`
		Layer       string     `json:"layer" jsonschema:"the layer in effect"`
		Stack       []layerOut `json:"stack" jsonschema:"every layer, top first"`
	}
	layerOut struct {
		Layer  string `json:"layer"`
		Value  any    `json:"value"`
		By     string `json:"by,omitempty"`
		Lease  uint64 `json:"lease,omitempty"`
		EndsIn string `json:"ends_in,omitempty" jsonschema:"time until the lease ends; absent means no expiry"`
		Kept   bool   `json:"kept,omitempty" jsonschema:"set back after a restart, until the lease ends"`
		Then   any    `json:"then" jsonschema:"what the knob falls back to when this layer ends"`
	}
	knobsIn struct {
		on
		Knob string `json:"knob,omitempty" jsonschema:"one knob's name; omit for all"`
	}
	knobsOut struct {
		Knobs  []knobOut `json:"knobs"`
		Freeze freezeOut `json:"freeze"`
	}
	freezeOut struct {
		Frozen bool   `json:"frozen"`
		By     string `json:"by,omitempty"`
		Reason string `json:"reason,omitempty"`
		Since  string `json:"since,omitempty"`
		On     string `json:"on,omitempty" jsonschema:"the member it was set on; a freeze holds the whole fleet"`
	}
	sloOut struct {
		Route    string  `json:"route"`
		P99      float64 `json:"p99_ms" jsonschema:"the objective"`
		Now      float64 `json:"now_p99_ms" jsonschema:"p99 over the last 20 seconds, measured inside the service; what met judges"`
		Minute   float64 `json:"minute_p99_ms" jsonschema:"p99 over the last minute, for context; it lags a recovery"`
		Requests int64   `json:"requests"`
		Met      bool    `json:"met"`
		Warning  string  `json:"warning,omitempty" jsonschema:"a reason to doubt the objective is set up right, such as a route that never serves"`
	}
	describeOut struct {
		Service   string      `json:"service"`
		Build     string      `json:"build"`
		SLOs      []sloOut    `json:"slos" jsonschema:"each route's latency objective against the last minute"`
		Pressures []Pressure  `json:"pressures" jsonschema:"resources running short now, why, and the knobs that spend them, most expensive setting first"`
		Memory    string      `json:"memory,omitempty" jsonschema:"when the container's memory reaches its limit at the rate since the last change, however far ahead, or that it doesn't within the hour: how long the service stays up"`
		Knobs     []knobOut   `json:"knobs"`
		Rules     []RuleState `json:"rules"`
		Freeze    freezeOut   `json:"freeze"`
		Warnings  []string    `json:"warnings,omitempty" jsonschema:"reasons to doubt the service is set up right, each saying how to fix it"`
	}
	metricsIn struct {
		on
		Window string `json:"window,omitempty" jsonschema:"how far back, like 1m; at most 5m; default 1m"`
	}
	routeOut struct {
		Route    string  `json:"route"`
		Requests int64   `json:"requests"`
		Errors   int64   `json:"errors"`
		P50      float64 `json:"p50_ms"`
		P95      float64 `json:"p95_ms"`
		P99      float64 `json:"p99_ms"`
		Refused  int64   `json:"refused,omitempty" jsonschema:"responses with a 429: turned away, fast, so they pull p99 down"`
	}
	runtimeOut struct {
		Over          string  `json:"over"`
		SchedP50      float64 `json:"sched_p50_ms" jsonschema:"how long runnable goroutines waited for a CPU"`
		SchedP99      float64 `json:"sched_p99_ms"`
		GCCPU         float64 `json:"gc_cpu" jsonschema:"share of CPU spent on garbage collection"`
		HeapRatio     float64 `json:"heap_ratio" jsonschema:"heap goal over the memory limit; 0 without a limit"`
		Goroutines    int     `json:"goroutines"`
		GOMAXPROCS    int     `json:"gomaxprocs"`
		CPULimit      float64 `json:"cpu_limit" jsonschema:"cores the container may use; 0 when unlimited or unknown"`
		CPUUsed       float64 `json:"cpu_used"`
		Throttled     float64 `json:"throttled" jsonschema:"share of scheduling periods in which the container hit its CPU limit"`
		ThrottledTime string  `json:"throttled_time"`
		MemFrom       string  `json:"mem_from,omitempty" jsonschema:"container: memory is the cgroup's working set (a container, a pod, a systemd unit); process: what the Go runtime holds, outside a cgroup"`
		MemUsedMB     uint64  `json:"mem_used_mb,omitempty" jsonschema:"MiB in use, as mem_from says"`
		MemLimitMB    uint64  `json:"mem_limit_mb,omitempty" jsonschema:"the memory limit in MiB, as mem_limit_by says; absent when there is none"`
		MemLimitBy    string  `json:"mem_limit_by,omitempty" jsonschema:"cgroup or lambda: where the process is killed; GOMEMLIMIT: where the collector runs almost without pause"`
		OOMKills      uint64  `json:"oom_kills,omitempty" jsonschema:"processes the kernel killed in this container for memory, since it began"`
		HeapLiveMB    float64 `json:"heap_live_mb" jsonschema:"what the GC kept at its last cycle: the heap without its garbage"`
		HeapGoalMB    float64 `json:"heap_goal_mb" jsonschema:"the heap the GC allows before its next collection: the live heap and the garbage it lets build up"`
		HeapGrowth    float64 `json:"heap_growth_mb_s" jsonschema:"how fast the live heap grew over the window, MiB a second: a leak shows here even when garbage hides it in the container's memory"`
	}
	routeMetricsOut struct {
		routeOut
		Probes []probeOut `json:"probes,omitempty" jsonschema:"time requests spent in each probe the service declared"`
		Paths  []pathOut  `json:"paths,omitempty" jsonschema:"requests grouped by the probes they ran, with their whole latency: a cache hit and a miss, say"`
	}
	probeOut struct {
		Probe    string  `json:"probe"`
		Requests int64   `json:"requests"`
		Share    float64 `json:"share" jsonschema:"of the route's requests"`
		P50      float64 `json:"p50_ms"`
		P99      float64 `json:"p99_ms"`
	}
	pathOut struct {
		Probes   []string `json:"probes"`
		Requests int64    `json:"requests"`
		Share    float64  `json:"share"`
		Errors   int64    `json:"errors"`
		P50      float64  `json:"p50_ms"`
		P99      float64  `json:"p99_ms"`
	}
	profileIn struct {
		on
		Kind    string `json:"kind" jsonschema:"cpu, goroutine, goroutineleak, heap, mutex or block"`
		Seconds int    `json:"seconds,omitempty" jsonschema:"how long cpu, mutex and block sample; default 10, at most 30"`
	}
	metricsOut struct {
		Window  string            `json:"window"`
		Routes  []routeMetricsOut `json:"routes"`
		Runtime runtimeOut        `json:"runtime"`
		App     []SignalValue     `json:"app" jsonschema:"signals the service publishes about itself, read now"`
	}
	historyIn struct {
		on
		Limit int `json:"limit,omitempty" jsonschema:"how many recent events; default 50"`
	}
	eventOut struct {
		At     string         `json:"at"`
		Kind   string         `json:"kind"`
		Fields map[string]any `json:"fields"`
	}
	historyOut struct {
		Stories []Story    `json:"stories" jsonschema:"what happened, newest first, told in sentences; your own actions are 'you'"`
		Events  []eventOut `json:"events" jsonschema:"the raw audit records, oldest first"`
	}
	rulesOut struct {
		Rules []RuleState `json:"rules"`
	}
	setIn struct {
		on
		Set    map[string]any `json:"set" jsonschema:"knob name to value, in the knob's own terms: numbers for ints, strings with units for durations"`
		For    string         `json:"for" jsonschema:"the lease, like 30m; the change ends on its own after it"`
		Reason string         `json:"reason" jsonschema:"why, in a sentence; it goes to the audit trail"`
		Keep   bool           `json:"keep,omitempty" jsonschema:"set it back after a restart too, until the lease ends: for a fix a restart would undo. A restart drops values otherwise, since one may be what killed the process; only keep a value you have seen hold. If the process it is set back on dies within a minute, vis stops setting it back"`
	}
	effectOut struct {
		Knob  string `json:"knob"`
		Value any    `json:"value"`
		Layer string `json:"layer" jsonschema:"the layer in effect, which may be above yours"`
	}
	changeOut struct {
		Instance   string        `json:"instance" jsonschema:"the member it ran on: change and lease numbers are that member's own"`
		Process    string        `json:"process" jsonschema:"the member's process: a restart starts another, which numbers its changes and leases on from this one's, and a revert that names this one is refused after it"`
		Change     uint64        `json:"change"`
		Lease      uint64        `json:"lease"`
		EndsAt     string        `json:"ends_at,omitempty"`
		MeasuredAt string        `json:"measured_at,omitempty" jsonschema:"when vis will have measured this change's effect; impact with this change waits until then and says what it did"`
		Effects    []effectOut   `json:"effects" jsonschema:"what is actually in effect now, per knob"`
		Replaced   []replacedOut `json:"replaced,omitempty" jsonschema:"values this change took the place of in its own layer: reverting it puts the layer below back, not these"`
		Warning    string        `json:"warning,omitempty" jsonschema:"why this change may not last: its process is about to be killed for memory"`
	}
	replacedOut struct {
		Knob  string `json:"knob"`
		Value any    `json:"value"`
		By    string `json:"by"`
		Left  string `json:"lease_left,omitempty" jsonschema:"what was left of its lease"`
		Until string `json:"until,omitempty" jsonschema:"when its lease would have ended; none means it had no end"`
	}
	impactIn struct {
		on
		Change uint64 `json:"change" jsonschema:"the change id from set_knob, revert or history"`
		NoWait bool   `json:"no_wait,omitempty" jsonschema:"answer at once, even while the change is still being measured"`
	}
	revertIn struct {
		on
		Lease   uint64 `json:"lease"`
		Reason  string `json:"reason"`
		Process string `json:"process,omitempty" jsonschema:"the process the lease belongs to, as a change or an undo line gives it: refused if the instance has restarted since, when lease numbers name other changes"`
	}
	reasonIn struct {
		Reason string `json:"reason"`
	}
	setRuleIn struct {
		on
		Rule    string `json:"rule"`
		Enabled bool   `json:"enabled" jsonschema:"false switches the rule off and ends its lease, so its knob falls back now"`
		Reason  string `json:"reason"`
	}
)

func (t *mcpTools) register(s *mcp.Server) {
	read := func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true}
	}
	act := func(title string) *mcp.ToolAnnotations { return &mcp.ToolAnnotations{Title: title} }

	addTool(t, s, &mcp.Tool{Name: "describe", Annotations: read("Describe the service"),
		Description: "The service, its build, its latency objectives and whether they are met, resources running short now with the knobs that spend them, every knob with what it does and costs, its rules, and whether changes are frozen. Start here."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ on) (*mcp.CallToolResult, describeOut, error) {
			return nil, describeOut{Service: t.in.service, Build: t.in.build, SLOs: t.slos(), Pressures: nonNil(t.in.Pressures()), Memory: t.in.memoryOutlook(), Knobs: t.knobs(""), Rules: t.in.Rules(), Freeze: t.freeze(), Warnings: t.in.Warnings()}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "fleet", Annotations: read("Compare the fleet"),
		Description: "Every instance of the service, compared: each member's objectives, CPU and throttling over the last 20 s beside the fleet's (percentiles from merged histograms, not averages), which members stand out and how to look closer, what differs between members, and what they all share (a pressure, an environment change, a value they all hold and when it ends). " +
			"A member that doesn't answer is shown from its gossip; one not heard from is unaccounted for, and the fleet's objective can't read as met while one is. " +
			"Every other tool but rollout, freeze and unfreeze takes instance, a member's name, and runs there; rollout changes every member, and a freeze or unfreeze reaches the whole fleet. Without peers, the fleet is this instance."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, fleetOut, error) {
			out := fleetJSON(t.in.Fleet(ctx))
			out.Rollout = t.in.lastRolloutAnywhere()
			return nil, out, nil
		})
	addTool(t, s, &mcp.Tool{Name: "knobs", Annotations: read("Read knobs"),
		Description: "Knob values with their layer stacks: who set each layer, when its lease ends, and what the knob falls back to."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in knobsIn) (*mcp.CallToolResult, knobsOut, error) {
			k := t.knobs(in.Knob)
			if in.Knob != "" && len(k) == 0 {
				return nil, knobsOut{}, fmt.Errorf("%w: %q", ErrUnknownKnob, in.Knob)
			}
			return nil, knobsOut{Knobs: k, Freeze: t.freeze()}, nil
		})
	addTool(t, s, &mcp.Tool{Name: "metrics", Annotations: read("Read metrics"),
		Description: "Request latency and errors per route, split by the probes the service declared (where requests spend their time, and hits apart from misses); the Go runtime and container: scheduler latency, GC, CPU used against the container's limit, and throttling; and the signals the service publishes about itself. Latency is measured inside the service, from when it sees a request: clients also wait in queues before that, so the latency they see is higher, especially under overload."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in metricsIn) (*mcp.CallToolResult, metricsOut, error) {
			w := time.Minute
			if in.Window != "" {
				d, err := time.ParseDuration(in.Window)
				if err != nil {
					return nil, metricsOut{}, err
				}
				w = d
			}
			out := metricsOut{Window: w.String(), Runtime: runtimeJSON(t.in.Runtime(w)), App: t.in.Signals()}
			for _, r := range t.in.Routes(w) {
				rm := routeMetricsOut{routeOut: routeOut{r.Route, r.Requests, r.Errors, msf(r.P50), msf(r.P95), msf(r.P99), r.Refused}}
				b := t.in.Breakdown(r.Route, w)
				for _, p := range b.Probes {
					rm.Probes = append(rm.Probes, probeOut{p.Probe, p.Requests, round2(p.Share), msf(p.P50), msf(p.P99)})
				}
				for _, p := range b.Paths {
					rm.Paths = append(rm.Paths, pathOut{p.Probes, p.Requests, round2(p.Share), p.Errors, msf(p.P50), msf(p.P99)})
				}
				out.Routes = append(out.Routes, rm)
			}
			return nil, out, nil
		})
	addTool(t, s, &mcp.Tool{Name: "profile", Annotations: read("Profile the service"),
		Description: "Take a pprof profile of the running service and read it: the code that owns the cost, biggest first, with where it is and what calls it. " +
			"cpu: what is using the CPU. goroutine: where goroutines are parked, and how many (a count that keeps growing is a leak). " +
			"goroutineleak: goroutines blocked on something nothing can reach. heap: memory in use. mutex: who holds the locks others wait for. block: where goroutines wait. " +
			"cpu, mutex and block sample for seconds (default 10, at most 30); the others are a moment." + leakNote()},
		func(ctx context.Context, req *mcp.CallToolRequest, in profileIn) (*mcp.CallToolResult, ProfileReport, error) {
			r, err := t.in.Profile(ctx, in.Kind, time.Duration(in.Seconds)*time.Second)
			if err == nil {
				// In the history: who profiled what, and what it found. A profile has a
				// cost, and the evidence belongs beside the change it led to.
				t.in.audit(slog.LevelInfo, "vis.profile", slog.String("actor", t.reader(req)), slog.String("kind", r.Kind),
					slog.Float64("seconds", r.Seconds), slog.String("summary", r.Summary))
			}
			return nil, r, err
		})
	addTool(t, s, &mcp.Tool{Name: "history", Annotations: read("Read history"),
		Description: "What happened: stories in sentences, newest first (a rule's whole run, a change with its warning and expiry, a freeze, a change vis noticed in its environment such as the container's CPU limit), and the raw audit records behind them."},
		func(ctx context.Context, req *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyOut, error) {
			n := in.Limit
			if n <= 0 {
				n = 50
			}
			var viewer Writer
			if ti := req.Extra.TokenInfo; ti != nil {
				g, _ := ti.Extra["grant"].(Grant)
				viewer = g.Writer
			}
			out := historyOut{Stories: t.in.Stories(20, viewer)}
			for _, e := range t.in.History(n) {
				out.Events = append(out.Events, eventOut{At: e.At.UTC().Format(time.RFC3339), Kind: e.Kind, Fields: e.Fields})
			}
			return nil, out, nil
		})
	addTool(t, s, &mcp.Tool{Name: "rules", Annotations: read("Read rules"),
		Description: "Compiled-in rules: what each watches, its threshold, and its state (watching, acting, holding while a person's or an agent's change is being measured, stalled when its steps stopped helping, stopped when a step made the SLO worse, which vis undid, at_limit, aside when its knob is at its special value, which no rule moves (aside says so), overridden, frozen, shadow, disabled)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ on) (*mcp.CallToolResult, rulesOut, error) {
			return nil, rulesOut{Rules: t.in.Rules()}, nil
		})

	addTool(t, s, &mcp.Tool{Name: "impact", Annotations: read("Read what a change did"),
		Description: "What a change did, measured by vis: each SLO route's p99 (the three busiest routes' when there is no SLO), CPU, throttling and every published signal over the 20 s before the change and, for the routes, the requests in the 10 s buckets that began 10 s or more after it, up to 30 s after (10 to 20 s of them: its first seconds, a backlog draining, aren't its effect; counts compare as rates), with anything else that changed in that window named as a confounder, and memory's outlook after it (when the container reaches its limit at the rate since). In a fleet it also says what the other members did over the same windows: their holding still while this one moved is what says the change did it. It waits until the change has been measured (measured_at, about 30 s after the change), so call it right after a change instead of sleeping."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in impactIn) (*mcp.CallToolResult, Impact, error) {
			im, ok := t.in.Impact(in.Change)
			if !ok {
				return nil, Impact{}, t.in.noMeasurement(in.Change)
			}
			if !im.Done && !in.NoWait {
				wait, cancel := context.WithDeadline(ctx, im.MeasuredAt.Add(lateMeasure))
				defer cancel()
				if r := t.in.ring.Load(); r != nil { // a member that stops doesn't keep a caller waiting
					defer context.AfterFunc(r.ctx, cancel)()
				}
				im, _ = t.in.WaitImpact(wait, in.Change)
				if !im.Done && ctx.Err() == nil { // late: answer with the pending report rather than hang
					im.Summary = fmt.Sprintf("Still measuring, %s late: the after window is read when the settle time ends, "+
						"and something is slow (a published signal, or the process itself is starved). Call impact again.", short(time.Since(im.MeasuredAt)))
				}
			}
			return nil, im, nil
		})
	addTool(t, s, &mcp.Tool{Name: "set_knob", Annotations: act("Set knobs"),
		Description: "Set one or more knobs in one change, under one lease, with a reason. You write the layer your token's writer owns, and the result says what is actually in effect, which may be a higher layer. In a fleet, a change is one member's: name it with instance."},
		func(ctx context.Context, req *mcp.CallToolRequest, in setIn) (*mcp.CallToolResult, changeOut, error) {
			by, err := t.actor(req)
			if err != nil {
				return nil, changeOut{}, err
			}
			lease, err := time.ParseDuration(in.For)
			if err != nil {
				return nil, changeOut{}, fmt.Errorf("for: %w", err)
			}
			tx := Tx{By: by, For: lease, Reason: in.Reason, Keep: in.Keep}
			names := make([]string, 0, len(in.Set))
			for n := range in.Set {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				info, ok := Lookup(n)
				if !ok {
					return nil, changeOut{}, fmt.Errorf("%w: %q", ErrUnknownKnob, n)
				}
				v, err := info.Parse(fmt.Sprint(in.Set[n]))
				if err != nil {
					return nil, changeOut{}, err
				}
				tx.Set = append(tx.Set, Write{Knob: n, Value: v})
			}
			rec, err := t.in.Apply(tx)
			if err != nil {
				return nil, changeOut{}, err
			}
			out := t.change(rec, lease)
			out.Warning = nearKill(t.in.Runtime(10 * time.Second))
			return nil, out, nil
		})
	addTool(t, s, &mcp.Tool{Name: "revert", Annotations: act("Revert a lease"),
		Description: "End a lease early: every value it still holds falls back to the layer below."},
		func(ctx context.Context, req *mcp.CallToolRequest, in revertIn) (*mcp.CallToolResult, changeOut, error) {
			by, err := t.actor(req)
			if err != nil {
				return nil, changeOut{}, err
			}
			if in.Process != "" && in.Process != t.in.id {
				return nil, changeOut{}, fmt.Errorf("vis: %s has restarted since that change (it was process %s, it is %s now): lease %d names another change here, so nothing was reverted; a restart drops runtime values, so the change is gone with the old process", t.in.name, in.Process, t.in.id, in.Lease)
			}
			rec, err := t.in.Revert(in.Lease, by, in.Reason)
			if err != nil {
				return nil, changeOut{}, err
			}
			return nil, t.change(rec, 0), nil
		})
	addTool(t, s, &mcp.Tool{Name: "set_rule", Annotations: act("Switch a rule off or on"),
		Description: "Disable or enable a compiled-in rule. Disabling ends its lease, so its knob falls back now. Use it when a rule is acting against the incident. Enabling also restarts a rule vis stopped."},
		func(ctx context.Context, req *mcp.CallToolRequest, in setRuleIn) (*mcp.CallToolResult, rulesOut, error) {
			by, err := t.actor(req)
			if err != nil {
				return nil, rulesOut{}, err
			}
			if err := t.in.SetRule(in.Rule, in.Enabled, by, in.Reason); err != nil {
				return nil, rulesOut{}, err
			}
			return nil, rulesOut{Rules: t.in.Rules()}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "rollout", Annotations: act("Change knobs across the fleet, in stages"),
		Description: "Change knobs on every member of the fleet in two stages: first on one member (chosen by a hash of its name and the knob, so the order is the same every time), then, " +
			"if that member got no worse than the rest over the same windows, on all the others at once. Every write is yours, under your token, with your lease and reason, on each member. " +
			"A first stage that made things worse (more errors, p99 rising beyond what the rest did or doubling past its objective, going over its objective, or its requests falling under a quarter while the rest kept theirs) is undone, putting back what it replaced, and the rest are left alone. " +
			"A first stage it can't judge (too few requests, someone else changed something on that member meanwhile, a member in its first minute, a measurement that failed) stops the rollout but keeps the change; undo says how to take it back. " +
			"It takes about 30 s, the time to measure the first stage. To change one member, use set_knob with instance."},
		func(ctx context.Context, req *mcp.CallToolRequest, in rolloutIn) (*mcp.CallToolResult, rolloutOut, error) {
			out, err := t.rollout(ctx, req, in)
			return nil, out, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "freeze", Annotations: act("Freeze automated changes"),
		Description: "Stop rules, agents and rollouts from changing knobs; every knob holds its value and lease clocks stop. In a fleet it reaches every member within seconds. Only a person can unfreeze."},
		func(ctx context.Context, req *mcp.CallToolRequest, in reasonIn) (*mcp.CallToolResult, freezeOut, error) {
			by, err := t.actor(req)
			if err != nil {
				return nil, freezeOut{}, err
			}
			if err := t.in.Freeze(by, in.Reason); err != nil {
				return nil, freezeOut{}, err
			}
			return nil, t.freeze(), nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "unfreeze", Annotations: act("Unfreeze"),
		Description: "Resume automated changes; leases pick up where they stopped. Only a person may."},
		func(ctx context.Context, req *mcp.CallToolRequest, in reasonIn) (*mcp.CallToolResult, freezeOut, error) {
			by, err := t.actor(req)
			if err != nil {
				return nil, freezeOut{}, err
			}
			if err := t.in.Unfreeze(by, in.Reason); err != nil {
				return nil, freezeOut{}, fmt.Errorf("%w (agents may freeze, but only a person can unfreeze)", err)
			}
			return nil, t.freeze(), nil
		})
}

func (t *mcpTools) knobs(only string) []knobOut {
	var out []knobOut
	for _, info := range Knobs() {
		if only != "" && info.Name != only {
			continue
		}
		st, err := t.in.Stack(info.Name)
		if err != nil {
			continue
		}
		k := knobOut{Name: info.Name, Type: info.Type, Unit: info.Unit, About: info.About, Uses: info.Uses, Saves: info.Saves, Min: showValue(info, info.Min),
			Max: showValue(info, info.Max), Default: showValue(info, info.Default), Pull: []string{"human"},
			Value: showValue(info, st[0].Value), Layer: st[0].Layer.String()}
		if info.HasSpecial {
			k.Special = info.SpecialName
		}
		for _, p := range info.Pull {
			k.Pull = append(k.Pull, p.String())
		}
		if info.FromEnv {
			k.DefaultFrom = info.Env
		}
		for _, e := range st {
			l := layerOut{Layer: e.Layer.String(), Value: showValue(info, e.Value), Lease: e.Lease, Then: showValue(info, e.Then)}
			if e.By.ID != "" {
				l.By = e.By.String()
			}
			if e.Left > 0 {
				l.EndsIn = ceilSecond(e.Left).String() // round up: "0s" would read as already over
				l.Kept = e.Kept
			}
			k.Stack = append(k.Stack, l)
		}
		out = append(out, k)
	}
	return out
}

func (t *mcpTools) slos() []sloOut {
	out := []sloOut{}
	for _, s := range t.in.SLOs() {
		out = append(out, sloOut{s.Route, msf(s.P99), msf(s.Now), msf(s.Minute), s.Requests, s.Met, s.Warning})
	}
	return out
}

func (t *mcpTools) freeze() freezeOut {
	f := t.in.FreezeState()
	out := freezeOut{Frozen: f.Frozen}
	if f.By.ID != "" {
		out.By, out.Reason, out.Since = f.By.String(), f.Reason, f.Since.UTC().Format(time.RFC3339)
		if t.in.ring.Load() != nil {
			out.On = f.On
		}
	}
	return out
}

func (t *mcpTools) change(rec Record, lease time.Duration) changeOut {
	out := changeOut{Instance: t.in.name, Process: t.in.id, Change: rec.Change, Lease: rec.Lease}
	if im, ok := t.in.Impact(rec.Change); ok {
		out.MeasuredAt = im.At.Add(im.Settle).UTC().Format(time.RFC3339)
	}
	if rec.Lease != 0 && lease > 0 && !t.in.FreezeState().Frozen { // frozen, a lease's clock stops: no end to say yet
		out.EndsAt = time.Now().Add(lease).UTC().Format(time.RFC3339)
	}
	for _, e := range rec.Effects {
		info, _ := Lookup(e.Knob)
		out.Effects = append(out.Effects, effectOut{Knob: e.Knob, Value: showValue(info, e.Value), Layer: e.Layer.String()})
	}
	for _, r := range rec.Replaced {
		info, _ := Lookup(r.Knob)
		o := replacedOut{Knob: r.Knob, Value: showValue(info, r.Value), By: r.By.String()}
		if r.Left > 0 {
			o.Left = ceilSecond(r.Left).String()
			o.Until = time.Now().Add(r.Left).UTC().Format(time.RFC3339)
		}
		out.Replaced = append(out.Replaced, o)
	}
	return out
}

func runtimeJSON(r RuntimeStats) runtimeOut {
	return runtimeOut{Over: r.Over.Round(time.Millisecond).String(), SchedP50: msf(r.SchedP50), SchedP99: msf(r.SchedP99),
		GCCPU: r.GCCPU, HeapRatio: r.HeapRatio, Goroutines: r.Goroutines, GOMAXPROCS: r.GOMAXPROCS,
		CPULimit: r.CPULimit, CPUUsed: r.CPUUsed, Throttled: r.Throttled, ThrottledTime: r.ThrottledTime.Round(time.Millisecond).String(),
		MemFrom: r.MemFrom, MemUsedMB: r.MemUsed >> 20, MemLimitMB: r.MemLimit >> 20, MemLimitBy: r.MemLimitBy, OOMKills: r.OOMKills,
		HeapLiveMB: round2(float64(r.HeapLive) / (1 << 20)), HeapGoalMB: round2(float64(r.HeapGoal) / (1 << 20)), HeapGrowth: round2(r.HeapGrowth / (1 << 20))}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func ceilSecond(d time.Duration) time.Duration { return (d + time.Second - 1).Truncate(time.Second) }

func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ParseTokens reads grants from configuration such as an environment variable:
// "token=kind:id[:act],…", for example "s3cret=agent:oncall:act,view=agent:dash".
func ParseTokens(s string) (map[string]Grant, error) {
	out := map[string]Grant{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		token, spec, ok := strings.Cut(part, "=")
		fields := strings.Split(spec, ":")
		if !ok || token == "" || len(fields) < 2 || len(fields) > 3 || fields[1] == "" {
			return nil, fmt.Errorf("vis: token %q: want token=kind:id[:act]", part)
		}
		var kind Kind
		for k, n := range kindNames {
			if n == fields[0] {
				kind = k
			}
		}
		if kind == 0 {
			return nil, fmt.Errorf("vis: token %q: unknown kind %q", part, fields[0])
		}
		g := Grant{Writer: Writer{Kind: kind, ID: fields[1]}}
		if len(fields) == 3 {
			if fields[2] != "act" {
				return nil, fmt.Errorf("vis: token %q: the third field can only be act", part)
			}
			g.Act = true
		}
		out[token] = g
	}
	return out, nil
}

// leakNote tells an agent up front when this build can't take a goroutineleak profile.
func leakNote() string {
	if pprof.Lookup("goroutineleak") != nil {
		return ""
	}
	return " Not in this build: goroutineleak (" + noLeakProfile + ")."
}
