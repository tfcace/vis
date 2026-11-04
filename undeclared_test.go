package vis_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tfcace/vis"
)

// A knob variable with a // vis:knob directive is declared by vis_gen.go. Before
// go generate has run, it is the zero value, and using it says so.
func TestAnUndeclaredKnobSaysToRunGoGenerate(t *testing.T) {
	var (
		n vis.Int
		d vis.Duration
		l vis.LogLevel
	)
	ctx := context.Background()
	uses := map[string]func(){
		"Int.Now":        func() { n.Now() },
		"Int.From":       func() { n.From(ctx) },
		"Int.Bind":       func() { n.Bind(func(int) {}) },
		"Int.Name":       func() { _ = n.Name() },
		"Int.Range":      func() { n.Range() },
		"Int.SetDefault": func() { _ = n.SetDefault(1) },
		"Duration.Now":   func() { d.Now() },
		"Duration.From":  func() { d.From(ctx) },
		"Duration.Bind":  func() { d.Bind(nil) },
		"LogLevel.Now":   func() { l.Now() },
		"LogLevel.Level": func() { l.Level() },
		"LogLevel.Bind":  func() { l.Bind(nil) },
		"NewRule":        func() { vis.NewRule(vis.RuleSpec{Name: "undeclared_knob_rule", Knob: n}) },
	}
	for name, use := range uses {
		t.Run(name, func(t *testing.T) {
			msg := panicOf(use)
			for _, want := range []string{"not declared", "go generate", "vis.Declare"} {
				if !strings.Contains(msg, want) {
					t.Errorf("panic %q, want it to say %q", msg, want)
				}
			}
		})
	}
}
