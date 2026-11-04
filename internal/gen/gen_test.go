package gen

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden file")

// The directives in testdata/app become one readable file of vis calls.
func TestGenerateWritesTheDirectivesAsVisCalls(t *testing.T) {
	src, err := Generate("testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "app.golden")
	if *update {
		os.WriteFile(golden, src, 0o644)
	}
	want, _ := os.ReadFile(golden)
	if string(src) != string(want) {
		t.Fatalf("got:\n%s\nwant:\n%s", src, want)
	}
}

func generateSource(t *testing.T, src string) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Generate(dir)
	return err
}

// Every mistake is reported at its line, saying how to fix it.
func TestGenerateSaysWhatIsWrongAndWhere(t *testing.T) {
	head := "package a\n\nimport (\n\t\"net/http\"\n\n\t\"vis\"\n)\n\nvar _ http.Handler\n\n"
	cases := []struct{ src, want string }{
		{"// vis:knob default=5\nvar K vis.Int\n", "a.go:11: // vis:knob K needs range="},
		{"// vis:knob range=1..9 default=5 spends=cpu\nvar K vis.Int\n", "unknown key spends"},
		{"// vis:knob range=1..9 default=5 scope=fleet\nvar K vis.Int\n", "scope= isn't built yet"},
		{"// vis:knob range=1..9 default=5ms\nvar K vis.Int\n", "an Int knob's values are whole numbers"},
		{"// vis:knob range=1..9 default=5 uses=origin\nvar K vis.Int\n", "a cost is cpu, memory, or one of this package's // vis:signal gauges"},
		{"// vis:knob range=1..9 default=5\nvar K vis.Gauge\n", "goes above a vis.Int, vis.Duration or vis.LogLevel var"},
		{"// vis:knob range=1..9 default=5\n// vis:rule when=\"sched.p99 > 5ms\" do=step(-1) lease=1m\nvar K vis.Int\n", "add rule to its pull="},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"sched.p99 > 5\" do=step(-1) lease=1m\nvar K vis.Int\n", "needs a unit, such as 5ms"},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"sched.p99 > 5ms\" lease=1m\nvar K vis.Int\n", "needs do=step(N)"},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"hits > 5ms\" do=step(-1) lease=1m\nvar K vis.Int\n", "hits is none of them"},
		{"// vis:slo p99=50ms\nfunc h(http.ResponseWriter, *http.Request) {}\n", "can't tell which route h serves"},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"sched.p99 > 5ms\" do=set(20) lease=1m\nvar K vis.Int\n", "do=set(20) is outside K's range 1..9"},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"sched.p99 > 5ms\" do=step(-20) lease=1m\nvar K vis.Int\n", "do=step(-20) is wider than K's range 1..9"},
		{"// vis:knob range=1ms..9ms default=5ms pull=rule\n// vis:rule when=\"sched.p99 > 5ms\" do=step(-1) lease=1m\nvar K vis.Duration\n", "K's values are a duration"},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"sched.p99 > 5ms\" do=jump lease=1m\nvar K vis.Int\n", "needs do=step(N), do=step(P%), do=set(V) or do=reset"},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"sched.p99 > 5ms\" do=step(0%) lease=1m\nvar K vis.Int\n", "do=step(0%) never moves K"},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"sched.p99 > 5ms\" do=step(-100%) lease=1m\nvar K vis.Int\n", "more than -100%"},
		{"// vis:probe\nvar K vis.Int\n", "// vis:probe isn't built yet"},
		{"// vis:nope\nvar K vis.Int\n", "unknown directive // vis:nope"},
		// slo.p99 watches the package's one objective; with several, or none, name a route.
		{"func routes(mux *http.ServeMux) {\n\tmux.HandleFunc(\"GET /a\", a)\n\tmux.HandleFunc(\"GET /b\", b)\n}\n\n// vis:slo p99=50ms\nfunc a(http.ResponseWriter, *http.Request) {}\n\n// vis:slo p99=50ms\nfunc b(http.ResponseWriter, *http.Request) {}\n\n// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"slo.p99 > 50ms\" do=step(-1) lease=1m\nvar K vis.Int\n", `slo.p99 is ambiguous: this package has 2 SLOs (GET /a, GET /b). Name the one to watch, such as when="slo(GET /a).p99 > 50ms"`},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"slo.p99 > 50ms\" do=step(-1) lease=1m\nvar K vis.Int\n", `slo.p99 needs a // vis:slo in this package; for a route served elsewhere, name it: when="slo(GET /path).p99 > 50ms"`},
		{"// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"slo(generate).p99 > 50ms\" do=step(-1) lease=1m\nvar K vis.Int\n", `slo(generate) names a route as vis.HTTP or a framework's middleware times it, such as slo(POST /api/generate)`},
		// A directive vis gen would skip is a mistake, not a no-op.
		{"func F() {\n\t// vis:knob range=1..9 default=5\n\tvar K vis.Int\n\t_ = K\n}\n", "a.go:12: // vis:knob is inside func F, where vis gen doesn't look: move it and its var to package level"},
		// An slo above an inline handler gets the line to write by hand instead.
		{"func main() {\n\tmux := http.NewServeMux()\n\t// vis:slo p99=300ms\n\tmux.HandleFunc(\"GET /search\", func(http.ResponseWriter, *http.Request) {})\n}\n",
			`a.go:13: // vis:slo is inside func main, where vis gen doesn't look: put it above a handler func, or declare the objective by hand at package level: var _ = vis.NewSLO("GET /search", vis.P99(300*time.Millisecond))`},
		{"func main() {\n\t// vis:slo p99=1s\n\tsetup()\n}\n",
			`a.go:12: // vis:slo is inside func main, where vis gen doesn't look: put it above a handler func, or declare the objective by hand at package level: var _ = vis.NewSLO("GET /path", vis.P99(time.Second))`},
		{"// vis:knob range=1..9 default=5\n\nvar K vis.Int\n", "a.go:11: // vis:knob isn't directly above a var or func, so vis gen ignores it"},
		{"// vis:knob range=1..9 default=5\nvar (\n\tK vis.Int\n\tJ vis.Int\n)\n", "a.go:11: // vis:knob is above a var group, so vis gen ignores it: put it inside the parentheses"},
		{"// vis:knob range=1..9 default=5\ntype T int\n", "a.go:11: // vis:knob isn't directly above a var or func"},
		{"//vis:knob range=1..9 default=5\nvar K vis.Int\n", "a.go:11: write //vis:knob as // vis:knob, with a space; vis gen reads only that"},
	}
	for _, c := range cases {
		err := generateSource(t, head+c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("for\n%s\ngot %v\nwant it to say %q", c.src, err, c.want)
		}
	}
}

// A rule can watch a route by name, so it may live with the knob in a
// package other than the handler's.
func TestRuleWatchesANamedRoute(t *testing.T) {
	dir := t.TempDir()
	src := "package a\n\nimport \"vis\"\n\n// vis:knob range=1..9 default=5 pull=rule\n// vis:rule when=\"slo(POST /api/generate).p99 > 300ms\" do=step(1) lease=1m\nvar K vis.Int\n"
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	out, err := Generate(dir)
	if err != nil || !strings.Contains(string(out), `Watch: vis.RouteP99("POST /api/generate")`) {
		t.Fatalf("got %v\n%s\nwant a rule watching vis.RouteP99(\"POST /api/generate\")", err, out)
	}
}

// A package with no directives gets no file.
func TestGenerateWritesNothingWithoutDirectives(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc F() {}\n"), 0o644)
	if src, err := Generate(dir); err != nil || src != nil {
		t.Fatalf("got %q, %v; want nothing", src, err)
	}
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{"CacheSize": "cache_size", "JPEGQuality": "jpeg_quality", "HTTPTimeout": "http_timeout", "P99Wait": "p99_wait", "x": "x"} {
		if got := snake(in); got != want {
			t.Errorf("snake(%q) = %q, want %q", in, got, want)
		}
	}
}

// The generated file compiles with the package, and at init declares what the
// directives say: run it as a real package inside the module.
func TestGeneratedCodeDeclaresTheDirectives(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a package")
	}
	src, err := Generate("testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".", "e2e") // inside the module, so it can import vis
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	app, _ := os.ReadFile("testdata/app/app.go")
	os.WriteFile(filepath.Join(dir, "app.go"), app, 0o644)
	os.WriteFile(filepath.Join(dir, File), src, 0o644)
	os.WriteFile(filepath.Join(dir, "app_test.go"), []byte(`package app

import (
	"slices"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

func TestDeclared(t *testing.T) {
	in := vis.NewInstance()
	if CacheSize.Get(in.Snapshot()) != 100 || JPEGQuality.Name() != "jpeg_quality" || FetchTimeout.Get(in.Snapshot()) != 5*time.Second {
		t.Fatal("knobs not declared as written")
	}
	info, _ := vis.Lookup("cache_size")
	if info.About != "Memory for cached images." || !slices.Equal(info.Saves, []vis.Resource{"origin_fetches"}) {
		t.Fatalf("cache_size = %+v", info)
	}
	if s := in.SLOs(); len(s) == 0 || s[0].Route != "GET /i/{name}" || s[0].P99 != 500*time.Millisecond {
		t.Fatalf("SLOs = %+v", s)
	}
	OriginFetches.Set(3)
	OriginWait.Set(20 * time.Millisecond)
	_ = routes()
}
`), 0o644)
	out, err := exec.Command("go", "test", "./"+dir).CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package failed: %v\n%s", err, out)
	}
}

// A rule's do= becomes its action: a step (by a duration, for a Duration knob), a percent step, a value to set, or a reset.
func TestGenerateWritesEachAction(t *testing.T) {
	dir := t.TempDir()
	src := "package a\n\nimport \"github.com/tfcace/vis\"\n\n" +
		"// vis:knob range=1..512 default=64 pull=rule\n" +
		"// vis:rule name=pin_low when=\"sched.p99 > 50ms\" do=set(8) lease=1m\n" +
		"var B vis.Int\n\n" +
		"// vis:knob range=1ms..50ms default=5ms pull=rule\n" +
		"// vis:rule name=ease when=\"sched.p99 > 20ms\" do=step(-1ms) lease=1m\n" +
		"// vis:rule name=restore when=\"gc.cpu > 0.5\" do=reset lease=1m\n" +
		"// vis:rule name=shrink when=\"gc.cpu > 0.25\" do=step(-25%) lease=1m\n" +
		"var T vis.Duration\n"
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	out, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(out)), " ") // gofmt aligns each rule's fields its own way
	for _, want := range []string{"Do: vis.SetTo(8)", "Step: int64(-1 * time.Millisecond)", "Do: vis.Reset()", "Do: vis.StepPercent(-25)"} {
		if !strings.Contains(flat, want) {
			t.Errorf("generated:\n%s\nlacks %q", out, want)
		}
	}
}

// Every name the control UI shows for a runtime signal is one a rule can watch.
func TestRulesCanWatchCPUInUseAndGoroutines(t *testing.T) {
	dir := t.TempDir()
	src := "package a\n\nimport \"github.com/tfcace/vis\"\n\n" +
		"// vis:knob range=1..512 default=64 pull=rule\n" +
		"// vis:rule name=busy when=\"cpu.used > 1.5\" do=step(-8) lease=1m\n" +
		"// vis:rule name=crowded when=\"goroutines > 10000\" do=step(-8) lease=1m\n" +
		"var B vis.Int\n"
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	out, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"vis.CPUUsed", "vis.Goroutines"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("generated:\n%s\nlacks %q", out, want)
		}
	}
}

// An SLO above a handler takes its route from where gin or echo registers
// it, named as visgin and visecho time it: "METHOD /path/:param", with any
// group's prefix.
func TestSLOsFindTheirRouteInGinAndEcho(t *testing.T) {
	gin := "package a\n\nimport (\n\t\"github.com/gin-gonic/gin\"\n\n\t\"github.com/tfcace/vis\"\n)\n\nvar _ vis.Int\n\ntype S struct{}\n\n"
	echo := strings.Replace(gin, "github.com/gin-gonic/gin", "github.com/labstack/echo/v4", 1)
	cases := []struct{ name, src, want string }{
		{"gin, handler last, after a wrapper's spread", gin +
			"func routes(s *S, r *gin.Engine) {\n\tr.POST(\"/api/generate\", s.logged(\"/api/generate\", s.Generate)...)\n}\n\n" +
			"// vis:slo p99=2s\nfunc (s *S) Generate(c *gin.Context) {}\n",
			`vis.NewSLO("POST /api/generate"`},
		{"gin group", gin +
			"func routes(s *S, r *gin.Engine) {\n\tv1 := r.Group(\"/v1\")\n\tv1.GET(\"/models/:model\", mw(), s.Show)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) Show(c *gin.Context) {}\n",
			`vis.NewSLO("GET /v1/models/:model"`},
		{"gin nested and chained groups", gin +
			"const api = \"/api\"\n\nfunc routes(s *S, r *gin.Engine) {\n\tg := r.Group(api)\n\tg.Group(\"/v2\").DELETE(\"/items/:id\", s.Drop)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) Drop(c *gin.Context) {}\n",
			`vis.NewSLO("DELETE /api/v2/items/:id"`},
		{"echo, handler second, middleware after", echo +
			"func routes(s *S, e *echo.Echo) {\n\te.GET(\"/k/:key\", s.Key, mw)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) Key(c echo.Context) error { return nil }\n",
			`vis.NewSLO("GET /k/:key"`},
		{"echo group", echo +
			"func routes(s *S, e *echo.Echo) {\n\tg := e.Group(\"/admin\")\n\tg.PUT(\"/x\", s.X)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) X(c echo.Context) error { return nil }\n",
			`vis.NewSLO("PUT /admin/x"`},
		{"two routes: say which", gin +
			"func routes(s *S, r *gin.Engine) {\n\tr.POST(\"/api/generate\", s.Generate)\n\tr.POST(\"/v1/completions\", mw(), s.Generate)\n}\n\n" +
			"// vis:slo p99=2s\nfunc (s *S) Generate(c *gin.Context) {}\n",
			"Generate serves POST /api/generate, POST /v1/completions; say which one with route="},
		{"a group whose prefix isn't a constant: don't guess", gin +
			"func routes(s *S, r *gin.Engine) {\n\tg := r.Group(prefix())\n\tg.GET(\"/x\", s.X)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) X(c *gin.Context) {}\n",
			"can't tell which route X serves"},
		{"a group kept in a field: don't guess", gin +
			"func routes(s *S, r *gin.Engine) {\n\ts.api = r.Group(\"/api\")\n\ts.api.GET(\"/x\", s.X)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) X(c *gin.Context) {}\n",
			"can't tell which route X serves"},
		{"a group passed in: don't guess", gin +
			"func items(s *S, g *gin.RouterGroup) {\n\tg.GET(\"/x\", s.X)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) X(c *gin.Context) {}\n",
			"can't tell which route X serves"},
		{"a router in a struct field", gin +
			"type Srv struct{ router *gin.Engine }\n\nfunc (v *Srv) routes(s *S) {\n\tv.router.GET(\"/x\", s.X)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) X(c *gin.Context) {}\n",
			`vis.NewSLO("GET /x"`},
		{"GET on something that isn't a router", "package a\n\nimport \"github.com/tfcace/vis\"\n\nvar _ vis.Int\n\ntype S struct{}\n\n" +
			"func f(s *S, client C) {\n\tclient.GET(\"/x\", s.X)\n}\n\n" +
			"// vis:slo p99=50ms\nfunc (s *S) X() {}\n",
			"can't tell which route X serves"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "a.go"), []byte(c.src), 0o644)
			out, err := Generate(dir)
			got := string(out)
			if err != nil {
				got = err.Error()
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("got:\n%s\nwant it to contain %q", got, c.want)
			}
		})
	}
}

// A package whose only directives are SLOs still imports vis, from where the
// package does or, if it doesn't yet, from vis's own path.
func TestSLOOnlyPackagesImportVis(t *testing.T) {
	for _, imp := range []string{"\t\"github.com/tfcace/vis\"\n", ""} {
		dir := t.TempDir()
		src := "package a\n\nimport (\n\t\"net/http\"\n" + imp + ")\n\n"
		if imp != "" {
			src += "var _ = vis.HTTP\n\n"
		}
		src += "func routes(mux *http.ServeMux) { mux.HandleFunc(\"GET /x\", h) }\n\n// vis:slo p99=50ms\nfunc h(http.ResponseWriter, *http.Request) {}\n"
		os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
		out, err := Generate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "\"github.com/tfcace/vis\"") {
			t.Errorf("with import %q, generated:\n%s\nwant it to import vis", imp, out)
		}
		// vis vet generates from the files it was given, and must agree.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "a.go", src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if same, _, err := FromFiles(fset, []*ast.File{f}); err != nil || string(same) != string(out) {
			t.Errorf("with import %q, FromFiles gave:\n%s\n%v\nwant what Generate gave:\n%s", imp, same, err, out)
		}
	}
}

// env= names the variable that sets the knob's default, as vis.Env.
func TestKnobEnvBecomesVisEnv(t *testing.T) {
	dir := t.TempDir()
	src := "package a\n\nimport \"github.com/tfcace/vis\"\n\n" +
		"// vis:knob range=0..8 default=0 unit=models env=OLLAMA_MAX_LOADED_MODELS\nvar MaxLoaded vis.Int // Models kept loaded at once.\n"
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	out, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `vis.Env("OLLAMA_MAX_LOADED_MODELS")`) {
		t.Fatalf("generated:\n%s\nlacks vis.Env", out)
	}
	bad := "package a\n\nimport \"github.com/tfcace/vis\"\n\n// vis:knob range=0..8 default=0 env=max-loaded\nvar M vis.Int\n"
	if err := generateSource(t, bad); err == nil || !strings.Contains(err.Error(), "env=max-loaded isn't an environment variable's name") {
		t.Fatalf("got %v, want it to reject env=max-loaded", err)
	}
}

// A level knob needs no range or default (vis gives DEBUG..ERROR from INFO),
// takes levels by name where it has them, and a rule sets it by name too:
// log more while the service is in trouble.
func TestLogLevelKnobs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a package")
	}
	src := "package lv\n\nimport \"github.com/tfcace/vis\"\n\n" +
		"// vis:knob pull=people,agent,rule env=APP_LOG\n" + // people always may; saying so is fine
		"// vis:rule name=log_more when=\"sched.p99 > 50ms\" do=set(debug) lease=10m\n" +
		"var Logging vis.LogLevel // What the service logs.\n\n" +
		"// vis:knob range=trace..error default=warn\n" +
		"var Tracing vis.LogLevel // What the scheduler logs.\n"
	dir, err := os.MkdirTemp(".", "lv") // inside the module, so it can import vis
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.WriteFile(filepath.Join(dir, "lv.go"), []byte(src), 0o644)
	out, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"log/slog"`, `vis.DeclareLogLevel(&Logging, "logging", vis.Env("APP_LOG")`, `vis.Default(slog.LevelWarn)`, `vis.SetTo(slog.LevelDebug)`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("generated:\n%s\nlacks %s", out, want)
		}
	}
	if strings.Contains(string(out), `"logging", vis.Range`) {
		t.Errorf("generated:\n%s\ngives Logging a range it didn't ask for", out)
	}
	os.WriteFile(filepath.Join(dir, File), out, 0o644)
	os.WriteFile(filepath.Join(dir, "lv_test.go"), []byte(`package lv

import (
	"log/slog"
	"testing"

	"github.com/tfcace/vis"
)

func TestDeclared(t *testing.T) {
	snap := vis.NewInstance().Snapshot()
	lo, hi := Tracing.Range()
	if Logging.Get(snap) != slog.LevelInfo || Tracing.Get(snap) != slog.LevelWarn || lo != slog.LevelDebug-4 || hi != slog.LevelError {
		t.Fatalf("logging %v, tracing %v in %v..%v", Logging.Get(snap), Tracing.Get(snap), lo, hi)
	}
}
`), 0o644)
	if out, err := exec.Command("go", "test", "./"+dir).CombinedOutput(); err != nil {
		t.Fatalf("the generated package failed: %v\n%s", err, out)
	}

	for bad, want := range map[string]string{
		"// vis:knob default=loud\nvar L vis.LogLevel\n":                                                         "default=loud: a log level knob's values are levels, such as trace, debug, info, warn or error",
		"// vis:knob unit=lines\nvar L vis.LogLevel\n":                                                           "a log level has no unit; drop unit=",
		"// vis:knob pull=rule\n// vis:rule when=\"sched.p99 > 50ms\" do=step(1) lease=1m\nvar L vis.LogLevel\n": "a rule sets a log level by name, such as do=set(debug), or ends what it set with do=reset",
	} {
		err := generateSource(t, "package a\n\nimport \"github.com/tfcace/vis\"\n\n"+bad)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", bad, err, want)
		}
	}
}

// special= names one value outside the range that the service gives its own
// meaning: imageproxy's -timeout 0 is "no limit".
func TestKnobSpecialBecomesVisSpecial(t *testing.T) {
	dir := t.TempDir()
	src := "package a\n\nimport \"github.com/tfcace/vis\"\n\n" +
		"// vis:knob range=1s..5m default=0s special=0s:\"no limit\" pull=agent,rule\nvar Timeout vis.Duration // Time limit for a request.\n\n" +
		"// vis:knob range=1..100 default=10 special=0:unlimited\nvar Pool vis.Int // Connections kept open.\n"
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	out, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`vis.Special(time.Duration(0), "no limit")`, `vis.Default(time.Duration(0))`, `vis.Special(0, "unlimited")`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("generated:\n%s\nlacks %s", out, want)
		}
	}
	head := "package a\n\nimport \"github.com/tfcace/vis\"\n\n"
	for _, c := range []struct{ knob, want string }{
		{`range=0s..5m default=0s special=0s:"no limit"`, `special=0s is inside the range 0s..5m: start the range past it, such as range=1s..5m`},
		{`range=1s..5m default=1s special=0s`, `special=0s needs a name for what it means, such as special=0s:"no limit"`},
		{`range=1s..5m default=1s special=0s:3s`, `special=0s:3s: its name reads as a value`},
		{`range=1s..5m default=1s special=none:"no limit"`, `special=none: a duration knob's values are durations`},
		{`range=1s..5m default=0s`, `default=0s is outside the range 1s..5m: if 0s means none, say so with special=0s:"no limit"`},
	} {
		err := generateSource(t, head+"// vis:knob "+c.knob+"\nvar T vis.Duration\n")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.knob, err, c.want)
		}
	}
	err = generateSource(t, head+"// vis:knob range=1s..5m default=3s special=0s:\"no limit\" pull=rule\n// vis:rule when=\"sched.p99 > 50ms\" do=set(0s) lease=10m\nvar T vis.Duration\n")
	if err == nil || !strings.Contains(err.Error(), `a rule never sets "no limit"`) {
		t.Errorf("do=set of the special value: got %v, want it refused", err)
	}
}

// A rule that steps its knob toward the end of the range where the default
// sits can never move it: imageproxy's time limit, 0 by default, stepped down.
func TestARuleThatCantMoveFromTheDefaultIsAdvised(t *testing.T) {
	dir := t.TempDir()
	src := "package a\n\nimport \"github.com/tfcace/vis\"\n\n" +
		"// vis:knob range=0s..5m default=0s pull=rule\n" +
		"// vis:rule name=fail_fast when=\"sched.p99 > 500ms\" do=step(-1s) lease=10m mode=shadow\nvar Timeout vis.Duration\n\n" +
		"// vis:knob range=1..64 default=64 pull=rule\n" +
		"// vis:rule name=grow when=\"sched.p99 > 500ms\" do=step(25%) lease=10m\nvar Workers vis.Int\n"
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644)
	_, advice, err := generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range advice {
		got = append(got, a.Msg)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		`// vis:rule fail_fast: do=step(-1s) moves Timeout down, but its default 0s is the bottom of its range, so the rule can't move it unless SetDefault starts it higher. If 0s means none, say so: special=0s:"no limit" with range=1s..5m`,
		`// vis:rule grow: do=step(25%) moves Workers up, but its default 64 is the top of its range, so the rule can't move it unless SetDefault starts it lower`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("advice:\n%s\nlacks %s", all, want)
		}
	}
}

// generate is Generate, with the advice vis vet reports.
func generate(dir string) ([]byte, []Diag, error) {
	g := &generator{fset: token.NewFileSet(), consts: map[string]string{}}
	if err := g.load(dir); err != nil {
		return nil, nil, err
	}
	return g.run()
}
