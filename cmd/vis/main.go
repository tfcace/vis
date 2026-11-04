// Command vis is vis's build tool. It never wraps the go command.
//
//	go tool vis gen [dir]       write vis_gen.go from the package's // vis: directives
//	go tool vis vet [packages]  check for mistakes in using vis that compile but are wrong
//	go tool vis diff [-check] [ref] [packages]
//	                            what the packages declare (knobs, signals, SLOs, rules), against the
//	                            same packages at a git revision (HEAD by default), for review; with
//	                            -check it exits 1 when a change gives automation more room, for CI
//
// Run gen from go generate, with this line in any file of the package:
//
//	//go:generate go tool vis gen
package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/tfcace/vis/internal/gen"
	"github.com/tfcace/vis/internal/manifest"
	"github.com/tfcace/vis/internal/vet"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "vet" {
		os.Args = append([]string{"vis vet"}, os.Args[2:]...)
		multichecker.Main(vet.Analyzers...) // exits
	}
	if len(os.Args) >= 2 && os.Args[1] == "diff" {
		if err := diff(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "gen" {
		fmt.Fprintln(os.Stderr, "usage: go tool vis gen [dir]\n       go tool vis vet [packages]\n       go tool vis diff [ref] [packages]")
		os.Exit(2)
	}
	dir := "."
	if len(os.Args) > 2 {
		dir = os.Args[2]
	}
	if err := run(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// diff compares what the packages declare now with what they declared at a
// git revision, which it reads with git archive: the repository isn't touched.
func diff(args []string) error {
	check := len(args) > 0 && args[0] == "-check"
	if check {
		args = args[1:]
	}
	ref, patterns := "HEAD", args
	if len(args) > 0 && !strings.HasPrefix(args[0], ".") && !strings.HasPrefix(args[0], "/") && !strings.Contains(args[0], "...") {
		ref, patterns = args[0], args[1:]
	}
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("vis diff: not in a git repository: %v", err)
	}
	root := strings.TrimSpace(string(top))
	rel, err := filepath.Rel(root, cwd)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "vis-diff")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := archive(root, ref, tmp); err != nil {
		return err
	}
	prev, err := manifest.Extract(filepath.Join(tmp, rel), patterns...)
	if err != nil {
		return fmt.Errorf("at %s: %w", ref, err)
	}
	next, err := manifest.Extract(cwd, patterns...)
	if err != nil {
		return err
	}
	changes := manifest.Diff(prev, next)
	if len(changes) == 0 {
		fmt.Printf("No changes to what %s declares since %s.\n", strings.Join(patterns, " "), ref)
		return nil
	}
	fmt.Printf("Since %s (! marks a change that gives automation more room, or loosens an objective):\n", ref)
	wider := false
	for _, c := range changes {
		fmt.Println(c)
		wider = wider || c.Wider
	}
	if check && wider {
		return errors.New("vis diff -check: a change marked ! needs review")
	}
	return nil
}

// archive writes the tree at ref into dir.
func archive(root, ref, dir string) error {
	cmd := exec.Command("git", "-C", root, "archive", "--format=tar", ref)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	tr := tar.NewReader(out)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, filepath.Clean(dir)+string(os.PathSeparator)) {
			continue // a path that would leave dir
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(p), 0o755)
			f, err := os.Create(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("vis diff: git archive %s: %v %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func run(dir string) error {
	src, err := gen.Generate(dir)
	var errs gen.Errors
	if errors.As(err, &errs) {
		for i, e := range errs {
			errs[i] = inDir(dir, e)
		}
		return errs
	}
	if err != nil {
		return err
	}
	out := filepath.Join(dir, gen.File)
	if src == nil {
		if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(out, src, 0o644)
}

// inDir puts dir in front of the file a vis gen message, "file.go:12: ...",
// is about. Only the file is joined: filepath.Join would clean the message's
// text too, turning "// vis:slo" into "/ vis:slo".
func inDir(dir, msg string) string {
	file, rest, ok := strings.Cut(msg, ":")
	if !ok {
		return msg
	}
	return filepath.Join(dir, file) + ":" + rest
}
