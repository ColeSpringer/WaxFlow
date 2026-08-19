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
			"0dcbecfdd529481f81c6620ae4147a2b8b4797a6168335f277786156f9fc322d"},
		{"he-mono-64", 1, true, false, 64000,
			"6654e9e89659809fe5b8f5b9b2df608b054f32762e39cf636ddb30d7186c7206"},
		{"he-stereo-64", 2, true, false, 64000,
			"f896e1804b032fe7aee2b53f668afc201d106847e49f6e4a979c8f467aa95583"},
		{"he-stereo-32", 2, true, false, 32000,
			"ddda508b3eece10e4b6198b47768dd6a87a006dc5c585d77df15ddf07c056aab"},
		{"hev2-stereo-32", 2, true, true, 32000,
			"21882a5efcdcbbbe9b5623758ca55d2c13822e7c15680482ccc0e1b440fe6605"},
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
