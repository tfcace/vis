package vis

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tfcace/vis/internal/spec"
)

// Info describes a declared knob: what the manifest, the admin UI and MCP show.
type Info struct {
	Name    string
	Type    string // "int", "duration" or "level"
	Unit    string
	About   string     // what it does and what turning it costs
	Uses    []Resource // what turning it up uses more of
	Saves   []Resource // what turning it up saves
	Min     int64
	Max     int64
	Default int64
	Pull    []Kind // automated writers that may change it; people always may
	Env     string // the environment variable that may set Default (vis.Env)
	FromEnv bool   // it did
	// The value outside the range the service gives its own meaning, when it
	// declares one (vis.Special): Special is the value, SpecialName what it means.
	HasSpecial  bool
	Special     int64
	SpecialName string
	d           *decl
}

// Knobs lists every declared knob, in declaration order.
func Knobs() []Info {
	ds := allDecls()
	out := make([]Info, len(ds))
	for i, d := range ds {
		out[i] = d.info()
	}
	return out
}

// Lookup describes one knob by name.
func Lookup(name string) (Info, bool) {
	d := lookup(name)
	if d == nil {
		return Info{}, false
	}
	return d.info(), true
}

func (d *decl) info() Info {
	i := Info{Name: d.name, Type: "int", Unit: d.unit, About: d.about, Min: d.min, Max: d.max, Default: d.def, Env: d.env, FromEnv: d.fromEnv,
		HasSpecial: d.hasSpecial, Special: d.special, SpecialName: d.specialName, d: d}
	if d.isDuration {
		i.Type = "duration"
	}
	if d.isLevel {
		i.Type = "level"
	}
	for _, s := range d.spends {
		if s.lower {
			i.Saves = append(i.Saves, s.r())
		} else {
			i.Uses = append(i.Uses, s.r())
		}
	}
	for _, k := range []Kind{Agent, Rule, Fleet} {
		if d.pull&(1<<k) != 0 {
			i.Pull = append(i.Pull, k)
		}
	}
	return i
}

// Format writes a value in the knob's own terms: "1m30s" for a duration, "WARN" for a level, "2000" for an int.
func (i Info) Format(v int64) string { return i.d.format(v) }

// Parse reads a value in the knob's own terms. A duration needs its unit, so
// "1500" is an error rather than 1.5 microseconds. The special value is read
// by its name ("no limit"). Parse doesn't check the range; Apply does.
func (i Info) Parse(s string) (int64, error) { return i.d.parse(s) }

// orSpecial is how a parse error names the special value: ` or "no limit"`.
func (d *decl) orSpecial() string {
	if !d.hasSpecial {
		return ""
	}
	return fmt.Sprintf(" or %q", d.specialName)
}

func (d *decl) parse(s string) (int64, error) {
	if d.hasSpecial && strings.EqualFold(strings.TrimSpace(s), d.specialName) {
		return d.special, nil
	}
	if d.isLevel {
		l, err := spec.ParseLevel(s)
		if err != nil {
			return 0, fmt.Errorf("vis: %s takes a log level: trace, debug, info, warn or error, or one with an offset such as debug+2", d.name)
		}
		return int64(l), nil
	}
	if d.isDuration {
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return 0, fmt.Errorf("vis: %s takes a duration with a unit, like %q%s", d.name, d.formatPlain(d.def), d.orSpecial())
		}
		dur, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("vis: %s: %w%s", d.name, err, d.orSpecial())
		}
		return int64(dur), nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("vis: %s takes a whole number%s: %w", d.name, d.orSpecial(), err)
	}
	return v, nil
}
