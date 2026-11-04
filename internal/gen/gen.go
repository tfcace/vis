// Package gen turns a package's // vis: directives into Go code: one
// vis_gen.go of ordinary vis calls, checked in and readable.
//
// It reads the package's syntax only, with go/parser: the directives built so
// far (knob, signal, slo, rule) need no type information. Directives are
// comments of the form
//
//	// vis:<kind> key=value key="a quoted value" ...
//
// directly above the declaration they configure.
package gen

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tfcace/vis/internal/spec"
)

// File is the name of the file Generate's output belongs in.
const File = "vis_gen.go"

// Errors is every problem found, each "file:line: message".
type Errors []string

func (e Errors) Error() string { return strings.Join(e, "\n") }

// Generate reads the Go package in dir and returns the source of vis_gen.go,
// or nil if the package has no directives.
func Generate(dir string) ([]byte, error) {
	g := &generator{fset: token.NewFileSet(), consts: map[string]string{}}
	if err := g.load(dir); err != nil {
		return nil, err
	}
	src, diags, err := g.run()
	if src == nil && len(diags) > 0 {
		var errs Errors
		for _, d := range diags {
			p := g.fset.Position(d.Pos)
			errs = append(errs, fmt.Sprintf("%s:%d: %s", filepath.Base(p.Filename), p.Line, d.Msg))
		}
		sort.Strings(errs)
		return nil, errs
	}
	return src, err
}

// Diag is one problem with a directive, at its position. Advice is a
// directive that works but likely isn't what was meant: vis vet reports it,
// and vis gen generates anyway.
type Diag struct {
	Pos    token.Pos
	Msg    string
	Advice bool
}

// FromFiles is Generate for files already parsed, with comments, such as an
// analyzer's: the package's own files, without tests or vis_gen.go. It
// returns the problems found instead of the source when there are any, and
// the source with any advice otherwise.
func FromFiles(fset *token.FileSet, files []*ast.File) ([]byte, []Diag, error) {
	g := &generator{fset: fset, consts: map[string]string{}}
	for _, f := range files {
		if g.pkg == "" {
			g.pkg = f.Name.Name
		}
		g.files = append(g.files, f)
	}
	return g.run()
}

func (g *generator) run() ([]byte, []Diag, error) {
	g.visPath = defaultVisPath // a package with only // vis:slo may not import vis yet
	for _, f := range g.files {
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == "vis" || strings.HasSuffix(path, "/vis") {
				g.visPath = path
			}
		}
	}
	g.read = map[token.Pos]bool{}
	g.collect()
	g.strays()
	g.resolve()
	if len(g.diags) > 0 {
		return nil, g.diags, nil
	}
	if len(g.knobs)+len(g.signals)+len(g.slos) == 0 {
		return nil, nil, nil
	}
	src, err := g.write()
	return src, g.advice, err
}

type generator struct {
	fset    *token.FileSet
	pkg     string
	visPath string // vis's import path, as the package imports it
	files   []*ast.File
	consts  map[string]string // the package's string constants, for routes
	diags   []Diag
	advice  []Diag // directives that work but likely aren't meant
	usesDur bool
	usesLog bool               // log/slog, for a level knob's values
	read    map[token.Pos]bool // the // vis: comments a declaration took

	knobs   []*knob
	signals []*signal
	slos    []*slo
	rules   []*rule
}

type directive struct {
	kind string
	args map[string]string
	pos  token.Pos
}

type knob struct {
	pos                      token.Pos
	v, name, about, unit     string
	duration                 bool
	level                    bool   // a vis.LogLevel: range and default are optional
	min, max, def            string // Go expressions
	lo, hi, dv               int64  // the range and default, in the knob's own representation (nanoseconds for a duration)
	pull, uses, saves        []string
	rawUses, rawSaves, pulls string
	env                      string // the variable that sets the default
	rawLo, rawHi             string // the range's ends as the directive wrote them
	special                  string // the special value as Go, when special= names one
	sv                       int64  // and in the knob's own representation
	specialName              string // what it means: "no limit"
}

func (k *knob) isSpecial(v int64) bool { return k.special != "" && v == k.sv }

// envName is what env= accepts: an environment variable's name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type signal struct {
	pos            token.Pos
	v, name, about string
	unit           string
	latency        bool
}

type slo struct {
	pos         token.Pos
	fn, route   string
	p99         string
	explicitRte bool
}

type rule struct {
	pos   token.Pos
	k     *knob
	d     directive
	name  string
	watch string // Go expression
	over  string
	extra []string // RuleSpec fields, "Field: expr"
	step  int64    // a fixed do=step, in the knob's own unit
	pct   float64  // a do=step(P%)
}

func (g *generator) errorf(pos token.Pos, format string, a ...any) {
	g.diags = append(g.diags, Diag{Pos: pos, Msg: fmt.Sprintf(format, a...)})
}

func (g *generator) advise(pos token.Pos, format string, a ...any) {
	g.advice = append(g.advice, Diag{Pos: pos, Msg: fmt.Sprintf(format, a...), Advice: true})
}

func (g *generator) load(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == File {
			continue
		}
		f, err := parser.ParseFile(g.fset, filepath.Join(dir, n), nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if g.pkg == "" {
			g.pkg = f.Name.Name
		}
		if f.Name.Name != g.pkg {
			continue
		}
		g.files = append(g.files, f)
	}
	if len(g.files) == 0 {
		return fmt.Errorf("vis gen: no Go files in %s", dir)
	}
	return nil
}

// defaultVisPath is vis's import path, for packages that don't import it yet.
const defaultVisPath = "github.com/tfcace/vis"

// directives splits a comment group into its // vis: directives and the rest, the description.
func (g *generator) directives(cg *ast.CommentGroup) (ds []directive, text []string) {
	if cg == nil {
		return nil, nil
	}
	for _, c := range cg.List {
		if !strings.HasPrefix(c.Text, "// vis:") {
			t := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), " "))
			text = append(text, t)
			continue
		}
		g.read[c.Pos()] = true
		body := strings.TrimPrefix(c.Text, "// vis:")
		kind, rest, _ := strings.Cut(body, " ")
		args, err := parseArgs(rest)
		if err != nil {
			g.errorf(c.Pos(), "// vis:%s: %v", kind, err)
			continue
		}
		ds = append(ds, directive{kind: kind, args: args, pos: c.Pos()})
	}
	return ds, text
}

var argRE = regexp.MustCompile(`\s*([a-z_][a-z0-9_]*)=("(?:[^"\\]|\\.)*"|[^\s"]*:"(?:[^"\\]|\\.)*"|\S*)`) // special=0s:"no limit" quotes after its colon

func parseArgs(s string) (map[string]string, error) {
	args := map[string]string{}
	s = strings.TrimSpace(s)
	for s != "" {
		m := argRE.FindStringSubmatchIndex(s)
		if m == nil || m[0] != 0 {
			return nil, fmt.Errorf("can't read %q: write key=value, quoting a value with spaces", s)
		}
		k, v := s[m[2]:m[3]], s[m[4]:m[5]]
		if strings.HasPrefix(v, `"`) {
			u, err := strconv.Unquote(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", k, err)
			}
			v = u
		}
		if _, dup := args[k]; dup {
			return nil, fmt.Errorf("%s is given twice", k)
		}
		args[k] = v
		s = strings.TrimSpace(s[m[1]:])
	}
	return args, nil
}

// visType names the vis type a declaration has, "" if none.
func (g *generator) visType(f *ast.File, e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == x.Name && (path == "vis" || strings.HasSuffix(path, "/vis")) {
			g.visPath = path
			return sel.Sel.Name
		}
	}
	return ""
}

func (g *generator) collect() {
	for _, f := range g.files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok == token.CONST {
					g.collectConsts(d)
				}
				if d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					doc := vs.Doc
					if doc == nil && len(d.Specs) == 1 {
						doc = d.Doc
					}
					ds, text := g.directives(doc)
					if len(ds) == 0 {
						continue
					}
					g.collectVar(f, vs, ds, describe(vs.Names[0].Name, text, vs.Comment))
				}
			case *ast.FuncDecl:
				ds, _ := g.directives(d.Doc)
				for _, dr := range ds {
					if dr.kind != "slo" {
						g.errorf(dr.pos, "// vis:%s doesn't go on a func; // vis:slo does", dr.kind)
						continue
					}
					g.collectSLO(d, dr)
				}
			}
		}
	}
}

// strays reports the // vis: comments no declaration took. vis gen would
// skip them, and a knob it skips is a zero value nobody declared.
func (g *generator) strays() {
	for _, f := range g.files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				kind, ok := strings.CutPrefix(c.Text, "//vis:")
				if ok {
					kind, _, _ = strings.Cut(kind, " ")
					g.errorf(c.Pos(), "write //vis:%s as // vis:%s, with a space; vis gen reads only that", kind, kind)
					continue
				}
				kind, ok = strings.CutPrefix(c.Text, "// vis:")
				kind, _, _ = strings.Cut(kind, " ")
				if !ok || kind == "" || g.read[c.Pos()] {
					continue // "// vis: " is prose
				}
				switch where := enclosing(f, c.Pos()).(type) {
				case *ast.FuncDecl:
					fix := "move it and its var to package level"
					if kind == "slo" {
						fix = "put it above a handler func, or declare the objective by hand at package level: " + g.handSLO(where, c)
					}
					g.errorf(c.Pos(), "// vis:%s is inside func %s, where vis gen doesn't look: %s", kind, where.Name.Name, fix)
				case *ast.GenDecl:
					g.errorf(c.Pos(), "// vis:%s is above a var group, so vis gen ignores it: put it inside the parentheses, directly above the var it declares", kind)
				default:
					g.errorf(c.Pos(), "// vis:%s isn't directly above a var or func, so vis gen ignores it: remove any blank line between them", kind)
				}
			}
		}
	}
}

// enclosing is the func whose body holds pos, or the var group whose doc
// comment is at pos.
func enclosing(f *ast.File, pos token.Pos) ast.Node {
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil && d.Body.Pos() < pos && pos < d.Body.End() {
				return d
			}
		case *ast.GenDecl:
			if d.Tok == token.VAR && d.Lparen.IsValid() && d.Doc != nil && d.Doc.Pos() <= pos && pos < d.Doc.End() {
				return d
			}
		}
	}
	return nil
}

func (g *generator) collectConsts(d *ast.GenDecl) {
	for _, s := range d.Specs {
		vs := s.(*ast.ValueSpec)
		for i, n := range vs.Names {
			if i < len(vs.Values) {
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					g.consts[n.Name], _ = strconv.Unquote(lit.Value)
				}
			}
		}
	}
}

// describe is what people and agents read about a declaration: its doc
// comment without the directives, or the comment after it. "CacheSize is the
// memory for ..." reads as "The memory for ...".
func describe(name string, doc []string, trailing *ast.CommentGroup) string {
	var parts []string
	for _, t := range doc {
		if t != "" {
			parts = append(parts, t)
		}
	}
	s := strings.Join(parts, " ")
	if s == "" && trailing != nil {
		s = strings.TrimSpace(trailing.Text())
	}
	for _, verb := range []string{" is ", " are ", " says "} {
		if rest, ok := strings.CutPrefix(s, name+verb); ok && rest != "" {
			r := []rune(rest)
			r[0] = unicode.ToUpper(r[0])
			s = string(r)
			break
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

func (g *generator) collectVar(f *ast.File, vs *ast.ValueSpec, ds []directive, about string) {
	if len(vs.Names) != 1 || len(vs.Values) != 0 {
		g.errorf(ds[0].pos, "a directive goes above one var with no value: var X vis.Int")
		return
	}
	v, typ := vs.Names[0].Name, g.visType(f, vs.Type)
	var k *knob
	for _, d := range ds {
		switch d.kind {
		case "knob":
			if typ != "Int" && typ != "Duration" && typ != "LogLevel" {
				g.errorf(d.pos, "// vis:knob goes above a vis.Int, vis.Duration or vis.LogLevel var; %s is %s", v, typeName(typ))
				continue
			}
			k = g.knob(d, v, typ, about)
		case "signal":
			if typ != "Gauge" && typ != "Latency" {
				g.errorf(d.pos, "// vis:signal goes above a vis.Gauge or vis.Latency var; %s is %s", v, typeName(typ))
				continue
			}
			g.signal(d, v, typ == "Latency", about)
		case "rule":
			if k == nil {
				g.errorf(d.pos, "// vis:rule goes below the // vis:knob of the knob it moves")
				continue
			}
			g.rules = append(g.rules, &rule{pos: d.pos, k: k, d: d})
		case "slo":
			g.errorf(d.pos, "// vis:slo goes above the handler func it judges")
		case "strategy", "probe", "invariant", "tool", "state", "ignore":
			g.errorf(d.pos, "// vis:%s isn't built yet", d.kind)
		default:
			g.errorf(d.pos, "unknown directive // vis:%s: the kinds are knob, signal, slo and rule", d.kind)
		}
	}
}

func typeName(t string) string {
	if t == "" {
		return "not a vis type"
	}
	return "vis." + t
}

// snake turns a Go name into a knob's name: CacheSize is cache_size, JPEGQuality jpeg_quality.
func snake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) && i > 0 && (unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1]) || i+1 < len(r) && unicode.IsLower(r[i+1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

func (g *generator) unknownKeys(d directive, known ...string) {
	later := map[string]bool{"scope": true, "owner": true, "affects": true, "settle": true, "window": true, "error_rate": true, "max_step": true}
	for k := range d.args {
		switch {
		case slices.Contains(known, k):
		case later[k]:
			g.errorf(d.pos, "// vis:%s: %s= isn't built yet", d.kind, k)
		default:
			g.errorf(d.pos, "// vis:%s: unknown key %s; it takes %s", d.kind, k, strings.Join(known, ", "))
		}
	}
}

func (g *generator) knob(d directive, v, typ, about string) *knob {
	g.unknownKeys(d, "range", "default", "special", "unit", "pull", "uses", "saves", "name", "about", "env")
	if e := d.args["env"]; e != "" && !envName.MatchString(e) {
		g.errorf(d.pos, "// vis:knob %s: env=%s isn't an environment variable's name, such as env=MAX_LOADED", v, e)
	}
	duration := typ == "Duration"
	k := &knob{pos: d.pos, v: v, name: cmpOr(d.args["name"], snake(v)), about: cmpOr(d.args["about"], about), unit: d.args["unit"], env: d.args["env"], duration: duration,
		level: typ == "LogLevel", rawUses: d.args["uses"], rawSaves: d.args["saves"], pulls: d.args["pull"]}
	if k.level { // vis gives a level DEBUG..ERROR from INFO
		k.lo, k.hi, k.dv = int64(slog.LevelDebug), int64(slog.LevelError), int64(slog.LevelInfo)
	}
	lo, hi, ok := strings.Cut(d.args["range"], "..")
	switch {
	case d.args["range"] == "" && k.level:
	case d.args["range"] == "":
		g.errorf(d.pos, "// vis:knob %s needs range=, for example range=1..100", v)
	case !ok:
		g.errorf(d.pos, "// vis:knob %s: range=%s needs two ends, low..high", v, d.args["range"])
	default:
		k.min, k.max = g.value(d, "range", lo, k), g.value(d, "range", hi, k)
		k.lo, _ = k.parse(lo)
		k.hi, _ = k.parse(hi)
		k.rawLo, k.rawHi = lo, hi
	}
	if raw := d.args["special"]; raw != "" {
		g.special(d, k, raw)
	}
	switch {
	case d.args["default"] == "" && k.level:
	case d.args["default"] == "":
		g.errorf(d.pos, "// vis:knob %s needs default=", v)
	default:
		k.def = g.value(d, "default", d.args["default"], k)
		var ok bool
		k.dv, ok = k.parse(d.args["default"])
		if ok && k.rawLo != "" && (k.dv < k.lo || k.dv > k.hi) && !k.isSpecial(k.dv) {
			hint := ""
			if k.special == "" && (k.dv == 0 || k.dv == -1) && !k.level {
				hint = fmt.Sprintf(": if %s means none, say so with special=%s:\"no limit\"", d.args["default"], d.args["default"])
			}
			g.errorf(d.pos, "// vis:knob %s: default=%s is outside the range %s..%s%s", v, d.args["default"], k.rawLo, k.rawHi, hint)
		}
	}
	if k.level && k.unit != "" {
		g.errorf(d.pos, "// vis:knob %s: a log level has no unit; drop unit=", v)
	}
	if duration && k.unit != "" {
		g.errorf(d.pos, "// vis:knob %s: a duration's range says its unit; drop unit=", v)
	}
	for _, p := range splitList(k.pulls) {
		switch p {
		case "people", "person", "human":
			// people always may; a directive may say so, so it doesn't read as agents only
		case "agent", "agent:act":
			k.pull = append(k.pull, "vis.Agent")
		case "rule", "rules":
			k.pull = append(k.pull, "vis.Rule")
		case "fleet":
			k.pull = append(k.pull, "vis.Fleet")
		case "agent:propose":
			g.errorf(d.pos, "// vis:knob %s: pull=agent:propose isn't built yet; pull=agent lets agents act", v)
		default:
			g.errorf(d.pos, "// vis:knob %s: pull=%s: writers are agent, rule and fleet (people always may)", v, p)
		}
	}
	g.knobs = append(g.knobs, k)
	return k
}

// special reads special=VALUE:NAME, one value outside the range the service
// gives its own meaning: special=0s:"no limit".
func (g *generator) special(d directive, k *knob, raw string) {
	val, name, _ := strings.Cut(raw, ":")
	if q, err := strconv.Unquote(name); err == nil {
		name = q
	}
	if k.level {
		g.errorf(d.pos, "// vis:knob %s: a log level has no special value; drop special=", k.v)
		return
	}
	expr := g.value(d, "special", val, k) // it reports a value of the wrong kind
	sv, ok := k.parse(val)
	_, nameIsValue := k.parse(name)
	switch {
	case !ok:
	case name == "":
		g.errorf(d.pos, "// vis:knob %s: special=%s needs a name for what it means, such as special=%s:\"no limit\"", k.v, val, val)
	case nameIsValue:
		g.errorf(d.pos, "// vis:knob %s: special=%s: its name reads as a value; name what %s means, such as special=%s:\"no limit\"", k.v, raw, val, val)
	case k.rawLo != "" && sv >= k.lo && sv <= k.hi:
		step := int64(1)
		if k.duration {
			step = int64(time.Second)
		}
		g.errorf(d.pos, "// vis:knob %s: special=%s is inside the range %s..%s: start the range past it, such as range=%s..%s", k.v, val, k.rawLo, k.rawHi, rangeEnd(k, sv+step), k.rawHi)
	default:
		k.special, k.sv, k.specialName = expr, sv, name
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// value is a knob value as Go: an integer, or for a duration knob a Go duration such as 50ms.
func (g *generator) value(d directive, key, s string, k *knob) string {
	if k.level {
		l, ok := parseLevel(s)
		if !ok {
			g.errorf(d.pos, "// vis:%s %s=%s: a log level knob's values are levels, such as trace, debug, info, warn or error", d.kind, key, s)
			return "0"
		}
		return g.levelExpr(l)
	}
	if k.duration {
		dv, err := time.ParseDuration(s)
		if err != nil {
			g.errorf(d.pos, "// vis:%s %s=%s: a duration knob's values are durations, such as 50ms or 2s", d.kind, key, s)
			return "0"
		}
		return g.durExpr(dv)
	}
	if _, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64); err != nil {
		g.errorf(d.pos, "// vis:%s %s=%s: an Int knob's values are whole numbers", d.kind, key, s)
		return "0"
	}
	return s
}

// parse reads a value in the knob's own representation.
func (k *knob) parse(s string) (int64, bool) {
	if k.level {
		l, ok := parseLevel(s)
		return int64(l), ok
	}
	return parseValue(s, k.duration)
}

// parseLevel reads a level by name: trace, debug, INFO, warn+2.
func parseLevel(s string) (slog.Level, bool) {
	l, err := spec.ParseLevel(s)
	return l, err == nil
}

// levelExpr writes a level as Go: slog.LevelWarn, slog.LevelDebug - 4.
func (g *generator) levelExpr(l slog.Level) string {
	g.usesLog = true
	base := slog.LevelError
	for _, b := range []slog.Level{slog.LevelWarn, slog.LevelInfo, slog.LevelDebug} {
		if l < b+4 {
			base = b
		}
	}
	name := "slog.Level" + map[slog.Level]string{slog.LevelDebug: "Debug", slog.LevelInfo: "Info", slog.LevelWarn: "Warn", slog.LevelError: "Error"}[base]
	switch off := l - base; {
	case off > 0:
		return fmt.Sprintf("%s + %d", name, off)
	case off < 0:
		return fmt.Sprintf("%s - %d", name, -off)
	}
	return name
}

// handSLO is the NewSLO call an slo inside fn would be by hand: the route of
// the call on the line below it, as in an inline mux.HandleFunc("GET /x", ...),
// and its p99, or placeholders.
func (g *generator) handSLO(fn *ast.FuncDecl, c *ast.Comment) string {
	route, p99 := "GET /path", "500*time.Millisecond"
	args, _ := parseArgs(strings.TrimPrefix(c.Text, "// vis:slo"))
	if d, err := time.ParseDuration(args["p99"]); err == nil && d > 0 {
		p99 = strings.ReplaceAll(durGo(d), " * ", "*") // as gofmt writes it in a call
	}
	line := g.fset.Position(c.Pos()).Line
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 || g.fset.Position(call.Pos()).Line != line+1 {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if r, err := strconv.Unquote(lit.Value); err == nil {
				route = r
			}
		}
		return false
	})
	return fmt.Sprintf("var _ = vis.NewSLO(%q, vis.P99(%s))", route, p99)
}

// durExpr writes d as Go in its largest whole unit: 500 * time.Microsecond.
func (g *generator) durExpr(d time.Duration) string {
	g.usesDur = true
	return durGo(d)
}

func durGo(d time.Duration) string {
	if d == 0 {
		return "time.Duration(0)"
	}
	units := []struct {
		d    time.Duration
		name string
	}{{time.Hour, "Hour"}, {time.Minute, "Minute"}, {time.Second, "Second"}, {time.Millisecond, "Millisecond"}, {time.Microsecond, "Microsecond"}}
	for _, u := range units {
		if d%u.d == 0 {
			if d == u.d {
				return "time." + u.name
			}
			return fmt.Sprintf("%d * time.%s", d/u.d, u.name)
		}
	}
	return fmt.Sprintf("%d * time.Nanosecond", d)
}

func (g *generator) signal(d directive, v string, latency bool, about string) {
	g.unknownKeys(d, "unit", "name", "about")
	if latency && d.args["unit"] != "" {
		g.errorf(d.pos, "// vis:signal %s: a latency is in ms; drop unit=", v)
	}
	g.signals = append(g.signals, &signal{pos: d.pos, v: v, name: cmpOr(d.args["name"], snake(v)), about: cmpOr(d.args["about"], about), unit: d.args["unit"], latency: latency})
}

func (g *generator) collectSLO(fn *ast.FuncDecl, d directive) {
	g.unknownKeys(d, "p99", "route")
	s := &slo{pos: d.pos, fn: fn.Name.Name, route: d.args["route"], explicitRte: d.args["route"] != ""}
	p, err := time.ParseDuration(d.args["p99"])
	switch {
	case d.args["p99"] == "":
		g.errorf(d.pos, "// vis:slo on %s needs p99=, for example p99=50ms", s.fn)
	case err != nil || p <= 0:
		g.errorf(d.pos, "// vis:slo p99=%s: a latency, such as 50ms", d.args["p99"])
	default:
		s.p99 = g.durExpr(p)
	}
	g.slos = append(g.slos, s)
}

// routes finds the patterns each handler func is registered under: by
// net/http, mux.HandleFunc(pattern, h) or mux.Handle(pattern, h), where h
// names the func or method, maybe wrapped in http.HandlerFunc; and by gin or
// echo, r.GET(path, ...) naming h among its handlers, as "GET /path" with the
// prefix of the group r is. A gin or echo route is inferred only when r is
// known to be a router or a group with a constant prefix: otherwise the SLO
// says its route itself.
func (g *generator) routes() map[string][]string {
	out := map[string][]string{}
	add := func(h, pattern string) {
		if h != "" && !slices.Contains(out[h], pattern) {
			out[h] = append(out[h], pattern)
		}
	}
	rt := g.routers()
	for _, f := range g.files {
		fw := framework(f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case len(call.Args) == 2 && (sel.Sel.Name == "HandleFunc" || sel.Sel.Name == "Handle"):
				if pattern, ok := g.str(call.Args[0]); ok && pattern != "" {
					add(handlerName(call.Args[1]), pattern)
				}
			case fw != "" && len(call.Args) >= 2 && routeMethods[sel.Sel.Name]:
				prefix, ok := rt.prefix(sel.X)
				path, isStr := g.str(call.Args[0])
				if !ok || !isStr {
					return true
				}
				pattern := sel.Sel.Name + " " + joinRoute(fw, prefix, path)
				for _, a := range call.Args[1:] {
					ast.Inspect(a, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok {
							add(id.Name, pattern)
						}
						return true
					})
				}
			}
			return true
		})
	}
	return out
}

// routeMethods are the gin and echo methods that register one route.
var routeMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}

// framework is "gin" or "echo" when f imports that router.
func framework(f *ast.File) string {
	for _, imp := range f.Imports {
		switch path, _ := strconv.Unquote(imp.Path.Value); {
		case path == "github.com/gin-gonic/gin":
			return "gin"
		case strings.HasPrefix(path, "github.com/labstack/echo"):
			return "echo"
		}
	}
	return ""
}

// importName is the name f refers to the package at path by, or "".
func importName(f *ast.File, match func(path string) bool) string {
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if !match(path) {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		if strings.HasPrefix(path, "github.com/labstack/echo") {
			return "echo"
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// routerNames is what the package's syntax says about the names its gin and
// echo routes are registered on: which are a router itself, and which a
// group with a known prefix. A name bound two ways, or to a group whose
// prefix isn't a constant, is in neither, so nothing is guessed for it.
type routerNames struct {
	g      *generator
	roots  map[string]bool   // variables, parameters and fields that hold a router
	groups map[string]string // variables holding a group, by its full prefix
	bad    map[string]bool
}

func (g *generator) routers() *routerNames {
	rn := &routerNames{g: g, roots: map[string]bool{}, groups: map[string]string{}, bad: map[string]bool{}}
	type bind struct {
		name string
		rhs  ast.Expr
	}
	var binds []bind
	for _, f := range g.files {
		pkg := importName(f, func(p string) bool {
			return p == "github.com/gin-gonic/gin" || strings.HasPrefix(p, "github.com/labstack/echo")
		})
		if pkg == "" {
			continue
		}
		rootType := func(e ast.Expr) bool { // *gin.Engine or *echo.Echo
			star, ok := e.(*ast.StarExpr)
			if !ok {
				return false
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && x.Name == pkg && (sel.Sel.Name == "Engine" || sel.Sel.Name == "Echo")
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Field: // a parameter or struct field typed as a router
				if rootType(n.Type) {
					for _, id := range n.Names {
						rn.roots[id.Name] = true
					}
				}
			case *ast.ValueSpec:
				if n.Type != nil && rootType(n.Type) {
					for _, id := range n.Names {
						rn.roots[id.Name] = true
					}
				}
				for i, v := range n.Values {
					if i < len(n.Names) {
						binds = append(binds, bind{n.Names[i].Name, v})
					}
				}
			case *ast.AssignStmt:
				for i, v := range n.Rhs {
					if i >= len(n.Lhs) {
						break
					}
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						binds = append(binds, bind{id.Name, v})
					}
				}
			}
			return true
		})
		for i := range binds { // gin.New(), gin.Default() and echo.New() make a router
			if call, ok := binds[i].rhs.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == pkg && (sel.Sel.Name == "New" || sel.Sel.Name == "Default") {
						rn.roots[binds[i].name] = true
					}
				}
			}
		}
	}
	// Groups made from groups: settle until nothing changes.
	for changed := true; changed; {
		changed = false
		for _, b := range binds {
			call, ok := b.rhs.(*ast.CallExpr)
			if !ok || !isGroup(call) || rn.bad[b.name] {
				continue
			}
			p, ok := rn.prefix(call)
			old, had := rn.groups[b.name]
			switch {
			case had && (!ok || old != p) || rn.roots[b.name]: // bound two ways: guess neither
				delete(rn.groups, b.name)
				rn.bad[b.name], changed = true, true
			case ok && !had:
				rn.groups[b.name], changed = p, true
			}
		}
	}
	return rn
}

func seen(m map[string]string, k string) bool { _, ok := m[k]; return ok }

func isGroup(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Group" && len(call.Args) >= 1
}

// prefix is the path prefix of the router or group e names, and whether
// that is known.
func (rn *routerNames) prefix(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.Ident:
		if rn.bad[e.Name] {
			return "", false
		}
		if p, ok := rn.groups[e.Name]; ok {
			return p, true
		}
		return "", rn.roots[e.Name]
	case *ast.SelectorExpr: // a field: s.router
		return "", rn.roots[e.Sel.Name] && !rn.bad[e.Sel.Name] && !seen(rn.groups, e.Sel.Name)
	case *ast.CallExpr: // r.Group("/v1"), chained
		if !isGroup(e) {
			return "", false
		}
		parent, ok := rn.prefix(e.Fun.(*ast.SelectorExpr).X)
		rel, isStr := rn.g.str(e.Args[0])
		if !ok || !isStr {
			return "", false
		}
		return parent + rel, true // joined as echo does; joinRoute tidies gin's
	}
	return "", false
}

// joinRoute is a route's full path as the router names it: gin joins a
// group's prefix and the path like path.Join, keeping a trailing slash;
// echo concatenates them.
func joinRoute(fw, prefix, rel string) string {
	if fw == "echo" {
		return prefix + rel
	}
	full := prefix + "/" + rel
	if prefix == "" {
		full = rel
	}
	j := path.Clean("/" + full)
	if strings.HasSuffix(rel, "/") && !strings.HasSuffix(j, "/") || rel == "" && strings.HasSuffix(prefix, "/") && j != "/" {
		j += "/"
	}
	return j
}

// str is the string e spells, as a literal or one of the package's constants.
func (g *generator) str(e ast.Expr) (string, bool) {
	switch a := e.(type) {
	case *ast.BasicLit:
		if a.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(a.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := g.consts[a.Name]
		return s, ok
	}
	return "", false
}

func handlerName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.CallExpr: // http.HandlerFunc(h)
		if len(e.Args) == 1 {
			return handlerName(e.Args[0])
		}
	}
	return ""
}

func (g *generator) resolve() {
	routes := g.routes()
	for _, s := range g.slos {
		if s.explicitRte {
			continue
		}
		switch rs := routes[s.fn]; len(rs) {
		case 1:
			s.route = rs[0]
		case 0:
			g.errorf(s.pos, "// vis:slo: can't tell which route %s serves: nothing registers it with HandleFunc or Handle, or as a gin or echo route vis gen can follow. Say it with route=\"GET /path\"", s.fn)
		default:
			g.errorf(s.pos, "// vis:slo: %s serves %s; say which one with route=", s.fn, strings.Join(rs, ", "))
		}
	}
	signals := map[string]*signal{}
	for _, s := range g.signals {
		signals[s.v], signals[s.name] = s, s
	}
	knobs := map[string]*knob{}
	for _, k := range g.knobs {
		knobs[k.v], knobs[k.name] = k, k
	}
	cost := func(k *knob, key, c string) string {
		switch c {
		case "cpu":
			return "vis.CPU"
		case "memory":
			return "vis.Memory"
		}
		if s := signals[c]; s != nil {
			return "&" + s.v
		}
		g.errorf(k.pos, "// vis:knob %s: %s=%s: a cost is cpu, memory, or one of this package's // vis:signal gauges", k.v, key, c)
		return ""
	}
	for _, k := range g.knobs {
		for _, c := range splitList(k.rawUses) {
			k.uses = append(k.uses, cost(k, "uses", c))
		}
		for _, c := range splitList(k.rawSaves) {
			k.saves = append(k.saves, cost(k, "saves", c))
		}
	}
	for _, r := range g.rules {
		g.resolveRule(r, signals, knobs)
	}
}

// measures are what a rule's when= can watch, beyond the package's own gauges.

var doRE = regexp.MustCompile(`^(step|set)\((-?[0-9a-zµ._]+)\)$|^step\((-?[0-9.]+)%\)$|^reset$`)

// parseValue reads a knob value as vis holds it: an integer, or for a
// duration knob a Go duration in nanoseconds.
func parseValue(s string, duration bool) (int64, bool) {
	if duration {
		d, err := time.ParseDuration(s)
		return int64(d), err == nil
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	return n, err == nil
}

func (g *generator) resolveRule(r *rule, signals map[string]*signal, knobs map[string]*knob) {
	d := r.d
	g.unknownKeys(d, "when", "for", "do", "cooldown", "lease", "deadband", "max_drift", "stall", "mode", "every", "name")
	r.name = cmpOr(d.args["name"], r.k.name+"_rule")
	if !slices.Contains(r.k.pull, "vis.Rule") {
		g.errorf(d.pos, "// vis:rule %s: %s doesn't let rules change it; add rule to its pull=", r.name, r.k.v)
	}
	lhs, rhs, ok := strings.Cut(d.args["when"], ">")
	lhs, rhs = strings.TrimSpace(lhs), strings.TrimSpace(rhs)
	if !ok || lhs == "" || rhs == "" {
		g.errorf(d.pos, `// vis:rule %s needs when="<measure> > <threshold>", for example when="sched.p99 > 5ms"`, r.name)
		return
	}
	var latency bool
	switch m, known := spec.MeasureNamed(lhs); {
	case known:
		r.watch, latency = "vis."+m.Var, m.Kind == spec.Latency
	case lhs == "slo.p99":
		switch {
		case len(g.slos) == 0:
			g.errorf(d.pos, `// vis:rule %s: slo.p99 needs a // vis:slo in this package; for a route served elsewhere, name it: when="slo(GET /path).p99 > %s"`, r.name, rhs)
			return
		case len(g.slos) > 1:
			var routes []string
			for _, o := range g.slos {
				routes = append(routes, cmpOr(o.route, o.fn))
			}
			g.errorf(d.pos, `// vis:rule %s: slo.p99 is ambiguous: this package has %d SLOs (%s). Name the one to watch, such as when="slo(%s).p99 > %s"`,
				r.name, len(g.slos), strings.Join(routes, ", "), routes[0], rhs)
			return
		case g.slos[0].route == "":
			return // the // vis:slo says why
		}
		r.watch, latency = fmt.Sprintf("vis.RouteP99(%q)", g.slos[0].route), true
	case strings.HasPrefix(lhs, "slo(") && strings.HasSuffix(lhs, ").p99"):
		route := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(lhs, "slo("), ").p99"))
		if !strings.Contains(route, "/") {
			g.errorf(d.pos, "// vis:rule %s: slo(%s) names a route as vis.HTTP or a framework's middleware times it, such as slo(POST /api/generate)", r.name, route)
			return
		}
		r.watch, latency = fmt.Sprintf("vis.RouteP99(%q)", route), true
	case signals[lhs] != nil:
		s := signals[lhs]
		r.watch, latency = s.v+".Measure()", spec.TypeKind(map[bool]string{true: "Latency", false: "Gauge"}[s.latency]) == spec.Latency
	default:
		g.errorf(d.pos, "// vis:rule %s: when= watches sched.p99, gc.cpu, cpu.throttled, cpu.used, goroutines, slo.p99, slo(ROUTE).p99 or one of this package's signals; %s is none of them", r.name, lhs)
		return
	}
	r.over = g.level(r, "when", rhs, latency, knobs)
	if db := d.args["deadband"]; db != "" {
		r.extra = append(r.extra, "Deadband: "+g.level(r, "deadband", db, latency, knobs))
	}
	g.action(r, d.args["do"])
	for _, f := range []struct{ key, field string }{{"for", "For"}, {"cooldown", "Cooldown"}, {"lease", "Lease"}, {"every", "Every"}} {
		if v := d.args[f.key]; v != "" {
			dv, err := time.ParseDuration(v)
			if err != nil || dv <= 0 {
				g.errorf(d.pos, "// vis:rule %s: %s=%s is a duration, such as 30s", r.name, f.key, v)
				continue
			}
			r.extra = append(r.extra, f.field+": "+g.durExpr(dv))
		} else if f.key == "lease" {
			g.errorf(d.pos, "// vis:rule %s needs lease=: how long each step lasts", r.name)
		}
	}
	var drift int64
	if v := d.args["max_drift"]; v != "" { // in the knob's own unit, like do=step
		n, ok := r.k.parse(v)
		drift = n
		switch {
		case !ok && r.k.duration:
			g.errorf(d.pos, "// vis:rule %s: max_drift=%s: %s is a duration knob, so its drift is a duration, such as 10ms", r.name, v, r.k.v)
		case !ok:
			g.errorf(d.pos, "// vis:rule %s: max_drift=%s is a whole number", r.name, v)
		case r.k.duration:
			r.extra = append(r.extra, "MaxDrift: int64("+g.durExpr(time.Duration(n))+")")
		default:
			r.extra = append(r.extra, "MaxDrift: "+v)
		}
	}
	if from, to, ok := spec.Lurch(r.k.dv, r.k.lo, r.step, drift); ok {
		g.advise(d.pos, "// vis:rule %s: do=%s takes %s from %s to %s in one step, a %d%% cut: do=step(-25%%) cuts by a quarter each time, or max_drift=%s stops the rule at %s",
			r.name, d.args["do"], r.k.v, rangeEnd(r.k, from), rangeEnd(r.k, to), spec.Cut(from, to), rangeEnd(r.k, r.k.dv-from), rangeEnd(r.k, from))
	}
	g.stuck(r)
	if v := d.args["stall"]; v != "" {
		if _, err := strconv.Atoi(v); err != nil {
			g.errorf(d.pos, "// vis:rule %s: stall=%s is a whole number", r.name, v)
		} else {
			r.extra = append(r.extra, "Stall: "+v)
		}
	}
	switch d.args["mode"] {
	case "":
	case "shadow":
		r.extra = append(r.extra, "Shadow: true")
	default:
		g.errorf(d.pos, "// vis:rule %s: mode=%s: the only mode is shadow", r.name, d.args["mode"])
	}
}

// stuck advises on a rule that steps its knob toward the end of the range
// where its default sits: it can never move it, unless SetDefault starts the
// knob elsewhere. A 0 at the bottom is often a setting's "none", which a
// special value says.
func (g *generator) stuck(r *rule) {
	k, do := r.k, r.d.args["do"]
	down, up := r.step < 0 || r.pct < 0, r.step > 0 || r.pct > 0
	if k.rawLo == "" || k.isSpecial(k.dv) || !down && !up {
		return
	}
	switch {
	case down && k.dv <= k.lo:
		msg := fmt.Sprintf("// vis:rule %s: do=%s moves %s down, but its default %s is the bottom of its range, so the rule can't move it unless SetDefault starts it higher",
			r.name, do, k.v, rangeEnd(k, k.dv))
		if k.dv == 0 && k.special == "" {
			step := int64(1)
			if k.duration {
				step = int64(time.Second)
			}
			msg += fmt.Sprintf(". If %s means none, say so: special=%s:\"no limit\" with range=%s..%s", rangeEnd(k, 0), rangeEnd(k, 0), rangeEnd(k, step), k.rawHi)
		}
		g.advise(r.pos, "%s", msg)
	case up && k.dv >= k.hi:
		g.advise(r.pos, "// vis:rule %s: do=%s moves %s up, but its default %s is the top of its range, so the rule can't move it unless SetDefault starts it lower",
			r.name, do, k.v, rangeEnd(k, k.dv))
	}
}

// action is a rule's do=: step(N), step(P%), set(V) or reset, each value
// checked against the knob's range. A duration knob steps and sets by
// durations; a percent step is a share of the knob's value, whatever its kind.
func (g *generator) action(r *rule, do string) {
	k := r.k
	m := doRE.FindStringSubmatch(do)
	if m == nil {
		g.errorf(r.pos, "// vis:rule %s needs do=step(N), do=step(P%%), do=set(V) or do=reset: what to do to %s, for example do=step(-25%%)", r.name, k.v)
		return
	}
	if do == "reset" {
		r.extra = append(r.extra, "Do: vis.Reset()")
		return
	}
	if pct := m[3]; pct != "" {
		p, err := strconv.ParseFloat(pct, 64)
		switch {
		case err != nil:
			g.errorf(r.pos, "// vis:rule %s: do=%s: %s isn't a number", r.name, do, pct)
		case p == 0:
			g.errorf(r.pos, "// vis:rule %s: do=%s never moves %s", r.name, do, k.v)
		case p <= -100:
			g.errorf(r.pos, "// vis:rule %s: do=%s cuts %s to nothing in one step; give it more than -100%%, such as do=step(-25%%)", r.name, do, k.v)
		default:
			r.extra, r.pct = append(r.extra, "Do: vis.StepPercent("+strconv.FormatFloat(p, 'g', -1, 64)+")"), p
		}
		return
	}
	verb, arg := m[1], m[2]
	if k.level && verb != "set" {
		g.errorf(r.pos, "// vis:rule %s: do=%s: a rule sets a log level by name, such as do=set(debug), or ends what it set with do=reset", r.name, do)
		return
	}
	v, ok := k.parse(arg)
	unit := "a whole number"
	switch {
	case k.duration:
		unit = "a duration, such as 1ms"
	case k.level:
		unit = "levels, such as trace, debug, info, warn or error"
	}
	rng := fmt.Sprintf("%s..%s", rangeEnd(k, k.lo), rangeEnd(k, k.hi))
	switch {
	case !ok:
		g.errorf(r.pos, "// vis:rule %s: do=%s: %s's values are %s", r.name, do, k.v, unit)
	case verb == "step" && v == 0:
		g.errorf(r.pos, "// vis:rule %s: do=step(0) never moves %s", r.name, k.v)
	case verb == "step" && (v > k.hi-k.lo || -v > k.hi-k.lo):
		g.errorf(r.pos, "// vis:rule %s: do=%s is wider than %s's range %s", r.name, do, k.v, rng)
	case verb == "set" && k.isSpecial(v):
		g.errorf(r.pos, "// vis:rule %s: do=%s: a rule never sets %q, %s's special value; people and agents choose it", r.name, do, k.specialName, k.v)
	case verb == "set" && (v < k.lo || v > k.hi):
		g.errorf(r.pos, "// vis:rule %s: do=%s is outside %s's range %s", r.name, do, k.v, rng)
	case verb == "step" && k.duration:
		r.extra, r.step = append(r.extra, "Step: int64("+g.durExpr(time.Duration(v))+")"), v
	case verb == "step":
		r.extra, r.step = append(r.extra, "Step: "+arg), v
	case k.duration:
		r.extra = append(r.extra, "Do: vis.SetTo("+g.durExpr(time.Duration(v))+")")
	case k.level:
		r.extra = append(r.extra, "Do: vis.SetTo("+g.levelExpr(slog.Level(v))+")")
	default:
		r.extra = append(r.extra, "Do: vis.SetTo("+arg+")")
	}
}

func rangeEnd(k *knob, v int64) string {
	if k.level {
		return spec.LevelName(slog.Level(v))
	}
	if k.duration {
		return time.Duration(v).String()
	}
	return strconv.FormatInt(v, 10)
}

// level is a threshold or dead band as Go: a duration, a number, or a knob.
func (g *generator) level(r *rule, key, s string, latency bool, knobs map[string]*knob) string {
	if k := knobs[s]; k != nil {
		if k.level {
			g.errorf(r.pos, "// vis:rule %s: %s=%s: a log level isn't a threshold", r.name, key, s)
			return "nil"
		}
		if k.duration != latency {
			g.errorf(r.pos, "// vis:rule %s: %s is %s, but what it watches is %s", r.name, k.v, kindOf(k.duration), kindOf(latency))
		}
		return k.v
	}
	if dv, err := time.ParseDuration(s); err == nil {
		if !latency {
			g.errorf(r.pos, "// vis:rule %s: %s=%s is a latency, but what it watches is a plain number", r.name, key, s)
		}
		return "vis.Fixed(" + g.durExpr(dv) + ")"
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		if latency {
			g.errorf(r.pos, "// vis:rule %s: %s=%s needs a unit, such as %sms: what it watches is a latency", r.name, key, s, s)
		}
		return "vis.Fixed(" + s + ")"
	}
	g.errorf(r.pos, "// vis:rule %s: %s=%s is neither a number, a duration, nor a knob in this package", r.name, key, s)
	return "nil"
}

func kindOf(latency bool) string {
	if latency {
		return "a latency"
	}
	return "a plain number"
}

func (g *generator) write() ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by vis gen from this package's // vis: directives. DO NOT EDIT.\n\npackage %s\n\nimport (\n", g.pkg)
	if g.usesLog {
		b.WriteString("\t\"log/slog\"\n")
	}
	if g.usesDur {
		b.WriteString("\t\"time\"\n")
	}
	if g.usesLog || g.usesDur {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\t%q\n)\n\n", g.visPath)
	b.WriteString("func init() {\n")
	for _, s := range g.signals { // first: knobs' costs can name them
		fn, opts := "vis.DeclareGauge", []string{}
		if s.latency {
			fn = "vis.DeclareLatency"
		}
		if s.unit != "" {
			opts = append(opts, fmt.Sprintf("vis.Unit(%q)", s.unit))
		}
		if s.about != "" {
			opts = append(opts, fmt.Sprintf("vis.About(%q)", s.about))
		}
		fmt.Fprintf(&b, "\t%s(&%s, %q%s)\n", fn, s.v, s.name, prefixed(opts))
	}
	for _, k := range g.knobs {
		fn := "vis.DeclareInt"
		switch {
		case k.duration:
			fn = "vis.DeclareDuration"
		case k.level:
			fn = "vis.DeclareLogLevel"
		}
		var opts []string
		if k.min != "" {
			opts = append(opts, fmt.Sprintf("vis.Range(%s, %s)", k.min, k.max))
		}
		if k.def != "" {
			opts = append(opts, fmt.Sprintf("vis.Default(%s)", k.def))
		}
		if k.special != "" {
			opts = append(opts, fmt.Sprintf("vis.Special(%s, %q)", k.special, k.specialName))
		}
		if k.unit != "" {
			opts = append(opts, fmt.Sprintf("vis.Unit(%q)", k.unit))
		}
		if k.env != "" {
			opts = append(opts, fmt.Sprintf("vis.Env(%q)", k.env))
		}
		if len(k.pull) > 0 {
			opts = append(opts, "vis.Pull("+strings.Join(k.pull, ", ")+")")
		}
		if len(k.uses) > 0 {
			opts = append(opts, "vis.Uses("+strings.Join(k.uses, ", ")+")")
		}
		if len(k.saves) > 0 {
			opts = append(opts, "vis.Saves("+strings.Join(k.saves, ", ")+")")
		}
		if k.about != "" {
			opts = append(opts, fmt.Sprintf("vis.About(%q)", k.about))
		}
		fmt.Fprintf(&b, "\t%s(&%s, %q%s)\n", fn, k.v, k.name, prefixed(opts))
	}
	for _, s := range g.slos {
		fmt.Fprintf(&b, "\tvis.NewSLO(%q, vis.P99(%s)) // %s\n", s.route, s.p99, s.fn)
	}
	for _, r := range g.rules {
		fields := append([]string{fmt.Sprintf("Name: %q", r.name), "Knob: " + r.k.v, "Watch: " + r.watch, "Over: " + r.over}, r.extra...)
		fmt.Fprintf(&b, "\tvis.NewRule(vis.RuleSpec{\n\t\t%s,\n\t})\n", strings.Join(fields, ",\n\t\t"))
	}
	b.WriteString("}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, errors.Join(err, errors.New(b.String()))
	}
	return src, nil
}

func prefixed(opts []string) string {
	if len(opts) == 0 {
		return ""
	}
	return ", " + strings.Join(opts, ", ")
}
