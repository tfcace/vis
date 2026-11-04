package vis_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/tfcace/vis"
)

// A LogLevel knob is the handler's level: changing the knob changes what the
// service logs, with no LevelVar or Bind in between.
func TestLogLevelIsTheHandlersLevel(t *testing.T) {
	k := vis.NewLogLevel("t_log_level")
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: k}))

	log.Debug("hidden")
	if k.Level() != slog.LevelInfo || buf.Len() != 0 {
		t.Fatalf("level = %v, logged %q; want INFO, nothing logged at debug", k.Level(), buf.String())
	}
	if _, err := vis.DefaultInstance().Set(vis.Change{Knob: "t_log_level", Value: int64(slog.LevelDebug), By: sam, Reason: "chasing a bug"}); err != nil {
		t.Fatal(err)
	}
	log.Debug("shown")
	if k.Now() != slog.LevelDebug || !strings.Contains(buf.String(), "shown") {
		t.Fatalf("level = %v, logged %q; want DEBUG, the debug line logged", k.Now(), buf.String())
	}
}

// With no Range or Default, a level knob runs DEBUG..ERROR from INFO, which
// is what a service's log level is almost always.
func TestLogLevelReadsAsLevels(t *testing.T) {
	vis.NewLogLevel("t_log_plain")
	i := info(t, "t_log_plain")
	if i.Type != "level" || i.Min != int64(slog.LevelDebug) || i.Max != int64(slog.LevelError) || i.Default != int64(slog.LevelInfo) {
		t.Fatalf("info = %+v, want a level DEBUG..ERROR from INFO", i)
	}
	if got := i.Format(int64(slog.LevelDebug - 4)); got != "TRACE" {
		t.Errorf("Format(debug-4) = %q, want TRACE", got)
	}
	if got := i.Format(int64(slog.LevelWarn)); got != "WARN" {
		t.Errorf("Format(warn) = %q, want WARN", got)
	}
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "warn": slog.LevelWarn, "debug-4": slog.LevelDebug - 4, "trace": slog.LevelDebug - 4} {
		if v, err := i.Parse(in); err != nil || v != int64(want) {
			t.Errorf("Parse(%q) = %d, %v; want %v", in, v, err, want)
		}
	}
	if _, err := i.Parse("loud"); err == nil || !strings.Contains(err.Error(), "debug, info, warn or error") {
		t.Errorf("Parse(loud) = %v, want an error naming the levels", err)
	}

	vis.NewLogLevel("t_log_trace", vis.Range(slog.LevelDebug-4, slog.LevelError), vis.Default(slog.LevelWarn))
	if i := info(t, "t_log_trace"); i.Min != -8 || i.Default != int64(slog.LevelWarn) {
		t.Fatalf("info = %+v, want the range and default given", i)
	}
}

// A level takes slog's levels, so a plain number, which reads as a level
// nobody meant, is a declaration mistake.
func TestLogLevelNeedsSlogLevels(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "slog.Level") {
			t.Fatalf("panic = %v, want one asking for a slog.Level", r)
		}
	}()
	vis.NewLogLevel("t_log_ints", vis.Range(0, 8))
}

func TestLogLevelFromTheEnvironment(t *testing.T) {
	t.Setenv("VIS_T_LOG", "debug")
	t.Setenv("VIS_T_LOG_BAD", "chatty")
	vis.NewLogLevel("t_log_env", vis.Env("VIS_T_LOG"))
	vis.NewLogLevel("t_log_env_bad", vis.Env("VIS_T_LOG_BAD"))
	if i := info(t, "t_log_env"); i.Default != int64(slog.LevelDebug) || !i.FromEnv {
		t.Fatalf("info = %+v, want default DEBUG from VIS_T_LOG", i)
	}
	if !strings.Contains(strings.Join(vis.NewInstance().Warnings(), "\n"), `VIS_T_LOG_BAD="chatty" isn't a log level such as trace, debug, info, warn or error, so t_log_env_bad keeps its default INFO`) {
		t.Fatalf("warnings = %q", vis.NewInstance().Warnings())
	}
}

// Agents see and set levels by name.
func TestLogLevelOverMCP(t *testing.T) {
	lvl := vis.NewLogLevel("t_log_mcp", vis.Pull(vis.Agent))
	in := vis.NewInstance()
	cs := connect(t, in, "oncall-secret", tokens)
	out, isErr := call(t, cs, "knobs", map[string]any{})
	if isErr {
		t.Fatal(out)
	}
	var k map[string]any
	for _, x := range out["knobs"].([]any) {
		if m := x.(map[string]any); m["name"] == "t_log_mcp" {
			k = m
		}
	}
	if k["type"] != "level" || k["value"] != "INFO" || k["min"] != "DEBUG" || k["max"] != "ERROR" {
		t.Fatalf("knob = %v, want levels by name", k)
	}
	if out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"t_log_mcp": "debug"}, "for": "10m", "reason": "chasing a bug"}); isErr {
		t.Fatal(out)
	}
	if got := lvl.Get(in.Snapshot()); got != slog.LevelDebug {
		t.Fatalf("after set_knob debug, level = %v", got)
	}
}
