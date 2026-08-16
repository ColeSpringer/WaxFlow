package testutil

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

// WriteFloatWAV writes planar channels as an interleaved IEEE float32 WAV.
//
// The float domain is the point: an ffmpeg differential that measured a
// quantized int16 file would be comparing its filter against samples the Go
// side never saw, so a rounding difference would read as a detection
// difference. Writing float32 makes ffmpeg measure bit-identical samples to
// what the analyzer saw, which is what lets these differentials assert tight
// bounds rather than fuzzy ones.
//
// chans must be non-empty and its channels equal length.
func WriteFloatWAV(t *testing.T, path string, rate int, chans [][]float32) {
	t.Helper()
	if err := os.WriteFile(path, FloatWAVBytes(t, rate, chans), 0o666); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// ToneChans is the quiet base fixture: a 0.4 amplitude 440 Hz stereo tone,
// nowhere near full scale on or between samples, for the warning tests'
// "says nothing" halves.
func ToneChans(rate, frames int) [][]float32 {
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, frames)
		for i := range chans[c] {
			chans[c][i] = float32(0.4 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		}
	}
	return chans
}

// HotFloatChans is the shared clipping fixture: ToneChans touching both
// exact rails (load-bearing: +1.0 must not be counted, and the rail step
// rings past full scale, so even overs 0 is not quiet; ToneChans is),
// plus overs samples per channel planted at alternating +-1.5.
func HotFloatChans(t *testing.T, rate, frames, overs int) [][]float32 {
	t.Helper()
	if need := 100 + max(overs-1, 0)*37 + 1; frames < need {
		t.Fatalf("HotFloatChans: %d frames cannot hold %d overs (need %d)", frames, overs, need)
	}
	chans := ToneChans(rate, frames)
	for c := range chans {
		chans[c][0], chans[c][1] = 1, -1
		for k := 0; k < overs; k++ {
			v := float32(1.5)
			if k%2 == 1 {
				v = -1.5
			}
			chans[c][100+k*37] = v
		}
	}
	return chans
}

// IntersampleHotChans is the fixture for the clip count's structural gap:
// a quarter-rate stereo sine phased off its crests, stored peak about
// 0.85, true peak 1.2. Resampling would turn the between-sample overs
// into stored ones and fire the count instead.
func IntersampleHotChans(rate, frames int) [][]float32 {
	freq := float64(rate) / 4
	chans := make([][]float32, 2)
	for c := range chans {
		chans[c] = make([]float32, frames)
		for i := range chans[c] {
			chans[c][i] = float32(1.2 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)+math.Pi/4))
		}
	}
	return chans
}

// PCM16WAVBytes renders planar float channels as an interleaved 16-bit PCM
// WAV in memory, samples rounded at full scale and clamped to the int16
// range: the way a fixture enters the engine as integers, a copy path no
// float source can reach.
func PCM16WAVBytes(t *testing.T, rate int, chans [][]float32) []byte {
	t.Helper()
	if len(chans) == 0 || len(chans[0]) == 0 {
		t.Fatalf("PCM16WAVBytes: no samples")
	}
	n, ch := len(chans[0]), len(chans)
	for c, s := range chans {
		if len(s) != n {
			t.Fatalf("PCM16WAVBytes: channel %d has %d samples, channel 0 has %d", c, len(s), n)
		}
	}
	data := 2 * n * ch
	buf := make([]byte, 44+data)
	le := binary.LittleEndian
	copy(buf[0:], "RIFF")
	le.PutUint32(buf[4:], uint32(36+data))
	copy(buf[8:], "WAVEfmt ")
	le.PutUint32(buf[16:], 16)
	le.PutUint16(buf[20:], 1) // WAVE_FORMAT_PCM
	le.PutUint16(buf[22:], uint16(ch))
	le.PutUint32(buf[24:], uint32(rate))
	le.PutUint32(buf[28:], uint32(2*rate*ch))
	le.PutUint16(buf[32:], uint16(2*ch))
	le.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	le.PutUint32(buf[40:], uint32(data))
	off := 44
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			v := math.RoundToEven(float64(chans[c][i]) * 32768)
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			le.PutUint16(buf[off:], uint16(int16(v)))
			off += 2
		}
	}
	return buf
}

// FloatWAVBytes renders planar channels as an interleaved IEEE float32 WAV in
// memory, for the callers that feed a container.BytesSource and have no use
// for a file on disk.
func FloatWAVBytes(t *testing.T, rate int, chans [][]float32) []byte {
	t.Helper()
	if len(chans) == 0 || len(chans[0]) == 0 {
		t.Fatalf("FloatWAVBytes: no samples")
	}
	n, ch := len(chans[0]), len(chans)
	for c, s := range chans {
		if len(s) != n {
			t.Fatalf("FloatWAVBytes: channel %d has %d samples, channel 0 has %d", c, len(s), n)
		}
	}
	data := 4 * n * ch
	buf := make([]byte, 44+data)
	le := binary.LittleEndian
	copy(buf[0:], "RIFF")
	le.PutUint32(buf[4:], uint32(36+data))
	copy(buf[8:], "WAVEfmt ")
	le.PutUint32(buf[16:], 16)
	le.PutUint16(buf[20:], 3) // WAVE_FORMAT_IEEE_FLOAT
	le.PutUint16(buf[22:], uint16(ch))
	le.PutUint32(buf[24:], uint32(rate))
	le.PutUint32(buf[28:], uint32(4*rate*ch))
	le.PutUint16(buf[32:], uint16(4*ch))
	le.PutUint16(buf[34:], 32)
	copy(buf[36:], "data")
	le.PutUint32(buf[40:], uint32(data))
	off := 44
	for i := 0; i < n; i++ {
		for c := 0; c < ch; c++ {
			le.PutUint32(buf[off:], math.Float32bits(chans[c][i]))
			off += 4
		}
	}
	return buf
}
