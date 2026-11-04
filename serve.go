package vis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// ServeOption chooses what Serve serves, and where.
type ServeOption func(*serveOptions)

type serveOptions struct {
	mcpAddr, uiAddr   string
	mcpChose, uiChose bool // the address came from an option or the environment, not vis's default
	mcp               []MCPOption
	ui                []UIOption

	seeds               []string // the ring (fleet.go)
	key                 string
	peerAddr, advertise string
	peerLn              net.Listener
	mcpHandler          http.Handler // built once, for the MCP listener and the ring
	fleetWait           time.Duration
	dial                func(ctx context.Context, network, addr string) (net.Conn, error)
	stateDir            string // where the instance keeps its last word (lastword.go); "" for none
}

// The addresses Serve uses when nothing chooses them. If another process
// holds one, vis serves on a free port instead and says where.
var (
	defaultMCPAddr = "127.0.0.1:7070"
	defaultUIAddr  = "127.0.0.1:7071"
)

// MCPAt serves MCP on addr, with opts; "" turns it off. The default is
// localhost:7070, or VIS_LISTEN when set; an address chosen here or there
// that is taken fails Serve. Exposing MCP beyond localhost needs tokens.
func MCPAt(addr string, opts ...MCPOption) ServeOption {
	return func(o *serveOptions) { o.mcpAddr, o.mcpChose, o.mcp = addr, true, append(o.mcp, opts...) }
}

// UIAt serves the control UI on addr, with opts; "" turns it off. The default
// is localhost:7071, or VIS_UI_LISTEN when set. Keep it on localhost: it acts as one person.
func UIAt(addr string, opts ...UIOption) ServeOption {
	return func(o *serveOptions) { o.uiAddr, o.uiChose, o.ui = addr, true, append(o.ui, opts...) }
}

// Served says where Serve is listening; an empty URL means that surface is off.
type Served struct {
	MCP string
	UI  string
}

// Serve listens for MCP and the control UI and serves them in the background
// until ctx ends. It returns once everything is listening, so a bad address
// fails at startup. A default address another process holds (a second
// service with vis on the machine, say) isn't one: vis serves on a free port
// instead and logs where. It reads the usual environment:
//
//	VIS_LISTEN     MCP's address (default 127.0.0.1:7070)
//	VIS_UI_LISTEN  the control UI's address (default 127.0.0.1:7071)
//	VIS_TOKENS     token=kind:id[:act],...  who may use MCP, and who may act
//	VIS_UI_USER    the person the UI acts as (default: the OS user)
//	VIS_UI_HOSTS   Host names the UI answers to beyond localhost
//
// stopper ends what one Serve started and says when it has.
type stopper struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once the fleet has been told this member is leaving
}

// Stop ends what Serve started, if its context hasn't ended already: it tells
// the fleet this member is leaving, closes MCP and the UI, and returns once
// the fleet has been told (or after a few seconds if it can't be). Defer it
// in main after Start: a process that exits as its context ends would
// otherwise be gone before vis could say so, and the fleet would take a
// clean stop for a crash. Calling it again does nothing.
func (in *Instance) Stop() {
	in.stopMu.Lock()
	stops := in.stops
	in.stopMu.Unlock()
	give := time.NewTimer(5 * time.Second)
	defer give.Stop()
	for _, s := range stops {
		s.cancel()
		select {
		case <-s.done:
		case <-give.C:
			return
		}
	}
}

func (in *Instance) Serve(ctx context.Context, opts ...ServeOption) (Served, error) {
	ctx, cancel := context.WithCancel(ctx)
	st := stopper{cancel: cancel, done: make(chan struct{})}
	s, err := in.serve(ctx, st.done, opts...)
	if err != nil {
		cancel()
		close(st.done)
		return s, err
	}
	in.stopMu.Lock()
	in.stops = append(in.stops, st)
	in.stopMu.Unlock()
	return s, nil
}

func (in *Instance) serve(ctx context.Context, done chan struct{}, opts ...ServeOption) (Served, error) {
	o := serveOptions{mcpAddr: defaultMCPAddr, uiAddr: defaultUIAddr, peerAddr: defaultPeerAt,
		key: os.Getenv("VIS_CLUSTER_KEY"), advertise: os.Getenv("VIS_PEER_ADVERTISE")}
	if s := os.Getenv("VIS_SEEDS"); s != "" {
		o.seeds = strings.Split(s, ",")
	}
	if a := os.Getenv("VIS_PEER_LISTEN"); a != "" {
		o.peerAddr = a
	}
	if w := os.Getenv("VIS_FLEET_WAIT"); w != "" {
		d, err := time.ParseDuration(w)
		if err != nil || d <= 0 {
			return Served{}, fmt.Errorf("vis: VIS_FLEET_WAIT=%q: want a duration such as 5s", w)
		}
		o.fleetWait = d
	}
	o.stateDir = os.Getenv("VIS_STATE_DIR")
	if a, ok := os.LookupEnv("VIS_LISTEN"); ok { // set but empty turns MCP off
		o.mcpAddr, o.mcpChose = a, true
	}
	if a, ok := os.LookupEnv("VIS_UI_LISTEN"); ok {
		o.uiAddr, o.uiChose = a, true
	}
	for _, opt := range opts {
		opt(&o)
	}
	if len(o.seeds) > 0 && o.key == "" {
		return Served{}, fmt.Errorf("vis: peers: seeds (%s) need a cluster key to join them: set VIS_CLUSTER_KEY, or vis.ClusterKey, to one secret every instance of %s shares",
			strings.Join(o.seeds, ","), in.service)
	}
	for _, w := range in.envWarnings() { // the operator who set it reads the log, not only the UI
		in.logger().Warn("vis: " + w)
	}
	if w := memLimitWarning(in.Runtime(0)); w != "" { // so does the one who sets GOMEMLIMIT
		in.logger().Warn("vis: " + w)
	}
	tokens := ""
	if env := os.Getenv("VIS_TOKENS"); env != "" {
		grants, err := ParseTokens(env)
		if err != nil {
			return Served{}, fmt.Errorf("VIS_TOKENS: %w", err)
		}
		o.mcp = append([]MCPOption{Tokens(grants)}, o.mcp...)
		tokens = env
	}
	if u := os.Getenv("VIS_UI_USER"); u != "" {
		o.ui = append([]UIOption{UIUser(u)}, o.ui...)
	}
	if h := os.Getenv("VIS_UI_HOSTS"); h != "" {
		o.ui = append([]UIOption{UIHosts(strings.Split(h, ",")...)}, o.ui...)
	}

	var s Served
	var lns []net.Listener
	listen := func(addr string, chose bool, what, env string) (net.Listener, string, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil && !chose && errors.Is(err, syscall.EADDRINUSE) {
			host, _, _ := net.SplitHostPort(addr)
			if ln, err = net.Listen("tcp", net.JoinHostPort(host, "0")); err == nil {
				in.logger().Warn(fmt.Sprintf("vis: %s: %s is in use (another service with vis?), so it serves on http://%s; set %s to choose", what, addr, ln.Addr(), env),
					"url", "http://"+ln.Addr().String())
			}
		}
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return nil, "", err
		}
		lns = append(lns, ln)
		return ln, "http://" + ln.Addr().String(), nil
	}
	var mln, uln net.Listener
	var err error
	if o.mcpAddr != "" {
		if mln, s.MCP, err = listen(o.mcpAddr, o.mcpChose, "MCP", "VIS_LISTEN"); err != nil {
			return Served{}, fmt.Errorf("vis: MCP: %w (set VIS_LISTEN, or vis.MCPAt, to serve it elsewhere; \"\" turns it off)", err)
		}
	}
	if o.uiAddr != "" {
		if uln, s.UI, err = listen(o.uiAddr, o.uiChose, "control UI", "VIS_UI_LISTEN"); err != nil {
			return Served{}, fmt.Errorf("vis: control UI: %w (set VIS_UI_LISTEN, or vis.UIAt, to serve it elsewhere; \"\" turns it off)", err)
		}
	}

	o.mcpHandler = in.MCP(o.mcp...)
	if o.stateDir != "" { // the note's writer closes done once it marked a clean stop and the rest stopped
		inner, path := make(chan struct{}), in.notePath(o.stateDir)
		deaths := in.readNote(path) // before serving: its changes are numbered on from the former process's
		go in.keepNote(ctx, path, deaths, inner, done)
		done = inner
	}
	if o.key == "" {
		go func() { <-ctx.Done(); close(done) }() // no fleet to tell
	} else {
		if err := in.startRing(ctx, &o, done); err != nil {
			for _, l := range lns {
				l.Close()
			}
			return Served{}, err
		}
	}

	serve := func(ln net.Listener, h http.Handler) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		go func() { <-ctx.Done(); srv.Close() }()
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				in.logger().Error("vis: serving stopped", "addr", ln.Addr().String(), "err", err)
			}
		}()
	}
	if mln != nil {
		serve(mln, o.mcpHandler)
		acts := "read tools only: set VIS_TOKENS to let agents act"
		if tokens != "" {
			acts = "tokens from VIS_TOKENS; act where granted"
		}
		in.logger().LogAttrs(ctx, slog.LevelInfo, "vis: MCP", slog.String("url", s.MCP), slog.String("access", acts))
	}
	if uln != nil {
		h := in.UI(o.ui...)
		serve(uln, h)
		in.logger().LogAttrs(ctx, slog.LevelInfo, "vis: control UI", slog.String("url", s.UI), slog.String("acts_as", h.(*uiHandler).user.String()),
			slog.String("hint", "it has no login: keep it on localhost and reach it from another machine with an SSH or kubectl port-forward"))
	}
	return s, nil
}
