package testutil

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// engineRunArgs is each run entry point's count of fixed arguments: a call
// that passes more has passed run options.
var engineRunArgs = map[string]int{
	"Transcode":      5,
	"TranscodeMedia": 4,
	"Remux":          5,
	"RemuxDemuxer":   5,
	"CutStream":      8,
}

// scannedFile is one parsed non-test Go file, its path from the root and its
// directory, which names its package.
type scannedFile struct {
	rel, dir string
	file     *ast.File
}

// runEntry is a function whose callers must pass run options: an engine entry
// point, or a wrapper that forwards its own.
type runEntry struct {
	fixed int
	recv  string // the receiver's type name, "" for a plain function
	dir   string // the declaring package's directory
}

// reaches reports whether code in dir can name e: an unexported name only
// from its own package.
func (e runEntry) reaches(name, dir string) bool {
	return ast.IsExported(name) || dir == e.dir
}

// runOptionParam is fn's trailing ...RunOption parameter, or nil.
func runOptionParam(fn *ast.FuncType) *ast.Ident {
	if fn.Params == nil || len(fn.Params.List) == 0 {
		return nil
	}
	last := fn.Params.List[len(fn.Params.List)-1]
	ell, ok := last.Type.(*ast.Ellipsis)
	if !ok || len(last.Names) != 1 {
		return nil
	}
	if typeName(ell.Elt) != "RunOption" {
		return nil
	}
	return last.Names[0]
}

// forwardable is the name of fn's run-options parameter when the body never
// declares or rebinds that name, or "": then the name means the parameter
// everywhere in the body, holding what the caller passed.
func forwardable(fn *ast.FuncDecl) string {
	p := runOptionParam(fn.Type)
	if p == nil || fn.Body == nil {
		return ""
	}
	if _, rebound := bindings(fn.Body)[p.Name]; rebound {
		return ""
	}
	return p.Name
}

// calleeName is the name a call's function is spelled with: a method or
// qualified function's selector, or a plain function's identifier.
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

// forwards reports whether call's last argument is the identifier param
// spread on.
func forwards(call *ast.CallExpr, param string) bool {
	if param == "" || !call.Ellipsis.IsValid() || len(call.Args) == 0 {
		return false
	}
	id, ok := call.Args[len(call.Args)-1].(*ast.Ident)
	return ok && id.Name == param
}

// typeName is the bare name of a type expression: T, *T, pkg.T, T[X].
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	}
	return ""
}

// valueType is the type name of a value expression where the syntax states
// it (a composite literal, or its address), or "".
func valueType(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.CompositeLit:
		return typeName(v.Type)
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return valueType(v.X)
		}
	}
	return ""
}

// bindings is every name n declares, assigns or addresses, with the type
// name its declarations state, or "" where they state none or disagree.
func bindings(n ast.Node) map[string]string {
	out := map[string]string{}
	bind := func(e ast.Expr, typ string, declares bool) {
		id, ok := e.(*ast.Ident)
		if !ok {
			return
		}
		if prev, seen := out[id.Name]; !seen {
			out[id.Name] = typ
		} else if declares && prev != typ {
			out[id.Name] = ""
		}
	}
	fields := func(lists ...*ast.FieldList) {
		for _, l := range lists {
			if l == nil {
				continue
			}
			for _, f := range l.List {
				for _, id := range f.Names {
					bind(id, typeName(f.Type), true)
				}
			}
		}
	}
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			fields(n.Recv, n.Type.TypeParams, n.Type.Params, n.Type.Results)
			for _, tp := range recvTypeParams(n.Recv) {
				bind(tp, "", true)
			}
		case *ast.TypeSpec:
			bind(n.Name, "", true)
			fields(n.TypeParams)
		case *ast.FuncLit:
			fields(n.Type.Params, n.Type.Results)
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				typ := ""
				if n.Tok == token.DEFINE && len(n.Lhs) == len(n.Rhs) {
					typ = valueType(n.Rhs[i])
				}
				bind(l, typ, n.Tok == token.DEFINE)
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				typ := typeName(n.Type)
				if n.Type == nil && i < len(n.Values) {
					typ = valueType(n.Values[i])
				}
				bind(id, typ, true)
			}
		case *ast.RangeStmt:
			bind(n.Key, "", n.Tok == token.DEFINE)
			bind(n.Value, "", n.Tok == token.DEFINE)
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				bind(n.X, "", false)
			}
		}
		return true
	})
	return out
}

// recvTypeParams is the type parameters a generic receiver declares: the T
// of (r *R[T]).
func recvTypeParams(recv *ast.FieldList) []ast.Expr {
	if recv == nil || len(recv.List) == 0 {
		return nil
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch t := t.(type) {
	case *ast.IndexExpr:
		return []ast.Expr{t.Index}
	case *ast.IndexListExpr:
		return t.Indices
	}
	return nil
}

// declKey names the declaration a call sits in: "(*T).name" for a method,
// "name" for a function, "var a, b" or "type T" for a package-level one.
func declKey(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name
		}
		star := ""
		if _, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
			star = "*"
		}
		return "(" + star + typeName(d.Recv.List[0].Type) + ")." + d.Name.Name
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				for _, n := range s.Names {
					names = append(names, n.Name)
				}
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			}
		}
		return d.Tok.String() + " " + strings.Join(names, ", ")
	}
	return "?"
}

// findWrappers adds to table every function that forwards its own run options
// to a function already in it, with its fixed-argument count, until no new
// one appears: a wrapper's callers then answer for the payloads it passes on.
func findWrappers(files []scannedFile, table map[string]runEntry) {
	for grew := true; grew; {
		grew = false
		for _, sf := range files {
			for _, decl := range sf.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				param := forwardable(fn)
				if param == "" {
					continue
				}
				if _, known := table[fn.Name.Name]; known {
					continue
				}
				found := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						name := calleeName(call)
						if e, in := table[name]; in && e.reaches(name, sf.dir) && forwards(call, param) {
							found = true
						}
					}
					return !found
				})
				if !found {
					continue
				}
				e := runEntry{dir: sf.dir}
				for _, f := range fn.Type.Params.List[:len(fn.Type.Params.List)-1] {
					e.fixed += max(1, len(f.Names))
				}
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					e.recv = typeName(fn.Recv.List[0].Type)
				}
				table[fn.Name.Name] = e
				grew = true
			}
		}
	}
}

// structFields is every struct field name declared in files, by the
// directory of the package declaring it.
func structFields(files []scannedFile) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, sf := range files {
		ast.Inspect(sf.file, func(n ast.Node) bool {
			if st, ok := n.(*ast.StructType); ok {
				for _, f := range st.Fields.List {
					for _, id := range f.Names {
						if out[id.Name] == nil {
							out[id.Name] = map[string]bool{}
						}
						out[id.Name][sf.dir] = true
					}
				}
			}
			return true
		})
	}
	return out
}

// TestEngineCallersPassRunOptions holds every product call of an engine run
// entry point to passing run options, or to an entry here saying why it has
// none. A forgotten variadic compiles and drops the run's tags and progress
// without a word, which no test of the engine can see.
//
// A function that forwards its own ...RunOption parameter is a wrapper, and
// its callers are held to the same rule. A spread counts only as that
// forward, so `nil...` passes nothing. The scan matches by name: a call with
// fewer arguments than the fixed ones is another type's method and is
// skipped, and one of the same name and arity on another type needs an entry.
// A function used as a value escapes all this, so it is refused.
func TestEngineCallersPassRunOptions(t *testing.T) {
	// Each exemption states how many calls in its declaration it covers.
	exempt := map[string]struct {
		calls int
		why   string
	}{
		"cli/doctor.go: benchChecks": {1,
			"the self-test tone has no source to carry tags from, and it times the run rather than watching it"},
		"server/timelinestream.go: (*Server).timelinePipeline": {1,
			"a timeline has no single source whose tags are the output's, and nothing reports a live stream's progress"},
	}
	root := PkgFile("..", "..")
	var files []scannedFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// oracletest is a module of oracle tests alone; testdata and the
			// dot directories hold no product code.
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "oracletest") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, scannedFile{rel, path.Dir(rel), file})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	table := map[string]runEntry{}
	for name, fixed := range engineRunArgs {
		table[name] = runEntry{fixed: fixed, recv: "Engine", dir: "."}
	}
	findWrappers(files, table)
	if _, ok := table["writeMedia"]; !ok {
		t.Error("the job runner's writeMedia was not found as a wrapper; the wrapper scan is broken")
	}
	fields := structFields(files)

	exemptCalls := map[string]int{}
	calls := 0
	for _, sf := range files {
		// Identifiers that are no value use: declaration and field names,
		// labels, a call's function, a selector's name and a literal's keys.
		named := map[*ast.Ident]bool{}
		called := map[*ast.SelectorExpr]bool{}
		for _, decl := range sf.file.Decls {
			key := sf.rel + ": " + declKey(decl)
			// rebound names run options the body declares, assigns or
			// addresses again, so a spread of that name may not forward them.
			param, rebound := "", ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				param = forwardable(fn)
				if p := runOptionParam(fn.Type); p != nil && param == "" {
					rebound = p.Name
				}
				named[fn.Name] = true
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					named[n.Sel] = true
				case *ast.Field:
					for _, id := range n.Names {
						named[id] = true
					}
				case *ast.LabeledStmt:
					named[n.Label] = true
				case *ast.BranchStmt:
					if n.Label != nil {
						named[n.Label] = true
					}
				case *ast.CompositeLit:
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok {
								named[id] = true
							}
						}
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch f := call.Fun.(type) {
				case *ast.SelectorExpr:
					called[f] = true
				case *ast.Ident:
					named[f] = true
				}
				name := calleeName(call)
				e, ok := table[name]
				if !ok || !e.reaches(name, sf.dir) || len(call.Args) < e.fixed {
					return true
				}
				calls++
				switch {
				case call.Ellipsis.IsValid():
					if forwards(call, param) {
						return true
					}
					if forwards(call, rebound) {
						t.Errorf("%s passes %s... to %s, but its body also declares, assigns or addresses %s: "+
							"rename the local, or leave the parameter as the caller passed it", key, rebound, name, rebound)
						return true
					}
				case len(call.Args) > e.fixed:
					return true
				}
				if _, ok := exempt[key]; ok {
					exemptCalls[key]++
					return true
				}
				t.Errorf("%s calls %s with no run options: pass the run's tags and progress, "+
					"or exempt it here with the reason it has none", key, name)
				return true
			})
		}
		for _, decl := range sf.file.Decls {
			key := sf.rel + ": " + declKey(decl)
			local := bindings(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					name := n.Sel.Name
					e, in := table[name]
					if !in || called[n] || !e.reaches(name, sf.dir) {
						return true
					}
					// A variable the declaration types as another type
					// selects that type's member; where it states no type,
					// a field of this name in reach makes it a possible
					// field read.
					xt := ""
					if x, ok := n.X.(*ast.Ident); ok {
						xt = local[x.Name]
					}
					switch {
					case xt != "" && xt != e.recv:
					case xt == "" && (fields[name][sf.dir] || ast.IsExported(name) && len(fields[name]) > 0):
					default:
						t.Errorf("%s uses %s as a value, which this scan cannot check: call it directly", key, name)
					}
				case *ast.Ident:
					// A bare name reaches a plain function only in its own
					// package, in a declaration that binds nothing of that
					// name.
					e, in := table[n.Name]
					if !in || named[n] || e.recv != "" || e.dir != sf.dir {
						return true
					}
					if _, shadowed := local[n.Name]; !shadowed {
						t.Errorf("%s uses %s as a value, which this scan cannot check: call it directly", key, n.Name)
					}
				}
				return true
			})
		}
	}
	// The server, the job runner and the CLI make at least ten between them.
	if calls < 10 {
		t.Fatalf("found %d engine run calls; the scan is not seeing the tree", calls)
	}
	for key, ex := range exempt {
		if got := exemptCalls[key]; got != ex.calls {
			t.Errorf("the exemption for %s covers %s without run options, and %d are there",
				key, plural(ex.calls, "call"), got)
		}
	}
}

// plural spells n of noun.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
