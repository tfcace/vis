// Package manifest reads what a service declares to vis (knobs, signals,
// objectives, rules) from its source, and says how two versions differ, for
// review: a change to a knob's bounds or to who may change it is reviewed
// like an API change.
//
// It reads the vis calls with type information, so hand-written calls and the
// ones vis gen writes from directives read the same. A value it can't compute
// from constants reads as "(computed)".
package manifest

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tfcace/vis/internal/spec"
	"golang.org/x/tools/go/packages"
)

const visPath = "github.com/tfcace/vis"

// Manifest is what a set of packages declares.
type Manifest struct {
	Knobs   map[string]Knob
	Signals map[string]Signal
	SLOs    map[string]SLO // by route
	Rules   map[string]Rule
}

// Knob is a declared knob, its values as written.
type Knob struct {
	Type              string // int, duration or level
	Min, Max, Default string
	Unit, About       string
	Env               string // the variable that sets the default when set (vis.Env)
	Special           string // the value outside the range the service gives its own meaning (vis.Special), "" if none
	SpecialName       string // what it means: "no limit"
	Pull, Uses, Saves []string
	minN, maxN, defN  int64
	numeric, defKnown bool
	specialN          int64
	specialKnown      bool
}

// Signal is a declared gauge.
type Signal struct{ Kind, Unit, About string }

// SLO is a declared objective.
type SLO struct{ P99 string }

// Rule is a declared rule.
type Rule struct {
	Knob, Watch, Over, Deadband, Action string
	For, Cooldown, Lease, Every         string
	MaxDrift, Stall                     string
	Shadow                              bool
}

// Extract loads the packages matching patterns, from dir, and reads their
// declarations. Test files aren't read.
func Extract(dir string, patterns ...string) (Manifest, error) {
	cfg := &packages.Config{Dir: dir, Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Knobs: map[string]Knob{}, Signals: map[string]Signal{}, SLOs: map[string]SLO{}, Rules: map[string]Rule{}}
	var errs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	})
	if len(errs) > 0 {
		return Manifest{}, fmt.Errorf("vis diff: %s", strings.Join(errs, "\n"))
	}
	for _, p := range pkgs {
		r := reader{fset: p.Fset, syntax: p.Syntax, info: p.TypesInfo, m: &m, names: map[types.Object]string{}, byVar: map[types.Object]Knob{}}
		r.read()
	}
	return m, nil
}

// FromFiles reads what one type-checked package declares, from its files
// (test files are skipped), as vis vet's analyzers see them. It also returns
// the knobs by the variables that hold them, so a check can ask about the
// knob a rule names.
func FromFiles(fset *token.FileSet, files []*ast.File, info *types.Info) (Manifest, map[types.Object]Knob) {
	m := Manifest{Knobs: map[string]Knob{}, Signals: map[string]Signal{}, SLOs: map[string]SLO{}, Rules: map[string]Rule{}}
	r := reader{fset: fset, syntax: files, info: info, m: &m, names: map[types.Object]string{}, byVar: map[types.Object]Knob{}}
	r.read()
	return m, r.byVar
}

// Range is the knob's range in its own representation (nanoseconds for a
// duration), when its ends are constants.
func (k Knob) Range() (lo, hi int64, ok bool) { return k.minN, k.maxN, k.numeric }

// DefaultValue is the knob's default in its own representation, when it is a constant.
func (k Knob) DefaultValue() (int64, bool) { return k.defN, k.defKnown }

// SpecialValue is the knob's special value in its own representation, when it declares one as a constant.
func (k Knob) SpecialValue() (int64, bool) { return k.specialN, k.specialKnown }

// Pulls is whether a writer ("agent", "rule", "fleet") may change the knob.
func (k Knob) Pulls(writer string) bool { return slices.Contains(k.Pull, writer) }

type reader struct {
	fset   *token.FileSet
	syntax []*ast.File
	info   *types.Info
	m      *Manifest
	names  map[types.Object]string // knob and gauge variables, to the names they're declared under
	byVar  map[types.Object]Knob   // knob variables, to their knobs
}

func (r *reader) files() []*ast.File {
	var out []*ast.File
	for _, f := range r.syntax {
		if !strings.HasSuffix(r.fset.File(f.Pos()).Name(), "_test.go") {
			out = append(out, f)
		}
	}
	return out
}

// visFunc is the name of the vis function a call calls, "" if none.
func (r *reader) visFunc(call *ast.CallExpr) string {
	fn := call.Fun
	if ix, ok := fn.(*ast.IndexExpr); ok {
		fn = ix.X
	}
	var id *ast.Ident
	switch f := fn.(type) {
	case *ast.SelectorExpr:
		id = f.Sel
	case *ast.Ident:
		id = f
	default:
		return ""
	}
	o := r.info.Uses[id]
	if o == nil || o.Pkg() == nil || o.Pkg().Path() != visPath {
		return ""
	}
	if _, isFunc := o.(*types.Func); !isFunc {
		return ""
	}
	return o.Name()
}

func (r *reader) str(e ast.Expr) string {
	if tv, ok := r.info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value)
	}
	return "(computed)"
}

func (r *reader) int(e ast.Expr) (int64, bool) {
	if tv, ok := r.info.Types[e]; ok && tv.Value != nil {
		if v, exact := constant.Int64Val(constant.ToInt(tv.Value)); exact {
			return v, true
		}
	}
	return 0, false
}

func (r *reader) isLevel(e ast.Expr) bool {
	t := r.info.TypeOf(e)
	return t != nil && types.TypeString(t, nil) == "log/slog.Level"
}

func (r *reader) isDuration(e ast.Expr) bool {
	t := r.info.TypeOf(e)
	return t != nil && types.TypeString(t, nil) == "time.Duration"
}

// value says a constant as written for its kind: 50ms for a duration.
func (r *reader) value(e ast.Expr, duration bool) (string, int64, bool) {
	v, ok := r.int(e)
	switch {
	case !ok:
		if tv, ok := r.info.Types[e]; ok && tv.Value != nil {
			if tv.Value.Kind() == constant.Float {
				f, _ := constant.Float64Val(tv.Value)
				return strconv.FormatFloat(f, 'g', -1, 64), 0, false
			}
			return tv.Value.ExactString(), 0, false
		}
		return "(computed)", 0, false
	case duration:
		return time.Duration(v).String(), v, true
	case r.isLevel(e):
		return spec.LevelName(slog.Level(v)), v, true
	}
	return fmt.Sprint(v), v, true
}

// varOf is the variable a declaration names: X in &X, or X itself.
func (r *reader) varOf(e ast.Expr) types.Object {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	if id, ok := e.(*ast.Ident); ok {
		if o := r.info.Uses[id]; o != nil {
			return o
		}
		return r.info.Defs[id]
	}
	return nil
}

func (r *reader) read() {
	// declarations first, so rules and costs can name what they refer to
	var rules []*ast.CompositeLit
	for _, f := range r.files() {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec: // var X = vis.NewInt(...)
				for i, name := range n.Names {
					if i < len(n.Values) {
						if c, ok := n.Values[i].(*ast.CallExpr); ok {
							r.declaration(c, r.info.Defs[name])
						}
					}
				}
				return true
			case *ast.CallExpr:
				switch r.visFunc(n) {
				case "DeclareInt", "DeclareDuration", "DeclareLogLevel", "DeclareGauge", "DeclareLatency":
					if len(n.Args) > 0 {
						r.declaration(n, r.varOf(n.Args[0]))
					}
				case "NewSLO", "SLO":
					r.slo(n)
				case "NewInt", "NewDuration", "NewLogLevel", "NewGauge", "NewLatency", "NewGaugeFunc":
					r.declaration(n, nil) // a ValueSpec saw it with its variable; this catches the rest
				}
			case *ast.CompositeLit:
				if t := r.info.TypeOf(n); t != nil && types.TypeString(t, nil) == visPath+".RuleSpec" {
					rules = append(rules, n)
				}
			}
			return true
		})
	}
	for _, lit := range rules {
		r.rule(lit)
	}
}

func (r *reader) declaration(c *ast.CallExpr, v types.Object) {
	fn := r.visFunc(c)
	var args []ast.Expr
	switch fn {
	case "NewInt", "NewDuration", "NewLogLevel", "NewGauge", "NewLatency":
		args = c.Args
	case "NewGaugeFunc":
		if len(c.Args) >= 2 {
			args = append([]ast.Expr{c.Args[0]}, c.Args[2:]...)
		}
	case "DeclareInt", "DeclareDuration", "DeclareLogLevel", "DeclareGauge", "DeclareLatency":
		if len(c.Args) > 1 {
			args = c.Args[1:]
		}
	default:
		return
	}
	if len(args) == 0 {
		return
	}
	name := r.str(args[0])
	if v != nil {
		r.names[v] = name
	}
	opts := r.options(args[1:], strings.Contains(fn, "Duration"))
	switch {
	case strings.Contains(fn, "Int") || strings.Contains(fn, "Duration") || strings.Contains(fn, "LogLevel"):
		k := opts
		k.Type = "int"
		switch {
		case strings.Contains(fn, "Duration"):
			k.Type = "duration"
		case strings.Contains(fn, "LogLevel"):
			k.Type = "level"
			if k.Min == "" { // vis's own range and default for a level
				k.Min, k.minN, k.Max, k.maxN, k.numeric = "DEBUG", int64(slog.LevelDebug), "ERROR", int64(slog.LevelError), true
			}
			if k.Default == "" {
				k.Default, k.defN, k.defKnown = "INFO", int64(slog.LevelInfo), true
			}
		}
		if _, dup := r.m.Knobs[name]; !dup || v != nil {
			r.m.Knobs[name] = k
		}
		if v != nil {
			r.byVar[v] = k
		}
	default:
		kind := "gauge"
		switch fn {
		case "NewLatency", "DeclareLatency":
			kind, opts.Unit = "latency", "ms"
		case "NewGaugeFunc":
			kind = "gauge, read when asked"
		}
		r.m.Signals[name] = Signal{Kind: kind, Unit: opts.Unit, About: opts.About}
	}
}

func (r *reader) options(opts []ast.Expr, duration bool) Knob {
	var k Knob
	for _, o := range opts {
		c, ok := o.(*ast.CallExpr)
		if !ok {
			continue
		}
		switch r.visFunc(c) {
		case "Range":
			if len(c.Args) == 2 {
				k.Min, k.minN, _ = r.value(c.Args[0], duration)
				k.Max, k.maxN, k.numeric = r.value(c.Args[1], duration)
			}
		case "Default":
			if len(c.Args) == 1 {
				k.Default, k.defN, k.defKnown = r.value(c.Args[0], duration)
			}
		case "Special":
			if len(c.Args) == 2 {
				k.Special, k.specialN, k.specialKnown = r.value(c.Args[0], duration)
				k.SpecialName = r.str(c.Args[1])
			}
		case "Unit":
			if len(c.Args) == 1 {
				k.Unit = r.str(c.Args[0])
			}
		case "Env":
			if len(c.Args) == 1 {
				k.Env = r.str(c.Args[0])
			}
		case "About":
			if len(c.Args) == 1 {
				k.About = r.str(c.Args[0])
			}
		case "Pull":
			for _, a := range c.Args {
				k.Pull = append(k.Pull, strings.ToLower(lastName(a)))
			}
		case "Uses", "Saves":
			var cs []string
			for _, a := range c.Args {
				cs = append(cs, r.cost(a))
			}
			if r.visFunc(c) == "Uses" {
				k.Uses = append(k.Uses, cs...)
			} else {
				k.Saves = append(k.Saves, cs...)
			}
		}
	}
	sort.Strings(k.Pull)
	sort.Strings(k.Uses)
	sort.Strings(k.Saves)
	return k
}

func lastName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return types.ExprString(e)
}

func (r *reader) cost(e ast.Expr) string {
	if o := r.varOf(e); o != nil {
		if n, ok := r.names[o]; ok {
			return n
		}
	}
	if s, ok := e.(*ast.SelectorExpr); ok {
		return strings.ToLower(s.Sel.Name) // vis.CPU, vis.Memory
	}
	return types.ExprString(e)
}

func (r *reader) slo(c *ast.CallExpr) {
	if len(c.Args) < 2 {
		return
	}
	route, p99 := r.str(c.Args[0]), "(computed)"
	switch t := c.Args[1].(type) {
	case *ast.CallExpr: // vis.P99(d)
		if r.visFunc(t) == "P99" && len(t.Args) == 1 {
			p99, _, _ = r.value(t.Args[0], true)
		}
	default: // the SLO option's duration
		p99, _, _ = r.value(t, true)
	}
	r.m.SLOs[route] = SLO{P99: p99}
}

func (r *reader) rule(lit *ast.CompositeLit) {
	fields := map[string]ast.Expr{}
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok {
				fields[k.Name] = kv.Value
			}
		}
	}
	name := "(unnamed)"
	if e := fields["Name"]; e != nil {
		name = r.str(e)
	}
	var rule Rule
	knobDuration := false
	if e := fields["Knob"]; e != nil {
		rule.Knob = types.ExprString(e)
		if o := r.varOf(e); o != nil {
			if n, ok := r.names[o]; ok {
				rule.Knob = n
				knobDuration = r.m.Knobs[n].Type == "duration"
			}
		}
	}
	if e := fields["Watch"]; e != nil {
		rule.Watch = types.ExprString(e)
	}
	rule.Over = r.level(fields["Over"])
	rule.Deadband = r.level(fields["Deadband"])
	switch {
	case fields["Do"] != nil:
		rule.Action = r.action(fields["Do"], knobDuration)
	case fields["Step"] != nil:
		v, _, _ := r.value(fields["Step"], knobDuration)
		rule.Action = "step(" + v + ")"
	}
	for _, f := range []struct {
		field string
		to    *string
	}{{"For", &rule.For}, {"Cooldown", &rule.Cooldown}, {"Lease", &rule.Lease}, {"Every", &rule.Every}} {
		if e := fields[f.field]; e != nil {
			*f.to, _, _ = r.value(e, true)
		}
	}
	for _, f := range []struct {
		field string
		to    *string
	}{{"MaxDrift", &rule.MaxDrift}, {"Stall", &rule.Stall}} {
		if e := fields[f.field]; e != nil {
			*f.to, _, _ = r.value(e, f.field == "MaxDrift" && knobDuration)
		}
	}
	if e := fields["Shadow"]; e != nil {
		if tv, ok := r.info.Types[e]; ok && tv.Value != nil {
			rule.Shadow = constant.BoolVal(tv.Value)
		}
	}
	r.m.Rules[name] = rule
}

func (r *reader) level(e ast.Expr) string {
	if e == nil {
		return ""
	}
	if o := r.varOf(e); o != nil {
		if n, ok := r.names[o]; ok {
			return "the knob " + n
		}
	}
	if c, ok := e.(*ast.CallExpr); ok && r.visFunc(c) == "Fixed" && len(c.Args) == 1 {
		v, _, _ := r.value(c.Args[0], r.isDuration(c.Args[0]))
		return v
	}
	return types.ExprString(e)
}

func (r *reader) action(e ast.Expr, duration bool) string {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return types.ExprString(e)
	}
	switch r.visFunc(c) {
	case "Reset":
		return "reset"
	case "SetTo":
		if len(c.Args) == 1 {
			v, _, _ := r.value(c.Args[0], duration)
			return "set(" + v + ")"
		}
	case "StepPercent":
		if len(c.Args) == 1 {
			v, _, _ := r.value(c.Args[0], false)
			return "step(" + v + "%)"
		}
	}
	return types.ExprString(e)
}

// Change is one difference between two manifests.
type Change struct {
	What  string // "knob cache_size", "rule batch_backoff"
	Says  string // what changed, as a reviewer reads it
	Wider bool   // gives automation more room, or loosens an objective: a new writer, a wider range, a new rule, a longer lease, a slower p99
}

func (c Change) String() string {
	mark := " "
	if c.Wider {
		mark = "!"
	}
	return fmt.Sprintf("%s %s: %s", mark, c.What, c.Says)
}

// Diff says how next differs from prev.
func Diff(prev, next Manifest) []Change {
	var out []Change
	for _, name := range keys(prev.Knobs, next.Knobs) {
		a, inA := prev.Knobs[name]
		b, inB := next.Knobs[name]
		what := "knob " + name
		switch {
		case !inA:
			out = append(out, Change{what, "added: " + describeKnob(b), len(b.Pull) > 0})
		case !inB:
			out = append(out, Change{what, "removed (it was " + describeKnob(a) + ")", false})
		default:
			out = append(out, knobChanges(what, a, b)...)
		}
	}
	for _, name := range keys(prev.Signals, next.Signals) {
		a, inA := prev.Signals[name]
		b, inB := next.Signals[name]
		what := "signal " + name
		switch {
		case !inA:
			out = append(out, Change{what, "added: a " + b.Kind + unitSuffix(b.Unit), false})
		case !inB:
			out = append(out, Change{what, "removed: rules or costs that named it no longer can", false})
		default:
			if a.Kind != b.Kind || a.Unit != b.Unit {
				out = append(out, Change{what, fmt.Sprintf("now a %s%s (was a %s%s)", b.Kind, unitSuffix(b.Unit), a.Kind, unitSuffix(a.Unit)), false})
			}
			if a.About != b.About {
				out = append(out, Change{what, fmt.Sprintf("description %q (was %q)", b.About, a.About), false})
			}
		}
	}
	for _, route := range keys(prev.SLOs, next.SLOs) {
		a, inA := prev.SLOs[route]
		b, inB := next.SLOs[route]
		what := "SLO " + route
		switch {
		case !inA:
			out = append(out, Change{what, "added: p99 " + b.P99, false})
		case !inB:
			out = append(out, Change{what, "removed (it was p99 " + a.P99 + "): rules and impact reports no longer answer to it", true})
		case a.P99 != b.P99:
			out = append(out, Change{what, fmt.Sprintf("p99 %s (was %s)", b.P99, a.P99), longer(b.P99, a.P99)})
		}
	}
	for _, name := range keys(prev.Rules, next.Rules) {
		a, inA := prev.Rules[name]
		b, inB := next.Rules[name]
		what := "rule " + name
		switch {
		case !inA:
			out = append(out, Change{what, "added: " + describeRule(b), !b.Shadow})
		case !inB:
			out = append(out, Change{what, "removed (it was " + describeRule(a) + ")", false})
		default:
			out = append(out, ruleChanges(what, a, b)...)
		}
	}
	return out
}

func keys[V any](a, b map[string]V) []string {
	var out []string
	for k := range a {
		out = append(out, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func unitSuffix(u string) string {
	if u == "" {
		return ""
	}
	return " in " + u
}

// orSpecial is a knob's special value as a description adds it: ` or 0 ("unlimited")`.
func orSpecial(k Knob) string {
	if k.Special == "" {
		return ""
	}
	return fmt.Sprintf(" or %s (%q)", k.Special, k.SpecialName)
}

func describeKnob(k Knob) string {
	s := fmt.Sprintf("%s %s..%s%s, default %s%s", k.Type, k.Min, k.Max, orSpecial(k), k.Default, unitSuffix(k.Unit))
	if len(k.Pull) > 0 {
		s += ", changed by people and " + strings.Join(k.Pull, ", ")
	} else {
		s += ", changed by people only"
	}
	return s
}

func knobChanges(what string, a, b Knob) []Change {
	var out []Change
	if a.Type != b.Type {
		out = append(out, Change{what, fmt.Sprintf("now %s (was %s)", b.Type, a.Type), false})
	}
	if a.Min != b.Min || a.Max != b.Max {
		wider := a.numeric && b.numeric && (b.minN < a.minN || b.maxN > a.maxN)
		out = append(out, Change{what, fmt.Sprintf("range %s..%s (was %s..%s)", b.Min, b.Max, a.Min, a.Max), wider || !b.numeric})
	}
	if a.Default != b.Default {
		out = append(out, Change{what, fmt.Sprintf("default %s (was %s)", b.Default, a.Default), false})
	}
	if a.Special != b.Special || a.SpecialName != b.SpecialName {
		switch { // a special value people and agents may choose, such as no limit, gives them room the range didn't
		case a.Special == "":
			out = append(out, Change{what, fmt.Sprintf("may also be %s, %q (was its range alone)", b.Special, b.SpecialName), true})
		case b.Special == "":
			out = append(out, Change{what, fmt.Sprintf("its range alone now (was also %s, %q)", a.Special, a.SpecialName), false})
		default:
			out = append(out, Change{what, fmt.Sprintf("may also be %s, %q (was %s, %q)", b.Special, b.SpecialName, a.Special, a.SpecialName), true})
		}
	}
	if a.Unit != b.Unit {
		out = append(out, Change{what, fmt.Sprintf("unit %q (was %q)", b.Unit, a.Unit), false})
	}
	if a.Env != b.Env {
		was := "the code alone"
		if a.Env != "" {
			was = "$" + a.Env
		}
		now := "the code alone sets its default"
		if b.Env != "" {
			now = "$" + b.Env + " sets its default when set"
		}
		out = append(out, Change{what, fmt.Sprintf("%s (was %s)", now, was), false})
	}
	for _, w := range b.Pull {
		if !slices.Contains(a.Pull, w) {
			out = append(out, Change{what, w + "s may change it now", true})
		}
	}
	for _, w := range a.Pull {
		if !slices.Contains(b.Pull, w) {
			out = append(out, Change{what, w + "s may no longer change it", false})
		}
	}
	if !slices.Equal(a.Uses, b.Uses) || !slices.Equal(a.Saves, b.Saves) {
		out = append(out, Change{what, fmt.Sprintf("costs: uses %s, saves %s (was uses %s, saves %s)", list(b.Uses), list(b.Saves), list(a.Uses), list(a.Saves)), false})
	}
	if a.About != b.About {
		out = append(out, Change{what, fmt.Sprintf("description %q (was %q)", b.About, a.About), false})
	}
	return out
}

func list(s []string) string {
	if len(s) == 0 {
		return "nothing"
	}
	return strings.Join(s, ", ")
}

func describeRule(r Rule) string {
	s := fmt.Sprintf("when %s > %s, %s %s", r.Watch, r.Over, r.Action, r.Knob)
	if r.Lease != "" {
		s += ", each for " + r.Lease
	}
	if r.Shadow {
		s += ", in shadow"
	}
	return s
}

func ruleChanges(what string, a, b Rule) []Change {
	var out []Change
	field := func(name, va, vb string, wider bool) {
		if va != vb {
			out = append(out, Change{what, fmt.Sprintf("%s %s (was %s)", name, orDefault(vb), orDefault(va)), wider})
		}
	}
	field("knob", a.Knob, b.Knob, true)
	field("watches", a.Watch, b.Watch, false)
	field("threshold", a.Over, b.Over, false)
	field("dead band", a.Deadband, b.Deadband, false)
	field("action", a.Action, b.Action, true)
	field("for", a.For, b.For, false)
	field("cooldown", a.Cooldown, b.Cooldown, false)
	field("lease", a.Lease, b.Lease, longer(b.Lease, a.Lease))
	field("every", a.Every, b.Every, false)
	field("max drift", a.MaxDrift, b.MaxDrift, true)
	field("stall", a.Stall, b.Stall, false)
	if a.Shadow != b.Shadow {
		says, wider := "now acts (it was in shadow)", true
		if b.Shadow {
			says, wider = "now in shadow: it records, without acting", false
		}
		out = append(out, Change{what, says, wider})
	}
	return out
}

func orDefault(s string) string {
	if s == "" {
		return "the default"
	}
	return s
}

// longer is whether duration a is longer than b, when both parse.
func longer(a, b string) bool {
	da, errA := time.ParseDuration(a)
	db, errB := time.ParseDuration(b)
	return errA == nil && errB == nil && da > db
}
