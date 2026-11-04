package vis

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The ring: instances of one service that share a cluster key find each other
// from a seed and keep a member list by gossip over HTTP, on a port of their
// own. Every second an instance sends its list to one live member and one
// seed, and merges what comes back. Membership is all the ring keeps: no
// leader, no quorum, nothing stored.

// How long a member's heartbeat may stay still before it reads as suspect,
// then dead, and how long a dead one stays listed. A member that stops cleanly
// says it is leaving and is gone at once, so one that goes silent instead is
// something gone wrong: it may still be taking traffic, and it stays listed,
// dead, until it has been quiet for keepDead or comes back under its name.
const (
	gossipEvery   = time.Second
	suspectAfter  = 5 * time.Second
	deadAfter     = 15 * time.Second
	keepDead      = 10 * time.Minute
	peerSkew      = time.Minute // a signed message older or newer than this is refused
	peerTimeout   = 2 * time.Second
	defaultPeerAt = ":7946"
	peerPath      = "/vis/peer/v1/"
	maxPeerBody   = 4 << 20
)

// Member is one instance of the service, as this instance sees it.
type Member struct {
	ID         string // this process's, new at every start
	Name       string // stable across restarts: VIS_INSTANCE_NAME, or the hostname
	Addr       string // where its peers reach it
	Service    string
	Build      string
	State      string        // alive, suspect (quiet for 5 s) or dead (15 s), listed for 10 minutes unless it left cleanly
	Quiet      time.Duration // since its heartbeat last rose
	Self       bool
	seen, rose time.Time // first heard of, and its heartbeat last rose: two under one name share it only if both rose since both were known
}

// InstanceName names the instance in its fleet: stable across restarts, unlike
// its ID. It defaults to VIS_INSTANCE_NAME, then the hostname.
func InstanceName(name string) InstanceOption { return func(in *Instance) { in.name = name } }

// Seeds are peers to join the ring through: host:port addresses, or a DNS name
// with a port that resolves to several. VIS_SEEDS sets them, comma-separated.
// An instance may list itself. Seeds need a ClusterKey.
func Seeds(addrs ...string) ServeOption {
	return func(o *serveOptions) { o.seeds = addrs }
}

// ClusterKey is the secret every instance of the service shares, signing what
// peers send each other. VIS_CLUSTER_KEY sets it. With a key and no seeds, an
// instance listens for peers that list it as theirs.
func ClusterKey(key string) ServeOption { return func(o *serveOptions) { o.key = key } }

// PeerAt is where the instance listens for peers: VIS_PEER_LISTEN, or :7946.
// Unlike MCP and the UI it must be reachable by the other instances.
func PeerAt(addr string) ServeOption { return func(o *serveOptions) { o.peerAddr = addr } }

// PeerAdvertise is the address other instances reach this one at, when the
// one it listens on isn't: VIS_PEER_ADVERTISE, or the hostname with the port.
func PeerAdvertise(addr string) ServeOption { return func(o *serveOptions) { o.advertise = addr } }

// FleetWait is how long a fleet question waits for each member to answer:
// VIS_FLEET_WAIT, or 2 s. A member that doesn't answer in time is shown from
// what its gossip last carried. Raise it where members are slow to answer by
// design, such as during long shutdown grace periods.
func FleetWait(d time.Duration) ServeOption { return func(o *serveOptions) { o.fleetWait = d } }

// PeerListener serves peers on ln instead of listening at PeerAt, and
// PeerDialer reaches them with dial: for networks of their own, such as an
// in-memory one in tests (vistest.Ring) or a tailnet.
func PeerListener(ln net.Listener) ServeOption { return func(o *serveOptions) { o.peerLn = ln } }

// PeerDialer reaches peers with dial. See PeerListener.
func PeerDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) ServeOption {
	return func(o *serveOptions) { o.dial = dial }
}

// wireMember is a member as gossip carries it.
type wireMember struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Addr      string      `json:"addr"`
	Service   string      `json:"service"`
	Build     string      `json:"build"`
	Heartbeat uint64      `json:"hb"`
	Quiet     int64       `json:"quiet_ms,omitempty"` // how long since the sender heard its heartbeat rise: second-hand news arrives as old as it is
	Freeze    *wireFreeze `json:"freeze,omitempty"`   // its freeze state, which spreads: the highest epoch wins
	Health    *wireHealth `json:"health,omitempty"`   // how it was doing at that heartbeat, for a fleet view it is too busy to answer
}

// wireHealth is a member's own digest: small enough to ride on every gossip
// message, so an instance too starved to answer a question is still seen.
type wireHealth struct {
	At        int64          `json:"at"` // Unix nanoseconds
	CPUUsed   float64        `json:"cpu_used"`
	CPULimit  float64        `json:"cpu_limit"`
	Throttled float64        `json:"throttled"`
	Frozen    bool           `json:"frozen,omitempty"`
	SLOs      []peerSLO      `json:"slos"`              // histograms and all, so the fleet's p99 can merge them
	Held      []Held         `json:"held,omitempty"`    // what it holds above its defaults: what a restart would drop
	Kept      []Held         `json:"kept,omitempty"`    // every kept lease's value, on any layer: what a restart sets back (Tx.Keep)
	Rollout   *rolloutReport `json:"rollout,omitempty"` // the last rollout it coordinated, so every member shows it
	Last      string         `json:"last,omitempty"`    // its last change by a person or an agent
	LastAt    int64          `json:"last_at,omitempty"`
	Mem       *memDigest     `json:"mem,omitempty"`     // its container's memory, so the fleet can say a member that went silent was most likely killed for it
	Started   int64          `json:"started,omitempty"` // when the process started, Unix nanoseconds: whether a death came while it was starting
	Dropped   []dropped      `json:"dropped,omitempty"` // what earlier deaths of it dropped and nothing has set again: a crash loop's later deaths still name them
	// Its last change and lease IDs: the process that replaces it numbers on from them.
	Changes uint64 `json:"changes,omitempty"`
	Leases  uint64 `json:"leases,omitempty"`
}

// memDigest is a member's container memory at a heartbeat.
type memDigest struct {
	Used   uint64  `json:"used"`             // bytes: its working set
	Limit  uint64  `json:"limit"`            // bytes; 0 when unlimited
	Growth float64 `json:"growth,omitempty"` // bytes a second, over the last window
	// Kill is how soon its memory forecast (forecastMemory) reached the limit:
	// the leak's kills come at the GC's peak, between heartbeats. 0 when it wasn't heading there.
	Kill time.Duration `json:"kill,omitempty"`
}

// memDigest is the container's memory now, nil when it isn't known.
func (in *Instance) memDigest(rt RuntimeStats) *memDigest {
	if rt.MemUsed == 0 {
		return nil
	}
	m := &memDigest{Used: rt.MemUsed, Growth: math.Round(rt.MemGrowth)}
	if !rt.memKills() { // GOMEMLIMIT only makes the collector work: it can't explain a death
		return m
	}
	m.Limit = rt.MemLimit
	if k := forecastMemory(rt).kill(); k >= 0 {
		m.Kill = max(k, time.Millisecond) // at the limit now isn't "not heading there"
	}
	return m
}

func (in *Instance) health() *wireHealth {
	s := in.summary()
	h := &wireHealth{At: time.Now().UnixNano(), CPUUsed: round2(s.CPUUsed), CPULimit: s.CPULimit, Throttled: round2(s.Throttled), Frozen: s.Frozen, SLOs: s.SLOs, Held: s.Held,
		Kept: in.kept(), Dropped: in.stillDropped(s.Held)}
	in.mu.Lock()
	h.Changes, h.Leases = in.change, in.leaseN
	in.mu.Unlock()
	if s.Last != "" {
		h.Last, h.LastAt = s.Last, time.Now().Add(-s.LastAgo).UnixNano()
	}
	h.Mem, h.Started = s.Mem, in.started.UnixNano()
	if rep := in.lastRollout.Load(); rep != nil {
		if at, err := time.Parse(time.RFC3339, rep.At); err == nil && time.Since(at) < 30*time.Minute {
			h.Rollout = rep
		}
	}
	return h
}

// summary is the digest read as a summary, without the knobs it doesn't carry.
func (h *wireHealth) summary(m Member) *peerSummary {
	s := &peerSummary{ID: m.ID, Name: m.Name, Build: m.Build, Frozen: h.Frozen, CPUUsed: h.CPUUsed, CPULimit: h.CPULimit, Throttled: h.Throttled, SLOs: h.SLOs, Held: h.Held, Last: h.Last, Mem: h.Mem}
	if h.LastAt != 0 { // by this clock less its: the sender's clock may differ, by no more than the skew the ring allows
		s.LastAgo = max(time.Since(time.Unix(0, h.LastAt)), time.Millisecond)
	}
	return s
}

// healthOf is what gossip last carried about a member, and how old it is.
func (r *ring) healthOf(id string) (*wireHealth, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.direct[id]; ok && time.Since(d.at) < deadAfter { // first-hand, histograms and all
		return d.h, true
	}
	if e := r.members[id]; e != nil && e.m.Health != nil {
		return e.m.Health, true
	}
	return nil, false
}

type wireFreeze struct {
	Frozen bool   `json:"frozen"`
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
	Since  int64  `json:"since"` // Unix nanoseconds
	Epoch  uint64 `json:"epoch"`
	On     string `json:"on"`
	Origin string `json:"origin"`
}

func freezeWire(f FreezeState) *wireFreeze {
	if f.Epoch == 0 {
		return nil
	}
	return &wireFreeze{Frozen: f.Frozen, By: f.By.String(), Reason: f.Reason, Since: f.Since.UnixNano(), Epoch: f.Epoch, On: f.On, Origin: f.origin}
}

// parseWriter reads a writer as Writer.String writes it: "human:sam".
func parseWriter(s string) (Writer, bool) {
	kind, id, ok := strings.Cut(s, ":")
	for k, n := range kindNames {
		if n == kind && ok && id != "" {
			return Writer{Kind: k, ID: id}, true
		}
	}
	return Writer{}, false
}

func (w *wireFreeze) state() (FreezeState, bool) {
	by, ok := parseWriter(w.By)
	if !ok {
		return FreezeState{}, false
	}
	return FreezeState{Frozen: w.Frozen, By: by, Reason: capReason(w.Reason), Since: time.Unix(0, w.Since),
		Epoch: w.Epoch, On: w.On, origin: w.Origin}, true
}

type gossipMsg struct {
	Members []wireMember `json:"members"`        // the sender's list, itself first
	Left    []leftWord   `json:"left,omitempty"` // the last words of members that left cleanly lately
}

// leftWord is a member's last digest as it left cleanly, relayed by its peers
// for keepDead: a process restarted under its name takes over from it as from
// a note (succeed), when it has no note to read.
type leftWord struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	At     int64       `json:"at"` // when it left, Unix nanoseconds
	Health *wireHealth `json:"health,omitempty"`
}

type memberEntry struct {
	m    wireMember
	rose time.Time // when its heartbeat last rose
	seen time.Time // when this instance first heard of it
}

type ring struct {
	rollouts map[string]*rolloutReport // by coordinator process: the newest report gossip carried from each; under mu
	in       *Instance
	key      []byte
	seeds    []string
	client   *http.Client
	mcp      http.Handler // this instance's MCP, tokens and all, for calls members forward

	mu       sync.Mutex
	self     wireMember
	members  map[string]*memberEntry // by ID, not self
	seedNext int
	refused  map[string]*refusal  // peer messages refused, by reason, for Warnings
	left     map[string]time.Time // members gone for good, by ID (they left, were replaced or forgotten): gossip doesn't bring them back
	wait     time.Duration        // how long a fleet question waits for each member
	seedErr  map[string]string    // seeds whose last exchange failed, and why: for an instance left alone
	nonces   map[string]time.Time // signed requests seen, by nonce: each is good once
	restarts map[string][]restart // by name: members replaced under their name, for the fleet view
	direct   map[string]heard     // by ID: each member's digest as it last sent it itself, histograms and all
	leaving  map[string]restart   // by name: members that left cleanly, a restart if they come back under it
	words    map[string]leftWord  // by ID: last words of members that left cleanly, relayed for keepDead
	upTo     map[string]uint64    // by ID: the heartbeat a member left or was forgotten at; first-hand gossip past it is that member back

	fleetMu    sync.Mutex // the last fleet comparison, for the page, which asks often
	fleetAt    time.Time
	fleetRep   FleetReport
	fleetBusy  bool            // a refresh is running
	fleetDone  chan struct{}   // closed when the running refresh ends
	fleetShut  bool            // the ring is stopping: no new background refreshes
	fleetAsked time.Time       // the last read: refreshes run while someone looks
	bg         sync.WaitGroup  // background refreshes, waited for at shutdown
	ctx        context.Context // the ring's life: background refreshes end with it
}

// The page's fleet comparison is kept fresh in the background while anyone
// is looking, so a read never waits on a slow member and is never much older
// than fleetReuse; a read after a quiet spell (coldAfter) waits once.
const (
	fleetReuse  = time.Second
	fleetLooked = 30 * time.Second // how long after the last read the background refresh keeps going
	coldAfter   = 10 * time.Second
)

func (r *ring) cachedFleet(context.Context) FleetReport {
	r.fleetMu.Lock()
	r.fleetAsked = time.Now()
	rep, at, busy := r.fleetRep, r.fleetAt, r.fleetBusy
	if busy && at.IsZero() { // the first comparison is running: an empty one would read as a lone instance
		done := r.fleetDone
		r.fleetMu.Unlock()
		<-done
		r.fleetMu.Lock()
		rep = r.fleetRep
		r.fleetMu.Unlock()
		return rep
	}
	if busy || time.Since(at) < coldAfter {
		r.fleetMu.Unlock()
		r.kickFleet()
		return rep
	}
	r.fleetBusy, r.fleetDone = true, make(chan struct{})
	r.fleetMu.Unlock()
	return r.refreshFleet()
}

// kickFleet refreshes the comparison in the background if it is due and none is running.
func (r *ring) kickFleet() {
	r.fleetMu.Lock()
	if r.fleetShut || r.fleetBusy || time.Since(r.fleetAt) < fleetReuse || time.Since(r.fleetAsked) >= fleetLooked {
		r.fleetMu.Unlock()
		return
	}
	r.fleetBusy, r.fleetDone = true, make(chan struct{})
	r.bg.Add(1) // under fleetMu, so it is ordered against the shutdown's Wait
	r.fleetMu.Unlock()
	go func() { defer r.bg.Done(); r.refreshFleet() }()
}

func (r *ring) refreshFleet() FleetReport {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	rep := r.in.Fleet(ctx)
	r.fleetMu.Lock()
	r.fleetRep, r.fleetAt, r.fleetBusy = rep, time.Now(), false
	close(r.fleetDone)
	r.fleetMu.Unlock()
	return rep
}

// heard is a digest received from the member itself, and when.
type heard struct {
	h  *wireHealth
	at time.Time
}

// death is a restart to tell: whose name, the record, and how many restarts
// its name has had lately, this one included.
type death struct {
	name string
	r    restart
	n    int
}

// restart is a member replaced under its name, and what its former self held.
type restart struct {
	at   time.Time
	lost []Held

	// The former self, for telling its death: its ID, its last digest, when
	// its heartbeat last rose (about when it died), and whether it left cleanly.
	id    string
	last  *wireHealth
	died  time.Time
	clean bool
}

// restartsOf says how often a name restarted lately, how long ago the last
// time was, and what that restart dropped.
func (r *ring) restartsOf(name string) (int, time.Duration, restart) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rs := r.restarts[name]
	if len(rs) == 0 {
		return 0, 0, restart{}
	}
	last := rs[len(rs)-1]
	return len(rs), time.Since(last.at), last
}

type refusal struct {
	n    int
	why  string    // the latest refusal of this kind, in full
	from string    // its sender
	at   time.Time // when; a kind not seen for keepDead drops out
}

// refusalKind groups refusals, so one peer's drifting clock can't crowd out a wrong key.
func refusalKind(err error) string {
	for _, k := range []string{"service", "clock", "key", "replayed"} {
		if strings.Contains(err.Error(), k) {
			return k
		}
	}
	return "other"
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func defaultName() string {
	if n := os.Getenv("VIS_INSTANCE_NAME"); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "instance"
}

// Members lists the instances of the service this one knows, itself first,
// then by name. Without peers it is the only one.
func (in *Instance) Members() []Member {
	self := Member{ID: in.id, Name: in.name, Service: in.service, Build: in.build, State: "alive", Self: true}
	r := in.ring.Load()
	if r == nil {
		return []Member{self}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	self.Addr = r.self.Addr
	now := time.Now()
	self.rose = now
	var out []Member
	for _, e := range r.members {
		q := now.Sub(e.rose)
		if q >= keepDead {
			continue
		}
		m := Member{ID: e.m.ID, Name: e.m.Name, Addr: e.m.Addr, Service: e.m.Service, Build: e.m.Build, State: stateAfter(q), Quiet: q, seen: e.seen, rose: e.rose}
		out = append(out, m)
	}
	// A restarted member and its former self: the former, quiet for two gossip
	// rounds while its name is heard fresh, is listed no more. The ring calls
	// it replaced, and tells its death, only once it is suspect.
	fresh := map[string]bool{self.Name: true}
	for _, m := range out {
		if m.Quiet < 2*gossipEvery {
			fresh[m.Name] = true
		}
	}
	out = slices.DeleteFunc(out, func(m Member) bool { return m.Quiet >= 2*gossipEvery && fresh[m.Name] })
	slices.SortFunc(out, func(a, b Member) int { return strings.Compare(a.Name+a.ID, b.Name+b.ID) })
	return append([]Member{self}, out...)
}

func stateAfter(quiet time.Duration) string {
	switch {
	case quiet < suspectAfter:
		return "alive"
	case quiet < deadAfter:
		return "suspect"
	}
	return "dead"
}

// startRing joins the ring and gossips until ctx ends.
func (in *Instance) startRing(ctx context.Context, o *serveOptions, done chan struct{}) error {
	ln := o.peerLn
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", o.peerAddr); err != nil {
			return fmt.Errorf("vis: peers: %w (set VIS_PEER_LISTEN, or vis.PeerAt, to listen elsewhere)", err)
		}
	}
	wait := o.fleetWait
	if wait <= 0 {
		wait = peerTimeout
	}
	dial := o.dial
	if dial == nil {
		// A starved member is slow even to accept, so a dial waits as long as a
		// change does. Its own bound, since the transport lets a dial outlive the
		// request that started it: a peer that drops SYNs would otherwise pile
		// up dials for the OS's minute or two.
		dial = (&net.Dialer{Timeout: max(15*time.Second, 3*wait)}).DialContext
	}
	tr := &http.Transport{DialContext: dial, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	r := &ring{in: in, key: []byte(o.key), seeds: o.seeds, client: &http.Client{Transport: tr, Timeout: max(peerTimeout, wait)},
		members: map[string]*memberEntry{}, left: map[string]time.Time{}, seedErr: map[string]string{}, mcp: o.mcpHandler, wait: wait, ctx: ctx}
	r.self = wireMember{ID: in.id, Name: in.name, Addr: advertised(o.advertise, ln.Addr()), Service: in.service, Build: in.build}
	in.ring.Store(r)

	srv := &http.Server{Handler: http.HandlerFunc(r.serve), ReadHeaderTimeout: peerTimeout, ReadTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go func() {
		t := time.NewTicker(gossipEvery)
		defer t.Stop()
		var wg sync.WaitGroup
		for {
			select {
			case <-ctx.Done():
				wg.Wait()
				if !in.skipLeave.Load() {
					r.leave() // so the fleet doesn't take a clean stop for an instance gone wrong
				}
				srv.Close()
				r.fleetMu.Lock()
				r.fleetShut = true
				r.fleetMu.Unlock()
				r.bg.Wait()
				close(done)
				tr.CloseIdleConnections()
				return
			case <-t.C:
				r.kickFleet()
				h := r.in.health() // before r.mu: it reads the instance
				r.mu.Lock()
				r.self.Health = h
				r.mu.Unlock()
				for _, addr := range r.targets() {
					wg.Go(func() { r.send(ctx, addr) })
				}
			}
		}
	}()
	in.logger().Info("vis: peers", "listen", ln.Addr().String(), "advertise", r.self.Addr, "seeds", strings.Join(o.seeds, ","), "name", in.name)
	return nil
}

// advertised is where peers reach this instance: the address given, or the
// listener's when it names a host, or the hostname with the listener's port.
func advertised(given string, a net.Addr) string {
	if given != "" {
		return given
	}
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return a.String()
	}
	h, err := os.Hostname()
	if err != nil {
		h = "localhost"
	}
	return net.JoinHostPort(h, port)
}

// targets are this round's: one random live member and the next seed.
func (r *ring) targets() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.self.Heartbeat++
	now := time.Now()
	for id, at := range r.left {
		if now.Sub(at) >= keepDead {
			delete(r.left, id)
		}
	}
	for id, d := range r.direct {
		if now.Sub(d.at) >= keepDead {
			delete(r.direct, id)
		}
	}
	for id := range r.upTo {
		if _, ok := r.left[id]; !ok {
			delete(r.upTo, id)
		}
	}
	for name, lv := range r.leaving {
		if now.Sub(lv.at) >= keepDead {
			delete(r.leaving, name)
		}
	}
	for id, w := range r.words {
		if now.Sub(time.Unix(0, w.At)) >= keepDead {
			delete(r.words, id)
		}
	}
	for name, rs := range r.restarts {
		rs = slices.DeleteFunc(rs, func(x restart) bool { return now.Sub(x.at) >= keepDead })
		if len(rs) == 0 {
			delete(r.restarts, name)
		} else {
			r.restarts[name] = rs
		}
	}
	var live []string
	for id, e := range r.members {
		q := now.Sub(e.rose)
		if q >= keepDead {
			delete(r.members, id)
			r.left[id] = now
			continue
		}
		if q < deadAfter {
			live = append(live, e.m.Addr)
		}
	}
	var out []string
	// Now and then, one member listed dead: a partition that healed while
	// every seed was down is found again this way, not only through a seed.
	if r.self.Heartbeat%5 == 0 {
		var dead []string
		for _, e := range r.members {
			if q := now.Sub(e.rose); q >= deadAfter && q < keepDead {
				dead = append(dead, e.m.Addr)
			}
		}
		if len(dead) > 0 {
			slices.Sort(dead)
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(dead))))
			out = append(out, dead[n.Int64()])
		}
	}
	if len(live) > 0 {
		slices.Sort(live) // map order isn't random enough to rely on, nor fixed enough to test
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(live))))
		out = append(out, live[n.Int64()])
	}
	for range r.seeds {
		s := r.seeds[r.seedNext%len(r.seeds)]
		r.seedNext++
		if s != r.self.Addr && !slices.Contains(out, s) {
			out = append(out, s)
			break
		}
	}
	return out
}

func (r *ring) list() gossipMsg {
	f := freezeWire(r.in.FreezeState()) // before r.mu: in.mu is never taken under it
	r.mu.Lock()
	defer r.mu.Unlock()
	r.self.Freeze = f
	msg := gossipMsg{Members: []wireMember{r.self}}
	now := time.Now()
	for _, e := range r.members {
		m := e.m
		m.Quiet = now.Sub(e.rose).Milliseconds()
		if m.Health != nil { // relayed: its p99s, not its histograms, which would grow with the fleet squared
			h := *m.Health
			h.SLOs = make([]peerSLO, len(m.Health.SLOs))
			for i, s := range m.Health.SLOs {
				s.Hist = nil
				h.SLOs[i] = s
			}
			m.Health = &h
		}
		msg.Members = append(msg.Members, m)
	}
	for _, w := range r.words {
		if w.Health != nil {
			h := *w.Health
			h.SLOs = nil // what a successor reads of it needs no latencies
			w.Health = &h
		}
		msg.Left = append(msg.Left, w)
	}
	return msg
}

// merge takes what a peer knows: for each instance, the higher heartbeat
// wins. A freeze newer than this instance's, from any member, is adopted.
func (r *ring) merge(msg gossipMsg) {
	var newest *FreezeState
	var former []leftWord // this name's former selves that left cleanly: taken over from after the lock
	r.mu.Lock()
	now := time.Now()
	for _, w := range msg.Left {
		if w.ID == "" || w.ID == r.self.ID || now.Sub(time.Unix(0, w.At)) >= keepDead {
			continue
		}
		if _, ok := r.words[w.ID]; !ok {
			if r.words == nil {
				r.words = map[string]leftWord{}
			}
			r.words[w.ID] = w
		}
		if w.Name == r.self.Name {
			former = append(former, w)
		}
	}
	if len(msg.Members) > 0 && msg.Members[0].Health != nil && msg.Members[0].ID != r.self.ID { // the sender, first-hand
		if r.direct == nil {
			r.direct = map[string]heard{}
		}
		r.direct[msg.Members[0].ID] = heard{msg.Members[0].Health, now}
	}
	for i, m := range msg.Members {
		if m.Health != nil {
			r.noteRollout(m.Health.Rollout)
		}
		if m.Freeze != nil {
			if st, ok := m.Freeze.state(); ok && (newest == nil || newerFreeze(st, *newest)) {
				newest = &st
			}
		}
		if hb, ok := r.upTo[m.ID]; ok && i == 0 && m.Heartbeat > hb { // first-hand, and alive since: it's back
			delete(r.left, m.ID)
			delete(r.upTo, m.ID)
		}
		if _, gone := r.left[m.ID]; m.ID == "" || m.ID == r.self.ID || m.Service != r.self.Service || gone {
			continue
		}
		rose := now.Add(-time.Duration(m.Quiet) * time.Millisecond) // when the heartbeat last rose, as the sender knew it
		if now.Sub(rose) >= keepDead {
			continue
		}
		if e, ok := r.members[m.ID]; !ok || m.Heartbeat > e.m.Heartbeat {
			m.Quiet = 0
			seen := now
			if ok {
				seen = e.seen
			}
			r.members[m.ID] = &memberEntry{m: m, rose: rose, seen: seen}
			if lv, back := r.leaving[m.Name]; !ok && back { // it left cleanly, and is back: restarted
				delete(r.leaving, m.Name)
				if r.restarts == nil {
					r.restarts = map[string][]restart{}
				}
				r.restarts[m.Name] = append(r.restarts[m.Name], lv)
			}
		}
	}
	// An instance restarted under its name replaces its quiet former self, this
	// one's included. Two alive under one name are left be: that's misconfigured, and both serve.
	var deaths []death // told after the lock, which telling doesn't need
	alive := map[string]bool{r.self.Name: true}
	for _, e := range r.members {
		if now.Sub(e.rose) < suspectAfter {
			alive[e.m.Name] = true
		}
	}
	for id, e := range r.members {
		if now.Sub(e.rose) >= suspectAfter && alive[e.m.Name] {
			delete(r.members, id)
			r.left[id] = now // replaced: a stale relay of it mustn't bring it back
			var held []Held
			if e.m.Health != nil {
				held = e.m.Health.Held
			}
			if r.restarts == nil {
				r.restarts = map[string][]restart{}
			}
			rs := restart{at: now, lost: held, id: id, last: e.m.Health, died: e.rose}
			r.restarts[e.m.Name] = append(r.restarts[e.m.Name], rs)
			deaths = append(deaths, death{name: e.m.Name, r: rs, n: len(r.restarts[e.m.Name])})
		}
	}
	r.mu.Unlock()
	for _, w := range former {
		r.in.succeed(w.Name, restart{id: w.ID, last: w.Health, died: time.Unix(0, w.At), clean: true}, 0)
	}
	for _, d := range deaths {
		if d.name == r.self.Name {
			r.in.succeed(d.name, d.r, d.n)
		} else {
			r.in.noteDeath(d.name, d.r, d.n, nil)
		}
	}
	if newest != nil {
		r.in.adoptFreeze(*newest)
	}
}

// Forget drops a member that is gone for good (scaled down, or replaced
// under a new name, as a Kubernetes pod is) from the whole fleet, so it stops
// reading as unaccounted for. Only a person may, and only a member that is
// dead (not heard from for 15 s). If it was only cut off, its own gossip
// brings it back once it is heard from again.
func (in *Instance) Forget(by Writer, name, reason string) error {
	if by.Kind != Human {
		return fmt.Errorf("%w: only a person can forget a member (%v)", ErrNotAllowed, by)
	}
	r := in.ring.Load()
	if r == nil {
		return fmt.Errorf("vis: this instance has no fleet")
	}
	r.mu.Lock()
	var ids, live []string
	hbs := map[string]uint64{}
	for id, e := range r.members {
		q := time.Since(e.rose)
		hbs[id] = e.m.Heartbeat
		switch {
		case e.m.Name == name && q < deadAfter: // a suspect one may only be cut off for a moment
			r.mu.Unlock()
			return fmt.Errorf("vis: %s was heard from %s ago: forget only a member that is dead, not heard from for %s", name, short(q), short(deadAfter))
		case e.m.Name == name:
			ids = append(ids, id)
		case q < deadAfter:
			live = append(live, e.m.Addr)
		}
	}
	if len(ids) == 0 {
		r.mu.Unlock()
		return fmt.Errorf("vis: no member named %q", name)
	}
	if r.upTo == nil {
		r.upTo = map[string]uint64{}
	}
	for _, id := range ids {
		delete(r.members, id)
		r.left[id] = time.Now()
		r.upTo[id] = hbs[id]
	}
	r.mu.Unlock()
	in.audit(slog.LevelInfo, "vis.forget", slog.String("actor", by.String()), slog.String("member", name), slog.String("reason", capReason(reason)))
	ctx, cancel := context.WithTimeout(context.Background(), peerTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, a := range live { // the others drop it too, as if it had left
		for _, id := range ids {
			wg.Go(func() { r.post(ctx, a, "leave", leaveMsg{ID: id, Heartbeat: hbs[id]}, &struct{}{}) })
		}
	}
	wg.Wait()
	return nil
}

type leaveMsg struct {
	ID        string      `json:"id"`
	Heartbeat uint64      `json:"hb,omitempty"`     // its last: gossip from it past this is it back
	Health    *wireHealth `json:"health,omitempty"` // its last word, from the member leaving itself (not a Forget): what a successor takes over
}

// leave tells every member heard from lately that this one is going, briefly:
// a member that misses it sees this one go silent instead, and lists it dead.
func (r *ring) leave() {
	r.mu.Lock()
	var addrs []string
	for _, e := range r.members {
		if time.Since(e.rose) < deadAfter {
			addrs = append(addrs, e.m.Addr)
		}
	}
	id, hb := r.self.ID, r.self.Heartbeat
	r.mu.Unlock()
	h := r.in.health()
	h.SLOs = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, a := range addrs {
		wg.Go(func() { r.post(ctx, a, "leave", leaveMsg{ID: id, Heartbeat: hb, Health: h}, &struct{}{}) })
	}
	wg.Wait()
}

func (r *ring) send(ctx context.Context, addr string) {
	var reply gossipMsg
	err := r.post(ctx, addr, "gossip", r.list(), &reply)
	if slices.Contains(r.seeds, addr) {
		r.mu.Lock()
		if err != nil {
			r.seedErr[addr] = err.Error()
		} else {
			delete(r.seedErr, addr)
		}
		r.mu.Unlock()
	}
	if err != nil {
		return // the member's heartbeat stops rising, and that is how the ring learns
	}
	r.merge(reply)
}

// alone says why an instance with seeds has found no one: every seed it
// tried failed, and no member is alive. Empty otherwise.
func (r *ring) alone() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seeds) == 0 || len(r.seedErr) == 0 {
		return ""
	}
	for _, e := range r.members {
		if time.Since(e.rose) < deadAfter {
			return ""
		}
	}
	var why []string
	for _, s := range r.seeds {
		if e, ok := r.seedErr[s]; ok {
			why = append(why, s+": "+e)
		}
	}
	return strings.Join(why, "; ")
}

// post sends a signed request to a peer and decodes its answer.
func (r *ring) post(ctx context.Context, addr, what string, body, reply any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+peerPath+what, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.sign(req, b)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("peer %s: %s: %s", addr, resp.Status, strings.TrimSpace(string(msg)))
	}
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxPeerBody))
	if err != nil {
		return err
	}
	// Only a member with the key answers this request: whatever else answers
	// at the address mustn't add members or move the freeze.
	if !hmac.Equal([]byte(resp.Header.Get("Vis-Peer-Reply")), []byte(r.mac("reply "+req.Header.Get("Vis-Peer-Nonce"), req.URL.Path, rb))) {
		return fmt.Errorf("peer %s: the reply wasn't signed with this ring's key", addr)
	}
	return json.Unmarshal(rb, reply)
}

// sign puts the time, a nonce and an HMAC on req, covering everything a
// member acts on: the method, the member it's for, the path, the person a
// page names, the caller's token and the body. Set those headers first.
func (r *ring) sign(req *http.Request, body []byte) {
	at, nonce := strconv.FormatInt(time.Now().Unix(), 10), newID()
	req.Header.Set("Vis-Peer-Time", at)
	req.Header.Set("Vis-Peer-Nonce", nonce)
	req.Header.Set("Vis-Peer-Service", r.self.Service)
	req.Header.Set("Vis-Peer-Sig", r.mac(signedPart(req, at, nonce), req.URL.Path, body))
}

// signedPart is what a signature covers beyond the path and the body.
func signedPart(req *http.Request, at, nonce string) string {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	return strings.Join([]string{at, nonce, req.Method, host, req.Header.Get("Vis-UI-As"), req.Header.Get("Authorization")}, "\n")
}

func (r *ring) mac(at, path string, body []byte) string {
	h := hmac.New(sha256.New, r.key)
	fmt.Fprintf(h, "%s\n%s\n", at, path)
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// fresh records a nonce, saying whether it is new: a signed request is good
// once. Nonces are kept as long as a signature is valid.
func (r *ring) fresh(nonce string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.nonces == nil {
		r.nonces = map[string]time.Time{}
	}
	if len(r.nonces) > 4096 { // drop the expired ones now and then
		for n, at := range r.nonces {
			if now.Sub(at) > 2*peerSkew {
				delete(r.nonces, n)
			}
		}
	}
	if _, seen := r.nonces[nonce]; seen {
		return false
	}
	r.nonces[nonce] = now
	return true
}

var errPeerRefused = errors.New("refused")

// verifyHeaders checks what costs nothing: the service, and that the time is recent.
func (r *ring) verifyHeaders(req *http.Request) error {
	if s := req.Header.Get("Vis-Peer-Service"); s != r.self.Service {
		return fmt.Errorf("%w: from service %q, not %q", errPeerRefused, s, r.self.Service)
	}
	sec, err := strconv.ParseInt(req.Header.Get("Vis-Peer-Time"), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: no time", errPeerRefused)
	}
	if d := time.Since(time.Unix(sec, 0)); d > peerSkew || d < -peerSkew {
		return fmt.Errorf("%w: its clock is %s off from this one's", errPeerRefused, d.Round(time.Second))
	}
	return nil
}

// verify checks a peer's request is signed with the key, recent, from this service, and new.
func (r *ring) verify(req *http.Request, body []byte) error {
	if err := r.verifyHeaders(req); err != nil {
		return err
	}
	at := req.Header.Get("Vis-Peer-Time")
	nonce := req.Header.Get("Vis-Peer-Nonce")
	if nonce == "" || !hmac.Equal([]byte(req.Header.Get("Vis-Peer-Sig")), []byte(r.mac(signedPart(req, at, nonce), req.URL.Path, body))) {
		return fmt.Errorf("%w: not signed with this ring's key (VIS_CLUSTER_KEY differs)", errPeerRefused)
	}
	if !r.fresh(nonce) {
		return fmt.Errorf("%w: a signed request replayed", errPeerRefused)
	}
	return nil
}

// reply answers a peer's request with v, signed for that request.
func (r *ring) reply(w http.ResponseWriter, req *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Vis-Peer-Reply", r.mac("reply "+req.Header.Get("Vis-Peer-Nonce"), req.URL.Path, b))
	w.Write(b)
}

func (r *ring) serve(w http.ResponseWriter, req *http.Request) {
	// The cheap checks before reading a body: the port is reachable by anyone on the network.
	if err := r.verifyHeaders(req); err != nil {
		r.refuse(w, req, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxPeerBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// after the body: until it is read, a client hanging up never cancels the request
	d := time.Duration(r.in.stallPeers.Load())
	if req.URL.Path == peerPath+"mcp" {
		d += time.Duration(r.in.stallCalls.Load())
	}
	if d > 0 {
		select {
		case <-time.After(d):
		case <-req.Context().Done():
			return
		}
	}
	if err := r.verify(req, body); err != nil {
		r.refuse(w, req, err)
		return
	}
	r.served(w, req, body)
}

// refuse answers a peer request that failed a check, and keeps the reason for Warnings.
func (r *ring) refuse(w http.ResponseWriter, req *http.Request, err error) {
	r.mu.Lock()
	if r.refused == nil {
		r.refused = map[string]*refusal{}
	}
	k := refusalKind(err)
	f := r.refused[k]
	if f == nil {
		f = &refusal{}
		r.refused[k] = f
	}
	f.n++
	f.why, f.from, f.at = err.Error(), req.RemoteAddr, time.Now()
	r.mu.Unlock()
	http.Error(w, "vis: peer "+err.Error(), http.StatusUnauthorized)
}

// served dispatches a verified peer request.
func (r *ring) served(w http.ResponseWriter, req *http.Request, body []byte) {
	if p, ok := strings.CutPrefix(req.URL.Path, peerPath+"ui/"); ok { // a member's page, forwarding its person
		as, ok := parseWriter(req.Header.Get("Vis-UI-As"))
		if !ok || as.Kind != Human {
			http.Error(w, "vis: a forwarded page request names the person acting (Vis-UI-As: human:<id>)", http.StatusBadRequest)
			return
		}
		req.URL.Path = "/" + p
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		r.in.uiAs(as).ServeHTTP(w, req)
		return
	}
	switch strings.TrimPrefix(req.URL.Path, peerPath) {
	case "leave":
		var msg leaveMsg
		if err := json.Unmarshal(body, &msg); err != nil || msg.ID == "" {
			http.Error(w, "vis: leave needs an id", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		if e := r.members[msg.ID]; e != nil { // back under its name soon, and it's a restart: a deploy, say
			var held []Held
			if e.m.Health != nil {
				held = e.m.Health.Held
			}
			if r.leaving == nil {
				r.leaving = map[string]restart{}
			}
			r.leaving[e.m.Name] = restart{at: time.Now(), lost: held, id: msg.ID, last: e.m.Health, died: time.Now(), clean: true}
			if msg.Health != nil {
				if r.words == nil {
					r.words = map[string]leftWord{}
				}
				r.words[msg.ID] = leftWord{ID: msg.ID, Name: e.m.Name, At: time.Now().UnixNano(), Health: msg.Health}
			}
		}
		delete(r.members, msg.ID)
		r.left[msg.ID] = time.Now()
		if msg.Heartbeat > 0 {
			if r.upTo == nil {
				r.upTo = map[string]uint64{}
			}
			r.upTo[msg.ID] = max(r.upTo[msg.ID], msg.Heartbeat)
		}
		r.mu.Unlock()
		r.reply(w, req, struct{}{})
	case "summary":
		if d := time.Duration(r.in.stallSummary.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-req.Context().Done():
				return
			}
		}
		r.reply(w, req, r.in.summary())
	case "mcp": // a tool call a member forwards, under its caller's token, which r.mcp checks
		if r.mcp == nil {
			http.Error(w, "vis: this instance serves no MCP", http.StatusNotFound)
			return
		}
		req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		r.mcp.ServeHTTP(w, req)
	case "gossip":
		var msg gossipMsg
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.merge(msg)
		r.reply(w, req, r.list())
	default:
		http.NotFound(w, req)
	}
}

// refusals says why peer messages were refused, one line per reason.
func (r *ring) refusals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k, f := range r.refused {
		if time.Since(f.at) >= keepDead {
			delete(r.refused, k)
			continue
		}
		out = append(out, fmt.Sprintf("%d %s, the last from %s", f.n, f.why, f.from))
	}
	slices.Sort(out)
	return out
}
