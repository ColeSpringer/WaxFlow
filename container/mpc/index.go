package mpc

import (
	"encoding/binary"

	"github.com/colespringer/waxflow/codec/musepack"
)

// Index snapshot format: magic, the stream version, the stream's identity
// (magic offset, data end, packet total), then the scanned decoder states in
// order. Packet positions are not stored: the open-time walk rebuilds them
// cheaply, and what a seek pays for is the state scan behind them.
const idxMagic = "WXMPCIDX1\x00"

// idxProbes is how many restored intervals get re-derived from the stream
// beyond the first.
const idxProbes = 8

// idxMinStates is the snapshot threshold: below this many scanned states the
// rescan costs less than a disk round trip. A variable so a test can lower it.
var idxMinStates = 64

// IndexSnapshot implements container.Indexer for the seek scanner's cache.
func (d *Demuxer) IndexSnapshot() []byte {
	var states []musepack.State
	var grew bool
	stateLen := musepack.StateLenSV8
	if d.cfg.StreamVersion == 7 {
		states, grew = d.sv7.states, d.sv7.grew
		stateLen = musepack.StateLenSV7
	} else {
		if d.cfg.PNS == 0 {
			return nil // nothing to scan: the generator is never drawn from
		}
		grew = d.sv8.grew
		states = make([]musepack.State, len(d.sv8.rng))
		for i, rng := range d.sv8.rng {
			states[i].RNG = rng
		}
	}
	if len(states) < idxMinStates || !grew {
		return nil
	}
	buf := make([]byte, 0, len(idxMagic)+1+8*3+4+len(states)*stateLen)
	buf = append(buf, idxMagic...)
	buf = append(buf, byte(d.cfg.StreamVersion))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(d.base))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(d.w.DataEnd()))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(d.total))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(states)))
	for i := range states {
		buf = states[i].AppendBinary(buf, d.cfg.StreamVersion)
	}
	return buf
}

// RestoreIndex implements container.Indexer. The blob is untrusted: its
// identity fields must match the open stream, its first state must be the
// stream-start state, and its second is re-derived from the stream and
// compared, so a blob for another file or a changed one is rejected and the
// demuxer scans. Restoring over a scan already begun is refused, since it
// would move the state a later seek reads.
func (d *Demuxer) RestoreIndex(blob []byte) bool {
	if len(blob) < len(idxMagic)+1+8*3+4 || string(blob[:len(idxMagic)]) != idxMagic {
		return false
	}
	rest := blob[len(idxMagic):]
	if int(rest[0]) != d.cfg.StreamVersion {
		return false
	}
	rest = rest[1:]
	base := int64(binary.LittleEndian.Uint64(rest))
	end := int64(binary.LittleEndian.Uint64(rest[8:]))
	total := int64(binary.LittleEndian.Uint64(rest[16:]))
	count := int(binary.LittleEndian.Uint32(rest[24:]))
	rest = rest[28:]
	if base != d.base || end != d.w.DataEnd() || total != d.total || count < 2 {
		return false
	}
	stateLen := musepack.StateLenSV8
	if d.cfg.StreamVersion == 7 {
		stateLen = musepack.StateLenSV7
		if int64(count) > d.total/checkpointFrames+1 || len(d.sv7.states) > 1 {
			return false
		}
	} else {
		if d.cfg.PNS == 0 || int64(count) > d.total || len(d.sv8.rng) > 1 {
			return false
		}
	}
	if len(rest) != count*stateLen {
		return false
	}
	states := make([]musepack.State, count)
	for i := range states {
		st, err := musepack.ParseState(rest[i*stateLen:(i+1)*stateLen], d.cfg.StreamVersion)
		if err != nil {
			return false
		}
		states[i] = st
	}
	if states[0] != musepack.InitialState() {
		return false
	}
	// Re-derive a spread of intervals from the stream, the endpoints
	// included: each costs one scan interval, and together they are what
	// says the blob is this file's and undamaged, short of the full scan the
	// sidecar exists to avoid.
	for i := 0; i <= idxProbes; i++ {
		k := int64(i) * int64(count-2) / idxProbes
		st := states[k]
		var err error
		if d.cfg.StreamVersion == 7 {
			err = d.sv7Advance(&st, k*checkpointFrames, (k+1)*checkpointFrames)
		} else {
			err = d.sv8Advance(&st, k)
		}
		if err != nil || st != states[k+1] {
			return false
		}
	}
	if d.cfg.StreamVersion == 7 {
		d.sv7.states = states
		d.sv7.scanned = int64(count-1) * checkpointFrames
		d.sv7.scanState = states[count-1]
		d.sv7.grew = false
		return true
	}
	d.sv8.rng = make([][2]uint32, len(states))
	for i := range states {
		d.sv8.rng[i] = states[i].RNG
	}
	d.sv8.grew = false
	return true
}
