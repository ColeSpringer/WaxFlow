// The cache-key net. TestGoldenCacheKeys prints, for a fixed matrix of
// requests, what each cache key hashes: the identity of its source, the
// canonical parameter string and the version terms, beside the surface that
// built it and the ladder rung it took.
//
// The rule it buys: a change that moves a line says so. An entry cached under
// the old line is orphaned, and two requests that come to share a line share
// bytes.
//
// No request here encodes. A HEAD on /stream answers from the plan, a media
// playlist plans and lists, and a timeline is minted from its members' headers.
package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/testutil"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
)

// builtKey is one key as server.OnKey reports it, its identity masked.
type builtKey struct{ surface, rung, identity, canonical, versions string }

// keyLog collects the keys a request builds.
type keyLog struct {
	mu   sync.Mutex
	keys []builtKey
}

func (l *keyLog) add(surface, rung, identity, canonical string, versions []string) {
	// The build's float term is left out: it is this build's, and the file
	// is every build's. The engine's own test holds which plans carry it.
	if waxflow.FloatBuildVersion != "" {
		versions = slices.DeleteFunc(slices.Clone(versions), func(v string) bool { return v == waxflow.FloatBuildVersion })
	}
	v := "-"
	if len(versions) > 0 {
		v = strings.Join(versions, ",")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, builtKey{surface, rung, maskIdentity(identity), canonical, v})
}

// take returns the keys built since the last take.
func (l *keyLog) take() []builtKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := l.keys
	l.keys = nil
	return keys
}

// fileID is the part of an identity that states one file's size and mtime.
var fileID = regexp.MustCompile(`\|\d+-\d+$`)

// maskIdentity keeps what an identity says about how a key names its source
// and drops what differs from run to run: a file's size and mtime, and a
// timeline's digest, which covers its members'. A byte that is not ASCII is
// written as an escape, so the reference's own bytes can be read off the line.
func maskIdentity(identity string) string {
	if strings.HasPrefix(identity, "tl:") {
		return "tl:<digest>"
	}
	q := strconv.QuoteToASCII(fileID.ReplaceAllString(identity, "|<id>"))
	return q[1 : len(q)-1]
}

// keyCase is one request, the surface it asks and the rung it is in the
// matrix to take. A stream or timeline query is asked of /stream by HEAD, an
// art or lyrics one of its own endpoint. An hls query is the keyed master
// form's, and every variant's media playlist is planned.
type keyCase struct{ name, surface, rung, query string }

// strayRef is a reference that is not UTF-8, and straySrc its src= spelling.
// Its key hashes the reference's own bytes, not the pct: text a URL carries.
const strayRef = "lib/caf\xe9.wav"

var straySrc = "src=" + url.QueryEscape("pct:lib/caf%E9.wav")

// aliasResolver serves one reference from another's file, so a daemon on any
// file system can be asked for a name only some of them can hold.
type aliasResolver struct {
	next    source.Resolver
	ref, to string
}

func (r aliasResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	if ref != r.ref {
		return r.next.Resolve(ctx, ref)
	}
	f, err := r.next.Resolve(ctx, r.to)
	if err == nil {
		f.Ref = ref
	}
	return f, err
}

// coverMapper is a tag mapper that finds a cover and a lyric sheet in every
// source, which is what /art and /lyrics need before they answer.
type coverMapper struct{}

func (coverMapper) Read(context.Context, container.Source, string, meta.ReadOptions) (*meta.Info, error) {
	return &meta.Info{
		Tags:        map[string][]string{"LYRICS": {"la la"}},
		HasPictures: true,
		Pictures:    []meta.Picture{{MIME: "image/png", Front: true, Data: []byte("\x89PNG")}},
	}, nil
}

func (coverMapper) Apply(context.Context, string, *meta.Info, []container.Tag) error { return nil }

// The matrix's sources, by what makes each one a case.
//
// The book is the one source with tags a stream embeds (a title, an artist,
// an album), so its lines end in a &tags= term: a fingerprint of them. Those
// lines move with that file's tags and with what the fingerprint covers, as
// well as with the plan. The ReplayGain tags of gained.m4a are no part of
// that set, so its lines carry no term.
const (
	srcWAV    = "src=lib/sine.wav"         // 44.1 kHz stereo, 16-bit, 0.05 s
	srcRamp   = "src=lib/ramp.wav"         // 48 kHz stereo, 16-bit, 4 s
	srcALaw   = "src=lib/sine-alaw.wav"    // A-law, which nothing plays from a WAV
	srcFLAC   = "src=lib/album/track.flac" // 44.1 kHz stereo, 16-bit
	srcFLAC24 = "src=lib/sine-s24.flac"    // 48 kHz stereo, 24-bit
	srcFLAC20 = "src=lib/sine-s20.flac"    // a depth HLS widens
	srcFloat  = "src=lib/sine-f32.wav"     // float PCM
	src51     = "src=lib/sine-5_1.flac"    // six channels
	srcGained = "src=lib/gained.m4a"       // REPLAYGAIN_TRACK_GAIN of -6 dB
	srcWebM   = "src=lib/seed.webm"        // Opus in WebM, seven 960-sample packets
	srcM4B    = "src=lib/book.m4b"         // AAC-LC in MP4, with tags
	srcHEAAC  = "src=lib/he.aac"           // HE-AAC in ADTS, signalled implicitly
	spanOpus  = "&from=960&to=3840"        // whole 960-sample packets
	spanAAC   = "&from=1024&to=4096"       // whole 1024-sample frames
	spanRamp  = "&from=48000&to=96000"     // one second of the ramp
	gainOff   = "&gain=off"                // a timeline refuses the daemon's tag mode
	xfade     = "&crossfadeSeconds=2"      // fits the timeline's two members
	seek      = "&t=1.5"                   // inside the ramp and the timeline
	lossyQ    = "low med high"             // every q= preset
	lossyRows = "mp3 opus aac he-aac"      // the rows a bit rate applies to
	liveRows  = "opus vorbis flac mp3 aac he-aac alac wavpack"
)

// keyMatrix lists the requests, by the surface each asks and the rung each
// takes. tl is a timeline of two 16-bit members, mixed one of a 16-bit and a
// 24-bit member, and withWebM one of a WAV and a Matroska member, whose
// demuxer's revision the timeline's key lists.
func keyMatrix(tl, mixed, withWebM string) (cases []keyCase) {
	type ask struct{ name, query string }
	add := func(surface, rung string, asks []ask) {
		for _, a := range asks {
			cases = append(cases, keyCase{name: a.name, surface: surface, rung: rung, query: a.query})
		}
	}

	add("stream", "direct", []ask{
		{"direct-flac", srcFLAC + "&format=flac"},
		{"direct-ref-not-utf8", straySrc + "&format=wav"},
	})

	add("stream", "remux", []ask{
		{"remux-webm-opus", srcWebM + "&format=opus"},
		{"remux-webm-mka", srcWebM + "&format=opus&container=mka"},
		{"remux-flac-mka", srcFLAC + "&format=flac&container=mka"},
		{"remux-flac-ogg", srcFLAC + "&format=flac&container=ogg"},
		{"remux-m4b-aac", srcM4B + "&format=aac"},
		{"remux-m4b-adts", srcM4B + "&format=aac&container=adts"},
		{"remux-m4b-fragmented", srcM4B + "&format=aac&container=fragmented"},
		// format=aac is the name an HE-AAC copy is asked for by.
		{"remux-heaac-as-aac", srcHEAAC + "&format=aac"},
	})

	add("stream", "cut", []ask{
		{"cut-opus", srcWebM + "&format=opus" + spanOpus},
		{"cut-aac", srcM4B + "&format=aac" + spanAAC},
	})
	// Named fragmented, the destination cannot state the cut's delay, so the
	// span the default container copies is encoded.
	add("stream", "transcode", []ask{
		{"cut-aac-fragmented", srcM4B + "&format=aac&container=fragmented" + spanAAC},
	})

	// Every live row bare. A WAV asked for as wav is direct play, so that row
	// reads the FLAC; the A-law WAV is what format=auto has to transcode.
	add("stream", "transcode", []ask{
		{"wav", srcFLAC + "&format=wav"},
		{"auto", srcALaw},
	})
	for row := range strings.FieldsSeq(liveRows) {
		add("stream", "transcode", []ask{{row, srcWAV + "&format=" + row}})
	}
	for row := range strings.FieldsSeq(lossyRows) {
		add("stream", "transcode", []ask{{row + "-bitrate", srcWAV + "&format=" + row + "&bitrate=96"}})
		for q := range strings.FieldsSeq(lossyQ) {
			add("stream", "transcode", []ask{{row + "-q-" + q, srcWAV + "&format=" + row + "&q=" + q}})
		}
	}
	add("stream", "transcode", []ask{
		{"he-aac-v2", srcWAV + "&format=he-aac&hev2=1"},
		{"mp3-24k", srcWAV + "&format=mp3&rate=24000"},
		// A cap the plan fits under refuses nothing and is no term of the key:
		// this line is the bare mp3 one.
		{"mp3-capped", srcWAV + "&format=mp3&maxBitRate=128"},
		{"flac-bits16", srcWAV + "&format=flac&bits=16"},
		{"flac-bits24", srcWAV + "&format=flac&bits=24"},
		{"flac-rate", srcWAV + "&format=flac&rate=48000"},
		{"flac-mono", srcWAV + "&format=flac&ch=1"},
		{"flac-from-float", srcFloat + "&format=flac"},
		{"aac-from-5.1", src51 + "&format=aac"},
		{"mp3-from-5.1", src51 + "&format=mp3"},
		{"gain-fixed", srcWAV + "&format=flac&gain=-3"},
		{"gain-fixed-voice", srcWAV + "&format=flac&gain=20&dynamics=voice"},
		{"gain-default", srcGained + "&format=opus"},
		{"gain-track", srcGained + "&format=opus&gain=track"},
		{"gain-album", srcGained + "&format=opus&gain=album"},
		{"gain-off", srcGained + "&format=opus&gain=off"},
		{"dynamics-voice", srcWAV + "&format=opus&dynamics=voice"},
		{"seek", srcRamp + "&format=opus" + seek},
		{"span", srcRamp + "&format=flac" + spanRamp},
		{"span-seek", srcRamp + "&format=flac&t=0.5" + spanRamp},
		{"wav-in-mka", srcFLAC + "&format=wav&container=mka"},
		{"opus-in-mka", srcWAV + "&format=opus&container=mka"},
		{"opus-in-webm", srcWAV + "&format=opus&container=webm"},
		{"vorbis-in-mka", srcWAV + "&format=vorbis&container=mka"},
		{"vorbis-in-webm", srcWAV + "&format=vorbis&container=webm"},
		{"flac-in-mka", srcWAV + "&format=flac&container=mka"},
		{"flac-in-ogg", srcWAV + "&format=flac&container=ogg"},
		{"aac-in-adts", srcWAV + "&format=aac&container=adts"},
		{"aac-fragmented", srcWAV + "&format=aac&container=fragmented"},
		{"aac-in-mka", srcWAV + "&format=aac&container=mka"},
		// A Matroska source: its demuxer's revision heads the versions on
		// this rung as on the copy rungs above.
		{"webm-flac", srcWebM + "&format=flac"},
		{"he-aac-in-adts", srcWAV + "&format=he-aac&container=adts"},
		{"he-aac-fragmented", srcWAV + "&format=he-aac&container=fragmented"},
		{"he-aac-in-mka", srcWAV + "&format=he-aac&container=mka"},
		{"alac-fragmented", srcWAV + "&format=alac&container=fragmented"},
		{"ref-not-utf8", straySrc + "&format=flac"},
	})

	// A timeline has one rung.
	add("timeline", "transcode", []ask{
		{"tl-flac", "tl=" + tl + "&format=flac" + gainOff},
		{"tl-opus", "tl=" + tl + "&format=opus" + gainOff},
		{"tl-flac-crossfade", "tl=" + tl + "&format=flac" + gainOff + xfade},
		{"tl-opus-crossfade", "tl=" + tl + "&format=opus" + gainOff + xfade},
		{"tl-opus-seek", "tl=" + tl + "&format=opus" + gainOff + seek},
		{"tl-opus-gain", "tl=" + tl + "&format=opus&gain=-3"},
		{"tl-flac-mixed-width", "tl=" + mixed + "&format=flac" + gainOff},
		{"tl-flac-webm-member", "tl=" + withWebM + "&format=flac" + gainOff},
	})

	add("hls", "remux", []ask{
		{"hls-remux-opus", srcWebM + "&format=opus"},
		{"hls-remux-flac16", srcFLAC + "&format=flac"},
		{"hls-remux-flac24", srcFLAC24 + "&format=flac"},
		{"hls-remux-aac", srcM4B + "&format=aac"},
		// A copy answers the stream's own signalling, where an encode answers LC.
		{"hls-remux-heaac-as-aac", srcHEAAC + "&format=aac"},
	})
	add("hls", "cut", []ask{{"hls-cut-opus", srcWebM + "&format=opus" + spanOpus}})
	add("hls", "transcode", []ask{
		{"hls-default", srcRamp},
		{"hls-opus", srcRamp + "&format=opus"},
		{"hls-opus-ladder", srcRamp + "&format=opus&bitrates=64,96"},
		{"hls-opus-segdur2", srcRamp + "&format=opus&segDur=2"},
		{"hls-opus-segdur6", srcRamp + "&format=opus&segDur=6"},
		{"hls-opus-span", srcRamp + "&format=opus" + spanRamp},
		{"hls-opus-gain-default", srcGained + "&format=opus"},
		{"hls-opus-voice", srcRamp + "&format=opus&dynamics=voice"},
		{"hls-aac", srcRamp + "&format=aac"},
		{"hls-aac-bitrate", srcRamp + "&format=aac&bitrate=96"},
		{"hls-he-aac", srcRamp + "&format=he-aac"},
		{"hls-he-aac-v2", srcRamp + "&format=he-aac&hev2=1"},
		{"hls-alac", srcRamp + "&format=alac"},
		{"hls-flac", srcRamp + "&format=flac"},
		{"hls-flac-bits24", srcRamp + "&format=flac&bits=24"},
		// A 20-bit FLAC cannot be copied: the segments carry it at 24.
		{"hls-flac20", srcFLAC20 + "&format=flac"},
		{"hls-tl-opus", "tl=" + tl + "&format=opus" + gainOff},
		{"hls-tl-opus-crossfade", "tl=" + tl + "&format=opus" + gainOff + xfade},
		{"hls-tl-flac-mixed-width", "tl=" + mixed + "&format=flac" + gainOff},
		{"hls-tl-flac-webm-member", "tl=" + withWebM + "&format=flac" + gainOff},
		{"hls-ref-not-utf8", straySrc + "&format=flac"},
		{"hls-webm-flac", srcWebM + "&format=flac"},
	})
	return cases
}

// keyEnv is a daemon with timelines on, no tag mapper, and the default gain
// named, so a change to the configuration's default does not move a line.
func keyEnv(t *testing.T) (*testEnv, *keyLog) {
	t.Helper()
	env := newTestEnv(t, func(cfg *server.Config) {
		cfg.TimelineDir = filepath.Join(t.TempDir(), "timelines")
		cfg.DefaultGain = "track"
		cfg.Resolver = aliasResolver{next: cfg.Resolver, ref: strayRef, to: "lib/sine.wav"}
	})
	put := func(name string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(env.root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path ...string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(path...))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	put("sine-s24.flac", read("..", "testdata", "sine-s24.flac"))
	put("sine-f32.wav", read("..", "testdata", "sine-f32.wav"))
	put("sine-5_1.flac", read("..", "testdata", "sine-5_1-s16.flac"))
	put("book.m4b", read("..", "testdata", "golden", "m4b-chapters.m4b"))
	put("he.aac", read("..", "container", "adts", "testdata", "golden-heaac-implicit.aac"))
	// A line holds no byte of these two, only the depth and the gain they
	// carry, so an encoder change does not move it. The gain rides an MP4
	// because that is a container whose tags the demuxer hands back.
	encode := func(src []byte, opts waxflow.TranscodeOptions) []byte {
		t.Helper()
		ws := &testutil.MemWriteSeeker{}
		if _, err := waxflow.New().Transcode(t.Context(), container.BytesSource(src), "wav", ws, opts); err != nil {
			t.Fatal(err)
		}
		return ws.Buf
	}
	put("sine-s20.flac", encode(read("..", "testdata", "sine-s24.wav"),
		waxflow.TranscodeOptions{Format: "flac", BitDepth: 20}))
	put("gained.m4a", encode(read(env.root, "sine.wav"), waxflow.TranscodeOptions{Format: "aac", Tags: []container.Tag{
		{Key: "REPLAYGAIN_TRACK_GAIN", Value: "-6.00 dB"},
		{Key: "REPLAYGAIN_TRACK_PEAK", Value: "0.500000"},
	}}))
	put("tl-a.wav", rampWAV(t, 48000, 2, 3*48000))
	put("tl-b.wav", rampWAV(t, 48000, 2, 5*24000))

	log := &keyLog{}
	env.srv.OnKey(log.add)
	return env, log
}

// ask sends the case's request and reports how many keys it should build. A
// HEAD that is refused carries no envelope, so the request is handed back in
// refused for its words to be fetched once the keys are counted.
func (tc keyCase) ask(t *testing.T, env *testEnv) (want int, refused string) {
	t.Helper()
	if tc.surface != "hls" {
		path := "/stream?" + tc.query
		if tc.surface == "art" || tc.surface == "lyrics" {
			path = "/" + tc.surface + "?" + tc.query
		}
		resp := env.req(t, http.MethodHead, path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 1, path
		}
		return 1, ""
	}
	resp := env.get(t, "/hls/master.m3u8?"+tc.query, nil)
	master := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%s: GET /hls/master.m3u8?%s = %d: %s", tc.name, tc.query, resp.StatusCode, master)
		return 1, ""
	}
	uris := variantURIs(t, master)
	if len(uris) == 0 {
		t.Errorf("%s: the master lists no variant:\n%s", tc.name, master)
		return 1, ""
	}
	for _, uri := range uris {
		resp := env.get(t, "/hls/"+uri, nil)
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: GET /hls/%s = %d: %s", tc.name, uri, resp.StatusCode, body)
		}
	}
	return len(uris), ""
}

// refusalOf fetches the words of a refusal a HEAD could not carry. The GET
// asks for one byte past the first, which a live transcode answers 416
// without starting: were the request not refused after all, nothing encodes.
func refusalOf(t *testing.T, env *testEnv, path string) string {
	t.Helper()
	resp := env.get(t, path, map[string]string{"Range": "bytes=1-1"})
	return fmt.Sprintf("%d: %s", resp.StatusCode, readBody(t, resp))
}

func TestGoldenCacheKeys(t *testing.T) {
	var out strings.Builder
	names := map[string]bool{}
	run := func(env *testEnv, log *keyLog, cases []keyCase) {
		for _, tc := range cases {
			if names[tc.name] {
				t.Fatalf("two cases are named %s", tc.name)
			}
			names[tc.name] = true
			log.take()
			want, refused := tc.ask(t, env)
			keys := log.take()
			if refused != "" {
				t.Errorf("%s: HEAD %s was refused, and a GET of it answers %s", tc.name, refused, refusalOf(t, env, refused))
				log.take()
			}
			if len(keys) != want {
				t.Errorf("%s built %d keys, want %d", tc.name, len(keys), want)
			}
			for i, k := range keys {
				// The surface and the rung are in the file, and checked here as
				// well: -update would otherwise repin a timeline keyed as a
				// stream, or a copy that had quietly become an encode.
				if k.surface != tc.surface || k.rung != tc.rung {
					t.Errorf("%s was keyed on %s and took the %s rung, want %s and %s",
						tc.name, k.surface, k.rung, tc.surface, tc.rung)
				}
				name := tc.name
				if len(keys) > 1 {
					name = fmt.Sprintf("%s#%d", tc.name, i+1)
				}
				fmt.Fprintf(&out, "%s | %s | %s | %s | %s | %s\n", name, k.surface, k.rung, k.identity, k.canonical, k.versions)
			}
		}
	}

	env, log := keyEnv(t)
	tl := mintTimeline(t, env, []string{"lib/tl-a.wav", "lib/tl-b.wav"}).Tl
	mixed := mintTimeline(t, env, []string{"lib/tl-a.wav", "lib/sine-s24.flac"}).Tl
	withWebM := mintTimeline(t, env, []string{"lib/tl-a.wav", "lib/seed.webm"}).Tl
	run(env, log, keyMatrix(tl, mixed, withWebM))

	// The cover and the lyric sheet answer to the direct key of their source.
	// Only a daemon with a tag mapper serves them, so they have one of their
	// own: a mapper would also move the tags every line above is keyed with.
	covers, coverLog := newTestEnv(t, func(cfg *server.Config) { cfg.Meta = coverMapper{} }), &keyLog{}
	covers.srv.OnKey(coverLog.add)
	run(covers, coverLog, []keyCase{
		{name: "art", surface: "art", rung: "direct", query: srcWAV},
		{name: "lyrics", surface: "lyrics", rung: "direct", query: srcWAV},
	})

	if t.Failed() {
		return
	}
	testutil.GoldenText(t, filepath.Join("testdata", "golden", "cache-keys.txt"), []byte(out.String()), *update,
		"a cache key moved: entries under the old key are orphaned, and requests that now share a key share bytes.\n"+
			"If the change says it moves these lines, run `make goldens` and review the diff line by line.")
}
