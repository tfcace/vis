package main

import (
	"os"
	"testing"

	"github.com/tfcace/vis/internal/gen"
)

// vis_gen.go is what the directives say: run go generate after changing one.
func TestGeneratedCodeIsCurrent(t *testing.T) {
	want, err := gen.Generate(".")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(gen.File)
	if string(got) != string(want) {
		t.Fatal("vis_gen.go is out of date with the // vis: directives: run go generate")
	}
}
