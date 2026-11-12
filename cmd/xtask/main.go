// Command xtask runs the per-module developer workflow of spec 25.10:
//
//	go run ./cmd/xtask list
//	go run ./cmd/xtask verify-module <name>
//	go run ./cmd/xtask test-module <name>
//	go run ./cmd/xtask bench-module [-benchtime 1s] <name>
//	go run ./cmd/xtask bench-compare <baseline.json> <new.json>
//	go run ./cmd/xtask fuzz-smoke [-fuzztime 60s] [name...]
//
// Module definitions come from modules.toml at the repository root.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "xtask:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: xtask list | verify-module <name> | test-module <name> | bench-module [-benchtime d] <name> | bench-compare <a.json> <b.json> | fuzz-smoke [-fuzztime d] [name...]")
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	cmd, rest := args[0], args[1:]
	if cmd == "bench-compare" {
		if len(rest) != 2 {
			return usage()
		}
		return benchCompare(rest[0], rest[1])
	}
	f, err := os.Open("modules.toml")
	if err != nil {
		return err
	}
	defer f.Close()
	man, err := parseManifest(f)
	if err != nil {
		return err
	}
	switch cmd {
	case "list":
		for _, n := range man.Names() {
			m := man.Modules[n]
			fmt.Printf("%-14s layer %d  ./%s  spec %v\n", n, m.Layer, m.Dir, m.Spec)
		}
		return nil
	case "verify-module":
		mod, err := pick(man, rest)
		if err != nil {
			return err
		}
		return verifyModule(mod)
	case "test-module":
		mod, err := pick(man, rest)
		if err != nil {
			return err
		}
		return goCmd("test", "-race", "-count=1", "./"+mod.Dir+"/...")
	case "bench-module":
		fs := flag.NewFlagSet("bench-module", flag.ContinueOnError)
		benchtime := fs.String("benchtime", "1s", "go test -benchtime value")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		mod, err := pick(man, fs.Args())
		if err != nil {
			return err
		}
		return benchModule(mod, *benchtime)
	case "fuzz-smoke":
		fs := flag.NewFlagSet("fuzz-smoke", flag.ContinueOnError)
		fuzztime := fs.String("fuzztime", "60s", "go test -fuzztime value per target")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		names := fs.Args()
		if len(names) == 0 {
			names = man.Names()
		}
		return fuzzSmoke(man, names, *fuzztime)
	}
	return usage()
}

func pick(man *Manifest, args []string) (*Module, error) {
	if len(args) != 1 {
		return nil, usage()
	}
	mod, ok := man.Modules[args[0]]
	if !ok {
		return nil, fmt.Errorf("unknown module %q (see xtask list)", args[0])
	}
	return mod, nil
}

func goCmd(args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}
