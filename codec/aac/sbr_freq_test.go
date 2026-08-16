package aac

import "testing"

// TestSBRFreqTables48k pins the complete band-table derivation for the
// header shape the 48 kHz conformance streams use (startFreq 12, stopFreq
// 9, xover 0, freqScale 2, alterScale 1, noiseBands 3). The values were
// cross-validated against ffmpeg's derivation on a real stream, so any
// drift here is a real decode change, not a table refresh.
func TestSBRFreqTables48k(t *testing.T) {
	h := sbrHeader{
		ampRes: 1, startFreq: 12, stopFreq: 9, xoverBand: 0,
		freqScale: 2, alterScale: 1, noiseBands: 3,
		limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1,
	}
	tbl, err := deriveFreqTables(h, 48000)
	if err != nil {
		t.Fatalf("deriveFreqTables: %v", err)
	}
	if tbl.k0 != 22 || tbl.k2 != 45 || tbl.kx != 22 || tbl.m != 23 {
		t.Errorf("k0/k2/kx/M = %d/%d/%d/%d, want 22/45/22/23", tbl.k0, tbl.k2, tbl.kx, tbl.m)
	}
	wantMaster := []int{22, 23, 25, 27, 29, 31, 33, 36, 39, 42, 45}
	if !equalInts(tbl.fMaster[:tbl.nMaster+1], wantMaster) {
		t.Errorf("fMaster = %v, want %v", tbl.fMaster[:tbl.nMaster+1], wantMaster)
	}
	if !equalInts(tbl.fHigh[:tbl.nHigh+1], wantMaster) {
		t.Errorf("fHigh = %v, want %v (xover 0)", tbl.fHigh[:tbl.nHigh+1], wantMaster)
	}
	wantLow := []int{22, 25, 29, 33, 39, 45}
	if !equalInts(tbl.fLow[:tbl.nLow+1], wantLow) {
		t.Errorf("fLow = %v, want %v", tbl.fLow[:tbl.nLow+1], wantLow)
	}
	if tbl.nQ != 3 {
		t.Fatalf("nQ = %d, want 3", tbl.nQ)
	}
	wantNoise := []int{22, 25, 33, 45}
	if !equalInts(tbl.fNoise[:tbl.nQ+1], wantNoise) {
		t.Errorf("fNoise = %v, want %v", tbl.fNoise[:tbl.nQ+1], wantNoise)
	}
	// The limiter table folds the patch border at 42 into the low bands.
	wantLim := []int{22, 29, 42, 45}
	if tbl.nL != 3 || !equalInts(tbl.fLim[:tbl.nL+1], wantLim) {
		t.Errorf("fLim = %v (nL %d), want %v", tbl.fLim[:tbl.nL+1], tbl.nL, wantLim)
	}
	if !equalInts(tbl.patchNum[:], []int{20, 3}) || !equalInts(tbl.patchStart[:], []int{2, 18}) {
		t.Errorf("patches num %v start %v, want [20 3]/[2 18]", tbl.patchNum, tbl.patchStart)
	}
}

// TestSBRFreqTables44k pins the fdk fixture's shape: 44100 output with the
// header fdk writes (validated end to end by the committed differential).
func TestSBRFreqTables44k(t *testing.T) {
	// startFreq 7, stopFreq 10 is a common fdk choice at 48 kbps; what this
	// test pins is not one encoder's header but the derivation invariants:
	// borders ascending, kx+M == k2, noise bands within [1, 5], and every
	// patch source inside the low band.
	for _, h := range []sbrHeader{
		{startFreq: 7, stopFreq: 10, freqScale: 2, alterScale: 1, noiseBands: 2,
			limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1},
		{startFreq: 12, stopFreq: 9, freqScale: 2, alterScale: 1, noiseBands: 3,
			limiterBands: 2, limiterGains: 2, interpolFreq: 1, smoothingMode: 1},
	} {
		tbl, err := deriveFreqTables(h, 44100)
		if err != nil {
			t.Fatalf("deriveFreqTables(%+v): %v", h, err)
		}
		if tbl.kx+tbl.m != tbl.k2 {
			t.Errorf("kx(%d)+M(%d) != k2(%d)", tbl.kx, tbl.m, tbl.k2)
		}
		for i := 0; i < tbl.nHigh; i++ {
			if tbl.fHigh[i] >= tbl.fHigh[i+1] {
				t.Errorf("fHigh not ascending at %d: %v", i, tbl.fHigh[:tbl.nHigh+1])
			}
		}
		if tbl.nQ < 1 || tbl.nQ > 5 {
			t.Errorf("nQ = %d out of range", tbl.nQ)
		}
		for p := range tbl.patchNum {
			if tbl.patchNum[p] == 0 {
				continue
			}
			if tbl.patchStart[p] < 0 || tbl.patchStart[p]+tbl.patchNum[p] > tbl.kx {
				t.Errorf("patch %d [%d,%d) escapes the low band (kx %d)",
					p, tbl.patchStart[p], tbl.patchStart[p]+tbl.patchNum[p], tbl.kx)
			}
		}
	}
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
