package cli

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/posixfs"
	"github.com/colespringer/waxflow/waxerr"
)

// clippingNote prints the engine's level note on stderr with the CLI's
// remedy attached; waxflow.TranscodeResult.LevelNote owns the decision, so
// every command that writes audio reports the same cases the same way.
func clippingNote(w io.Writer, res *waxflow.TranscodeResult) {
	note := res.LevelNote()
	if note == "" {
		return
	}
	remedy := "; lower the level with --gain"
	if strings.HasPrefix(note, "clipping: ") {
		remedy = "; lower the level with --gain or choose a float output"
	}
	fmt.Fprintf(w, "%s%s\n", note, remedy)
}

// isMP4Container reports whether an output written by format with this
// container override goes through the mp4 muxer: the one path that embeds tags
// in moov at Begin and so takes the mp4-specific ReplayGain patch. AAC also
// rides in adts (elementary) and mka (Matroska), which are NOT MP4: patching
// those as MP4 would fail and delete the output. ALAC is always MP4.
func isMP4Container(format, container string) bool {
	if format == "alac" {
		return true
	}
	return format == "aac" && (container == "" ||
		container == waxflow.ContainerProgressive || container == waxflow.ContainerFragmented)
}

func newTranscodeCmd(flavor Flavor) *cobra.Command {
	var formatName, containerName string
	var force bool
	var rate, channels, bits int
	var flacLevel int
	var mp3Bitrate int
	var mp3VBR bool
	var opusBitrate int
	var opusComplexity int
	var opusVBR bool
	var opusSignal string
	var aacBitrate int
	var gainDB float64
	var dynamics dynamicsFlag
	var profileName, ditherName string
	var loudness string
	var noTags bool
	cmd := &cobra.Command{
		Use:   "transcode <input> <output>",
		Short: "Transcode an audio file locally through the engine",
		Long: `Transcode decodes the input and writes it to the output path via the
same engine the daemon uses: decode -> DSP -> encode -> mux. The output
format comes from --format or the output extension. Without conversion
flags no DSP node is inserted at all, so a lossless input to a lossless
output is a bit-exact container rewrite; a lossy input is still decoded
and re-encoded, which costs a generation. --rate, --channels, --bits and
--gain insert only the DSP nodes they need (resampling, downmix, gain
with true-peak limiting, dither).`,
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			profile, err := parseProfile(profileName)
			if err != nil {
				return err
			}
			shaping, err := parseDither(ditherName)
			if err != nil {
				return err
			}
			logger, err := newLogger(cmd.ErrOrStderr(), cfg) // CLI logs to stderr
			if err != nil {
				return err
			}

			outFormat := formatName
			ext := extHint(args[1])
			if outFormat == "" {
				// The engine's output table is the single source of truth
				// for extensions, so the CLI cannot drift from it. A
				// container-selecting extension (.mka/.webm) names a
				// container form rather than a top-level format, so it
				// resolves to a (format, container) pair.
				if f, _, ok := waxflow.OutputContainerForExt(ext); ok {
					outFormat = f
				} else {
					outFormat = waxflow.OutputFormatForExt(ext)
					if outFormat == "" {
						return waxerr.New(waxerr.CodeInvalidRequest,
							fmt.Sprintf("cannot infer output format from %q; pass --format (%s)",
								filepath.Base(args[1]), strings.Join(waxflow.OutputFormats(), ", ")))
					}
				}
			}
			// The output extension also implies a container when --container
			// was not given, whether or not --format was explicit: a .mka/.webm
			// output writes Matroska/WebM (so `--format opus out.webm` is
			// Opus-in-WebM, not an Ogg stream misnamed .webm), and a bare .aac
			// output is the ADTS elementary stream (the .m4a extension is the
			// fMP4 default). An explicit --container always wins.
			if containerName == "" {
				if _, c, ok := waxflow.OutputContainerForExt(ext); ok {
					containerName = c
				} else if outFormat == "aac" && ext == "aac" {
					containerName = "adts"
				}
			}

			// Before anything is created on disk: a refused transcode must
			// not leave a directory tree behind. split orders its own
			// MkdirAll after validation for the same reason.
			if loudness != "" && loudness != "analyze" {
				return waxerr.New(waxerr.CodeInvalidRequest,
					fmt.Sprintf("loudness %q: want analyze (or omit)", loudness))
			}
			if loudness == "analyze" && gainDB != 0 {
				return waxerr.New(waxerr.CodeInvalidRequest, "--loudness analyze replaces --gain; drop one")
			}

			src, srcHint, cleanup, err := openSourceRef(cmd, flavor, args[0], &cfg, logger)
			if err != nil {
				return err
			}
			defer cleanup()

			// An in-place transcode would truncate the input before it is
			// ever read (and the failure path would then unlink it), so
			// refuse when both paths name the same file. os.SameFile
			// catches hard links and symlinked spellings, not just equal
			// path strings.
			if outFi, err := os.Stat(args[1]); err == nil {
				if inFi, err := os.Stat(args[0]); err == nil && os.SameFile(inFi, outFi) {
					return waxerr.New(waxerr.CodeInvalidRequest,
						"input and output are the same file; transcode to a new path")
				}
			}

			e := waxflow.New(waxflow.WithLogger(logger))

			// The options fields cannot say "level 0" or "complexity 0"
			// with a plain 0 (that selects the default), so the flags' 0
			// maps to the sentinels.
			optLevel := flacLevel
			if optLevel == 0 {
				optLevel = waxflow.FLACLevelFastest
			}
			optComplexity := opusComplexity
			if optComplexity == 0 {
				optComplexity = waxflow.OpusComplexityLowest
			}

			// One probe of the source, read before anything is created on
			// disk. Three things downstream need it and each used to fetch
			// (or skip) its own: the container's own tags for the metadata
			// fallback, the track the output form is planned from, and the
			// source layout the loudness pass measures against.
			srcInfo, err := e.Probe(src, srcHint, nil)
			if err != nil {
				return err
			}
			srcTrack := srcInfo.Default()

			opts := waxflow.TranscodeOptions{
				Format:          outFormat,
				Container:       containerName,
				Rate:            rate,
				Channels:        channels,
				BitDepth:        bits,
				Dynamics:        gain.Preset(dynamics),
				Shaping:         shaping,
				ResampleProfile: profile,
				FLACLevel:       optLevel,
				MP3Bitrate:      mp3Bitrate * 1000,
				MP3VBR:          mp3VBR,
				OpusBitrate:     opusBitrate * 1000,
				OpusComplexity:  optComplexity,
				OpusVBR:         opusVBR,
				OpusSignal:      opusSignal,
				AACBitrate:      aacBitrate * 1000,
			}
			plan, err := e.PlanTranscode(srcTrack, opts)
			if err != nil {
				return err
			}
			// A file output takes the flat MP4 form unless the caller named a
			// container. The rule is the engine's, shared with split and with
			// every job type, and the re-plan is what proves the form it chose
			// is one this format can produce.
			if c := waxflow.FileOutputContainer(containerName, plan); c != containerName {
				containerName, opts.Container = c, c
				if plan, err = e.PlanTranscode(srcTrack, opts); err != nil {
					return err
				}
			}

			// The output is created exclusively at its final path, or,
			// under --force, staged in the same directory and renamed
			// into place only after the transcode succeeds. Overwriting
			// in place would truncate first, so any failure (a bad flag
			// caught by chain validation, an unreadable source, a full
			// disk) would destroy the file it was asked to replace.
			outPath := args[1]
			writePath := outPath
			if force {
				writePath = fmt.Sprintf("%s.tmp-%d", outPath, os.Getpid())
			}
			// Created rather than required, as split does. Both write paths
			// (final, and the staged .tmp under --force) sit here.
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating the output directory", err)
			}
			out, err := os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				if !force && errors.Is(err, os.ErrExist) {
					return waxerr.Wrap(waxerr.CodeInvalidRequest, "output exists (use --force to overwrite)", err)
				}
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating output", err)
			}

			// The file-output passthrough matrix: full tags, chapters,
			// and art flow onto the output (the MP4 muxer embeds them;
			// every other format gets the mapping post-pass below).
			mapper := label.NewLogged(logger)
			var info *meta.Info
			if !noTags {
				m, merr := mapper.Read(cmd.Context(), src, srcHint, meta.ReadOptions{Pictures: true})
				if merr != nil {
					// The container fallback below does not go through the
					// mapper, so a read that failed outright must not take with
					// it the tags the demuxer already parsed off the header.
					// Reported rather than silently dropped, which is what a
					// failure here used to be.
					m = &meta.Info{Warnings: []string{"metadata unread: " + merr.Error()}}
				}
				logMetaRead(logger, m)
				// Folded where the mapper's own tags arrive, and so before the
				// ReplayGain drop below rather than after it: the container
				// carries the four REPLAYGAIN_* keys too, and a later fold would
				// put the source's stale values back onto a gained output.
				info = meta.WithContainerTags(m, srcInfo.Tags)
			}
			analyzeLoudness := loudness == "analyze"
			var srcRes *waxflow.AnalyzeResult
			var peakLimited bool
			if analyzeLoudness {
				// Both the measurement and the predicted peak key on the width
				// the encode actually produces, which is the plan's resolved
				// channel count and not the --channels flag: a lossy row that
				// cannot hold the source layout folds to stereo on its own, and
				// a flag-keyed test sees none of it.
				//
				// Measuring on that basis is what puts the two-pass gain on the
				// audio the encode meters. Clamping on it is what keeps the
				// predicted peak (predictedRG / analyzeOutputRG) at the ceiling
				// whenever the chain's true-peak limiter is engaged: positive
				// gain, a dynamics preset, or a downmix whose matrix can sum
				// past unity. Analyze runs the raw fold with no limiter, so its
				// true peak can sit above the ceiling the encode holds.
				outChannels := plan.Format.Channels
				res, aerr := e.Analyze(cmd.Context(), src, srcHint, waxflow.AnalyzeOptions{Channels: outChannels})
				if aerr != nil {
					out.Close()
					os.Remove(writePath)
					return aerr
				}
				srcRes = res
				if !math.IsInf(res.IntegratedLUFS, -1) {
					gainDB = meta.ReplayGainGainDB(res.IntegratedLUFS)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "loudness: source %.2f LUFS, applying %+.2f dB\n",
					res.IntegratedLUFS, gainDB)
				peakLimited = gainDB > 0 || gain.Preset(dynamics) != gain.PresetOff ||
					outChannels < srcTrack.Fmt.Channels
			}
			dropRG := gainDB != 0 || analyzeLoudness
			tagInfo := info
			if dropRG {
				tagInfo = meta.WithoutReplayGain(info)
			}
			tags := meta.FullTags(tagInfo)
			// Only the MP4 path patches placeholders after the encode;
			// any other format gets its measured values through the
			// mapping post-pass, and embedding unity placeholders there
			// would ship wrong ReplayGain whenever that post-pass is
			// skipped (--no-tags) or fails.
			isMP4 := isMP4Container(outFormat, containerName)
			// The two meanings isMP4 used to carry, which have diverged: both
			// MP4 forms embed tags at Begin and so need the fixed-width
			// placeholders, but only the fragmented one cannot be decoded back,
			// which is the condition for deriving the output's ReplayGain
			// instead of measuring it. With a file output now flat by default,
			// keeping them as one predicate would publish an estimate where a
			// measurement is there for the reading.
			fragmentedMP4 := isMP4 && containerName != waxflow.ContainerProgressive
			switch {
			case analyzeLoudness && isMP4:
				// Unity placeholders, patched with the measured RG by
				// analyzeOutputRG after the encode.
				tags = append(tags,
					container.Tag{Key: "REPLAYGAIN_TRACK_GAIN", Value: meta.FormatGain(0)},
					container.Tag{Key: "REPLAYGAIN_TRACK_PEAK", Value: meta.FormatPeak(0)})
			case analyzeLoudness && containerName == "ogg":
				// The Ogg muxer embeds the comment header at Begin and cannot be
				// patched afterward, and the post-pass is skipped for Ogg, so the
				// measured RG would otherwise be computed and dropped. Embed the
				// RG predicted from the source loudness and the applied gain now
				// (the same estimate the MP4 path patches in).
				rg, outLUFS := predictedRG(srcRes, gainDB, peakLimited)
				tags = append(tags, rg...)
				// Labelled as an estimate: analyzeOutputRG prints the same
				// shape from a real measurement.
				fmt.Fprintf(cmd.ErrOrStderr(), "loudness: output ~%.2f LUFS (estimated), %s / %s\n",
					outLUFS, rg[0].Value, rg[1].Value)
			}
			var chapters []container.Chapter
			var art *container.Picture
			if tagInfo != nil {
				chapters = tagInfo.Chapters
				if p := tagInfo.FrontPicture(); p != nil {
					art = &container.Picture{MIME: p.MIME, Data: p.Data}
				}
			}

			// The per-call payloads and the measured gain land on the options
			// the plan was taken from, so what runs is what was planned. None
			// of them shape a plan: PlanTranscode normalizes the payloads away
			// and gain changes no output field the container default or the
			// resolved width above read.
			opts.GainDB = gainDB
			opts.Tags, opts.Chapters, opts.Art = tags, chapters, art
			res, err := e.Transcode(cmd.Context(), src, srcHint, out, opts)
			if err != nil {
				out.Close()
				// A failed transcode leaves no half-written artifact;
				// under --force the target was never touched.
				os.Remove(writePath)
				return err
			}
			if err := out.Close(); err != nil {
				os.Remove(writePath)
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "closing output", err)
			}

			// Post-pass on the finished file: measured ReplayGain under
			// --loudness analyze, and the full metadata set for formats
			// the mapper can rewrite (MP4 got everything at Begin).
			var rg []container.Tag
			if analyzeLoudness && containerName != "ogg" {
				// Ogg already embedded its predicted RG at Begin (it cannot be
				// patched); mp4 patches its placeholders here, and post-pass
				// formats get their measured values written below.
				if rg, err = analyzeOutputRG(cmd, e, writePath, extHint(outPath), isMP4, fragmentedMP4, srcRes, gainDB, peakLimited); err != nil {
					os.Remove(writePath)
					return err
				}
			}
			// embedsTags names the outputs whose muxer already wrote the tags at
			// mux time, so the post-pass must skip them to avoid a redundant (or
			// conflicting) second write. The MP4 muxers embed an ilst in moov,
			// which is also the only way the fragmented form gets tags at all:
			// the mapper reads that shape but refuses to rewrite it. The Ogg
			// muxer embeds the comment header at Begin (and the label mapper has
			// no Ogg-FLAC writer anyway). Every other output, incl.
			// Matroska (.mka/.webm), defers to the post-pass: the mka muxer
			// accepts Tags but does not emit them (see container/mka.MuxerOptions),
			// so if it ever starts writing tags at Begin, add its containers here.
			embedsTags := isMP4 || containerName == "ogg"
			if !noTags && !embedsTags && tagInfo != nil {
				if aerr := mapper.Apply(cmd.Context(), writePath, tagInfo, rg); aerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "metadata: post-pass failed: %v\n", aerr)
				}
			}
			if force {
				if err := posixfs.Replace(writePath, outPath); err != nil {
					os.Remove(writePath)
					return waxerr.Wrap(waxerr.CodeOutputUnwritable, "replacing output", err)
				}
			}
			clippingNote(cmd.ErrOrStderr(), res)
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %s %d samples (%.3fs)\n",
				outPath, res.Format, res.Samples, durationSeconds(res.Samples, res.Format.Rate))
			return nil
		},
	}
	// Derived from the output table, like the inference error above: the
	// hand-written list had already fallen a format behind.
	cmd.Flags().StringVar(&formatName, "format", "",
		fmt.Sprintf("output format: %s (default: from output extension)", strings.Join(waxflow.OutputFormats(), ", ")))
	cmd.Flags().StringVar(&containerName, "container", "", "container override where the format has one: adts for aac, progressive/fragmented for aac/alac (flat vs CMAF MP4), mka/webm for opus/aac/flac/wav, ogg for flac (default: the format's native container; a bare .aac output implies adts, and an mp4-family file output is progressive, the form players and taggers expect)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output if it exists")
	cmd.Flags().IntVar(&rate, "rate", 0, "output sample rate in Hz (default: source rate)")
	cmd.Flags().IntVar(&channels, "channels", 0, "output channel count: 1 or 2 (default: source layout)")
	cmd.Flags().IntVar(&bits, "bits", 0, "output bit depth, dithered when reducing (default: source depth)")
	cmd.Flags().Float64Var(&gainDB, "gain", 0, "gain in dB; positive gain engages the true-peak limiter")
	cmd.Flags().Var(&dynamics, "dynamics", "dynamics preset: off (default) or voice; acts on the post-gain signal")
	cmd.Flags().StringVar(&profileName, "resample-profile", "hq", "resampler quality: hq or fast")
	cmd.Flags().StringVar(&ditherName, "dither", "tpdf", "dither when reducing depth: tpdf, shaped, or off")
	cmd.Flags().IntVar(&flacLevel, "flac-level", 5, "FLAC compression level 0-8, size vs speed (flac output only)")
	cmd.Flags().IntVar(&mp3Bitrate, "mp3-bitrate", 128, "MP3 bit rate in kbit/s: constant, or the quality anchor under --mp3-vbr (mp3 output only)")
	cmd.Flags().BoolVar(&mp3VBR, "mp3-vbr", false, "encode MP3 at variable bit rate anchored at --mp3-bitrate (mp3 output only)")
	cmd.Flags().IntVar(&opusBitrate, "opus-bitrate", 96, "Opus target bit rate in kbit/s (opus output only)")
	cmd.Flags().IntVar(&opusComplexity, "opus-complexity", 5, "Opus encoder complexity 0-10, quality vs speed (opus output only)")
	cmd.Flags().BoolVar(&opusVBR, "opus-vbr", false, "encode Opus at variable bit rate around --opus-bitrate (opus output only)")
	cmd.Flags().StringVar(&opusSignal, "opus-signal", "auto", "Opus content hint: auto, voice, or music (opus output only)")
	cmd.Flags().IntVar(&aacBitrate, "aac-bitrate", 128, "AAC target bit rate in kbit/s (aac output only)")
	cmd.Flags().StringVar(&loudness, "loudness", "", "analyze: two-pass loudness (exact gain to the ReplayGain reference, measured RG tags on the output); the gain is measured after any --channels downmix")
	cmd.Flags().BoolVar(&noTags, "no-tags", false, "skip the metadata passthrough (tags, chapters, art)")
	return cmd
}

// predictedRG estimates the output ReplayGain from the source analysis and the
// applied gain, for the Ogg path (whose tags are written at Begin) and the MP4
// placeholders. See meta.ProjectLoudness for the projection and the direction
// each result errs in.
func predictedRG(srcRes *waxflow.AnalyzeResult, gainDB float64, limited bool) (rg []container.Tag, outLUFS float64) {
	outLUFS, outTP := meta.ProjectLoudness(srcRes.IntegratedLUFS, srcRes.TruePeakDB, gainDB, limited)
	return meta.ReplayGainTags(outLUFS, outTP), outLUFS
}

// analyzeOutputRG returns (after patching MP4 headers in place) the
// ReplayGain tags for the finished output: measured from the file where
// the engine can decode it back, derived from the source measurement
// plus the applied gain for fragmented MP4, which has no read path
// (exact for lossless ALAC, within the encoder's fraction of a dB for
// AAC; the encode's limiter caps the derived peak).
//
// The two MP4 flags are separate because they answer different questions:
// isMP4 says the muxer embedded fixed-width placeholders to patch, which
// both forms do, and fragmented says the output cannot be read back, which
// only the fragmented one is.
func analyzeOutputRG(cmd *cobra.Command, e *waxflow.Engine, path, hint string, isMP4, fragmented bool, srcRes *waxflow.AnalyzeResult, gainDB float64, limited bool) ([]container.Tag, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "reopening output", err)
	}
	defer f.Close()
	var rg []container.Tag
	var outLUFS float64
	if fragmented {
		// No read path, so the RG is predicted from the source and gain and
		// then patched into the placeholders.
		rg, outLUFS = predictedRG(srcRes, gainDB, limited)
	} else {
		fsrc, err := container.FileSource(f)
		if err != nil {
			return nil, err
		}
		outRes, err := e.Analyze(cmd.Context(), fsrc, hint, waxflow.AnalyzeOptions{})
		if err != nil {
			return nil, err
		}
		rg, outLUFS = meta.ReplayGainTags(outRes.IntegratedLUFS, outRes.TruePeakDB), outRes.IntegratedLUFS
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "loudness: output %.2f LUFS, %s / %s\n",
		outLUFS, rg[0].Value, rg[1].Value)
	if isMP4 {
		if err := mp4.PatchFreeform(f, "REPLAYGAIN_TRACK_GAIN", meta.FormatGain(0), rg[0].Value); err != nil {
			return nil, err
		}
		if err := mp4.PatchFreeform(f, "REPLAYGAIN_TRACK_PEAK", meta.FormatPeak(0), rg[1].Value); err != nil {
			return nil, err
		}
	}
	return rg, nil
}

func parseProfile(name string) (resample.Profile, error) {
	switch name {
	case "hq", "":
		return resample.HQ, nil
	case "fast":
		return resample.Fast, nil
	default:
		return "", waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("unknown resample profile %q (hq, fast)", name))
	}
}

func parseDither(name string) (dither.Shaping, error) {
	switch name {
	case "tpdf", "":
		return dither.TPDF, nil
	case "shaped":
		return dither.Shaped, nil
	case "off", "none":
		return dither.None, nil
	default:
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("unknown dither mode %q (tpdf, shaped, off)", name))
	}
}

// dynamicsFlag is the --dynamics flag: a closed vocabulary, so an
// unrecognized preset is rejected at parse time with the list rather than
// reaching the engine as an unsupported-format error.
type dynamicsFlag gain.Preset

func (d *dynamicsFlag) String() string { return string(*d) }
func (d *dynamicsFlag) Type() string   { return "preset" }

func (d *dynamicsFlag) Set(v string) error {
	switch strings.ToLower(v) {
	case "", "off":
		*d = dynamicsFlag(gain.PresetOff)
		return nil
	}
	for _, p := range gain.Presets() {
		if strings.EqualFold(v, string(p)) {
			*d = dynamicsFlag(p)
			return nil
		}
	}
	names := []string{"off"}
	for _, p := range gain.Presets() {
		names = append(names, string(p))
	}
	return waxerr.New(waxerr.CodeInvalidRequest,
		fmt.Sprintf("dynamics %q: want %s", v, strings.Join(names, ", ")))
}
