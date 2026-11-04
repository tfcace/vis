// Package vet holds vis vet's checks, as go/analysis analyzers: the mistakes
// in using vis that compile and run but are wrong. Each says how to fix it.
// Test files are not checked: tests declare objectives and rules freely,
// often wrong on purpose.
package vet

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/analysis"

	"github.com/tfcace/vis/internal/gen"
	"github.com/tfcace/vis/internal/manifest"
	"github.com/tfcace/vis/internal/spec"
)

const visPath = "github.com/tfcace/vis"

// Analyzers are vis vet's checks, in the order they run.
var Analyzers = []*analysis.Analyzer{Directives, KnobInit, Rules, SLOs}

// Directives checks every // vis: directive, as vis gen reads them, and that
// vis_gen.go is what they generate.
var Directives = &analysis.Analyzer{
	Name: "visdirectives",
	Doc:  "check // vis: directives and that vis_gen.go is up to date with them",
	Run:  runDirectives,
}

// KnobInit checks for knobs read during package initialization, and for
// knobs and gauges vis gen declares being used before its init runs.
var KnobInit = &analysis.Analyzer{
	Name: "visknobinit",
	Doc:  "check that knobs aren't read, or copied before vis gen declares them, during package initialization",
	Run:  runKnobInit,
}

// Rules checks hand-written rules: a knob rules may change, and a threshold
// and dead band in the unit of what the rule watches.
var Rules = &analysis.Analyzer{
	Name: "visrules",
	Doc:  "check vis.RuleSpec literals against their knob and what they watch",
	Run:  runRules,
}

// SLOs checks that an objective names a route a handler in its package is
// registered under.
var SLOs = &analysis.Analyzer{
	Name: "visslos",
	Doc:  "check that each SLO, and each route a rule watches, names a route its package registers",
	Run:  runSLOs,
}

func fileName(pass *analysis.Pass, n ast.Node) string { return pass.Fset.File(n.Pos()).Name() }

func isTest(pass *analysis.Pass, n ast.Node) bool {
	return strings.HasSuffix(fileName(pass, n), "_test.go")
}

func isGenerated(pass *analysis.Pass, n ast.Node) bool {
	return filepath.Base(fileName(pass, n)) == gen.File
}

func runDirectives(pass *analysis.Pass) (any, error) {
	var files []*ast.File
	var dir, genPath string
	var genFile *ast.File
	for _, f := range pass.Files {
		switch {
		case isTest(pass, f):
			return nil, nil // the test variant: its package was checked on its own
		case isGenerated(pass, f):
			genPath, genFile = fileName(pass, f), f
		default:
			files, dir = append(files, f), filepath.Dir(fileName(pass, f))
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	src, diags, err := gen.FromFiles(pass.Fset, files)
	for _, d := range diags {
		pass.Reportf(d.Pos, "%s", d.Msg)
	}
	if src == nil && len(diags) > 0 || err != nil { // mistakes, not just advice: there's no source to compare
		return nil, err
	}
	at := files[0].Package
	if genFile != nil {
		at = genFile.Package
	} else {
		genPath = filepath.Join(dir, gen.File)
	}
	have, readErr := os.ReadFile(genPath)
	switch {
	case src == nil && readErr == nil:
		pass.Reportf(at, "%s has no // vis: directives behind it: run go generate, which removes it", gen.File)
	case src != nil && readErr != nil:
		pass.Reportf(at, "this package has // vis: directives but no %s: run go generate (with //go:generate go tool vis gen in a file of the package)", gen.File)
	case src != nil && !bytes.Equal(src, have):
		pass.Reportf(at, "%s is out of date with the // vis: directives: run go generate", gen.File)
	}
	return nil, nil
}

// visNamed is whether t, or what it points to, is the vis type name.
func visNamed(t types.Type, names ...string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == visPath && slices.Contains(names, n.Obj().Name())
}

// visObj is the vis package-level object e names, if it names one.
func visObj(pass *analysis.Pass, e ast.Expr) types.Object {
	var id *ast.Ident
	switch e := e.(type) {
	case *ast.SelectorExpr:
		id = e.Sel
	case *ast.Ident:
		id = e
	default:
		return nil
	}
	o := pass.TypesInfo.Uses[id]
	if o == nil || o.Pkg() == nil || o.Pkg().Path() != visPath {
		return nil
	}
	return o
}

// declaredLater are the package-level knobs and gauges with no value: vis gen
// (or other init code) declares them in init.
func declaredLater(pass *analysis.Pass) map[types.Object]bool {
	out := map[types.Object]bool{}
	for _, f := range pass.Files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				if len(vs.Values) > 0 {
					continue
				}
				for _, n := range vs.Names {
					if o := pass.TypesInfo.Defs[n]; o != nil && visNamed(o.Type(), "Int", "Duration", "LogLevel", "Gauge", "Latency") {
						out[o] = true
					}
				}
			}
		}
	}
	return out
}

func runKnobInit(pass *analysis.Pass) (any, error) {
	later := declaredLater(pass)
	reads := func(n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false // runs later, when it's called, not during initialization
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !slices.Contains([]string{"Get", "Now", "From", "Level"}, sel.Sel.Name) {
				return true
			}
			if t := pass.TypesInfo.TypeOf(sel.X); t != nil && visNamed(t, "Int", "Duration", "LogLevel") {
				pass.Reportf(call.Pos(), "%s.%s runs during package initialization, before anything can change the knob, so the value goes stale: read it where it is used",
					types.ExprString(sel.X), sel.Sel.Name)
			}
			return true
		})
	}
	copies := func(n ast.Node, where string) {
		var addr ast.Expr // &X takes the variable itself, which is fine
		ast.Inspect(n, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false // runs later, after vis gen's init
			}
			if u, ok := n.(*ast.UnaryExpr); ok && u.Op == token.AND {
				addr = u.X
			}
			id, ok := n.(*ast.Ident)
			if !ok || id == addr {
				return true
			}
			if o := pass.TypesInfo.Uses[id]; later[o] {
				pass.Reportf(id.Pos(), "%s is declared in vis gen's init, which runs after %s, so %s is still empty here: use it inside a function, or declare what needs it with a // vis: directive",
					id.Name, where, id.Name)
			}
			return true
		})
	}
	for _, f := range pass.Files {
		if isTest(pass, f) || isGenerated(pass, f) {
			continue
		}
		early := filepath.Base(fileName(pass, f)) < gen.File // its init runs before vis_gen.go's
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					for _, v := range s.(*ast.ValueSpec).Values {
						reads(v)
						copies(v, "package-level variables are initialized")
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "init" && d.Body != nil {
					reads(d.Body)
					if early {
						copies(d.Body, "this file's init (files' inits run in name order)")
					}
				}
			}
		}
	}
	return nil, nil
}

type kind = spec.Kind

const (
	unknown = spec.Unknown
	latency = spec.Latency
	number  = spec.Number
)

// visTypeName is the vis type t is, or points to, "" if none.
func visTypeName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == visPath {
		return n.Obj().Name()
	}
	return ""
}

// watched is the kind of measure e is, when it can tell.
func watched(pass *analysis.Pass, e ast.Expr) kind {
	if o := visObj(pass, e); o != nil {
		if m, ok := spec.MeasureVar(o.Name()); ok {
			return m.Kind
		}
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return unknown
	}
	if o := visObj(pass, call.Fun); o != nil && o.Name() == "RouteP99" {
		return spec.RouteP99
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Measure" {
		if t := pass.TypesInfo.TypeOf(sel.X); t != nil {
			return spec.TypeKind(visTypeName(t))
		}
	}
	return unknown
}

// level is the kind of threshold e is: vis.Fixed of a duration or a number, or a knob.
func level(pass *analysis.Pass, e ast.Expr) kind {
	if t := pass.TypesInfo.TypeOf(e); t != nil {
		if n := visTypeName(t); n == "Duration" || n == "Int" {
			return spec.TypeKind(n)
		}
	}
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return unknown
	}
	fn := call.Fun
	if ix, ok := fn.(*ast.IndexExpr); ok { // vis.Fixed[time.Duration](d)
		fn = ix.X
	}
	if o := visObj(pass, fn); o == nil || o.Name() != "Fixed" {
		return unknown
	}
	if t := pass.TypesInfo.TypeOf(call.Args[0]); t != nil && types.TypeString(t, nil) == "time.Duration" {
		return latency
	}
	return number
}

func runRules(pass *analysis.Pass) (any, error) {
	_, knobs := manifest.FromFiles(pass.Fset, pass.Files, pass.TypesInfo) // what each knob variable declares, by hand or by vis gen
	for _, f := range pass.Files {
		if isTest(pass, f) || isGenerated(pass, f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !visNamed(pass.TypesInfo.TypeOf(lit), "RuleSpec") {
				return true
			}
			fields := map[string]ast.Expr{}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						fields[k.Name] = kv.Value
					}
				}
			}
			if k := fields["Knob"]; k != nil {
				checkKnob(pass, lit, fields, k, knobs)
			}
			w := watched(pass, fields["Watch"])
			for _, name := range []string{"Over", "Deadband"} {
				e := fields[name]
				if e == nil || w == unknown {
					continue
				}
				if l := level(pass, e); l != unknown && l != w {
					fix := "vis.Fixed(400*time.Millisecond) or a Duration knob"
					if w == number {
						fix = "vis.Fixed(0.9) or an Int knob"
					}
					pass.Reportf(e.Pos(), "%s is %s, so %s must be one too: %s; as written, vis.NewRule panics at startup",
						types.ExprString(fields["Watch"]), w, name, fix)
				}
			}
			return true
		})
	}
	return nil, nil
}

// checkKnob reports a rule its knob can't take: one rules may not change, no
// action, a step wider than the knob's range or a value outside it, a
// nanosecond step on a Duration knob, or a percent step that never moves it or
// cuts it to nothing, when the values are constants. It also reports a fixed
// step that, within the rule's reach, cuts the knob by more than half.
func checkKnob(pass *analysis.Pass, lit *ast.CompositeLit, fields map[string]ast.Expr, k ast.Expr, knobs map[types.Object]manifest.Knob) {
	if fields["Step"] == nil && fields["Do"] == nil {
		pass.Reportf(lit.Pos(), "this rule has no action, so vis.NewRule panics at startup: give it a Step, or a Do (vis.SetTo(v) or vis.Reset())")
		return
	}
	id, ok := k.(*ast.Ident)
	if !ok {
		return
	}
	knob, ok := knobs[pass.TypesInfo.Uses[id]]
	if !ok {
		return // declared elsewhere: its own package is checked on its own
	}
	if !knob.Pulls("rule") {
		pass.Reportf(k.Pos(), "%s doesn't let rules change it, so this rule can't act (vis.NewRule panics at startup): add vis.Pull(vis.Rule) to its declaration", id.Name)
	}
	if knob.Type == "duration" {
		for _, f := range []string{"Step", "MaxDrift"} {
			if e := fields[f]; e != nil {
				if v, ok := intConst(pass, e); ok && v != 0 && max(v, -v) < int64(spec.MinDurationStep) {
					pass.Reportf(e.Pos(), "a %s of %d on %s, a Duration knob, is %dns: write it as a duration, such as int64(5*time.Millisecond); as written, vis.NewRule panics at startup", f, v, id.Name, v)
				}
			}
		}
	}
	lo, hi, ok := knob.Range()
	if !ok {
		return
	}
	if e := fields["Step"]; e != nil {
		v, ok := intConst(pass, e)
		if ok && (v > hi-lo || -v > hi-lo) {
			pass.Reportf(e.Pos(), "a Step of %d is wider than %s's range, so vis.NewRule panics at startup", v, id.Name)
			return
		}
		var drift int64
		if d := fields["MaxDrift"]; d != nil {
			if drift, ok = intConst(pass, d); !ok {
				return // a drift vet can't read: it may stop the rule anywhere
			}
		}
		def, known := knob.DefaultValue()
		if ok && known {
			stuck(pass, e, id.Name, knob, def, lo, hi, v < 0)
		}
		if from, to, lurches := spec.Lurch(def, lo, v, drift); ok && known && lurches {
			show := func(n int64) string { return fmt.Sprint(n) }
			if knob.Type == "duration" {
				show = func(n int64) string { return time.Duration(n).String() }
			}
			pass.Reportf(e.Pos(), "a Step of %s takes %s from %s to %s in one step, a %d%% cut: Do: vis.StepPercent(-25) cuts by a quarter each time, or MaxDrift: %s stops the rule at %s",
				types.ExprString(e), id.Name, show(from), show(to), spec.Cut(from, to), show(def-from), show(from))
		}
	}
	if c, ok := fields["Do"].(*ast.CallExpr); ok && len(c.Args) == 1 {
		switch fn := visObj(pass, c.Fun); {
		case fn == nil:
		case fn.Name() == "SetTo":
			if v, ok := intConst(pass, c.Args[0]); ok && knob.Special != "" && func() bool { s, k := knob.SpecialValue(); return k && s == v }() {
				pass.Reportf(c.Args[0].Pos(), "SetTo gives %s its special value %q, which a rule never sets (vis.NewRule panics at startup): people and agents choose it", id.Name, knob.SpecialName)
			} else if ok && (v < lo || v > hi) {
				pass.Reportf(c.Args[0].Pos(), "SetTo puts %s outside its range, so vis.NewRule panics at startup", id.Name)
			}
		case fn.Name() == "StepPercent":
			tv, ok := pass.TypesInfo.Types[c.Args[0]]
			if !ok || tv.Value == nil {
				break
			}
			p, _ := constant.Float64Val(constant.ToFloat(tv.Value))
			if def, known := knob.DefaultValue(); known && p != 0 {
				stuck(pass, c.Args[0], id.Name, knob, def, lo, hi, p < 0)
			}
			switch {
			case p == 0:
				pass.Reportf(c.Args[0].Pos(), "StepPercent(0) never moves %s, so vis.NewRule panics at startup", id.Name)
			case p <= -100:
				pass.Reportf(c.Args[0].Pos(), "StepPercent(%g) cuts %s to nothing in one step, so vis.NewRule panics at startup: give it more than -100%%, such as -25", p, id.Name)
			}
		}
	}
}

// stuck reports a rule that steps its knob toward the end of the range where
// its default sits: it can never move it, unless SetDefault starts it
// elsewhere. A 0 at the bottom is often a setting's "none", which a special
// value says. A default at the special value is meant: the rule stands aside there.
func stuck(pass *analysis.Pass, e ast.Expr, name string, knob manifest.Knob, def, lo, hi int64, down bool) {
	if s, ok := knob.SpecialValue(); ok && s == def {
		return
	}
	show := func(n int64) string { return fmt.Sprint(n) }
	zero, one := "0", "1"
	if knob.Type == "duration" {
		show = func(n int64) string { return time.Duration(n).String() }
		zero, one = "time.Duration(0)", "1s"
	}
	switch {
	case down && def <= lo:
		msg := fmt.Sprintf("a Step of %s moves %s down, but its default %s is the bottom of its range, so this rule can't move it unless SetDefault starts it higher",
			types.ExprString(e), name, show(def))
		if def == 0 && knob.Special == "" {
			msg += fmt.Sprintf(". If %s means none, say so: vis.Special(%s, \"no limit\") with a range from %s", show(0), zero, one)
		}
		pass.Reportf(e.Pos(), "%s", msg)
	case !down && def >= hi:
		pass.Reportf(e.Pos(), "a Step of %s moves %s up, but its default %s is the top of its range, so this rule can't move it unless SetDefault starts it lower",
			types.ExprString(e), name, show(def))
	}
}

func intConst(pass *analysis.Pass, e ast.Expr) (int64, bool) {
	tv, ok := pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(tv.Value)
}

func runSLOs(pass *analysis.Pass) (any, error) {
	var patterns []string
	type objective struct {
		at    ast.Expr
		route string
		rule  bool // vis.RouteP99, which a rule watches
	}
	var objectives []objective
	namesRoutes, unreadable := false, false
	rt := newRouters(pass)
	str := func(e ast.Expr) (string, bool) {
		tv, ok := pass.TypesInfo.Types[e]
		if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
			return "", false
		}
		return constant.StringVal(tv.Value), true
	}
	for _, f := range pass.Files {
		if isTest(pass, f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if o := pass.TypesInfo.Uses[n]; o != nil && o.Pkg() != nil && o.Pkg().Path() == visPath && o.Name() == "RouteName" {
					namesRoutes = true
				}
			case *ast.CallExpr:
				if len(n.Args) == 0 {
					return true
				}
				if o := visObj(pass, n.Fun); o != nil && (o.Name() == "NewSLO" || o.Name() == "SLO" || o.Name() == "RouteP99") {
					if r, ok := str(n.Args[0]); ok {
						objectives = append(objectives, objective{n.Args[0], r, o.Name() == "RouteP99"})
					}
					return true
				}
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
				if !ok || fn.Pkg() == nil {
					return true
				}
				var ps []string
				switch pkg := fn.Pkg().Path(); {
				case pkg == "net/http" && len(n.Args) == 2 && (sel.Sel.Name == "HandleFunc" || sel.Sel.Name == "Handle"):
					if p, ok := str(n.Args[0]); ok {
						ps = []string{p}
					}
				case pkg == ginPath || strings.HasPrefix(pkg, echoPath):
					var read bool
					if ps, read = rt.patterns(sel, n, pkg == ginPath); !read {
						unreadable = true
					}
				}
				for _, p := range ps {
					if !slices.Contains(patterns, p) {
						patterns = append(patterns, p)
					}
				}
			}
			return true
		})
	}
	// Routes registered elsewhere, named by RouteName, or registered where vet
	// can't read the path: nothing to compare against.
	if len(patterns) == 0 || namesRoutes || unreadable {
		return nil, nil
	}
	sort.Strings(patterns)
	for _, o := range objectives {
		if !slices.Contains(patterns, o.route) {
			consequence := "this SLO would never see a request"
			if o.rule {
				consequence = "this rule would watch a route that never sees a request"
			}
			if len(patterns) <= 5 {
				pass.Reportf(o.at.Pos(), "no handler in this package is registered under %q (it registers %s), so %s: name the route as the package registers it",
					o.route, quoted(patterns), consequence)
			} else {
				pass.Reportf(o.at.Pos(), "no handler in this package is registered under %q; the nearest it registers are %s, so %s: name the route as the package registers it",
					o.route, quoted(nearest(o.route, patterns, 3)), consequence)
			}
		}
	}
	return nil, nil
}

const (
	ginPath  = "github.com/gin-gonic/gin"
	echoPath = "github.com/labstack/echo" // any major version
)

// routers reads the routes a package registers with gin or echo, named as
// visgin and visecho time them: "METHOD /path/:param", with the prefix of the
// group the route is registered on.
type routers struct {
	pass *analysis.Pass
	set  map[types.Object][]ast.Expr // what each variable is assigned
}

func newRouters(pass *analysis.Pass) *routers {
	rt := &routers{pass: pass, set: map[types.Object][]ast.Expr{}}
	bind := func(id *ast.Ident, v ast.Expr) {
		o := pass.TypesInfo.Defs[id]
		if o == nil {
			o = pass.TypesInfo.Uses[id]
		}
		if o != nil {
			rt.set[o] = append(rt.set[o], v)
		}
	}
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Lhs) == len(n.Rhs) {
					for i, l := range n.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							bind(id, n.Rhs[i])
						}
					}
				}
			case *ast.ValueSpec:
				if len(n.Names) == len(n.Values) {
					for i, id := range n.Names {
						bind(id, n.Values[i])
					}
				}
			}
			return true
		})
	}
	return rt
}

// anyMethods are the methods gin's and echo's Any register a route under.
var anyMethods = []string{"GET", "POST", "PUT", "PATCH", "HEAD", "OPTIONS", "DELETE", "CONNECT", "TRACE"}

// patterns are the routes call registers, if it registers any, and whether
// they could be read.
func (rt *routers) patterns(sel *ast.SelectorExpr, call *ast.CallExpr, gin bool) ([]string, bool) {
	var methods []string
	pathArg := 0
	switch name := sel.Sel.Name; name {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		methods = []string{name}
	case "Any":
		methods = anyMethods
	case "Handle", "Add": // gin's Handle(method, path, ...), echo's Add(method, path, ...)
		if len(call.Args) < 3 {
			return nil, true
		}
		m, ok := rt.str(call.Args[0])
		if !ok {
			return nil, false
		}
		methods, pathArg = []string{m}, 1
	case "Match", "RouteNotFound":
		return nil, false
	default: // Group, Use, Static and the rest register no route vet follows
		return nil, true
	}
	if len(call.Args) <= pathArg {
		return nil, false
	}
	prefix, ok := rt.prefix(sel.X, gin, 0)
	rel, isStr := rt.str(call.Args[pathArg])
	if !ok || !isStr {
		return nil, false
	}
	path := prefix + rel
	if gin {
		path = joinGin(prefix, rel)
	}
	out := make([]string, len(methods))
	for i, m := range methods {
		out[i] = m + " " + path
	}
	return out, true
}

// prefix is the path prefix of the router or group e is, and whether vet can tell.
func (rt *routers) prefix(e ast.Expr, gin bool, depth int) (string, bool) {
	if depth > 8 {
		return "", false
	}
	if t := rt.pass.TypesInfo.TypeOf(e); t != nil {
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil {
			pkg := n.Obj().Pkg().Path()
			if (pkg == ginPath || strings.HasPrefix(pkg, echoPath)) && (n.Obj().Name() == "Engine" || n.Obj().Name() == "Echo") {
				return "", true // the router itself
			}
		}
	}
	switch e := ast.Unparen(e).(type) {
	case *ast.CallExpr: // r.Group("/v1")
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Group" || len(e.Args) == 0 {
			return "", false
		}
		parent, ok := rt.prefix(sel.X, gin, depth+1)
		rel, isStr := rt.str(e.Args[0])
		if !ok || !isStr {
			return "", false
		}
		if gin {
			return joinGin(parent, rel), true
		}
		return parent + rel, true
	case *ast.Ident: // v1 := r.Group("/v1"), assigned once
		if vs := rt.set[rt.pass.TypesInfo.Uses[e]]; len(vs) == 1 {
			return rt.prefix(vs[0], gin, depth+1)
		}
	}
	return "", false
}

func (rt *routers) str(e ast.Expr) (string, bool) {
	tv, ok := rt.pass.TypesInfo.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// joinGin joins a group's prefix and a route's path as gin does: like
// path.Join, keeping a trailing slash.
func joinGin(prefix, rel string) string {
	if rel == "" {
		if prefix == "" {
			return "/"
		}
		return prefix
	}
	j := filepath.ToSlash(filepath.Clean("/" + prefix + "/" + rel))
	if strings.HasSuffix(rel, "/") && !strings.HasSuffix(j, "/") {
		j += "/"
	}
	return j
}

// nearest are the n patterns closest to route, by edit distance.
func nearest(route string, patterns []string, n int) []string {
	ranked := slices.Clone(patterns)
	sort.SliceStable(ranked, func(i, j int) bool { return distance(route, ranked[i]) < distance(route, ranked[j]) })
	return ranked[:min(n, len(ranked))]
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func quoted(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
