package spec

import (
	"log/slog"
	"testing"
)

// slog has no TRACE; DEBUG-4 is the usual one (Ollama's, among others), so
// vis names it, instead of showing an offset nobody chose.
func TestLevelsReadByName(t *testing.T) {
	for l, want := range map[slog.Level]string{
		slog.LevelDebug - 4: "TRACE",
		slog.LevelDebug - 5: "TRACE-1",
		slog.LevelDebug - 2: "DEBUG-2",
		slog.LevelDebug:     "DEBUG",
		slog.LevelInfo:      "INFO",
		slog.LevelError + 4: "ERROR+4",
	} {
		if got := LevelName(l); got != want {
			t.Errorf("LevelName(%d) = %q, want %q", l, got, want)
		}
		if back, err := ParseLevel(want); err != nil || back != l {
			t.Errorf("ParseLevel(%q) = %d, %v; want %d", want, back, err, l)
		}
	}
	for in, want := range map[string]slog.Level{"trace": -8, "Trace+2": -6, "debug-4": -8, "warn": 4} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"loud", "trace2", "tracer", ""} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("ParseLevel(%q) = nil error, want one", bad)
		}
	}
}
