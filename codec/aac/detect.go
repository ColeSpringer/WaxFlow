package aac

import "github.com/colespringer/waxflow/audio"

// Implicit-signalling detection for ADTS: the transport has no
// AudioSpecificConfig, so HE-AAC announces itself only by the SBR fill
// extensions riding inside the access units, and v2 only by the ps_data
// inside those. Elements are entropy-coded with no length prefix, so
// locating a fill takes the full syntax parse; DetectSBR runs a throwaway
// decoder with its output discarded and reports what the extension stream
// carried.

// DetectSBR probes an AAC-LC stream for implicit SBR and PS signalling.
// cfg is the stream's core config (what the transport headers imply);
// next supplies consecutive access units and returns nil when the caller
// is done feeding (it bounds the scan).
//
// The SBR fill rides in every access unit of an HE stream, so the first
// unit decides SBR, and a plain LC stream is settled after one unit. PS
// lives deeper: the ps_data sits behind sbr_data, which is parseable only
// once an sbr_header has arrived, and encoders repeat headers only every
// ten frames or so. The probe therefore keeps consuming units until the
// PS question is conclusive: ps_data seen (v2), or two units whose
// sbr_data parsed cleanly without one (v1; two rather than one guards the
// stream whose first data frame happens to omit its ps_data). A unit that
// fails to decode ends the scan with whatever the clean run established;
// only a failing FIRST unit is an error, and the caller falls back to the
// plain LC reading.
func DetectSBR(cfg Config, next func() []byte) (sbr, ps bool, err error) {
	probe := cfg
	probe.SBR = true
	probe.PS = false
	probe.ExtensionRate = 2 * probe.SampleRate
	f, err := probe.Format()
	if err != nil {
		return false, false, err
	}
	d, err := NewDecoder(probe, f)
	if err != nil {
		return false, false, err
	}
	defer d.Release()
	first := true
	psLess := 0
	for {
		au := next()
		if au == nil {
			break
		}
		if err := d.Decode(au, func(*audio.Buffer) error { return nil }); err != nil {
			if first {
				return false, false, err
			}
			break
		}
		first = false
		parsed := false
		for _, el := range d.sbrEls {
			if el == nil {
				continue
			}
			sbr = sbr || el.sawSBR
			ps = ps || el.sawPS
			parsed = parsed || (el.haveHdr && el.dataOK)
		}
		if parsed && !ps {
			psLess++
		}
		if !sbr || ps || psLess >= 2 {
			break
		}
	}
	// ps_data outside an SBR fill cannot occur; keep the pair coherent.
	ps = ps && sbr
	return sbr, ps, nil
}

// BuildHEASC synthesizes the explicit hierarchical AudioSpecificConfig
// for an HE-AAC stream: AOT 5 (or 29 with PS) at the core rate and
// channel configuration, the doubled extension rate, then the wrapped
// AAC-LC config with a zero GASpecificConfig. This is the form an esds
// carries, so a detected ADTS stream remuxes into MP4 with its identity
// intact. Both rates must be in the sampling-frequency table.
func BuildHEASC(coreRate, chanConfig int, ps bool) ([]byte, error) {
	coreIdx := samplingIndex(coreRate)
	extIdx := samplingIndex(2 * coreRate)
	if coreIdx < 0 || extIdx < 0 {
		return nil, malformed("no sampling-frequency index pair for a %d Hz core", coreRate)
	}
	if chanConfig < 1 || chanConfig > 6 {
		return nil, malformed("channel configuration %d has no HE-AAC form here", chanConfig)
	}
	aot := uint64(aotSBR)
	if ps {
		aot = aotPS
	}
	var bits uint64
	bits = aot
	bits = bits<<4 | uint64(coreIdx)
	bits = bits<<4 | uint64(chanConfig)
	bits = bits<<4 | uint64(extIdx)
	bits = bits<<5 | aotAACLC
	bits <<= 3 // GASpecificConfig: frameLength 1024, no core coder, no extension
	bits <<= 7 // pad the 25 bits to 4 bytes
	return []byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)}, nil
}
