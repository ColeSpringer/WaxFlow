package oracletest

// The APEv2 key agreement cell. Two readers speak for a .wv, .ape or .mpc
// source's tags: container/internal/apev2 behind the demuxer, and the tag
// library behind cli/label, whose keys win per key on the mapper-wired route
// while the demuxer's fill in the rest. A spelling one side folds and the
// other does not surfaces as two tags carrying one value, so the native key
// of every item name has to be the library's. apev2's alias table is a hand
// mirror of the library's resolution; this sweep is what fails when the
// library learns a spelling the mirror lacks.

import (
	"encoding/binary"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/wv"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxlabel/tag"
)

// apeConventionNames are the names the library's APE convention table folds
// (internal/mapping/ape.go apeKeys), which no exported API lists. The sweep
// also reads the table itself out of the library's source (libraryTable), so
// a row added there is swept without an edit here.
var apeConventionNames = []string{
	"title", "artist", "album", "album artist", "composer", "lyricist", "producer",
	"engineer", "mixer", "arranger", "writer", "djmixer", "genre", "track", "disc",
	"year", "comment", "lyrics", "isrc", "catalog", "label", "musicbrainz_albumstatus",
	"musicbrainz_albumtype", "lead_performer", "date_recorded", "date_released",
	"date_release", "date_original", "original_date", "encoded_by", "part_number",
	"total_parts", "total_discs", "catalog_number", "publisher", "remixed_by",
	"content_group",
}

// apeOddNames are spellings at the edges of the rules: white space at an
// edge or inside, a name of one byte or past 255, picture and reserved names,
// and bytes the canonical vocabulary refuses.
var apeOddNames = []string{
	" Title", "Title ", "ALBUM ", "Album Artist ", "\tYear", "Album ", "Album  Artist",
	"Record Date", "Total Tracks", "Disc Number", "Unsynced Lyrics", "uslt", "Uslt",
	"X", strings.Repeat("K", 256), strings.Repeat("k", 300),
	"Cover Art (Front)", "Artist Picture", "TAG", "id3", "OggS", "MP+",
	"tilde~", "with=equals", "café", "",
}

// apeNonASCIIFolds are names the library folds through Unicode case mapping
// (a Kelvin sign lowercases to k, a long s and a dotless i uppercase to S and
// I) where its own key parser refuses any byte past ASCII. The native reader
// refuses them all. Each entry is the key the library reads; an entry it no
// longer reads that way is stale and fails.
var apeNonASCIIFolds = map[string]string{
	"TracK": "TRACKNUMBER",
	"Diſc":  "DISCNUMBER",
	"Dısc":  "DISCNUMBER",
}

// apeTag renders a whole APEv2 tag (header, items, footer) holding the items.
func apeTag(items ...[2]string) []byte {
	var body []byte
	for _, it := range items {
		body = binary.LittleEndian.AppendUint32(body, uint32(len(it[1])))
		body = binary.LittleEndian.AppendUint32(body, 0)
		body = append(body, it[0]...)
		body = append(body, 0)
		body = append(body, it[1]...)
	}
	pre := func(flags uint32) []byte {
		b := []byte("APETAGEX")
		b = binary.LittleEndian.AppendUint32(b, 2000)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(body)+32))
		b = binary.LittleEndian.AppendUint32(b, uint32(len(items)))
		b = binary.LittleEndian.AppendUint32(b, flags)
		return append(b, make([]byte, 8)...)
	}
	out := pre(1<<31 | 1<<29)
	out = append(out, body...)
	return append(out, pre(1<<31)...)
}

// apeReaders reads a .wv through both readers.
func apeReaders(t *testing.T, raw []byte) (native, mapper map[string][]string) {
	t.Helper()
	d, err := wv.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := label.New().Read(t.Context(), container.BytesSource(raw), "wv", meta.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Tags(), info.Tags
}

// withoutEmpty is tags less its empty values and the keys left with none.
func withoutEmpty(tags map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, vs := range tags {
		for _, v := range vs {
			if v != "" {
				out[k] = append(out[k], v)
			}
		}
	}
	return out
}

// libraryTable returns the string keys of the map literal var named name in
// the tag library's source file rel (relative to its module), as the
// oracletest module resolves the library. It fails, never skips, when the
// table is not there: a moved table is an extractor to update.
func libraryTable(t *testing.T, rel, name string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/colespringer/waxlabel").Output()
	if err != nil {
		t.Fatalf("locating the waxlabel module: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), rel)
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("the library table %s moved (%v); update the extractor", name, err)
	}
	var keys []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s in %s is not a map literal; update the extractor", name, path)
				}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						k, err := strconv.Unquote(bl.Value)
						if err != nil {
							t.Fatal(err)
						}
						keys = append(keys, k)
					}
				}
			}
		}
	}
	if len(keys) == 0 {
		t.Fatalf("no rows of the library table %s in %s: it moved or changed shape; update the extractor", name, path)
	}
	return keys
}

func bareWavPack(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "container", "wv", "testdata", "seek-mono.wv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[len(raw)-32:len(raw)-24]) == "APETAGEX" {
		t.Fatal("seek-mono.wv carries a tag; the cell needs a bare file")
	}
	return raw
}

func TestAPEv2KeysAgreeAcrossReaders(t *testing.T) {
	base := bareWavPack(t)
	names := map[string]bool{}
	for _, k := range tag.KnownKeys() {
		names[string(k)] = true
		names[strings.ToLower(string(k))] = true
		for _, a := range tag.KeyAliases(k) {
			names[a] = true
			names[strings.ToLower(a)] = true
		}
	}
	for _, n := range append(libraryTable(t, "internal/mapping/ape.go", "apeKeys"), apeConventionNames...) {
		names[n] = true
		names[strings.ToUpper(n)] = true
	}
	for _, n := range apeOddNames {
		names[n] = true
	}
	for _, n := range slices.Sorted(maps.Keys(names)) {
		native, mapper := apeReaders(t, append(slices.Clip(base), apeTag([2]string{n, "v"})...))
		if !maps.EqualFunc(native, mapper, slices.Equal) {
			t.Errorf("item %q: native %v, library %v", n, native, mapper)
		}
	}
	for n, want := range apeNonASCIIFolds {
		if names[n] {
			t.Fatalf("%q is both swept and recorded", n)
		}
		native, mapper := apeReaders(t, append(slices.Clip(base), apeTag([2]string{n, "v"})...))
		if len(native) != 0 || !maps.EqualFunc(mapper, map[string][]string{want: {"v"}}, slices.Equal) {
			t.Errorf("item %q: native %v, library %v; the recorded divergence is stale", n, native, mapper)
		}
	}
}

// TestAPEv2ValuesAgreeAcrossReaders: a track or disc number holding its total
// reads as the same two keys through both readers, whichever way it is written
// and whatever total key sits beside it, and a value that is not UTF-8 reads
// as the same Latin-1 text. The library keeps an empty value the demuxer
// drops, so the library's side is compared without its empty values; what it
// counts them toward, whether a number splits, has to agree.
func TestAPEv2ValuesAgreeAcrossReaders(t *testing.T) {
	base := bareWavPack(t)
	for i, items := range [][][2]string{
		{{"Track", "3/12"}, {"Disc", "1 of 2"}},
		{{"Track", "3/12"}, {"TRACKTOTAL", "10"}},
		{{"TOTALTRACKS", "10"}, {"Track", " 03 / 12 "}},
		{{"Track", "/12"}, {"Disc", "2/"}},
		{{"Track", "3/12\x004/12"}},
		{{"Track", "Part 3 of 12"}},
		{{"Artist", "caf\xe9"}, {"Title", "\xe9t\xe9\x00Na\xefve"}},
		{{"Track", "\x003/12"}},
		{{"Track", "3/12\x00\x00"}},
		{{"TRACKNUMBER", ""}, {"Track", "3/12"}},
		{{"Track", "3/12"}, {"TRACKTOTAL", ""}},
	} {
		native, mapper := apeReaders(t, append(slices.Clip(base), apeTag(items...)...))
		if !maps.EqualFunc(native, withoutEmpty(mapper), slices.Equal) {
			t.Errorf("case %d %s: native %v, library %v", i, fmt.Sprint(items), native, mapper)
		}
	}
	// A name the library folds onto DISCNUMBER through Unicode case mapping
	// (apeNonASCIIFolds) is a second value only it reads: the demuxer must not
	// split the one it reads into a total the library does not derive.
	native, mapper := apeReaders(t, append(slices.Clip(base), apeTag([2]string{"Disc", "1/2"}, [2]string{"Di\u017fc", "3"})...))
	if !slices.Equal(native["DISCNUMBER"], []string{"1/2"}) || native["DISCTOTAL"] != nil || mapper["DISCTOTAL"] != nil {
		t.Errorf("a folded second disc number: native %v, library %v", native, mapper)
	}
	// A folded total beside a blank number side: the library drops the
	// number and holds its own total, so the demuxer drops the number too and
	// derives none.
	native, mapper = apeReaders(t, append(slices.Clip(base), apeTag([2]string{"Track", "/12"}, [2]string{"Total_Part\u017f", "10"})...))
	if len(native) != 0 || mapper["TRACKNUMBER"] != nil || !slices.Equal(mapper["TRACKTOTAL"], []string{"10"}) {
		t.Errorf("a blank number beside a folded total: native %v, library %v", native, mapper)
	}
}
