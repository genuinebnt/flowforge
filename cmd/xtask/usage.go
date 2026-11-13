package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// usageCommand implements "xtask usage": for one package, list every
// exported identifier (package-level names, methods and struct fields) and
// where the rest of the module uses it. It answers "what does the code that
// I have not ported yet need from this package?" before porting it.
//
//	go run ./cmd/xtask usage core                  # by identifier
//	go run ./cmd/xtask usage -by consumer core     # by using package
//	go run ./cmd/xtask usage -tests -md core > docs/porting/usage/core.md
//
// The argument is a modules.toml module name or a package directory
// (core/netnorm). Uses come from go/types, so method calls through
// interfaces, promoted fields and aliased imports are all counted; a grep
// would miss them. Uses inside the package itself are not reported.
func usageCommand(man *Manifest, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	by := fs.String("by", "identifier", "group by identifier or consumer")
	tests := fs.Bool("tests", false, "also count uses in _test.go files")
	md := fs.Bool("md", false, "print Markdown instead of plain text")
	examples := fs.Int("examples", 3, "example locations per identifier and consumer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || (*by != "identifier" && *by != "consumer") {
		return usage()
	}
	dir := fs.Arg(0)
	if m, ok := man.Modules[dir]; ok {
		dir = m.Dir
	}
	dir = strings.TrimSuffix(strings.TrimPrefix(dir, "./"), "/")
	rep, err := collectUsage(man, dir, *tests)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	if *by == "consumer" {
		rep.printByConsumer(w, *md, *examples)
	} else {
		rep.printByIdentifier(w, *md, *examples)
	}
	return nil
}

// listedPackage is the subset of "go list -json" output this command needs.
type listedPackage struct {
	ImportPath   string
	Dir          string
	Export       string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Module       *struct{ Path string }
}

// usageSite is one use of an identifier.
type usageSite struct {
	Pkg  string // using package, relative to the module root
	Pos  string // file:line, relative to the module root
	file string
	line int
	Test bool
}

// usageReport holds the uses of one target package's exported identifiers.
type usageReport struct {
	target string            // package directory, e.g. "core"
	idents []string          // every exported identifier, sorted
	kinds  map[string]string // identifier -> func, type, method, field, var, const
	sites  map[string][]usageSite
	// viaIface maps "Type.Method" to "Iface.Method" when Type implements
	// an exported interface of the same package.
	viaIface map[string]string
	layerOf  func(pkgDir string) string
}

func collectUsage(man *Manifest, target string, withTests bool) (*usageReport, error) {
	// PORT: not ported; this is developer tooling over the Go tree.
	listArgs := []string{"list", "-export", "-deps", "-json"}
	if withTests {
		// -test adds the test-only dependencies (testkit, go-cmp, ...), so
		// test files type-check against real export data.
		listArgs = append(listArgs, "-test")
	}
	out, err := exec.Command("go", append(listArgs, "./...")...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("go list: %s", ee.Stderr)
		}
		return nil, err
	}
	var pkgs []listedPackage
	exports := map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	modPath := ""
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		// Test variants ("p [p.test]") and generated test mains
		// ("p.test") duplicate a package's files; only plain packages are
		// scanned, but every variant's export data stays reachable.
		if strings.ContainsAny(p.ImportPath, " ") || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		exports[p.ImportPath] = p.Export
		pkgs = append(pkgs, p)
	}
	root, _ := os.Getwd()
	targetPath := ""
	for _, p := range pkgs {
		if rel, err := filepath.Rel(root, p.Dir); err == nil && filepath.ToSlash(rel) == target {
			targetPath = p.ImportPath
			if p.Module != nil {
				modPath = p.Module.Path
			}
		}
	}
	if targetPath == "" {
		return nil, fmt.Errorf("no package in directory %q", target)
	}

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	tpkg, err := imp.Import(targetPath)
	if err != nil {
		return nil, err
	}
	rep := &usageReport{
		target:   target,
		kinds:    map[string]string{},
		sites:    map[string][]usageSite{},
		viaIface: map[string]string{},
		layerOf: func(pkgDir string) string {
			best := ""
			layer := "?"
			for _, m := range man.Modules {
				if (pkgDir == m.Dir || strings.HasPrefix(pkgDir, m.Dir+"/")) && len(m.Dir) > len(best) {
					best, layer = m.Dir, fmt.Sprint(m.Layer)
				}
			}
			if strings.HasPrefix(pkgDir, "services/") && best == "" {
				layer = "6"
			}
			if strings.HasPrefix(pkgDir, "cmd/") && best == "" {
				layer = "7"
			}
			return layer
		},
	}
	// Index the target's exported surface. Fields and methods are keyed
	// "Type.Name" so they read the way a port's impl blocks are organised.
	fieldOwner := map[*types.Var]string{}
	scope := tpkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		rep.add(name, objKind(obj))
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		if named, ok := tn.Type().(*types.Named); ok {
			for i := range named.NumMethods() {
				m := named.Method(i)
				if m.Exported() {
					rep.add(name+"."+m.Name(), "method")
				}
			}
			if iface, ok := named.Underlying().(*types.Interface); ok {
				for i := range iface.NumExplicitMethods() {
					m := iface.ExplicitMethod(i)
					if m.Exported() {
						rep.add(name+"."+m.Name(), "method")
					}
				}
			}
		}
		if st, ok := tn.Type().Underlying().(*types.Struct); ok {
			for i := range st.NumFields() {
				f := st.Field(i)
				if f.Exported() {
					fieldOwner[f] = name
					rep.add(name+"."+f.Name(), "field")
				}
			}
		}
	}
	for _, tname := range scope.Names() {
		tn, ok := scope.Lookup(tname).(*types.TypeName)
		if !ok || !tn.Exported() || types.IsInterface(tn.Type()) {
			continue
		}
		for _, iname := range scope.Names() {
			in, ok := scope.Lookup(iname).(*types.TypeName)
			if !ok || !in.Exported() || !types.IsInterface(in.Type()) {
				continue
			}
			iface := in.Type().Underlying().(*types.Interface)
			if !types.Implements(tn.Type(), iface) && !types.Implements(types.NewPointer(tn.Type()), iface) {
				continue
			}
			for i := range iface.NumMethods() {
				m := iface.Method(i).Name()
				rep.viaIface[tname+"."+m] = iname + "." + m
			}
		}
	}
	keyOf := func(obj types.Object) string {
		if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != targetPath || !obj.Exported() {
			return ""
		}
		switch o := obj.(type) {
		case *types.Func:
			sig, _ := o.Type().(*types.Signature)
			if sig != nil && sig.Recv() != nil {
				rt := sig.Recv().Type()
				if p, ok := rt.(*types.Pointer); ok {
					rt = p.Elem()
				}
				if n, ok := rt.(*types.Named); ok {
					return n.Obj().Name() + "." + o.Name()
				}
				return ""
			}
		case *types.Var:
			if o.IsField() {
				if owner, ok := fieldOwner[o.Origin()]; ok {
					return owner + "." + o.Name()
				}
				return ""
			}
		}
		if obj.Parent() == obj.Pkg().Scope() {
			return obj.Name()
		}
		return ""
	}

	for _, p := range pkgs {
		if p.ImportPath == targetPath || p.Module == nil || p.Module.Path != modPath {
			continue
		}
		rel, _ := filepath.Rel(root, p.Dir)
		rel = filepath.ToSlash(rel)
		groups := [][]string{p.GoFiles}
		testGroup := []bool{false}
		if withTests {
			groups = append(groups, append(append([]string{}, p.GoFiles...), p.TestGoFiles...), p.XTestGoFiles)
			testGroup = append(testGroup, true, true)
		}
		for gi, files := range groups {
			if len(files) == 0 {
				continue
			}
			var parsed []*ast.File
			for _, f := range files {
				af, err := parser.ParseFile(fset, filepath.Join(p.Dir, f), nil, parser.SkipObjectResolution)
				if err != nil {
					return nil, err
				}
				parsed = append(parsed, af)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
			// Errors are collected and ignored: an external test package
			// can reference export_test.go helpers that the export data
			// does not carry, and that must not hide every other use.
			conf := types.Config{Importer: imp, Error: func(error) {}}
			path := p.ImportPath
			if gi == 2 {
				path += "_test"
			}
			_, _ = conf.Check(path, fset, parsed, info)
			for id, obj := range info.Uses {
				k := keyOf(obj)
				if k == "" {
					continue
				}
				pos := fset.Position(id.Pos())
				isTest := strings.HasSuffix(pos.Filename, "_test.go")
				if gi == 1 && !isTest {
					continue // non-test files were already counted in group 0
				}
				fr, _ := filepath.Rel(root, pos.Filename)
				fr = filepath.ToSlash(fr)
				rep.sites[k] = append(rep.sites[k], usageSite{Pkg: rel, Pos: fmt.Sprintf("%s:%d", fr, pos.Line), file: fr, line: pos.Line, Test: testGroup[gi] && isTest})
			}
		}
	}
	for k := range rep.sites {
		s := rep.sites[k]
		sort.Slice(s, func(i, j int) bool {
			if s[i].Pkg != s[j].Pkg {
				return s[i].Pkg < s[j].Pkg
			}
			// Non-test uses first: they show what production code needs.
			if s[i].Test != s[j].Test {
				return !s[i].Test
			}
			if s[i].file != s[j].file {
				return s[i].file < s[j].file
			}
			return s[i].line < s[j].line
		})
	}
	sort.Strings(rep.idents)
	return rep, nil
}

// implicitMethods are method names that the standard library calls through
// an interface the caller never names (error, fmt.Stringer,
// encoding.TextMarshaler, json.Marshaler, sql.Scanner, driver.Valuer,
// errors.Is/As/Unwrap). go/types records no use for them, so the report
// labels them instead of listing them as unused: a port still needs the
// equivalent trait impl (Display, Error, Serialize, FromSql, ...).
var implicitMethods = map[string]string{
	"Error": "error", "String": "fmt.Stringer", "Format": "fmt.Formatter",
	"MarshalText": "encoding.TextMarshaler", "UnmarshalText": "encoding.TextUnmarshaler",
	"MarshalJSON": "json.Marshaler", "UnmarshalJSON": "json.Unmarshaler",
	"MarshalBinary": "encoding.BinaryMarshaler", "UnmarshalBinary": "encoding.BinaryUnmarshaler",
	"Scan": "sql.Scanner", "Value": "driver.Valuer",
	"Is": "errors.Is", "As": "errors.As", "Unwrap": "errors.Unwrap",
	"GoString": "fmt.GoStringer",
}

// implicitVia reports which interface calls id, or "": first an interface
// of the target package that id's type implements (core.SystemClock.Now is
// reached through core.Clock.Now), then a standard-library interface.
func (r *usageReport) implicitVia(id string) string {
	if r.kinds[id] != "method" {
		return ""
	}
	if via, ok := r.viaIface[id]; ok {
		return via
	}
	_, m, _ := strings.Cut(id, ".")
	return implicitMethods[m]
}

func (r *usageReport) add(name, kind string) {
	if _, ok := r.kinds[name]; ok {
		return
	}
	r.kinds[name] = kind
	r.idents = append(r.idents, name)
}

func objKind(obj types.Object) string {
	switch obj.(type) {
	case *types.Func:
		return "func"
	case *types.TypeName:
		return "type"
	case *types.Const:
		return "const"
	case *types.Var:
		return "var"
	}
	return "other"
}

// consumers returns the distinct using packages of sites, in order.
func consumers(sites []usageSite) []string {
	var out []string
	for _, s := range sites {
		if len(out) == 0 || out[len(out)-1] != s.Pkg {
			out = append(out, s.Pkg)
		}
	}
	return out
}

func (r *usageReport) printByIdentifier(w io.Writer, md bool, examples int) {
	order := append([]string(nil), r.idents...)
	sort.SliceStable(order, func(i, j int) bool {
		return len(consumers(r.sites[order[i]])) > len(consumers(r.sites[order[j]]))
	})
	var unused []string
	if md {
		fmt.Fprintf(w, "# Uses of `%s`\n\nGenerated by `go run ./cmd/xtask usage -md %s`. Identifiers are ordered by how many packages use them; port the top of the list first. L = layer of the using package.\n\n", r.target, r.target)
		fmt.Fprintln(w, "| Identifier | Kind | Packages | Uses | Used by (layer) | Examples |")
		fmt.Fprintln(w, "|---|---|---:|---:|---|---|")
	}
	for _, id := range order {
		s := r.sites[id]
		if len(s) == 0 {
			unused = append(unused, id)
			continue
		}
		cs := consumers(s)
		var labelled []string
		for _, c := range cs {
			labelled = append(labelled, fmt.Sprintf("%s (L%s)", c, r.layerOf(c)))
		}
		var ex []string
		for i := 0; i < len(s) && i < examples; i++ {
			ex = append(ex, s[i].Pos)
		}
		if md {
			fmt.Fprintf(w, "| `%s` | %s | %d | %d | %s | %s |\n", id, r.kinds[id], len(cs), len(s), strings.Join(labelled, ", "), "`"+strings.Join(ex, "`, `")+"`")
		} else {
			fmt.Fprintf(w, "%-40s %-6s %3d pkgs %5d uses  %s\n", id, r.kinds[id], len(cs), len(s), strings.Join(labelled, ", "))
			for _, e := range ex {
				fmt.Fprintf(w, "    %s\n", e)
			}
		}
	}
	var implicit, none []string
	for _, id := range unused {
		if via := r.implicitVia(id); via != "" {
			implicit = append(implicit, id+" ("+via+")")
		} else {
			none = append(none, id)
		}
	}
	if md {
		if len(implicit) > 0 {
			fmt.Fprintf(w, "\n## Called implicitly\n\nNo direct call site, but the standard library calls these through an interface, so the port needs the matching trait impl.\n\n")
			for _, id := range implicit {
				fmt.Fprintf(w, "* `%s`\n", id)
			}
		}
		if len(none) > 0 {
			fmt.Fprintf(w, "\n## Not used outside `%s`\n\nNeeded only if the package's own tests or conformance data use them.\n\n", r.target)
			for _, id := range none {
				fmt.Fprintf(w, "* `%s` (%s)\n", id, r.kinds[id])
			}
		}
	} else {
		if len(implicit) > 0 {
			fmt.Fprintf(w, "\ncalled implicitly: %s\n", strings.Join(implicit, ", "))
		}
		if len(none) > 0 {
			fmt.Fprintf(w, "\nnot used outside %s: %s\n", r.target, strings.Join(none, ", "))
		}
	}
}

func (r *usageReport) printByConsumer(w io.Writer, md bool, examples int) {
	byPkg := map[string]map[string][]usageSite{}
	for id, sites := range r.sites {
		for _, s := range sites {
			if byPkg[s.Pkg] == nil {
				byPkg[s.Pkg] = map[string][]usageSite{}
			}
			byPkg[s.Pkg][id] = append(byPkg[s.Pkg][id], s)
		}
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	// Lowest layer first: that is the porting order.
	sort.Slice(pkgs, func(i, j int) bool {
		li, lj := r.layerOf(pkgs[i]), r.layerOf(pkgs[j])
		if li != lj {
			return li < lj
		}
		return pkgs[i] < pkgs[j]
	})
	if md {
		fmt.Fprintf(w, "# Uses of `%s`, by using package\n\nGenerated by `go run ./cmd/xtask usage -by consumer -md %s`. Packages are in layer order (the porting order); each lists exactly what it needs from `%s`.\n", r.target, r.target, r.target)
	}
	for _, p := range pkgs {
		ids := make([]string, 0, len(byPkg[p]))
		for id := range byPkg[p] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if md {
			fmt.Fprintf(w, "\n## `%s` (layer %s)\n\n", p, r.layerOf(p))
		} else {
			fmt.Fprintf(w, "\n%s (layer %s): %d identifiers\n", p, r.layerOf(p), len(ids))
		}
		for _, id := range ids {
			s := byPkg[p][id]
			var ex []string
			for i := 0; i < len(s) && i < examples; i++ {
				ex = append(ex, s[i].Pos)
			}
			if md {
				fmt.Fprintf(w, "* `%s` (%s, %d) — `%s`\n", id, r.kinds[id], len(s), strings.Join(ex, "`, `"))
			} else {
				fmt.Fprintf(w, "    %-40s %3d  %s\n", id, len(s), strings.Join(ex, " "))
			}
		}
	}
}
