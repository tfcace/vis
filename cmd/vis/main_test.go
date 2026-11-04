package main

import (
	"path/filepath"
	"testing"
)

// A message from vis gen keeps its text when it gains the directory it's from.
func TestInDirKeepsTheMessage(t *testing.T) {
	msg := "routes.go:309: // vis:slo: GenerateHandler serves two routes"
	cases := map[string]string{
		".":            msg,
		"server":       filepath.Join("server", "routes.go") + ":309: // vis:slo: GenerateHandler serves two routes",
		"/src/server/": filepath.Join("/src/server", "routes.go") + ":309: // vis:slo: GenerateHandler serves two routes",
	}
	for dir, want := range cases {
		if got := inDir(dir, msg); got != want {
			t.Errorf("inDir(%q) = %q, want %q", dir, got, want)
		}
	}
}
