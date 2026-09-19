package server_test

// The ladder's answer for a Matroska source that trims samples in the middle
// of its run. The fixture is server.MidTrimWebM; what the daemon has to get
// right is which rung serves it, because the copy rungs would play the trimmed
// frames back as audio and the transcode rung trims them in PCM.

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/server"
)

// decodedBodyFrames is decodedFrames over a response body rather than a file:
// what a read of the bytes the daemon sent delivers.
func decodedBodyFrames(t *testing.T, raw []byte, hint string) int64 {
	t.Helper()
	med, err := format.Open(container.BytesSource(raw), hint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	buf := audio.Get(med.Info().Default().Fmt, audio.StandardChunk)
	defer audio.Put(buf)
	var n int64
	for {
		err := med.ReadChunk(buf)
		if err == io.EOF {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		n += int64(buf.N)
	}
}

// TestStreamOfAnInnerTrimTakesTheTranscodeRung: an Ogg granule names one end
// trim, so a copy of this source into Ogg-Opus would play the inner trim's
// frames. The plan declines rung 2 and the body is a re-encode of the trimmed
// audio, which is the answer that keeps what the source plays.
//
// Two fixtures, because the plan only knows about the trim if something walked
// the file: one that declares an Info Duration and one that declares no length
// at all. The second is the shape prepareSource now measures for the same
// reason the playlist always did.
func TestStreamOfAnInnerTrimTakesTheTranscodeRung(t *testing.T) {
	for _, declared := range []bool{true, false} {
		name := "duration-less"
		if declared {
			name = "with a Duration"
		}
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, nil)
			path := filepath.Join(env.root, "midtrim.webm")
			want := server.MidTrimWebM(t, path, declared)

			before := env.srv.Metrics().Remuxes.Load()
			resp := env.get(t, "/stream?src=lib/midtrim.webm&format=opus", nil)
			body := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("stream = %d", resp.StatusCode)
			}
			if got := env.srv.Metrics().Remuxes.Load(); got != before {
				t.Errorf("remux_total moved to %d: rung 2 copied a source with a trim inside the run", got)
			}
			if got := decodedBodyFrames(t, body, "opus"); got != want {
				t.Errorf("the body decodes to %d frames, want the trimmed %d", got, want)
			}
		})
	}
}

// TestStreamOfAnInnerTrimRemuxesToWebM is the destination that can say it: the
// copy rung serves, and the trim comes out on the block that stated it.
func TestStreamOfAnInnerTrimRemuxesToWebM(t *testing.T) {
	env := newTestEnv(t, nil)
	path := filepath.Join(env.root, "midtrim.webm")
	want := server.MidTrimWebM(t, path, true)

	before := env.srv.Metrics().Remuxes.Load()
	resp := env.get(t, "/stream?src=lib/midtrim.webm&format=opus&container=webm", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d", resp.StatusCode)
	}
	if got := env.srv.Metrics().Remuxes.Load(); got != before+1 {
		t.Fatalf("remux_total = %d, want %d: the request took a rung other than 2", got, before+1)
	}
	if got := decodedBodyFrames(t, body, "webm"); got != want {
		t.Errorf("the body decodes to %d frames, want %d", got, want)
	}

	demux, info, err := format.OpenDemuxer(container.BytesSource(body), "webm", nil)
	if err != nil {
		t.Fatal(err)
	}
	var pkt container.Packet
	for i := 0; ; i++ {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			t.Fatal("the copy states no trim on any block")
		}
		if err != nil {
			t.Fatal(err)
		}
		if pkt.Track != info.Default().ID {
			continue
		}
		if i == server.MidTrimBlock {
			if pkt.Padding != server.MidTrimPad {
				t.Errorf("block %d of the copy states a trim of %d, want the source's %d",
					i, pkt.Padding, server.MidTrimPad)
			}
			return
		}
	}
}

// TestCutOfAnInnerTrimSource: a span of the same source takes the cut rung
// when the destination can carry the trim its kept packets hold (WebM) and
// the transcode rung when it cannot (Ogg-Opus), and both bodies hold the
// span asked for.
//
// [1000, 4000) of the seven-packet fixture: the head snaps to packet 0 and
// the tail past the trimmed packet 2 to the end of packet 5, so the trim is
// among the kept packets.
func TestCutOfAnInnerTrimSource(t *testing.T) {
	env := newTestEnv(t, nil)
	path := filepath.Join(env.root, "midtrim.webm")
	server.MidTrimWebM(t, path, true)
	const want = 3000
	for _, tc := range []struct {
		name, query, hint string
		cut               bool
	}{
		{"webm takes the cut", "&container=webm", "webm", true},
		{"ogg re-encodes", "", "opus", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cuts, remuxes := env.srv.Metrics().Cuts.Load(), env.srv.Metrics().Remuxes.Load()
			resp := env.get(t, "/stream?src=lib/midtrim.webm&format=opus&from=1000&to=4000"+tc.query, nil)
			body := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("stream = %d: %s", resp.StatusCode, body)
			}
			if got := env.srv.Metrics().Remuxes.Load(); got != remuxes {
				t.Errorf("remux_total moved to %d: a span cannot take rung 2", got)
			}
			wantCuts := cuts
			if tc.cut {
				wantCuts++
			}
			if got := env.srv.Metrics().Cuts.Load(); got != wantCuts {
				t.Errorf("cut_total = %d, want %d", got, wantCuts)
			}
			if got := decodedBodyFrames(t, body, tc.hint); got != want {
				t.Errorf("the body decodes to %d frames, want the %d asked for", got, want)
			}
		})
	}
}
