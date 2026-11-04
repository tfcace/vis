package vet_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// vet runs the real go tool vis vet on packages written into the module, so
// they import the real vis, and returns its output.
func vet(t *testing.T, pkgs map[string]map[string]string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and vets packages")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	var patterns []string
	for name, files := range pkgs {
		p := filepath.Join(dir, name)
		os.MkdirAll(p, 0o755)
		for f, src := range files {
			if err := os.WriteFile(filepath.Join(p, f), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		patterns = append(patterns, "./internal/vet/"+filepath.ToSlash(p))
	}
	cmd := exec.Command("go", append([]string{"run", "./cmd/vis", "vet"}, patterns...)...)
	cmd.Dir = root
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func has(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("vet output lacks %q:\n%s", w, out)
		}
	}
}

const imports = `package a

import (
	"net/http"
	"time"

	"github.com/tfcace/vis"
)

`

// The mistakes that compile and run but are wrong, each reported where it is,
// with how to fix it.
func TestVetFindsTheMistakesThatCompile(t *testing.T) {
	out := vet(t, map[string]map[string]string{
		"a": {"a.go": imports + `// vis:knob range=1..9 default=5 pull=agent
var Gen vis.Int // Declared by vis gen.

var Copied = Gen

var Hand = vis.NewInt("vet_hand", vis.Range(1, 9), vis.Default(5))

var Stale = Hand.Now()

var Bad = vis.NewRule(vis.RuleSpec{Name: "bad", Knob: Hand, Watch: vis.SchedP99, Over: vis.Fixed(0.5), Step: -1, Lease: time.Minute})

var KeySLO = vis.NewSLO("GET /k", vis.P99(50*time.Millisecond))

func routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k/{key}", h)
	return mux
}

func h(http.ResponseWriter, *http.Request) {}

var Wide = vis.NewRule(vis.RuleSpec{Name: "wide", Knob: Small, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Step: -20, Lease: time.Minute})

var Far = vis.NewRule(vis.RuleSpec{Name: "far", Knob: Small, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Do: vis.SetTo(50), Lease: time.Minute})

var Idle = vis.NewRule(vis.RuleSpec{Name: "idle", Knob: Small, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Lease: time.Minute})

var Small = vis.NewInt("vet_small", vis.Range(1, 9), vis.Default(5), vis.Pull(vis.Rule))

var Timeout = vis.NewDuration("vet_timeout", vis.Range(time.Millisecond, time.Second), vis.Default(100*time.Millisecond), vis.Pull(vis.Rule))

var Tiny = vis.NewRule(vis.RuleSpec{Name: "tiny", Knob: Timeout, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Step: 5, Lease: time.Minute})

var Big = vis.NewInt("vet_big", vis.Range(1, 512), vis.Default(64), vis.Pull(vis.Rule))

var Lurchy = vis.NewRule(vis.RuleSpec{Name: "lurchy", Knob: Big, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Step: -16, Lease: time.Minute})

var Gone = vis.NewRule(vis.RuleSpec{Name: "gone", Knob: Big, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Do: vis.StepPercent(-100), Lease: time.Minute})

var Watcher = vis.NewRule(vis.RuleSpec{Name: "watcher", Knob: Small, Watch: vis.RouteP99("GET /kk"), Over: vis.Fixed(time.Millisecond), Step: -1, Lease: time.Minute})
`},
		"b": {"b.go": "package b\n\nimport \"github.com/tfcace/vis\"\n\n// vis:knob default=5\nvar K vis.Int\n"},
		"e": {"e.go": "package e\n\nimport \"github.com/tfcace/vis\"\n\n// vis:knob range=1..512 default=64 pull=rule\n// vis:rule name=e_backoff when=\"sched.p99 > 5ms\" do=step(-16) lease=1m\nvar Batch vis.Int\n"},
	})
	has(t, out,
		"a.go:13:14: Gen is declared in vis gen's init",              // copied before it is declared
		"a.go:17:13: Hand.Now runs during package initialization",    // read at init
		"a.go:19:55: Hand doesn't let rules change it",               // a rule's knob without Pull(Rule)
		"a.go:19:88: vis.SchedP99 is a latency, so Over must be one", // a number against a latency
		`a.go:21:25: no handler in this package is registered under "GET /k"`,
		"has // vis: directives but no vis_gen.go", // directives never generated
		"b.go:5:1: // vis:knob K needs range=",     // a directive mistake, as vis gen reports it
		"a.go:31:126: a Step of -20 is wider than Small's range",
		"a.go:33:132: SetTo puts Small outside its range",
		"a.go:35:24: this rule has no action",
		"a.go:41:128: a Step of 5 on Timeout, a Duration knob, is 5ns",
		"a.go:45:128: a Step of -16 takes Big from 16 to 1 in one step, a 94% cut: Do: vis.StepPercent(-25) cuts by a quarter each time, or MaxDrift: 48 stops the rule at 16",
		`a.go:49:90: no handler in this package is registered under "GET /kk" (it registers "GET /k/{key}"), so this rule would watch a route that never sees a request`,
		"a.go:47:138: StepPercent(-100) cuts Big to nothing in one step, so vis.NewRule panics at startup",
		"e.go:6:1: // vis:rule e_backoff: do=step(-16) takes Batch from 16 to 1 in one step, a 94% cut: do=step(-25%) cuts by a quarter each time, or max_drift=48 stops the rule at 16",
		"this package has // vis: directives but no vis_gen.go", // advice doesn't stop the other checks
	)
}

// A generated file that no longer matches its directives is reported.
func TestVetFindsAStaleGeneratedFile(t *testing.T) {
	out := vet(t, map[string]map[string]string{
		"c": {
			"c.go":       "package c\n\nimport \"github.com/tfcace/vis\"\n\n// vis:knob range=1..9 default=6\nvar K vis.Int // A knob.\n",
			"vis_gen.go": "// Code generated by vis gen from this package's // vis: directives. DO NOT EDIT.\n\npackage c\n\nimport (\n\t\"github.com/tfcace/vis\"\n)\n\nfunc init() {\n\tvis.DeclareInt(&K, \"k\", vis.Range(1, 9), vis.Default(5), vis.About(\"A knob.\"))\n}\n",
		},
	})
	has(t, out, "vis_gen.go:3:1: vis_gen.go is out of date with the // vis: directives: run go generate")
}

// A package used as the guide says has nothing to report.
func TestVetIsQuietOnCorrectCode(t *testing.T) {
	out := vet(t, map[string]map[string]string{
		"d": {"d.go": imports + `var Q = vis.NewInt("vet_q", vis.Range(1, 100), vis.Default(95), vis.Pull(vis.Rule))

var _ = vis.NewRule(vis.RuleSpec{Name: "q_backoff", Knob: Q, Watch: vis.RouteP99("GET /i"), Over: vis.Fixed(400 * time.Millisecond), Step: -5, Lease: time.Minute})

var _ = vis.NewSLO("GET /i", vis.P99(500*time.Millisecond))

func routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /i", func(w http.ResponseWriter, r *http.Request) { _ = Q.From(r.Context()) })
	return mux
}

// Reads in function literals run later, not during initialization.
var serveQ = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = Q.From(r.Context()) })

func init() {
	http.HandleFunc("GET /t", func(w http.ResponseWriter, r *http.Request) { _ = Q.From(r.Context()) })
}

// A latency gauge is watched with a latency threshold.
var Wait = vis.NewLatency("vet_wait")

var _ = vis.NewRule(vis.RuleSpec{Name: "on_wait", Knob: Q, Watch: Wait.Measure(), Over: vis.Fixed(2 * time.Second), Do: vis.Reset(), Lease: time.Minute})

// A batch cut by a share of its value, never lurching near its floor.
var Batch = vis.NewInt("vet_batch", vis.Range(1, 512), vis.Default(64), vis.Pull(vis.Rule))

var _ = vis.NewRule(vis.RuleSpec{Name: "batch_backoff", Knob: Batch, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Do: vis.StepPercent(-25), Lease: time.Minute})
`},
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("vet reported on correct code:\n%s", out)
	}
}

// A handler built during package initialization takes a LogLevel knob before
// vis gen's init declares it, or reads it once: either way the level never
// changes. Build the handler in main, or in a func.
func TestVetFindsALogLevelTakenAtInit(t *testing.T) {
	out := vet(t, map[string]map[string]string{
		"l": {"l.go": `package l

import (
	"log/slog"
	"os"

	"github.com/tfcace/vis"
)

// vis:knob pull=agent
var Logging vis.LogLevel // What the service logs.

var Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: Logging}))

var Hand = vis.NewLogLevel("vet_hand_level")

var Once = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: Hand.Now()}))
`},
	})
	has(t, out,
		"l.go:13:79: Logging is declared in vis gen's init",
		"l.go:17:80: Hand.Now runs during package initialization",
	)
}

// A rule that steps its knob toward the end of the range where its default
// sits can never move it: vet says so, and that a 0 meaning none is a special
// value. A rule never sets the special value.
func TestVetFindsARuleThatCantMove(t *testing.T) {
	out := vet(t, map[string]map[string]string{
		"a": {"a.go": imports + `var Timeout = vis.NewDuration("vet_stuck", vis.Range(0, 5*time.Minute), vis.Default(time.Duration(0)), vis.Pull(vis.Rule))

var Stuck = vis.NewRule(vis.RuleSpec{Name: "stuck", Knob: Timeout, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Step: int64(-time.Second), Lease: time.Minute})

var Limit = vis.NewDuration("vet_limit", vis.Range(time.Second, 5*time.Minute), vis.Default(3*time.Second), vis.Special(time.Duration(0), "no limit"), vis.Pull(vis.Rule))

var Off = vis.NewRule(vis.RuleSpec{Name: "off", Knob: Limit, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Do: vis.SetTo(time.Duration(0)), Lease: time.Minute})

var Fine = vis.NewRule(vis.RuleSpec{Name: "fine", Knob: Limit, Watch: vis.SchedP99, Over: vis.Fixed(time.Millisecond), Step: int64(-time.Second), Lease: time.Minute})

var _ http.Handler
`},
	})
	has(t, out,
		`a.go:12:130: a Step of int64(-time.Second) moves Timeout down, but its default 0s is the bottom of its range, so this rule can't move it unless SetDefault starts it higher. If 0s means none, say so: vis.Special(time.Duration(0), "no limit") with a range from 1s`,
		`a.go:16:132: SetTo gives Limit its special value "no limit", which a rule never sets`,
	)
	if strings.Contains(out, "a.go:18") {
		t.Errorf("vet reported the rule that can move:\n%s", out)
	}
}
