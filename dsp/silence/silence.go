// Package silence maps the near-silent spans of a stream: the detector
// behind the analyze job's silence half, which a library manager reads to
// trim leading and trailing pauses or to propose track boundaries.
//
// Detection matches ffmpeg's silencedetect deliberately and exactly, which
// is what lets the differential oracle in this package's tests assert span
// counts rather than approximate them. A frame is silent when every
// channel's magnitude is strictly inside the threshold band; a span is a
// maximal run of silent frames; a run shorter than the minimum duration is
// discarded. There is no hysteresis and no smoothing, which is a decision
// rather than an omission: either would diverge from the oracle, and the
// oracle is what makes this detector trustworthy. The parameter that
// absorbs a noisy floor is the threshold, which the caller sets because
// only the caller knows the content. See New for the guidance, and Dropped
// for the number that says the threshold is wrong.
//
// Like loudness.Meter, a Detector is a pure streaming analyzer over planar
// float32 PCM rather than a pipeline Stage: it consumes chunks and produces
// only numbers, so one decode can feed several analyzers at once. A
// Detector is not safe for concurrent use.
package silence

import (
	"fmt"
	"math"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// Version identifies the detector algorithm revision (ADR-0004 style).
// WaxFlow recomputes the map fresh per job and does not cache it, but a
// caller that persists one (WaxDeck's is the anticipated case) should
// store this alongside so a detector revision invalidates it.
const Version = "silence-1"

// Span is one detected silence, in frames on the analyzed stream's own
// timeline (ADR-0006). To is exclusive, so To-From is the length.
type Span struct {
	From int64
	To   int64
}

// Len returns the span's length in frames.
func (s Span) Len() int64 { return s.To - s.From }

// Detector finds the silence spans of one stream. Feed planar chunks to
// Process, call Flush after the last one, then read the results.
type Detector struct {
	channels int
	thresh   float32
	minLen   int64

	pos     int64 // absolute frame position of the next sample
	inRun   bool
	runFrom int64 // first frame of the open run

	spans        []Span
	dropped      int
	droppedFrame int64
	total        int64
	flushed      bool
}

// New returns a detector for one stream. thresholdDB is the silence
// threshold in dBFS and must be negative and finite; minDuration is the
// shortest span worth reporting and must be positive. Those are the only
// bounds here, because they are the only ones the detector needs to be
// correct; a caller with policy of its own (the daemon has) applies it at
// its own boundary.
//
// This is the canonical guidance for the threshold, which analyze.go and
// docs/api.md restate rather than reinterpret.
//
// The threshold is the parameter that matters, and the right value is a
// property of the content rather than of the detector, because what counts
// as silence is wherever the source's own noise floor sits:
//
//   - a studio podcast, or any clean digital capture, near -80 dBFS
//   - an audiobook's room tone near -55 dBFS
//   - an analog or vinyl transfer near -40 dBFS, its floor being far higher
//
// Setting it below a source's floor does not fail cleanly: it reports no
// silence at all. See DroppedSamples, which is how that shows up, and note
// that it is DroppedSamples and not Dropped: the bare count is large for
// healthy audio too.
func New(rate, channels int, thresholdDB float64, minDuration time.Duration) (*Detector, error) {
	if rate <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence: detector rate %d must be positive", rate))
	}
	if channels <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence: detector channel count %d must be positive", channels))
	}
	// Written to fail NaN as well as out-of-range values: a non-finite
	// threshold would otherwise convert to a NaN comparison bound, against
	// which every frame reads as loud and the map comes back empty.
	if !(thresholdDB < 0 && thresholdDB > math.Inf(-1)) {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence: threshold %g dBFS must be negative and finite", thresholdDB))
	}
	if minDuration <= 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence: minimum duration %v must be positive", minDuration))
	}
	return &Detector{
		channels: channels,
		thresh:   float32(math.Pow(10, thresholdDB/20)),
		minLen:   minFrames(minDuration, rate),
	}, nil
}

// minFrames is the shortest reportable span in frames: minDuration * rate,
// rounded up, in exact integer arithmetic.
//
// Exact and integer for a reason that is easy to dismiss as pedantry and is
// not. float64 cannot represent most durations: 700 ms at 44100 Hz is
// exactly 30870 frames, but (700*time.Millisecond).Seconds() is a hair
// under 0.7, so the float product lands on 30869.999999999998 and truncates
// to 30869, one frame short. The bound is then a frame looser than asked
// for, and a span shorter than the minimum gets reported.
//
// Rounded up, not truncated, because that is what makes "a run shorter than
// MinDuration is discarded" literally true when the product is not a whole
// number of frames: at 333 ms and 44100 Hz the exact bound is 14685.3
// frames, so 14685 frames (332.99 ms) must drop and only 14686 can be kept.
// ffmpeg's own conversion rounds to nearest instead, so the two can differ
// by one frame of threshold, but only for a span sitting within a frame of
// the bound; the differential's fixtures are nowhere near it.
//
// The split by whole seconds is what keeps the multiply exact rather than
// overflowing: a time.Duration reaches 9.2e18 ns, which times any sample
// rate does not fit in an int64.
func minFrames(minDuration time.Duration, rate int) int64 {
	whole := int64(minDuration / time.Second)
	rem := int64(minDuration % time.Second)
	// Rounding up guarantees at least one frame for any positive duration,
	// so a sub-frame minimum needs no separate floor.
	return whole*int64(rate) + (rem*int64(rate)+int64(time.Second)-1)/int64(time.Second)
}

// Process consumes one chunk of planar float32 PCM: chans[c][i] is sample
// i of channel c. All channel slices must be the same length. Values are
// nominal full scale +-1.0.
func (d *Detector) Process(chans [][]float32) error {
	if d.flushed {
		return waxerr.New(waxerr.CodeInvalidRequest, "silence: Process after Flush")
	}
	if len(chans) != d.channels {
		return waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("silence: chunk has %d channels, detector expects %d", len(chans), d.channels))
	}
	n := len(chans[0])
	for _, ch := range chans[1:] {
		if len(ch) != n {
			return waxerr.New(waxerr.CodeInvalidRequest, "silence: channel slices differ in length")
		}
	}
	for i := 0; i < n; i++ {
		var peak float32
		for c := range chans {
			v := chans[c][i]
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		// The strict comparison is silencedetect's own (|x| < threshold on
		// every channel), and matching it is what keeps the differential
		// able to assert exact span boundaries.
		if peak < d.thresh {
			if !d.inRun {
				d.inRun = true
				d.runFrom = d.pos + int64(i)
			}
		} else if d.inRun {
			d.close(d.pos + int64(i))
		}
	}
	d.pos += int64(n)
	return nil
}

// close ends the open run at to (exclusive), keeping it only if it reaches
// the minimum length and counting it as dropped otherwise.
func (d *Detector) close(to int64) {
	d.inRun = false
	if to-d.runFrom < d.minLen {
		d.dropped++
		d.droppedFrame += to - d.runFrom
		return
	}
	d.spans = append(d.spans, Span{From: d.runFrom, To: to})
	d.total += to - d.runFrom
}

// Flush ends the stream, closing a span that runs to the last sample (the
// trailing-silence case, which silencedetect reports the same way). The
// results are complete only after Flush; a second call is a no-op.
func (d *Detector) Flush() {
	if d.flushed {
		return
	}
	d.flushed = true
	if d.inRun {
		d.close(d.pos)
	}
}

// Spans returns the detected silences in stream order. The slice is the
// detector's own; callers that retain it past further Process calls must
// copy it.
func (d *Detector) Spans() []Span { return d.spans }

// Dropped counts the runs discarded for falling short of the minimum
// duration.
//
// Read it with DroppedSamples, never alone. Ordinary audio crosses zero
// hundreds of times a second, and each crossing dips under any threshold
// for a fraction of a sample, which opens and closes a one-sample run: a
// clean -6 dBFS tone with no silence whatever drops ~240 runs per second
// at a -50 dBFS threshold. So a large count is the normal state of healthy
// audio and diagnoses nothing by itself. DroppedSamples is the number that
// separates the cases.
func (d *Detector) Dropped() int { return d.dropped }

// DroppedSamples is the summed length of the dropped runs, in frames, and
// it is the diagnostic a caller cannot otherwise compute.
//
// Against the stream's own length it says how much of the source sat below
// the threshold without ever staying there long enough to report. Zero
// crossings contribute a fraction of a percent, so a healthy source reads
// near zero however large Dropped grows. A figure that is instead a
// sizeable share of the stream is the signature of a noise floor sitting
// near the threshold: it crosses repeatedly, fragmenting one long silence
// into many short ones that are each then discarded, which reports a
// plainly quiet source as having no silence at all. That means the
// threshold is wrong for this source, not that the source is loud.
func (d *Detector) DroppedSamples() int64 { return d.droppedFrame }

// TotalSamples is the summed length of the kept spans, in frames.
func (d *Detector) TotalSamples() int64 { return d.total }

// Samples is the number of frames the detector has consumed.
func (d *Detector) Samples() int64 { return d.pos }
