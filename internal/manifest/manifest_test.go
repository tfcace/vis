package manifest_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tfcace/vis/internal/gen"
	"github.com/tfcace/vis/internal/manifest"
)

// write puts packages into the module, so they load with the real vis.
func write(t *testing.T, pkgs map[string]map[string]string) (root string, dirs map[string]string) {
	t.Helper()
	if testing.Short() {
		t.Skip("loads packages")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(".", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	dirs = map[string]string{}
	for name, files := range pkgs {
		p := filepath.Join(tmp, name)
		os.MkdirAll(p, 0o755)
		for f, src := range files {
			os.WriteFile(filepath.Join(p, f), []byte(src), 0o644)
		}
		dirs[name] = "./internal/manifest/" + filepath.ToSlash(p)
	}
	return root, dirs
}

const head = "package p\n\nimport (\n\t\"time\"\n\n\t\"github.com/tfcace/vis\"\n)\n\nvar _ time.Duration\n\n"

// What a reviewer reads: each change, the ones that give automation more room marked.
func TestDiffSaysWhatChangedForReview(t *testing.T) {
	root, dirs := write(t, map[string]map[string]string{
		"before": {"p.go": head + `var Cache = vis.NewInt("m_cache", vis.Range(1, 100), vis.Default(10), vis.Unit("MB"), vis.Pull(vis.Agent))
var Gone = vis.NewInt("m_gone", vis.Range(1, 9), vis.Default(5))
var Q = vis.NewInt("m_q", vis.Range(1, 100), vis.Default(95), vis.Pull(vis.Rule))
var _ = vis.NewSLO("GET /m", vis.P99(50*time.Millisecond))
var _ = vis.NewRule(vis.RuleSpec{Name: "m_backoff", Knob: Q, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Step: -5, Lease: time.Minute})
var _ = vis.NewRule(vis.RuleSpec{Name: "m_shrink", Knob: Q, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Step: -16, Lease: time.Minute})
`},
		"after": {"p.go": head + `var Cache = vis.NewInt("m_cache", vis.Range(1, 500), vis.Default(10), vis.Unit("MB"), vis.Pull(vis.Agent, vis.Rule))
var Q = vis.NewInt("m_q", vis.Range(1, 100), vis.Default(90), vis.Env("M_Q"), vis.Pull(vis.Rule))
var Hits = vis.NewGauge("m_hits")
var _ = vis.NewSLO("GET /m", vis.P99(80*time.Millisecond))
var _ = vis.NewRule(vis.RuleSpec{Name: "m_backoff", Knob: Q, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Do: vis.SetTo(70), Lease: 10 * time.Minute})
var _ = vis.NewRule(vis.RuleSpec{Name: "m_new", Knob: Cache, Watch: Hits.Measure(), Over: vis.Fixed(0.9), Step: 10, Lease: time.Minute, Shadow: true})
var _ = vis.NewRule(vis.RuleSpec{Name: "m_shrink", Knob: Q, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Do: vis.StepPercent(-25), Lease: time.Minute})
`},
	})
	prev, err := manifest.Extract(root, dirs["before"])
	if err != nil {
		t.Fatal(err)
	}
	next, err := manifest.Extract(root, dirs["after"])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range manifest.Diff(prev, next) {
		got = append(got, c.String())
	}
	want := []string{
		"! knob m_cache: range 1..500 (was 1..100)",
		"! knob m_cache: rules may change it now",
		"  knob m_gone: removed (it was int 1..9, default 5, changed by people only)",
		"  knob m_q: default 90 (was 95)",
		"  knob m_q: $M_Q sets its default when set (was the code alone)",
		"  signal m_hits: added: a gauge",
		"! SLO GET /m: p99 80ms (was 50ms)",
		"! rule m_backoff: action set(70) (was step(-5))",
		"! rule m_backoff: lease 10m0s (was 1m0s)",
		"  rule m_new: added: when Hits.Measure() > 0.9, step(10) m_cache, each for 1m0s, in shadow",
		"! rule m_shrink: action step(-25%) (was step(-16))",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("diff:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Removing an objective is the largest loosening of all: rules answer to it.
func TestRemovingAnSLOIsMarked(t *testing.T) {
	prev := manifest.Manifest{SLOs: map[string]manifest.SLO{"GET /k": {P99: "50ms"}}}
	next := manifest.Manifest{SLOs: map[string]manifest.SLO{}}
	got := manifest.Diff(prev, next)
	if len(got) != 1 || !got[0].Wider || !strings.Contains(got[0].String(), "! SLO GET /k: removed") {
		t.Fatalf("diff = %v, want the removal marked", got)
	}
}

// Directives and the calls written by hand declare the same things, and read the same.
func TestDirectivesAndCallsReadTheSame(t *testing.T) {
	directives := "package p\n\nimport (\n\t\"net/http\"\n\n\t\"github.com/tfcace/vis\"\n)\n\n" + `// vis:knob range=1..100 default=95 unit=MB pull=agent,rule uses=cpu
// vis:rule name=d_backoff when="sched.p99 > 5ms" do=step(-5) lease=1m
var Q vis.Int // Quality.

// vis:signal unit=img/s
var Fetches vis.Gauge // Fetches.

func routes(m *http.ServeMux) { m.HandleFunc("GET /d", h) }

// vis:slo p99=50ms
func h(http.ResponseWriter, *http.Request) {}
`
	root, dirs := write(t, map[string]map[string]string{
		"directives": {"p.go": directives},
		"calls": {"p.go": "package p\n\nimport (\n\t\"time\"\n\n\t\"github.com/tfcace/vis\"\n)\n\n" + `var Fetches = vis.NewGauge("fetches", vis.Unit("img/s"), vis.About("Fetches."))
var Q = vis.NewInt("q", vis.Range(1, 100), vis.Default(95), vis.Unit("MB"), vis.Pull(vis.Agent, vis.Rule), vis.Uses(vis.CPU), vis.About("Quality."))
var _ = vis.NewSLO("GET /d", vis.P99(50*time.Millisecond))
var _ = vis.NewRule(vis.RuleSpec{Name: "d_backoff", Knob: Q, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Step: -5, Lease: time.Minute})
`},
	})
	src, err := gen.Generate(filepath.Join(root, dirs["directives"]))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, dirs["directives"], gen.File), src, 0o644)
	fromDirectives, err := manifest.Extract(root, dirs["directives"])
	if err != nil {
		t.Fatal(err)
	}
	fromCalls, err := manifest.Extract(root, dirs["calls"])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromDirectives, fromCalls) {
		t.Fatalf("from directives: %+v\nfrom calls:      %+v", fromDirectives, fromCalls)
	}
}

// A level knob reads by name, from a directive or a call, with vis's range
// and default when the declaration leaves them out.
func TestLogLevelKnobsReadAsLevels(t *testing.T) {
	root, dirs := write(t, map[string]map[string]string{
		"calls": {"p.go": "package p\n\nimport (\n\t\"log/slog\"\n\n\t\"github.com/tfcace/vis\"\n)\n\n" +
			`var Logging = vis.NewLogLevel("logging", vis.Pull(vis.Agent))
var Tracing = vis.NewLogLevel("tracing", vis.Range(slog.LevelDebug-4, slog.LevelError), vis.Default(slog.LevelWarn))
`},
	})
	m, err := manifest.Extract(root, dirs["calls"])
	if err != nil {
		t.Fatal(err)
	}
	l, tr := m.Knobs["logging"], m.Knobs["tracing"]
	if l.Type != "level" || l.Min != "DEBUG" || l.Max != "ERROR" || l.Default != "INFO" {
		t.Errorf("logging = %+v, want a level DEBUG..ERROR from INFO", l)
	}
	if tr.Min != "TRACE" || tr.Default != "WARN" {
		t.Errorf("tracing = %+v, want TRACE..ERROR from WARN (DEBUG-4 is TRACE)", tr)
	}
}

// A special value is a change for review: people and agents may now choose
// what it means, such as no limit.
func TestASpecialValueReadsForReview(t *testing.T) {
	root, dirs := write(t, map[string]map[string]string{
		"before": {"p.go": head + `var Timeout = vis.NewDuration("m_timeout", vis.Range(time.Second, 5*time.Minute), vis.Default(3*time.Second), vis.Pull(vis.Agent))
`},
		"after": {"p.go": head + `var Timeout = vis.NewDuration("m_timeout", vis.Range(time.Second, 5*time.Minute), vis.Default(time.Duration(0)), vis.Special(time.Duration(0), "no limit"), vis.Pull(vis.Agent))
var Pool = vis.NewInt("m_pool", vis.Range(1, 100), vis.Default(10), vis.Special(0, "unlimited"))
`},
	})
	prev, err := manifest.Extract(root, dirs["before"])
	if err != nil {
		t.Fatal(err)
	}
	next, err := manifest.Extract(root, dirs["after"])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range manifest.Diff(prev, next) {
		got = append(got, c.String())
	}
	want := []string{
		`  knob m_pool: added: int 1..100 or 0 ("unlimited"), default 10, changed by people only`,
		`  knob m_timeout: default 0s (was 3s)`,
		`! knob m_timeout: may also be 0s, "no limit" (was its range alone)`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("diff:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
