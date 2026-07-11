package waxflow

import (
	"context"
	"io"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/dsp"
	"github.com/colespringer/waxflow/dsp/loudness"
	"github.com/colespringer/waxflow/waxerr"
)

// AnalyzeOptions configures Engine.Analyze.
type AnalyzeOptions struct {
	// Progress, when non-nil, is called after each decoded chunk with the
	// samples measured so far and the projected total (-1 unknown). It
	// runs on the analyzing goroutine, so blocking it pauses the
	// analysis; the job runner's yield-to-live-streams check rides on
	// exactly that.
	Progress func(done, total int64)
}

// AnalyzeResult is a full-stream loudness measurement of the decoded
// audio per ITU-R BS.1770-4 and EBU R128.
type AnalyzeResult struct {
	// Format is the PCM format the measurement ran on: the source's rate
	// and layout in the float domain.
	Format audio.Format
	// Samples is the number of frames measured.
	Samples int64
	// IntegratedLUFS is the gated integrated loudness. Silence that
	// never passes the absolute gate reports math.Inf(-1).
	IntegratedLUFS float64
	// LoudnessRange is the EBU Tech 3342 loudness range in LU.
	LoudnessRange float64
	// TruePeakDB is the maximum oversampled true peak in dBTP,
	// math.Inf(-1) for silence.
	TruePeakDB float64
	// SamplePeakDB is the maximum sample magnitude in dBFS, math.Inf(-1)
	// for silence.
	SamplePeakDB float64
}

// Analyze decodes src end to end and measures its loudness: integrated
// LUFS, loudness range, true peak, and sample peak. It powers the
// type:analyze job and the loudness:analyze two-pass transcode (the R128
// half of the loudness design: live streams stay tag-based, exact
// measurement belongs to jobs, where a second pass is affordable).
func (e *Engine) Analyze(ctx context.Context, src container.Source, hint string, opts AnalyzeOptions) (*AnalyzeResult, error) {
	med, err := e.OpenStream(src, hint)
	if err != nil {
		return nil, err
	}
	defer med.Close()

	track := med.Info().Default()
	// The chain only converts to float here (no resample, no mix): the
	// meter is rate-aware and weighs channels itself, so measurement runs
	// on the source's own timeline.
	chain, err := dsp.NewChain(dsp.NewSource(med, track.Fmt), dsp.ChainSpec{Float: true})
	if err != nil {
		return nil, err
	}
	defer chain.Release()

	f := chain.Format()
	meter, err := loudness.NewMeter(f.Rate, f.Channels, f.Layout)
	if err != nil {
		return nil, err
	}
	buf := audio.Get(f, audio.StandardChunk)
	defer audio.Put(buf)
	chans := make([][]float32, f.Channels)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeCanceled, "analyze canceled", err)
		}
		err := chain.ReadChunk(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for c := range chans {
			chans[c] = buf.ChanF(c)
		}
		if err := meter.Process(chans); err != nil {
			return nil, err
		}
		done += int64(buf.N)
		if opts.Progress != nil {
			opts.Progress(done, track.Samples)
		}
	}
	meter.Flush()
	return &AnalyzeResult{
		Format:         f,
		Samples:        done,
		IntegratedLUFS: meter.Integrated(),
		LoudnessRange:  meter.Range(),
		TruePeakDB:     meter.TruePeak(),
		SamplePeakDB:   meter.SamplePeak(),
	}, nil
}
