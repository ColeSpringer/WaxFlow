package musepack

import "encoding/binary"

// State is the cross-packet decoder state a seek landing restores: the last
// scalefactor of every band and channel (which SV7 frames code deltas against
// forever) and the noise generator's two words (which neither version ever
// resets). An SV8 block starts with a key frame, so only the generator matters
// there; the scalefactors ride along unused.
type State struct {
	SCF [2][MaxBands]int32
	RNG [2]uint32
}

// InitialState is the state at stream start.
func InitialState() State {
	p := newPRNG()
	return State{RNG: [2]uint32{p.r1, p.r2}}
}

// AppendBinary appends the state block for a packet of the given stream
// version: StateLenSV7 or StateLenSV8 bytes.
func (s *State) AppendBinary(b []byte, streamVersion int) []byte {
	if streamVersion == 7 {
		for ch := range 2 {
			for n := range MaxBands {
				b = binary.LittleEndian.AppendUint32(b, uint32(s.SCF[ch][n]))
			}
		}
	}
	b = binary.LittleEndian.AppendUint32(b, s.RNG[0])
	return binary.LittleEndian.AppendUint32(b, s.RNG[1])
}

// ParseState reads a state block back.
func ParseState(b []byte, streamVersion int) (State, error) {
	var s State
	want := StateLenSV8
	if streamVersion == 7 {
		want = StateLenSV7
	}
	if len(b) != want {
		return s, malformed("state block of %d bytes, want %d", len(b), want)
	}
	if streamVersion == 7 {
		for ch := range 2 {
			for n := range MaxBands {
				s.SCF[ch][n] = int32(binary.LittleEndian.Uint32(b))
				b = b[4:]
			}
		}
	}
	s.RNG[0] = binary.LittleEndian.Uint32(b)
	s.RNG[1] = binary.LittleEndian.Uint32(b[4:])
	return s, nil
}

// load installs a State into the frame state. Only the generator matters to
// SV8, whose blocks re-arm every scalefactor at their key frame.
func (f *frameState) load(s *State, streamVersion int) {
	if streamVersion == 7 {
		for ch := range 2 {
			for n := range MaxBands {
				f.scf[ch][n][2] = s.SCF[ch][n]
			}
		}
	}
	f.rng = prng{s.RNG[0], s.RNG[1]}
}

// store reads the State back out; an SV8 state carries the generator alone.
func (f *frameState) store(s *State, streamVersion int) {
	if streamVersion == 7 {
		for ch := range 2 {
			for n := range MaxBands {
				s.SCF[ch][n] = f.scf[ch][n][2]
			}
		}
	}
	s.RNG = [2]uint32{f.rng.r1, f.rng.r2}
}

// ScanSV7Frame advances st over one SV7 frame without decoding it: the frame's
// header (resolutions, scalefactors) is parsed and the samples are skipped by
// the frame's declared length, with the noise generator advanced by the 36
// draws every noise-coded band would have taken. It is how a container builds
// the state a seek landing needs, at a fraction of a decode.
func ScanSV7Frame(data []byte, bitLen int, cfg Config, st *State) error {
	if cfg.StreamVersion != 7 {
		return malformed("ScanSV7Frame on an SV%d stream", cfg.StreamVersion)
	}
	if bitLen <= 0 || uint64(bitLen) > uint64(len(data))*8 {
		return malformed("frame of %d bits in a %d-byte buffer", bitLen, len(data))
	}
	var f frameState
	f.load(st, 7)
	var r bitReader
	r.reset(data, bitLen)
	maxUsed, err := f.readSV7Header(&r, &cfg)
	if err != nil {
		return err
	}
	for n := 0; n < maxUsed; n++ {
		for ch := range 2 {
			if f.res[ch][n] == -1 {
				f.rng.advance(36)
			}
		}
	}
	f.store(st, 7)
	return nil
}

// ScanSV8Block advances st over one SV8 block of the given frame count. SV8
// frames carry no length, so the block is parsed in full (without
// requantisation or synthesis) and the same padding rule the decoder applies
// holds here.
func ScanSV8Block(data []byte, frames int, cfg Config, st *State) error {
	if cfg.StreamVersion != 8 {
		return malformed("ScanSV8Block on an SV%d stream", cfg.StreamVersion)
	}
	if frames < 1 || frames > cfg.FramesPerBlock() {
		return malformed("block of %d frames, want 1..%d", frames, cfg.FramesPerBlock())
	}
	var f frameState
	f.load(st, 8)
	var r bitReader
	r.reset(data, len(data)*8)
	for i := 0; i < frames; i++ {
		if err := f.readSV8(&r, &cfg, i == 0); err != nil {
			return err
		}
	}
	if rest := r.remaining(); rest >= 8 {
		return malformed("block has %d bits past its last frame, more than padding", rest)
	}
	f.store(st, 8)
	return nil
}

// CountSV8Frames parses a block whose frame count nothing states (a stream
// declaring no length) and returns how many frames it holds: frames are read
// until fewer than eight bits remain, which is the decoder's own padding rule,
// or the block's maximum is reached. A frame that fails to parse, or bits left
// past the maximum, is an error; the caller then falls back to a full block.
func CountSV8Frames(data []byte, cfg Config) (int, error) {
	if cfg.StreamVersion != 8 {
		return 0, malformed("CountSV8Frames on an SV%d stream", cfg.StreamVersion)
	}
	var f frameState
	f.init()
	var r bitReader
	r.reset(data, len(data)*8)
	n := 0
	for r.remaining() >= 8 && n < cfg.FramesPerBlock() {
		if err := f.readSV8(&r, &cfg, n == 0); err != nil {
			return 0, err
		}
		n++
	}
	if n == 0 {
		return 0, malformed("block holds no frame")
	}
	if rest := r.remaining(); rest >= 8 {
		return 0, malformed("block has %d bits past its last frame, more than padding", rest)
	}
	return n, nil
}
