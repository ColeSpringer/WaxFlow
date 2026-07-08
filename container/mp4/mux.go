package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/alac"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/waxerr"
)

var _ container.Muxer = (*Muxer)(nil)

// trackID is the single audio track's ID in the produced movie.
const trackID = 1

// defaultFragmentSeconds is the target fragment length. Progressive fMP4
// carries samples in self-contained moof+mdat fragments, so shorter
// fragments lower time-to-first-audio at the cost of a little per-fragment
// box overhead; ~1 second balances the two.
const defaultFragmentSeconds = 1

// MuxerOptions configures the fragmented writer.
type MuxerOptions struct {
	// FragmentSamples is the target sample count per fragment. Zero selects
	// roughly defaultFragmentSeconds of audio at the track rate. A fragment
	// closes once it reaches the target (the final one may be shorter).
	FragmentSamples int
}

// Muxer writes one audio track as a progressive fragmented MP4 (fMP4): an
// ftyp+moov init header declaring an empty sample table plus a movie-extends
// (mvex) box, then a moof+mdat fragment per bounded run of samples. Nothing
// is back-patched, so NeedsSeek reports false and a plain io.Writer streams
// live. The design is the CMAF/DASH shape HLS segments (a later milestone)
// reuse.
//
// v1 carries ALAC only (the first fMP4 encoder). ALAC is lossless and
// signals no gapless trims, so a nonzero Delay/Padding is rejected rather
// than silently dropped; the edit-list path arrives with the lossy fMP4
// codecs.
type Muxer struct {
	w io.Writer

	rate     int
	fragTgt  int
	began    bool
	ended    bool
	seq      uint32 // next moof sequence number (1-based)
	baseTime int64  // decode time (in samples) of the current fragment's first sample

	// current fragment accumulator.
	fragSamples int
	fragData    []byte
	durs        []uint32
	sizes       []uint32
}

// NewMuxer returns a fragmented MP4 muxer writing to w.
func NewMuxer(w io.Writer, opts *MuxerOptions) *Muxer {
	m := &Muxer{w: w, seq: 1}
	if opts != nil {
		m.fragTgt = opts.FragmentSamples
	}
	return m
}

// NeedsSeek reports false: fragmented MP4 has a compliant streaming form.
func (m *Muxer) NeedsSeek() bool { return false }

// Begin validates the track and writes the ftyp and moov init header.
func (m *Muxer) Begin(tracks []container.Track) error {
	if m.began {
		return waxerr.New(waxerr.CodeInternal, "mp4: Begin called twice")
	}
	if len(tracks) != 1 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp4: muxers are single-track, got %d", len(tracks)))
	}
	t := tracks[0]
	if t.Codec != codec.ALAC {
		return waxerr.New(waxerr.CodeUnsupportedFormat, fmt.Sprintf("mp4: cannot mux codec %q (only alac)", t.Codec))
	}
	if t.Delay != 0 || t.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: ALAC signals no gapless trims (lossless streams have none)")
	}
	if err := t.Fmt.Valid(); err != nil {
		return err
	}
	cfg, err := alac.ParseMagicCookie(t.CodecConfig)
	if err != nil {
		return err
	}
	// Compare the fields the cookie constrains, not the whole Format: the
	// cookie has no channel-layout field, so cfg.Format always reports the
	// default layout, but the chain may deliver a non-default mask (a WAV
	// EXTENSIBLE stereo file, say) that ALAC codes in bitstream order all the
	// same. Requiring an exact Layout match would reject a track the plan and
	// the encoder both accepted.
	if want := cfg.Format(); t.Fmt.Rate != want.Rate || t.Fmt.Channels != want.Channels ||
		t.Fmt.Type != want.Type || t.Fmt.BitDepth != want.BitDepth {
		return waxerr.New(waxerr.CodeUnsupportedFormat,
			fmt.Sprintf("mp4: track format %v does not match the ALAC cookie (%v)", t.Fmt, want))
	}

	m.rate = t.Fmt.Rate
	if m.fragTgt <= 0 {
		m.fragTgt = defaultFragmentSeconds * m.rate
	}
	m.began = true

	if err := m.write(ftypBox()); err != nil {
		return err
	}
	return m.write(moovBox(t.Fmt, cfg.Cookie))
}

// WritePacket appends one ALAC frame to the current fragment, flushing when
// the fragment reaches its target length.
func (m *Muxer) WritePacket(pkt container.Packet) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: WritePacket outside Begin/End")
	}
	if pkt.Track != 0 {
		return waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("mp4: no track %d", pkt.Track))
	}
	if len(pkt.Data) == 0 || pkt.Dur <= 0 {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet of %d bytes, %d samples", len(pkt.Data), pkt.Dur))
	}
	if len(pkt.Data) > maxSampleBytes || pkt.Dur > maxSampleDur {
		return waxerr.New(waxerr.CodeInternal, fmt.Sprintf("mp4: packet too large (%d bytes, %d samples)", len(pkt.Data), pkt.Dur))
	}

	m.fragData = append(m.fragData, pkt.Data...)
	m.durs = append(m.durs, uint32(pkt.Dur))
	m.sizes = append(m.sizes, uint32(len(pkt.Data)))
	m.fragSamples += int(pkt.Dur)

	// Flush on the sample target, or on the byte cap so a fragment never grows
	// past what a 32-bit box size can hold (the sample target is derived from
	// the rate, which a crafted cookie could inflate; the byte cap bounds
	// memory and box size regardless).
	if m.fragSamples >= m.fragTgt || len(m.fragData) >= maxFragmentBytes {
		return m.flush()
	}
	return nil
}

// End flushes the final fragment. ALAC carries no trailing gapless padding,
// so a nonzero trailer trim is rejected.
func (m *Muxer) End(trailer codec.Trailer) error {
	if !m.began || m.ended {
		return waxerr.New(waxerr.CodeInternal, "mp4: End outside Begin")
	}
	m.ended = true
	if trailer.Delay != 0 || trailer.Padding != 0 {
		return waxerr.New(waxerr.CodeUnsupportedFormat, "mp4: ALAC signals no gapless trims")
	}
	if len(m.durs) > 0 {
		return m.flush()
	}
	return nil
}

// flush writes the accumulated fragment as a moof+mdat pair and resets the
// accumulator for the next one.
func (m *Muxer) flush() error {
	frag := fragmentBoxes(m.seq, m.baseTime, m.durs, m.sizes, m.fragData)
	if err := m.write(frag); err != nil {
		return err
	}
	m.seq++
	m.baseTime += int64(m.fragSamples)
	m.fragSamples = 0
	m.fragData = m.fragData[:0]
	m.durs = m.durs[:0]
	m.sizes = m.sizes[:0]
	return nil
}

func (m *Muxer) write(b []byte) error {
	if _, err := m.w.Write(b); err != nil {
		return waxerr.Wrap(waxerr.CodeOutputUnwritable, "mp4: write", err)
	}
	return nil
}

// Fragment field caps: a single ALAC frame stays well under these, so a
// value past them is a wiring bug, not a legal stream.
const (
	maxSampleBytes = 1 << 24
	maxSampleDur   = 1 << 20
	// maxFragmentBytes caps one fragment's mdat payload, a backstop that keeps
	// every box size inside the 32-bit box-length field even if the sample
	// target (rate-derived) is absurd. Normal ~1 s fragments stay far under it.
	maxFragmentBytes = 8 << 20
)

// ftypBox declares an ISO base media file compatible with the M4A audio and
// fragmentation (iso5) brands.
func ftypBox() []byte {
	return makeBox("ftyp",
		[]byte("M4A "), u32(0),
		[]byte("M4A "), []byte("mp42"), []byte("isom"), []byte("iso2"), []byte("iso5"))
}

// moovBox builds the movie header for a fragmented file: a track with an
// empty sample table and a movie-extends box declaring the fragment
// defaults. The media timescale is the sample rate, so decode times are
// sample counts.
func moovBox(f audio.Format, cookie []byte) []byte {
	rate := uint32(f.Rate)
	mvhd := makeFullBox("mvhd", 0, 0,
		u32(0), u32(0), // creation, modification time
		u32(rate),                                            // timescale
		u32(0),                                               // duration (0: fragmented, from moof)
		u32(0x00010000), u16(0x0100), u16(0), u32(0), u32(0), // rate, volume, reserved
		identityMatrix(),
		make([]byte, 24), // pre_defined
		u32(trackID+1))   // next_track_ID
	trak := trakBox(f, cookie)
	mvex := makeBox("mvex", trexBox())
	return makeBox("moov", mvhd, trak, mvex)
}

func trakBox(f audio.Format, cookie []byte) []byte {
	tkhd := makeFullBox("tkhd", 0, 0x000007, // track enabled, in movie, in preview
		u32(0), u32(0), // creation, modification
		u32(trackID),
		u32(0),         // reserved
		u32(0),         // duration (fragmented)
		u32(0), u32(0), // reserved
		u16(0), u16(0), // layer, alternate_group
		u16(0x0100), u16(0), // volume (audio), reserved
		identityMatrix(),
		u32(0), u32(0)) // width, height (audio: 0)
	mdia := mdiaBox(f, cookie)
	return makeBox("trak", tkhd, mdia)
}

func mdiaBox(f audio.Format, cookie []byte) []byte {
	rate := uint32(f.Rate)
	mdhd := makeFullBox("mdhd", 0, 0,
		u32(0), u32(0), // creation, modification
		u32(rate),           // timescale
		u32(0),              // duration (fragmented)
		u16(0x55C4), u16(0)) // language 'und', pre_defined
	hdlr := makeFullBox("hdlr", 0, 0,
		u32(0),                 // pre_defined
		[]byte("soun"),         // handler_type
		u32(0), u32(0), u32(0), // reserved
		append([]byte("SoundHandler"), 0)) // name
	minf := minfBox(f, cookie)
	return makeBox("mdia", mdhd, hdlr, minf)
}

func minfBox(f audio.Format, cookie []byte) []byte {
	smhd := makeFullBox("smhd", 0, 0, u16(0), u16(0)) // balance, reserved
	dref := makeFullBox("dref", 0, 0, u32(1),
		makeFullBox("url ", 0, 0x000001)) // self-contained
	dinf := makeBox("dinf", dref)
	stbl := stblBox(f, cookie)
	return makeBox("minf", smhd, dinf, stbl)
}

// stblBox holds the sample description plus empty timing/size/offset tables:
// a fragmented file carries no samples in moov.
func stblBox(f audio.Format, cookie []byte) []byte {
	stsd := makeFullBox("stsd", 0, 0, u32(1), alacSampleEntry(f, cookie))
	stts := makeFullBox("stts", 0, 0, u32(0))
	stsc := makeFullBox("stsc", 0, 0, u32(0))
	stsz := makeFullBox("stsz", 0, 0, u32(0), u32(0)) // sample_size, sample_count
	stco := makeFullBox("stco", 0, 0, u32(0))
	return makeBox("stbl", stsd, stts, stsc, stsz, stco)
}

// alacSampleEntry builds the 'alac' AudioSampleEntry wrapping the magic
// cookie. The cookie is authoritative for rate and depth; the legacy 16.16
// sample-rate field and samplesize are cosmetic (our demuxer and ffmpeg both
// read the cookie), so samplesize stays 16 and the rate is written best
// effort.
func alacSampleEntry(f audio.Format, cookie []byte) []byte {
	inner := makeFullBox("alac", 0, 0, cookie)
	// The 16.16 fixed-point field holds only a 16-bit integer part, so rates
	// at or above 65536 (96/176.4/192 kHz hi-res ALAC) are clamped rather
	// than left to wrap into the fractional bits and read back as garbage.
	// The cookie carries the true rate and is authoritative.
	sampleRate := uint32(f.Rate)
	if sampleRate > 0xFFFF {
		sampleRate = 0xFFFF
	}
	// makeBox concatenates its parts, so the AudioSampleEntry header fields go
	// straight in ahead of the child cookie box, no intermediate slice.
	return makeBox("alac",
		make([]byte, 6), u16(1), // reserved, data_reference_index
		u16(0), u16(0), u32(0), // version, revision, vendor
		u16(uint16(f.Channels)), u16(16), // channelcount, samplesize
		u16(0), u16(0), // compressionID, packetsize
		u32(sampleRate<<16), // samplerate 16.16
		inner)
}

// trexBox declares the fragment defaults: one sample description, the ALAC
// frame length as the default duration, and sync-sample flags (every ALAC
// frame is independently decodable). trun overrides duration/size per
// sample, so these are only fallbacks.
func trexBox() []byte {
	return makeFullBox("trex", 0, 0,
		u32(trackID),
		u32(1),               // default_sample_description_index
		u32(alac.FrameSize),  // default_sample_duration
		u32(0),               // default_sample_size
		u32(syncSampleFlags)) // default_sample_flags
}

// syncSampleFlags marks a sample as independently decodable: sample_depends_on
// = 2 (depends on nothing) and sample_is_non_sync_sample = 0.
const syncSampleFlags = 0x02000000

// fragmentBoxes assembles one moof+mdat pair. The trun data offset is
// resolved to point at the mdat payload once the moof size is known.
func fragmentBoxes(seq uint32, baseTime int64, durs, sizes []uint32, data []byte) []byte {
	n := len(durs)

	mfhd := makeFullBox("mfhd", 0, 0, u32(seq))
	tfhd := makeFullBox("tfhd", 0, 0x020000, u32(trackID)) // default-base-is-moof
	tfdt := makeFullBox("tfdt", 1, 0, u64(uint64(baseTime)))

	// trun body: version(1) flags(3) sample_count(4) data_offset(4) then
	// per-sample duration(4)+size(4).
	body := make([]byte, 0, 12+8*n)
	body = append(body, 0)                                // version
	body = append(body, 0x00, 0x03, 0x01)                 // flags: data-offset, sample-duration, sample-size
	body = binary.BigEndian.AppendUint32(body, uint32(n)) // sample_count
	dataOffPos := len(body)
	body = binary.BigEndian.AppendUint32(body, 0) // data_offset placeholder
	for i := 0; i < n; i++ {
		// Append directly into the preallocated body rather than through u32,
		// whose throwaway 4-byte slices would allocate twice per sample.
		body = binary.BigEndian.AppendUint32(body, durs[i])
		body = binary.BigEndian.AppendUint32(body, sizes[i])
	}
	trun := makeBox("trun", body)

	traf := makeBox("traf", tfhd, tfdt, trun)
	moof := makeBox("moof", mfhd, traf)

	// data_offset counts from the moof start (default-base-is-moof) to the
	// first mdat payload byte: the whole moof plus the 8-byte mdat header. The
	// trun is moof's last box, so its data_offset field lands at the moof end
	// minus the trun size, past the trun's 8-byte header and body prefix.
	dataOffset := uint32(len(moof) + 8)
	patchAt := len(moof) - len(trun) + 8 + dataOffPos
	binary.BigEndian.PutUint32(moof[patchAt:], dataOffset)

	mdat := makeBox("mdat", data)
	return append(moof, mdat...)
}

// identityMatrix is the 3x3 unity transform matrix boxes carry.
func identityMatrix() []byte {
	return bytes.Join([][]byte{
		u32(0x00010000), u32(0), u32(0),
		u32(0), u32(0x00010000), u32(0),
		u32(0), u32(0), u32(0x40000000),
	}, nil)
}
