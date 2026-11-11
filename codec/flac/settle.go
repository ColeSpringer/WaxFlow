package flac

import "fmt"

// Settled is a FLAC reader's track length, settled against its frames.
type Settled struct {
	// Samples is the frames' span, or the declared count where no span was
	// found (-1 when that is 0 too, unknown rather than empty).
	Samples int64
	// Measured says the frames' span settled it.
	Measured bool
	// StreamInfo is the header to hand on: restated when its total disagrees
	// with the frames, and with no MD5 for a stream that starts past frame
	// 0, whose signature is the source's it was cut from.
	StreamInfo []byte
	// Notes are for the reader to note, and Damage, when set, to warn.
	Notes  []string
	Damage string
	// Truncated is how many samples the declared total claims past the end
	// of the frames, set with a Damage that says they end short of it.
	Truncated int64
}

// Settle settles a FLAC stream's length from raw, its STREAMINFO block body,
// origin, the first frame's position, and span, the frames' span from it, -1
// when no whole frame was found or the bytes could not hold it.
//
// STREAMINFO's total is the encoder's back-patched claim, which a file cut
// short afterwards still carries, so the frames decide. A shortfall is damage
// and an overrun a note, both adopting the span. A total of zero is no claim.
// One equal to where the frames end counted from sample 0 is the length of the
// source a stream-copy cut came from, which the cut keeps: a note too.
func Settle(raw []byte, origin, span int64) (Settled, error) {
	si, err := ParseStreamInfo(raw)
	if err != nil {
		return Settled{}, err
	}
	s := Settled{Samples: si.Samples, StreamInfo: raw}
	if origin > 0 {
		// RFC 9639 section 9.1.5 numbers the first frame 0, and a stream-copy
		// cut keeps its source's numbers. Every frame still checks and the
		// audio is whole, so it is a note.
		s.Notes = append(s.Notes, fmt.Sprintf("the first frame starts at sample %d, not 0; positions run from it", origin))
		if s.StreamInfo, err = ClearMD5(raw); err != nil {
			return Settled{}, err
		}
	}
	if span < 0 {
		if si.Samples == 0 {
			s.Samples = -1
		}
		return s, nil
	}
	s.Samples, s.Measured = span, true
	switch declared := si.Samples; {
	case declared == 0 || declared == span:
		return s, nil
	case origin > 0 && declared == origin+span:
		s.Notes = append(s.Notes, fmt.Sprintf(
			"STREAMINFO declares %d samples, its source's length: the frames run from sample %d to it, %d in all",
			declared, origin, span))
	case span < declared:
		s.Damage = fmt.Sprintf("STREAMINFO declares %d samples but the frames end at %d", declared, span)
		// The least the total can mean: its source's length where the frames
		// end inside one, else the stream's own.
		s.Truncated = declared - span
		if origin+span < declared {
			s.Truncated -= origin
		}
	default:
		s.Notes = append(s.Notes, fmt.Sprintf("STREAMINFO declares %d samples but the frames run to %d", declared, span))
	}
	s.StreamInfo, err = RestateStreamInfo(s.StreamInfo, span)
	return s, err
}
