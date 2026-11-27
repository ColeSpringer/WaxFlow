package diag

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

// TestLogReportsFileOffsets: a finding is listed at its offset in the file,
// and one with no position stays at -1.
func TestLogReportsFileOffsets(t *testing.T) {
	l := New("x: ", &container.DemuxerOptions{Base: 100})
	if err := l.Warn(5, "damage %d", 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Warn(-1, "nowhere"); err != nil {
		t.Fatal(err)
	}
	l.Note(7, "note")
	want := []container.Warning{
		{Offset: 105, Msg: "damage 1", Kind: container.Damage},
		{Offset: -1, Msg: "nowhere", Kind: container.Damage},
		{Offset: 107, Msg: "note", Kind: container.Note},
	}
	if got := l.List(); !slices.Equal(got, want) {
		t.Errorf("listed %v, want %v", got, want)
	}
	if l.DamageCount() != 2 || l.At(5) != 105 || l.At(-1) != -1 {
		t.Errorf("count %d, At(5) %d, At(-1) %d", l.DamageCount(), l.At(5), l.At(-1))
	}
}

// TestFileOffsetHoldsAtTheLargest: an offset within Base of the largest
// int64 reports as the largest, never wrapped negative.
func TestFileOffsetHoldsAtTheLargest(t *testing.T) {
	const off = math.MaxInt64 - 1
	l := New("x: ", &container.DemuxerOptions{Base: 4096})
	if got := l.At(off); got != math.MaxInt64 {
		t.Errorf("At(%d) = %d, want %d", int64(off), got, int64(math.MaxInt64))
	}
	if got := l.At(-1); got != -1 {
		t.Errorf("At(-1) = %d, want -1", got)
	}
	if err := l.Warn(off, "damage"); err != nil {
		t.Fatal(err)
	}
	if got := l.List(); len(got) != 1 || got[0].Offset != math.MaxInt64 {
		t.Errorf("listed %v, want one at %d", got, int64(math.MaxInt64))
	}
	s := New("x: ", &container.DemuxerOptions{Strict: true, Base: 4096})
	want := fmt.Sprintf("x: damage (at offset %d)", int64(math.MaxInt64))
	if err := s.Warn(off, "damage"); err == nil || err.Error() != want {
		t.Errorf("Warn = %v, want %q", err, want)
	}
}

// TestLogRefusesUnderStrict: a strict Log records nothing and refuses with
// the file offset, or with no offset clause where the finding has none.
func TestLogRefusesUnderStrict(t *testing.T) {
	l := New("x: ", &container.DemuxerOptions{Strict: true, Base: 100})
	for _, c := range []struct {
		off  int64
		want string
	}{{5, "x: damage (at offset 105)"}, {-1, "x: damage"}} {
		err := l.Warn(c.off, "damage")
		if waxerr.CodeOf(err) != waxerr.CodeMalformedInput || err.Error() != c.want {
			t.Errorf("Warn(%d) = %v, want %q", c.off, err, c.want)
		}
	}
	if l.DamageCount() != 0 || len(l.List()) != 0 {
		t.Errorf("a strict Log recorded %d: %v", l.DamageCount(), l.List())
	}
}

// TestProbeRollsBackWithStrictOff: what a probe records is rolled back, its
// first damage handed back at the source's offset, and Strict is as it was.
func TestProbeRollsBackWithStrictOff(t *testing.T) {
	l := New("x: ", &container.DemuxerOptions{Strict: true, Base: 100})
	clean, first := l.Probe(func() {
		if l.Strict() {
			t.Error("Strict inside a probe")
		}
		l.Note(3, "note")
		if err := l.Warn(5, "damage"); err != nil {
			t.Errorf("Warn under a probe = %v, want it recorded", err)
		}
	})
	if clean || first == nil || *first != (container.Warning{Offset: 5, Msg: "damage", Kind: container.Damage}) {
		t.Errorf("Probe = %v, %v", clean, first)
	}
	if l.DamageCount() != 0 || len(l.List()) != 0 {
		t.Errorf("the probe left %d: %v", l.DamageCount(), l.List())
	}
	if !l.Strict() || l.Warn(5, "damage") == nil {
		t.Error("Strict did not come back after the probe")
	}
	if clean, first := l.Probe(func() { l.Note(3, "note") }); !clean || first != nil {
		t.Errorf("a probe that met no damage = %v, %v", clean, first)
	}
}

// TestRollbackDropsWhatFollowsTheMark: a Rollback drops the findings after
// its Mark, the count with them, and keeps those before.
func TestRollbackDropsWhatFollowsTheMark(t *testing.T) {
	var l Log
	if err := l.Warn(1, "kept"); err != nil {
		t.Fatal(err)
	}
	m := l.Mark()
	if err := l.Warn(2, "dropped"); err != nil {
		t.Fatal(err)
	}
	l.Rollback(m)
	if l.DamageCount() != 1 || !slices.Equal(l.List(), []container.Warning{{Offset: 1, Msg: "kept"}}) {
		t.Errorf("after the rollback: %d, %v", l.DamageCount(), l.List())
	}
}

// TestDemuxersExportNoDiagnosticsMutators: a demuxer holds its Log in an
// unexported field, so no exported or embedded field, at any depth, reaches
// it from outside. Each Demuxer exports what it exported before the Log,
// mp4's DamageCount, the MeasureSamples of wv and ogg, the Tags of adts and
// the TruncatedSamples of apen aside, and embeds nothing it did not.
func TestDemuxersExportNoDiagnosticsMutators(t *testing.T) {
	allowed := map[string][]string{
		"adts":  {"DamageCount", "ReadPacket", "SeekSample", "Tags", "Tracks", "Walk", "Walked", "Warnings"},
		"aiff":  {"DamageCount", "IndexSnapshot", "KeepsIndex", "ReadPacket", "RestoreIndex", "SeekSample", "Tracks", "TruncatedSamples", "Walk", "Walked", "Warnings"},
		"apen":  {"DamageCount", "ReadPacket", "SeekSample", "Tags", "Tracks", "TruncatedSamples", "Warnings"},
		"asf":   {"Chapters", "DamageCount", "IndexSnapshot", "KeepsIndex", "ReadPacket", "RestoreIndex", "SeekSample", "Tags", "Tracks", "Walk", "Walked", "Warnings"},
		"flacn": {"DamageCount", "ReadPacket", "SeekSample", "Tracks", "TruncatedSamples", "Warnings"},
		"mka":   {"DamageCount", "ReadPacket", "SeekSample", "Tracks", "Walk", "Walked", "Warnings"},
		"mp4":   {"Brands", "Chapters", "DamageCount", "ReadPacket", "SeekSample", "Tags", "Tracks", "TruncatedSamples", "Walk", "Walked", "Warnings"},
		"mpa":   {"DamageCount", "IndexSnapshot", "KeepsIndex", "ReadPacket", "RestoreIndex", "SeekSample", "Tracks", "TruncatedSamples", "Walk", "Walked", "Warnings"},
		"mpc":   {"Chapters", "DamageCount", "IndexSnapshot", "KeepsIndex", "ReadPacket", "RestoreIndex", "SeekSample", "Tags", "Tracks", "TruncatedSamples", "Warnings"},
		"ogg":   {"DamageCount", "MeasureSamples", "ReadPacket", "SeekSample", "Tracks", "TruncatedSamples", "Warnings"},
		"riff":  {"DamageCount", "IndexSnapshot", "KeepsIndex", "ReadPacket", "RestoreIndex", "SeekSample", "Tracks", "TruncatedSamples", "Walk", "Walked", "Warnings"},
		"wv":    {"DamageCount", "MeasureSamples", "ReadPacket", "SeekSample", "Tags", "Tracks", "TruncatedSamples", "Warnings"},
	}
	embeds := map[string][]string{"mka": {"readerState"}}
	for pkg, want := range allowed {
		files, err := filepath.Glob(filepath.Join("..", "..", pkg, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: no sources (%v)", pkg, err)
		}
		var methods, embedded []string
		types := map[string]typeDecl{}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			diagName := importName(f, reflect.TypeFor[Log]().PkgPath())
			for _, decl := range f.Decls {
				switch decl := decl.(type) {
				case *ast.FuncDecl:
					if decl.Recv != nil && decl.Name.IsExported() && isDemuxer(decl.Recv.List[0].Type) {
						methods = append(methods, decl.Name.Name)
					}
				case *ast.GenDecl:
					for _, spec := range decl.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok {
							types[ts.Name.Name] = typeDecl{ts.Type, diagName}
						}
					}
				}
			}
		}
		demuxer, found := types["Demuxer"].typ.(*ast.StructType)
		if !found {
			t.Errorf("%s: no Demuxer struct type", pkg)
			continue
		}
		for _, field := range demuxer.Fields.List {
			if len(field.Names) == 0 {
				embedded = append(embedded, typeName(field.Type))
			}
		}
		if path := logPath(types, demuxer, types["Demuxer"].diag, map[string]bool{"Demuxer": true}); path != "" {
			t.Errorf("%s: Demuxer exposes diag.Log as %s", pkg, path)
		}
		for _, m := range methods {
			if !slices.Contains(want, m) {
				t.Errorf("%s: Demuxer exports %s", pkg, m)
			}
		}
		for _, e := range embedded {
			if !slices.Contains(embeds[pkg], e) {
				t.Errorf("%s: Demuxer embeds %s", pkg, e)
			}
		}
	}
}

// isDemuxer reports whether a receiver is Demuxer or *Demuxer.
func isDemuxer(recv ast.Expr) bool {
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	id, ok := recv.(*ast.Ident)
	return ok && id.Name == "Demuxer"
}

// typeDecl is a type spec's type, an alias's included, and its file's name
// for this package.
type typeDecl struct {
	typ  ast.Expr
	diag string
}

// importName is the name f gives the package at path, or "" if f does not
// import it.
func importName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if imp.Path.Value == strconv.Quote(path) {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return filepath.Base(path)
		}
	}
	return ""
}

// logPath is the selector by which code outside the package reaches a Log
// in st, through exported and embedded fields at any depth, or "" if none.
// diag is the name st's file gives this package.
func logPath(types map[string]typeDecl, st *ast.StructType, diag string, seen map[string]bool) string {
	for _, field := range st.Fields.List {
		typ := field.Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		// An embedded field is named by its type, less any package.
		name, reachable := typeName(typ), len(field.Names) == 0
		name = name[strings.LastIndex(name, ".")+1:]
		for _, n := range field.Names {
			if n.IsExported() {
				name, reachable = n.Name, true
			}
		}
		if !reachable {
			continue
		}
		if below, ok := holdsLog(types, field.Type, diag, seen); ok {
			return name + below
		}
	}
	return ""
}

// holdsLog reports whether a value of type e reaches a Log, through
// pointers, array and slice elements, local types and aliases, and their
// exported or embedded fields; below is the selector past e.
func holdsLog(types map[string]typeDecl, e ast.Expr, diag string, seen map[string]bool) (below string, ok bool) {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return holdsLog(types, e.X, diag, seen)
	case *ast.StarExpr:
		return holdsLog(types, e.X, diag, seen)
	case *ast.ArrayType:
		return holdsLog(types, e.Elt, diag, seen)
	case *ast.SelectorExpr:
		x, isIdent := e.X.(*ast.Ident)
		return "", isIdent && diag != "" && x.Name == diag && e.Sel.Name == "Log"
	case *ast.Ident:
		td, local := types[e.Name]
		if !local || seen[e.Name] {
			return "", false
		}
		seen[e.Name] = true
		return holdsLog(types, td.typ, td.diag, seen)
	case *ast.StructType:
		if p := logPath(types, e, diag, seen); p != "" {
			return "." + p, true
		}
	}
	return "", false
}

// typeName names an embedded field's type as its source spells it.
func typeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return "*" + typeName(e.X)
	case *ast.SelectorExpr:
		return typeName(e.X) + "." + e.Sel.Name
	case *ast.Ident:
		return e.Name
	}
	return "?"
}

// TestLeadIsListedFirstAtItsOwnOffset: what the source's cutter found in
// front of it is a file offset already, so it is listed ahead of the log's
// own findings untranslated, and its damage counts.
func TestLeadIsListedFirstAtItsOwnOffset(t *testing.T) {
	lead := container.Warning{Offset: 60, Msg: "100 bytes in front of the stream belong to no tag", Kind: container.Damage}
	l := New("x: ", &container.DemuxerOptions{Base: 160, Lead: []container.Warning{lead}})
	if got := l.List(); !slices.Equal(got, []container.Warning{lead}) || l.DamageCount() != 1 {
		t.Errorf("list %v, damage %d; want the lead alone", got, l.DamageCount())
	}
	if err := l.Warn(5, "own"); err != nil {
		t.Fatal(err)
	}
	want := []container.Warning{lead, {Offset: 165, Msg: "own", Kind: container.Damage}}
	if got := l.List(); !slices.Equal(got, want) || l.DamageCount() != 2 {
		t.Errorf("list %v, damage %d; want %v", got, l.DamageCount(), want)
	}
}
