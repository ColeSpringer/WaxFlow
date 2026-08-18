package aac

import (
	"math"
	"testing"
)

// TestSBRHuffEncTableCoverage pins the invariant the writer rests on:
// sbrHuffEncTable.write emits nothing for an unpopulated slot, so every
// delta the decision phase can produce must have a code. Non-balance
// tables must cover every integer in [-max, max]; balance tables carry
// pre-doubled deltas, so they must cover every EVEN value there and,
// just as load-bearing, no odd one (an odd delta reaching a balance
// table means the even-grid clamps failed and the payload would end
// short with no error).
func TestSBRHuffEncTableCoverage(t *testing.T) {
	tabs := []struct {
		name string
		tab  *sbrHuffEncTable
		step int
	}{
		{"TEnv15", &sbrEncTEnv15, 1},
		{"FEnv15", &sbrEncFEnv15, 1},
		{"TEnvBal15", &sbrEncTEnvBal15, 2},
		{"FEnvBal15", &sbrEncFEnvBal15, 2},
		{"TEnv30", &sbrEncTEnv30, 1},
		{"FEnv30", &sbrEncFEnv30, 1},
		{"TEnvBal30", &sbrEncTEnvBal30, 2},
		{"FEnvBal30", &sbrEncFEnvBal30, 2},
		{"TNoise30", &sbrEncTNoise30, 1},
		{"TNoiseBal30", &sbrEncTNoiseBal3, 2},
	}
	for _, tc := range tabs {
		if tc.tab.max == 0 {
			t.Errorf("%s: empty encode table", tc.name)
			continue
		}
		for d := -tc.tab.max; d <= tc.tab.max; d++ {
			onGrid := tc.step == 1 || d%2 == 0
			coded := tc.tab.bits[d+64] != 0
			if onGrid && !coded {
				t.Errorf("%s: delta %d on the table's grid has no code", tc.name, d)
			}
			if !onGrid && coded {
				t.Errorf("%s: odd delta %d has a code; the even-grid clamps assume it cannot", tc.name, d)
			}
		}
	}
}

// TestSBRPayloadRoundTrip drives the SBR extractor over a signal with
// steady, tonal, and transient regions and parses every written payload
// back through the decoder's own parser: the header, grid, delta flags,
// inverse-filtering levels, envelopes, noise floors, and sinusoid markers
// must all survive exactly, across the delta-time anchor chain.
func TestSBRPayloadRoundTrip(t *testing.T) {
	for _, chans := range []int{1, 2} {
		s, err := newSBREnc(48000, chans, 64000)
		if err != nil {
			t.Fatal(err)
		}
		el := newSBRElement(chans, 48000, false, false)

		// 40 frames of input: tones across the band, a noise bed, and a
		// hard burst two thirds in (a transient grid somewhere mid-run).
		const frames = 40
		seed := uint64(12345)
		noise := func() float32 {
			seed = seed*6364136223846793005 + 1442695040888963407
			return float32(2*float64(seed>>11)/float64(1<<53) - 1)
		}
		var slot [2][64]float32
		for n := 0; n < frames*2048; n++ {
			v := 0.3*math.Sin(2*math.Pi*440*float64(n)/48000) +
				0.2*math.Sin(2*math.Pi*9000*float64(n)/48000) +
				0.1*math.Sin(2*math.Pi*14000*float64(n)/48000)
			v += 0.02 * float64(noise())
			if n >= 26*2048 && n < 26*2048+400 {
				v += 0.7 * float64(noise())
			}
			for c := 0; c < chans; c++ {
				slot[c][n%64] = float32(v)
				if n%64 == 63 {
					s.pushSlot(c, slot[c][:])
				}
			}
		}

		for au := int64(0); au < frames; au++ {
			payload := s.payloadFor(au)
			want := make([]sbrFrameData, chans)
			for c := 0; c < chans; c++ {
				want[c] = s.fd[c]
			}

			var w bitWriter
			writeFIL(&w, payload)
			w.align()
			r := newBitReader(w.buf)
			if tag := r.read(3); tag != elFIL {
				t.Fatalf("au %d: element tag %d, want FIL", au, tag)
			}
			el.beginFrame()
			el.parseFill(r, filCount(r))
			if !el.dataOK {
				t.Fatalf("chans=%d au %d: payload did not parse", chans, au)
			}
			if au == 0 && el.hdr != s.hdr {
				t.Fatalf("parsed header %+v, want %+v", el.hdr, s.hdr)
			}
			for c := 0; c < chans; c++ {
				compareSBRFrame(t, chans, au, c, &el.tbl, &want[c], &el.ch[c].frame)
			}
			if chans == 2 && !el.coupled {
				t.Fatalf("au %d: pair parsed uncoupled", au)
			}
		}
	}
}

func compareSBRFrame(t *testing.T, chans int, au int64, c int, tbl *sbrFreqTables, want, got *sbrFrameData) {
	t.Helper()
	fail := func(field string, wantV, gotV any) {
		t.Errorf("chans=%d au=%d ch=%d %s: wrote %v, parsed %v", chans, au, c, field, wantV, gotV)
	}
	if got.grid.frameClass != want.grid.frameClass || got.grid.numEnv != want.grid.numEnv ||
		got.grid.lA != want.grid.lA || got.grid.numNoise != want.grid.numNoise {
		fail("grid", want.grid, got.grid)
		return
	}
	for i := 0; i <= want.grid.numEnv; i++ {
		if got.grid.tE[i] != want.grid.tE[i] {
			fail("tE", want.grid.tE, got.grid.tE)
		}
	}
	for i := 0; i <= want.grid.numNoise; i++ {
		if got.grid.tQ[i] != want.grid.tQ[i] {
			fail("tQ", want.grid.tQ, got.grid.tQ)
		}
	}
	if got.ampRes != want.ampRes {
		fail("ampRes", want.ampRes, got.ampRes)
	}
	for l := 0; l < want.grid.numEnv; l++ {
		if got.grid.freqRes[l] != want.grid.freqRes[l] {
			fail("freqRes", want.grid.freqRes, got.grid.freqRes)
		}
		if got.dfEnv[l] != want.dfEnv[l] {
			fail("dfEnv", want.dfEnv, got.dfEnv)
		}
		n := envBands(tbl, want.grid.freqRes[l])
		for b := 0; b < n; b++ {
			if got.env[l][b] != want.env[l][b] {
				fail("env", want.env[l][:n], got.env[l][:n])
				break
			}
		}
	}
	for l := 0; l < want.grid.numNoise; l++ {
		if got.dfNoise[l] != want.dfNoise[l] {
			fail("dfNoise", want.dfNoise, got.dfNoise)
		}
		for q := 0; q < tbl.nQ; q++ {
			if got.noise[l][q] != want.noise[l][q] {
				fail("noise", want.noise[l][:tbl.nQ], got.noise[l][:tbl.nQ])
				break
			}
		}
	}
	for q := 0; q < tbl.nQ; q++ {
		if got.invf[q] != want.invf[q] {
			fail("invf", want.invf[:tbl.nQ], got.invf[:tbl.nQ])
			break
		}
	}
	for b := 0; b < tbl.nHigh; b++ {
		if got.addHarmonic[b] != want.addHarmonic[b] {
			fail("addHarmonic", want.addHarmonic[:tbl.nHigh], got.addHarmonic[:tbl.nHigh])
			break
		}
	}
}
