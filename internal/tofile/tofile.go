// Package tofile writes one whole-file transcode, the pipeline the job runner
// and `waxflow transcode` share: metadata fold, loudness pass, tags, encode,
// ReplayGain, MP4 patch, sync, close and the tagging post-pass. Split jobs and
// `waxflow split` tag their pieces through Album and write them through
// WriteMedia, as merges do their output. Creating, replacing and removing the
// file, and wording what happened, stay the caller's.
package tofile

import (
	"context"
	"io"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/waxerr"
)

// NoteKind says what a Note reports; each front end words it itself.
type NoteKind int

const (
	NoteMetadataUnread  NoteKind = iota // Text and Err: the read's error
	NoteMetadataWarning                 // Text: the mapper's warning
	NoteTagTooLarge                     // Key: the tag dropped from the output
	NotePostPassFailed                  // Text and Err: the mapper's error
	NoteReadBackDamage                  // Text: what the output read back with
	NoteLoudnessSource                  // LUFS, GainDB
	NoteLoudnessOutput                  // LUFS, RG; Estimated when predicted or derived
	NoteInputDamage                     // Text: one finding of a source read
	NoteMetadataLint                    // Text: one of the mapper's notes on the source
)

// Note is one thing Run reports as it happens.
type Note struct {
	Kind      NoteKind
	Text, Key string
	// Err is the error behind Text, for a front end that must word it
	// without the paths it quotes.
	Err       error
	LUFS      float64
	GainDB    float64
	RG        []container.Tag
	Estimated bool
}

// Request is one whole-file transcode.
type Request struct {
	Engine  *waxflow.Engine
	Source  container.Source
	Hint    string
	Probe   *format.Info             // the caller's probe: tags, chapters, default track
	Options waxflow.TranscodeOptions // final: container settled by the caller; GainDB the fixed gain
	Plan    *waxflow.TranscodePlan   // taken from Options
	Out     *os.File                 // read-write; Run closes it on every path
	Path    string
	Mapper  meta.Mapper // nil: no tag library; container tags still fold
	// NoMetadata is --no-tags: no read, no fold, no chapters, no art, no
	// post-pass. It is not the same as a nil Mapper.
	NoMetadata bool
	Gain       func(info *meta.Info) (float64, error) // after the fold; unused under Loudness
	Loudness   bool
	// MeasurePredicted decodes the output even where the file carries a
	// predicted ReplayGain: a job's Analysis promises the measurement.
	MeasurePredicted bool
	Progress         func(phase string, done, total int64) // "analyze", "transcode", "finalize"
	Note             func(Note)                            // called as each thing happens
}

// Result is what Run wrote.
type Result struct {
	Transcode                    *waxflow.TranscodeResult
	Source                       *waxflow.AnalyzeResult // the loudness pass; nil without one
	GainDB                       float64
	OutputLUFS, OutputTruePeakDB float64
	OutputEstimated              bool
	RG                           []container.Tag // what describes the output; nil without Loudness
}

// Run writes the whole file transcode: metadata fold, loudness pass, tags,
// encode, ReplayGain measure or prediction, MP4 patch, sync, close, tagging
// post-pass. It removes nothing. An error after the encode still returns the
// Result with Transcode set, so the caller can report what was written.
func Run(ctx context.Context, req Request) (*Result, error) {
	r := &run{req: req, ctx: ctx, seen: map[string]bool{}, dropped: map[string]bool{}}
	if err := r.prepare(); err != nil {
		req.Out.Close()
		return nil, err
	}
	res := &Result{Source: r.srcRes, GainDB: r.opts.GainDB}
	tres, err := WriteMedia(req.Out, r.encode, func(f *os.File, _ *waxflow.TranscodeResult) error {
		return r.finish(f, res)
	})
	if tres == nil {
		return nil, err
	}
	res.Transcode = tres
	if err != nil {
		return res, err
	}
	// The post-pass writes the full set onto the outputs whose muxer did not
	// embed it.
	var apply func() error
	if r.postPasses() {
		apply = func() error { return req.Mapper.Apply(ctx, req.Path, r.tagInfo, res.RG) }
	}
	return res, postPass(ctx, "transcode", apply, func(err error) {
		r.note(Note{Kind: NotePostPassFailed, Text: err.Error(), Err: err})
	})
}

// postPass runs apply, a tagging post-pass on a finished output, when it is
// not nil. A failure under a live context goes to failed, which must not be
// nil: the audio is finished and correct. A cancel is returned, "<what> canceled", even with
// no post-pass to run, since the output may be untagged and is not finished.
func postPass(ctx context.Context, what string, apply func() error, failed func(error)) error {
	if apply != nil {
		if err := apply(); err != nil && ctx.Err() == nil {
			failed(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return waxerr.Wrap(waxerr.CodeCanceled, what+" canceled", err)
	}
	return nil
}

// WriteMedia is the tail every file writer shares: encode into f, patch it
// while it is open, sync, close. f is closed on every path, and the first
// error wins. A result comes back whenever the encode succeeded.
func WriteMedia(f *os.File, encode func(dst io.Writer) (*waxflow.TranscodeResult, error),
	patch func(f *os.File, res *waxflow.TranscodeResult) error) (*waxflow.TranscodeResult, error) {
	res, err := encode(f)
	if err == nil && patch != nil {
		err = patch(f, res)
	}
	if err == nil {
		if serr := f.Sync(); serr != nil {
			err = waxerr.Wrap(waxerr.CodeOutputUnwritable, "output sync", serr)
		}
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = waxerr.Wrap(waxerr.CodeOutputUnwritable, "output close", cerr)
	}
	return res, err
}

// run is one Run's state between its steps.
type run struct {
	req  Request
	ctx  context.Context
	opts waxflow.TranscodeOptions
	seen map[string]bool // input findings already noted
	// dropped is the tag keys already noted dropped.
	dropped map[string]bool

	tagInfo                       *meta.Info
	tags                          []container.Tag
	chapters                      []container.Chapter
	art                           *container.Picture
	srcRes                        *waxflow.AnalyzeResult
	isMP4, fragmented, embedsTags bool
	peakLimited                   bool
	// predicted is the ReplayGain embedded at Begin, nil when none was.
	predicted *Result
}

// postPasses reports whether the post-pass rewrites the output with the
// full tag set after the muxer, so a tag the muxer was not handed is
// restored, never lost.
func (r *run) postPasses() bool {
	return !r.req.NoMetadata && r.tagInfo != nil && postPasses(r.req.Mapper, r.opts.Format, r.opts.Container)
}

// postPasses reports whether an output in this form, tagged through
// mapper, is rewritten by the post-pass: its muxer is not the final tag
// writer and there is a mapper to run one.
func postPasses(mapper meta.Mapper, formatName, containerName string) bool {
	return mapper != nil && !waxflow.OutputEmbedsTags(formatName, containerName)
}

// fitTags is the tag set the muxer of the output form is handed: the source's
// tags, then own, the file writer's (a piece's number and title, a run's
// ReplayGain), which are never dropped. The source's go whole when the set
// fits the muxer's block, else in order, each kept if it still fits; dropped
// is the keys left out.
func fitTags(formatName, containerName string, source, own []container.Tag) (tags []container.Tag, dropped []string) {
	fits := func(set []container.Tag) bool {
		return waxflow.TagsFit(formatName, containerName, append(slices.Clip(set), own...)) == nil
	}
	if fits(source) {
		return append(slices.Clip(source), own...), nil
	}
	var kept []container.Tag
	for _, t := range source {
		if fits(append(slices.Clip(kept), t)) {
			kept = append(kept, t)
		} else {
			dropped = append(dropped, t.Key)
		}
	}
	return append(kept, own...), dropped
}

func (r *run) note(n Note) {
	if r.req.Note != nil {
		r.req.Note(n)
	}
}

func (r *run) progress(phase string) func(done, total int64) {
	if r.req.Progress == nil {
		return nil
	}
	return func(done, total int64) { r.req.Progress(phase, done, total) }
}

// noteInput reports the findings of a source read not already reported.
func (r *run) noteInput(found []string) {
	for _, w := range found {
		if !r.seen[w] {
			r.seen[w] = true
			r.note(Note{Kind: NoteInputDamage, Text: w})
		}
	}
}

// prepare runs everything before the encode: the metadata read and fold, the
// gain, and the tags, chapters and art the muxer takes at Begin.
func (r *run) prepare() error {
	req, plan := r.req, r.req.Plan
	r.opts = req.Options
	info, err := r.readMetadata()
	if err != nil {
		return err
	}

	gainDB := r.opts.GainDB
	switch {
	case req.Loudness:
		// Measured on the plan's resolved width, the audio the encode meters.
		res, err := req.Engine.Analyze(r.ctx, req.Source, req.Hint, waxflow.AnalyzeOptions{
			Progress: r.progress("analyze"),
			Channels: plan.Format.Channels,
		})
		if err != nil {
			return err
		}
		// Reported now, so an encode that fails afterwards cannot lose them.
		r.noteInput(res.InputWarnings)
		r.srcRes = res
		if !math.IsInf(res.IntegratedLUFS, -1) {
			// The exact gain to the ReplayGain reference, unclamped: the
			// true-peak limiter guards the top end.
			gainDB = meta.ReplayGainGainDB(res.IntegratedLUFS)
		}
		r.note(Note{Kind: NoteLoudnessSource, LUFS: res.IntegratedLUFS, GainDB: gainDB})
	case req.Gain != nil:
		if gainDB, err = req.Gain(info); err != nil {
			return err
		}
	}
	r.opts.GainDB = gainDB

	r.tagInfo = info
	if gainDB != 0 || req.Loudness {
		r.tagInfo = meta.WithoutReplayGain(info)
	}
	r.embedsTags = waxflow.OutputEmbedsTags(r.opts.Format, r.opts.Container)
	// The container's chapters are the floor and the mapper's win.
	if !req.NoMetadata {
		r.chapters = req.Probe.Chapters
	}
	if info != nil {
		if len(info.Chapters) > 0 {
			r.chapters = info.Chapters
		}
		if p := info.FrontPicture(); p != nil {
			r.art = &container.Picture{MIME: p.MIME, Data: p.Data}
		}
	}
	// The mapper's titles were repaired with its Info; the container's were not.
	if chs, repaired := meta.WritableChapters(r.chapters); repaired {
		r.chapters = chs
		r.note(Note{Kind: NoteMetadataWarning, Text: unwritable("a chapter title")})
	}

	// Both MP4 forms embed tags at Begin and take the patch; only the
	// fragmented one is derived rather than read back.
	r.isMP4 = plan.MediaType == "audio/mp4"
	r.fragmented = r.isMP4 && r.opts.Container != waxflow.ContainerProgressive
	// The true-peak limiter engages for positive gain, a dynamics preset or a
	// downmix, and caps a projected peak at its ceiling.
	r.peakLimited = gainDB > 0 || r.opts.Dynamics != gain.PresetOff ||
		plan.Format.Channels < req.Probe.Default().Fmt.Channels
	var own []container.Tag
	switch {
	case req.Loudness && r.isMP4:
		// Fixed-width placeholders, patched with the values after the encode.
		// Only MP4 patches: any other format would ship the unity values
		// whenever the post-pass cannot run.
		own = []container.Tag{
			{Key: "REPLAYGAIN_TRACK_GAIN", Value: meta.FormatGain(0)},
			{Key: "REPLAYGAIN_TRACK_PEAK", Value: meta.FormatPeak(0)},
		}
	case req.Loudness && r.embedsTags:
		// The muxer takes its tags at Begin, cannot be patched and skips the
		// post-pass, so the projection is embedded now.
		lufs, tp := deriveOutputLoudness(r.srcRes, gainDB, r.peakLimited)
		r.predicted = &Result{OutputLUFS: lufs, OutputTruePeakDB: tp, OutputEstimated: true,
			RG: meta.ReplayGainTags(lufs, tp)}
		own = r.predicted.RG
	}
	// The source's own tags: one the output's block cannot hold is dropped
	// rather than failing the transcode, and noted only where no post-pass
	// restores it.
	var dropped []string
	r.tags, dropped = fitTags(r.opts.Format, r.opts.Container, meta.FullTags(r.tagInfo), own)
	r.noteDropped(dropped)
	if !r.postPasses() {
		noteLeftOut(r.opts.Format, r.opts.Container, r.tags, r.dropped, r.note)
	}
	if p := r.predicted; p != nil {
		r.note(Note{Kind: NoteLoudnessOutput, LUFS: p.OutputLUFS, RG: p.RG, Estimated: true})
	}
	return nil
}

// noteDropped reports each key with a value left out of what the muxer is
// handed, once, unless the post-pass writes them.
func (r *run) noteDropped(keys []string) {
	if r.postPasses() {
		return
	}
	for _, key := range keys {
		if !r.dropped[key] {
			r.dropped[key] = true
			r.note(Note{Kind: NoteTagTooLarge, Key: key})
		}
	}
}

// noteLeftOut reports the keys of tags that the output's muxer writes no form
// of, in one note, each key once in seen: a name the format forbids or has
// no place for, or one that would read back as another key. It is for an
// output no post-pass writes after.
func noteLeftOut(formatName, containerName string, tags []container.Tag, seen map[string]bool, note func(Note)) {
	if seen == nil {
		return
	}
	var keys []string
	for _, key := range waxflow.TagsLeftOut(formatName, containerName, tags) {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	switch len(keys) {
	case 0:
	case 1:
		note(Note{Kind: NoteMetadataWarning, Text: "tag " + keys[0] + " has no form in the output and was not embedded"})
	default:
		note(Note{Kind: NoteMetadataWarning,
			Text: "tags " + strings.Join(keys, ", ") + " have no form in the output and were not embedded"})
	}
}

// readMetadata reads and folds the source's metadata. A canceled read aborts;
// any other failure is a note, and the container's tags still fold.
func (r *run) readMetadata() (*meta.Info, error) {
	req := r.req
	if req.NoMetadata {
		return nil, nil
	}
	var info *meta.Info
	if req.Mapper != nil {
		mi, err := req.Mapper.Read(r.ctx, req.Source, req.Hint, meta.ReadOptions{Pictures: true})
		switch {
		case err != nil && r.ctx.Err() != nil:
			return nil, err
		case err != nil:
			r.note(Note{Kind: NoteMetadataUnread, Text: err.Error(), Err: err})
			mi = &meta.Info{}
		}
		if mi != nil {
			for _, w := range mi.Warnings {
				r.note(Note{Kind: NoteMetadataWarning, Text: w})
			}
			for _, n := range mi.Notes {
				r.note(Note{Kind: NoteMetadataLint, Text: n})
			}
		}
		info = mi
	}
	// Folded before the ReplayGain drop: the container carries those keys
	// too, and a later fold would restore the source's stale values. The
	// same goes for the source's encoder stamp, which every file output
	// drops: each is a fresh encode.
	info = meta.WithoutEncoding(meta.WithContainerTags(info, req.Probe.Tags))
	// Repaired once here, so the muxer and the post-pass write the same text.
	info, repaired := meta.Writable(info)
	for _, what := range repaired {
		r.note(Note{Kind: NoteMetadataWarning, Text: unwritable(what)})
	}
	return info, nil
}

// unwritable words the warning for text meta.Writable repaired.
func unwritable(what string) string {
	return what + " held a NUL byte or invalid UTF-8 and is written repaired"
}

// encode runs the transcode into dst.
func (r *run) encode(dst io.Writer) (*waxflow.TranscodeResult, error) {
	res, err := r.req.Engine.Transcode(r.ctx, r.req.Source, r.req.Hint, dst, r.opts,
		waxflow.WithTags(r.tags), waxflow.WithChapters(r.chapters), waxflow.WithArt(r.art),
		waxflow.WithProgress(r.progress("transcode")))
	if err != nil {
		return nil, err
	}
	r.noteInput(res.InputWarnings)
	return res, nil
}

// finish settles the output's ReplayGain on the open file: the prediction
// embedded at Begin, a projection for fragmented MP4, or a measurement of
// the bytes written, patched over an MP4's placeholders.
func (r *run) finish(f *os.File, res *Result) error {
	if !r.req.Loudness {
		return nil
	}
	if p := r.predicted; p != nil && !r.req.MeasurePredicted {
		res.OutputLUFS, res.OutputTruePeakDB, res.OutputEstimated, res.RG = p.OutputLUFS, p.OutputTruePeakDB, true, p.RG
		return nil
	}
	if r.fragmented {
		res.OutputLUFS, res.OutputTruePeakDB = deriveOutputLoudness(r.srcRes, r.opts.GainDB, r.peakLimited)
		res.OutputEstimated = true
	} else {
		fsrc, err := container.FileSource(f)
		if err != nil {
			return err
		}
		out, err := r.req.Engine.Analyze(r.ctx, fsrc, waxflow.OutputExt(r.opts.Format, r.opts.Container),
			waxflow.AnalyzeOptions{Progress: r.progress("finalize")})
		if err != nil {
			return err
		}
		// Only a bug of ours makes a file just written read back damaged,
		// and the values below then describe a partial decode.
		for _, w := range out.InputWarnings {
			r.note(Note{Kind: NoteReadBackDamage, Text: w})
		}
		res.OutputLUFS, res.OutputTruePeakDB = out.IntegratedLUFS, out.TruePeakDB
	}
	res.RG = meta.ReplayGainTags(res.OutputLUFS, res.OutputTruePeakDB)
	r.note(Note{Kind: NoteLoudnessOutput, LUFS: res.OutputLUFS, RG: res.RG, Estimated: res.OutputEstimated})
	if r.isMP4 {
		if err := mp4.PatchFreeform(f, "REPLAYGAIN_TRACK_GAIN", meta.FormatGain(0), res.RG[0].Value); err != nil {
			return err
		}
		if err := mp4.PatchFreeform(f, "REPLAYGAIN_TRACK_PEAK", meta.FormatPeak(0), res.RG[1].Value); err != nil {
			return err
		}
	}
	return nil
}

// deriveOutputLoudness projects the output's loudness and true peak from the
// source measurement, for an output the pipeline does not read back. See
// meta.ProjectLoudness for the direction each result errs in.
func deriveOutputLoudness(src *waxflow.AnalyzeResult, gainDB float64, limited bool) (lufs, truePeakDB float64) {
	return meta.ProjectLoudness(src.IntegratedLUFS, src.TruePeakDB, gainDB, limited)
}
