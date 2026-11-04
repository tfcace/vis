package spec

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// LevelTrace is the level vis calls TRACE. slog has none; DEBUG-4 is the
// usual one (Ollama's, among others), so a range down to it reads as
// TRACE..ERROR instead of an offset nobody chose.
const LevelTrace = slog.LevelDebug - 4

// LevelName is how people and agents see a log level: TRACE, DEBUG, INFO,
// WARN or ERROR, with an offset for one in between, as slog writes it.
func LevelName(l slog.Level) string {
	if l > LevelTrace {
		return l.String()
	}
	if l == LevelTrace {
		return "TRACE"
	}
	return fmt.Sprintf("TRACE%d", l-LevelTrace)
}

// ParseLevel reads a level by name, in any case: trace, debug, info, warn or
// error, or one with an offset such as debug+2 or trace-1.
func ParseLevel(s string) (slog.Level, error) {
	if rest, ok := strings.CutPrefix(strings.ToUpper(strings.TrimSpace(s)), "TRACE"); ok {
		if rest == "" {
			return LevelTrace, nil
		}
		n, err := strconv.Atoi(rest)
		if err != nil || (rest[0] != '+' && rest[0] != '-') {
			return 0, fmt.Errorf("%q isn't a log level such as trace, debug, info, warn or error", s)
		}
		return LevelTrace + slog.Level(n), nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("%q isn't a log level such as trace, debug, info, warn or error", s)
	}
	return l, nil
}
