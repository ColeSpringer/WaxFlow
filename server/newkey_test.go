package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// cachePkg is the package whose NewKey hashes a key.
const cachePkg = "github.com/colespringer/waxflow/internal/cache"

// keyHashSites lists every use of the cache package's NewKey in dir's
// non-test files: a call, or the function taken as a value, under whatever
// name a file imports the package by. inside holds the uses within a method
// named newKey, outside the rest.
func keyHashSites(t *testing.T, dir string) (inside, outside []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no package sources found in %s (%v)", dir, err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		local := ""
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == cachePkg {
				local = "cache"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			within := fn != nil && fn.Recv != nil && fn.Name.Name == "newKey"
			ast.Inspect(decl, func(n ast.Node) bool {
				var used bool
				switch e := n.(type) {
				case *ast.SelectorExpr:
					x, ok := e.X.(*ast.Ident)
					used = ok && x.Name == local && e.Sel.Name == "NewKey"
				case *ast.Ident:
					// A dot import names the function bare.
					used = local == "." && e.Name == "NewKey"
				}
				if used {
					pos := fset.Position(n.Pos())
					site := fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line)
					if within {
						inside = append(inside, site)
					} else {
						outside = append(outside, site)
					}
				}
				return true
			})
		}
	}
	return inside, outside
}

// TestKeysAreBuiltInOnePlace: newKey is the one place a key is hashed, which
// is what lets a test watching keys see every one of them. A cache.NewKey
// anywhere else in the package builds a key that test never shows.
func TestKeysAreBuiltInOnePlace(t *testing.T) {
	inside, outside := keyHashSites(t, ".")
	if len(outside) != 0 {
		t.Errorf("cache.NewKey is used outside newKey:\n%s", strings.Join(outside, "\n"))
	}
	// Exactly one, so a scan that stopped finding the call fails here too.
	if len(inside) != 1 {
		t.Errorf("newKey uses cache.NewKey at %v, want one use", inside)
	}
}

// TestKeyHashSitesSeesEveryForm holds the scan itself: it finds a call under
// an import alias and the function taken as a value, and it reads code, not
// text.
func TestKeyHashSitesSeesEveryForm(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("alias.go", `package x

import c "`+cachePkg+`"

// A comment naming cache.NewKey( is not a use.
var text = "cache.NewKey("

func called() { _ = c.NewKey("identity", "canonical", nil) }

var value = c.NewKey
`)
	write("method.go", `package x

import "`+cachePkg+`"

type S struct{}

func (S) newKey() { _ = cache.NewKey("identity", "canonical", nil) }

func newKey() { _ = cache.NewKey("identity", "canonical", nil) }
`)
	write("dot.go", `package x

import . "`+cachePkg+`"

func dotted() { _ = NewKey("identity", "canonical", nil) }
`)
	write("other.go", `package x

type cache struct{}

func (cache) NewKey() {}

func unrelated() { cache{}.NewKey() }
`)
	write("skipped_test.go", `package x

import "`+cachePkg+`"

func inATest() { _ = cache.NewKey("identity", "canonical", nil) }
`)

	inside, outside := keyHashSites(t, dir)
	if want := []string{"method.go:7"}; !slices.Equal(inside, want) {
		t.Errorf("inside newKey: %v, want %v", inside, want)
	}
	// alias.go's call and its value, dot.go's call, and method.go's plain
	// function, which is named newKey and is no method.
	if want := []string{"alias.go:8", "alias.go:10", "dot.go:5", "method.go:9"}; !slices.Equal(outside, want) {
		t.Errorf("outside newKey: %v, want %v", outside, want)
	}
}
