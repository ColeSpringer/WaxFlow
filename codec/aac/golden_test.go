package aac

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
)

// TestEncoderGoldenBytes pins the encoders' output bytes for fixed
// synthetic inputs, the committed artifact behind ADR-0004's contract:
// an unchanged version string means unchanged bytes. Behavioural tests
// (delay pins, round trips, cross-decoder differentials) all pass on
// drifted-but-valid output, so without this pin a refactor can move
// bytes silently and one cached stream can mix generations under one
// key.
//
// A hash change here is not a failure to paper over: either revert the
// byte change, or bump the matching version constant (EncoderVersion /
// HEEncoderVersion / HEV2EncoderVersion) and update the hash in the
// same commit. The encoders are pure functions of their input on every
// platform by design (float tables rounded once, no FMA in the
// kernels); a platform-dependent hash means that promise broke, which
// this test exists to surface too.
func TestEncoderGoldenBytes(t *testing.T) {
	sig := func(n, ch int) [][]float32 {
		out := make([][]float32, ch)
		for c := range out {
			out[c] = make([]float32, n)
		}
		seed := uint64(42)
		for i := 0; i < n; i++ {
			seed = seed*6364136223846793005 + 1442695040888963407
			noise := 0.02 * (2*float64(seed>>11)/float64(1<<53) - 1)
			v := 0.25*math.Sin(2*math.Pi*440*float64(i)/48000) +
				0.15*math.Sin(2*math.Pi*9500*float64(i)/48000) +
				0.08*math.Sin(2*math.Pi*14100*float64(i)/48000) + noise
			if i >= 30000 && i < 30400 {
				v += 0.6 * math.Sin(2*math.Pi*12000*float64(i)/48000)
			}
			for c := 0; c < ch; c++ {
				out[c][i] = float32(v * (1 - 0.2*float64(c)))
			}
		}
		return out
	}
	const n = 48000 * 2

	for _, tc := range []struct {
		name string
		ch   int
		he   bool
		v2   bool
		kbps int
		want string
	}{
		{"lc-stereo-128", 2, false, false, 128000,
			"20b9d0966d8e43c834363cb23f2ed02395b62cf43bfdb9686beaaac3b94fd976"},
		{"he-mono-64", 1, true, false, 64000,
			"7daad116e64bd7d6cd18139a94f88df4308c5733af7e307509e09c011cedcf5a"},
		{"he-stereo-64", 2, true, false, 64000,
			"fcac7117ad3069098a5fedff53f21af833350fe177b05f98807ecfc5a134a912"},
		{"he-stereo-32", 2, true, false, 32000,
			"6a32160dce9d1b074ec18d042289d3aa90112476ddb61c3dbfa605d964eb3423"},
		{"hev2-stereo-32", 2, true, true, 32000,
			"03edd7cf9ea7d664fd60e5926b908cfe2523a835e33b915fcd4d94c8179ee964"},
	} {
		f := audio.Format{Rate: 48000, Channels: tc.ch, Layout: audio.DefaultLayout(tc.ch), Type: audio.Float, BitDepth: 32}
		h := sha256.New()
		emit := func(p codec.Packet) error {
			h.Write(p.Data)
			return nil
		}
		in := sig(n, tc.ch)
		buf := audio.Get(f, 2048)
		feed := func(enc interface {
			Encode(*audio.Buffer, func(codec.Packet) error) error
			Finish(func(codec.Packet) error) (codec.Trailer, error)
		}) {
			for off := 0; off < n; off += 2048 {
				m := min(2048, n-off)
				buf.N = m
				for c := 0; c < tc.ch; c++ {
					copy(buf.ChanF(c)[:m], in[c][off:off+m])
				}
				if err := enc.Encode(buf, emit); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := enc.Finish(emit); err != nil {
				t.Fatal(err)
			}
		}
		if tc.he {
			enc, err := NewHEEncoder(f, &EncoderOptions{Bitrate: tc.kbps, ParametricStereo: tc.v2})
			if err != nil {
				t.Fatal(err)
			}
			feed(enc)
		} else {
			enc, err := NewEncoder(f, &EncoderOptions{Bitrate: tc.kbps})
			if err != nil {
				t.Fatal(err)
			}
			feed(enc)
		}
		audio.Put(buf)
		if got := hex.EncodeToString(h.Sum(nil)); got != tc.want {
			t.Errorf("%s: AU stream hashes %s, golden %s\n(a deliberate byte change needs its version constant bumped in the same commit)",
				tc.name, got, tc.want)
		}
	}
}
