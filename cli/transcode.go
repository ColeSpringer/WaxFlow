package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/cli/label"
	"github.com/colespringer/waxflow/codec/vorbis"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/gain"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/internal/outshape"
	"github.com/colespringer/waxflow/internal/posixfs"
	"github.com/colespringer/waxflow/internal/tofile"
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

// lossNote prints the engine's loss note on stderr: how much of what was
// written is silence standing in for a hole, and how much more the source
// declared than it holds. waxflow.TranscodeResult.LossNote owns the wording.
func lossNote(w io.Writer, res *waxflow.TranscodeResult) {
	if note := res.LossNote(); note != "" {
		fmt.Fprintf(w, "input damage: %s\n", note)
	}
}

// transcodeNote prints what the file pipeline reports as it happens. The
// metadata read's own findings go to the logger, as split's do.
func transcodeNote(w io.Writer, logger *slog.Logger, n tofile.Note) {
	if logMetaNote(logger, n) {
		return
	}
	switch n.Kind {
	case tofile.NoteTagTooLarge:
		fmt.Fprintf(w, "metadata: tag %s is too large for the output and was not embedded\n", n.Key)
	case tofile.NoteInputDamage:
		fmt.Fprintf(w, "input damage: %s\n", n.Text)
	case tofile.NoteLoudnessSource:
		fmt.Fprintf(w, "loudness: source %.2f LUFS, applying %+.2f dB\n", n.LUFS, n.GainDB)
	case tofile.NoteLoudnessOutput:
		// An estimate is one projected from the source, never read back.
		if n.Estimated {
			fmt.Fprintf(w, "loudness: output ~%.2f LUFS (estimated), %s / %s\n", n.LUFS, n.RG[0].Value, n.RG[1].Value)
		} else {
			fmt.Fprintf(w, "loudness: output %.2f LUFS, %s / %s\n", n.LUFS, n.RG[0].Value, n.RG[1].Value)
		}
	case tofile.NoteReadBackDamage:
		fmt.Fprintf(w, "output read back with damage: %s\n", n.Text)
	case tofile.NotePostPassFailed:
		fmt.Fprintf(w, "metadata: post-pass failed: %v\n", n.Err)
	}
}

// transcodeHook, when set, sees each transcode's request before it runs:
// the tests' gate for a cancel that lands mid-encode or in the post-pass.
var transcodeHook func(*tofile.Request)

func newTranscodeCmd(flavor Flavor) *cobra.Command {
	var formatName, containerName string
	var force bool
	var rate, channels, bits int
	var flacLevel int
	var wavpackLevel int
	var apeLevel int
	var bitrateKbps int
	var vbr bool
	var vorbisQuality float64
	var opusComplexity int
	var opusSignal string
	var heV2 bool
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
				} else if (outFormat == "aac" || outFormat == "he-aac") && ext == "aac" {
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

			// What the flags alone decide is refused before the source opens, in
			// this order: the format and container, then --bitrate or --vbr off
			// its formats, then the bit rate's bound, then the other knob flags
			// off their formats, then their ranges.
			if err := (waxflow.TranscodeOptions{Format: outFormat, Container: containerName}).Check(); err != nil {
				return err
			}
			if err := givenKnobRefusal(cmd, outFormat, waxflow.KnobBitrate|waxflow.KnobVBR); err != nil {
				return err
			}
			bitrate, err := bitrateOption(cmd, bitrateKbps)
			if err != nil {
				return err
			}
			if err := givenKnobRefusal(cmd, outFormat, ^waxflow.Knob(0)); err != nil {
				return err
			}
			quality, qualityErr := vorbisQualityOption(cmd, vorbisQuality)
			level, levelErr := flacLevelOption(cmd, flacLevel)
			complexity, complexityErr := opusComplexityOption(cmd, opusComplexity)
			opts := waxflow.TranscodeOptions{
				Format:          outFormat,
				Container:       containerName,
				Rate:            rate,
				Channels:        channels,
				BitDepth:        bits,
				Dynamics:        gain.Preset(dynamics),
				Shaping:         shaping,
				ResampleProfile: profile,
				FLACLevel:       level,
				WavPackLevel:    changedOr(cmd, "wavpack-level", wavpackLevel),
				APELevel:        changedOr(cmd, "ape-level", apeLevel),
				Bitrate:         bitrate,
				VBR:             vbr,
				VorbisQuality:   quality,
				OpusComplexity:  complexity,
				OpusSignal:      changedOr(cmd, "opus-signal", opusSignal),
				HEAACv2:         changedOr(cmd, "he-v2", heV2),
			}
			if err := opts.Check(); err != nil {
				return flagWording.Reword(err)
			}
			if err := cmp.Or(qualityErr, levelErr, complexityErr); err != nil {
				return err
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

			// One probe of the source, read before anything is created on
			// disk. Three things downstream need it and each used to fetch
			// (or skip) its own: the container's own tags for the metadata
			// fallback, the track the output form is planned from, and the
			// source layout the loudness pass measures against.
			srcInfo, err := e.Probe(cmd.Context(), src, srcHint, nil)
			if err != nil {
				return err
			}
			srcTrack := srcInfo.Default()

			plan, err := e.PlanTranscode(srcTrack, opts)
			if err != nil {
				return flagWording.Reword(err)
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

			// Staged before the long passes, so "output exists" answers
			// first. A failure or a cancel removes it, and the target only
			// ever holds a finished file.
			outPath := args[1]
			// Created rather than required, as split does.
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating the output directory", err)
			}
			st, err := stageOutput(outPath, force)
			if err != nil {
				return err
			}

			stderr := cmd.ErrOrStderr()
			opts.GainDB = gainDB
			req := tofile.Request{
				Engine: e, Source: src, Hint: srcHint, Probe: srcInfo,
				Options: opts, Plan: plan, Out: st.File, Path: st.Path,
				Mapper:     label.NewLogged(logger),
				NoMetadata: noTags,
				Loudness:   loudness == "analyze",
				Note:       func(n tofile.Note) { transcodeNote(stderr, logger, n) },
			}
			if transcodeHook != nil {
				transcodeHook(&req)
			}
			res, err := tofile.Run(cmd.Context(), req)
			if err != nil {
				st.Abort()
				return err
			}
			if err := commitOutput(st, force); err != nil {
				return err
			}
			clippingNote(stderr, res.Transcode)
			lossNote(stderr, res.Transcode)
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %s %d samples (%.3fs)\n",
				outPath, res.Transcode.Format, res.Transcode.Samples,
				durationSeconds(res.Transcode.Samples, res.Transcode.Format.Rate))
			return nil
		},
	}
	// Derived from the output table, like the inference error above: the
	// hand-written list had already fallen a format behind.
	cmd.Flags().StringVar(&formatName, "format", "",
		fmt.Sprintf("output format: %s (default: from output extension)", strings.Join(waxflow.OutputFormats(), ", ")))
	cmd.Flags().StringVar(&containerName, "container", "", "container override where the format has one: adts for aac/he-aac, progressive/fragmented for aac/he-aac/alac (flat vs CMAF MP4), mka for aac/he-aac/flac/wav, mka or webm for opus/vorbis, ogg for flac (default: the format's native container; a bare .aac output implies adts, and an mp4-family file output is progressive, the form players and taggers expect)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output if it exists")
	cmd.Flags().IntVar(&rate, "rate", 0, "output sample rate in Hz (default: source rate)")
	cmd.Flags().IntVar(&channels, "channels", 0, "output channel count: 1 or 2 (default: source layout)")
	cmd.Flags().IntVar(&bits, "bits", 0, "output bit depth, dithered when reducing (default: source depth)")
	cmd.Flags().Float64Var(&gainDB, "gain", 0, "gain in dB; positive gain engages the true-peak limiter")
	cmd.Flags().Var(&dynamics, "dynamics", "dynamics preset: off (default) or voice; acts on the post-gain signal")
	cmd.Flags().StringVar(&profileName, "resample-profile", "hq", "resampler quality: hq or fast")
	cmd.Flags().StringVar(&ditherName, "dither", "tpdf", "dither when reducing depth: tpdf, shaped, or off")
	levelFlags(cmd, &flacLevel, &wavpackLevel, &apeLevel)
	cmd.Flags().IntVar(&bitrateKbps, "bitrate", 0, "target bit rate in kbit/s, the quality anchor under --vbr on mp3 (mp3, opus, aac and he-aac outputs; default: the encoder's, 128 for mp3 and aac, 96 for opus, 64 for he-aac, 32 under --he-v2)")
	cmd.Flags().BoolVar(&vbr, "vbr", false, "encode at variable bit rate around --bitrate (mp3 and opus outputs; default: the encoder's, constant)")
	cmd.Flags().Float64Var(&vorbisQuality, "vorbis-quality", 0, "Vorbis quality on libvorbis's -1 to 10 scale, higher is larger and better (vorbis output only; default: the encoder's, 3)")
	cmd.Flags().IntVar(&opusComplexity, "opus-complexity", 0, "Opus encoder complexity 0-10, quality vs speed (opus output only; default: the encoder's, 5)")
	cmd.Flags().StringVar(&opusSignal, "opus-signal", "", "Opus content hint: auto, voice, or music (opus output only; default: the encoder's, auto)")
	cmd.Flags().BoolVar(&heV2, "he-v2", false, "encode HE-AAC v2: parametric stereo over a mono SBR core, for low-rate stereo (he-aac output only; stereo sources; default: the encoder's, v1)")
	cmd.Flags().StringVar(&loudness, "loudness", "", "analyze: two-pass loudness (exact gain to the ReplayGain reference, then RG tags measured from the output, or estimated from the source for Ogg-FLAC, WavPack, Monkey's Audio and fragmented MP4 outputs); the gain is measured after any --channels downmix")
	cmd.Flags().BoolVar(&noTags, "no-tags", false, "skip the metadata passthrough (tags, chapters, art)")
	return cmd
}

// stageOutput stages a command's output in a temp beside path, published
// there by [commitOutput]: a new file, or under force over path. An existing
// path without force is a usage mistake.
func stageOutput(path string, force bool) (*posixfs.Staged, error) {
	st, err := posixfs.Stage(path, 0o644, force)
	if err != nil {
		if !force && errors.Is(err, os.ErrExist) {
			return nil, errOutputExists(err)
		}
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating output", err)
	}
	return st, nil
}

// commitOutput publishes what [stageOutput] staged. Without force a path
// that appeared meanwhile is refused as an existing one is at the stage.
func commitOutput(st *posixfs.Staged, force bool) error {
	err := st.Commit()
	if err == nil {
		return nil
	}
	if !force && errors.Is(err, os.ErrExist) {
		return errOutputExists(err)
	}
	return waxerr.Wrap(waxerr.CodeOutputUnwritable, "publishing output", err)
}

// errOutputExists is the one refusal of an output path that exists, when
// --force does not allow replacing it.
func errOutputExists(err error) error {
	return waxerr.Wrap(waxerr.CodeInvalidRequest, "output exists (use --force to overwrite)", err)
}

// changedOr is a knob flag's value when the user set it and the zero value,
// the encoder's default, when not, so a bare command sends no knob the output
// format might not take.
func changedOr[T any](cmd *cobra.Command, flag string, v T) T {
	if cmd.Flags().Changed(flag) {
		return v
	}
	var zero T
	return zero
}

// levelFlags registers the compression level flags transcode and split share.
// Each reaches the options only when given, so none shows a default of its own.
func levelFlags(cmd *cobra.Command, flac, wavpack, ape *int) {
	cmd.Flags().IntVar(flac, "flac-level", 0, "FLAC compression level 0-8, size vs speed (flac output only; default: the encoder's, 5)")
	cmd.Flags().IntVar(wavpack, "wavpack-level", 0, "WavPack compression level: 1 fast, 2 normal, 3 high, 4 very high (wavpack output only; default: the encoder's, 2)")
	cmd.Flags().IntVar(ape, "ape-level", 0, "Monkey's Audio compression level: 1000 fast, 2000 normal, 3000 high (ape output only; default: the encoder's, 2000)")
}

// bitrateOption is --bitrate's Bitrate: 0 when the flag is unset, so the
// encoder default stands. A negative value, or one above outshape.MaxKbps, is
// refused before it is multiplied.
func bitrateOption(cmd *cobra.Command, kbps int) (int, error) {
	if !cmd.Flags().Changed("bitrate") {
		return 0, nil
	}
	if kbps < 0 || kbps > outshape.MaxKbps {
		return 0, waxerr.New(waxerr.CodeInvalidRequest,
			fmt.Sprintf("--bitrate %d: want 0 (the encoder's default) to %d kbit/s", kbps, outshape.MaxKbps))
	}
	return kbps * 1000, nil
}

// vorbisQualityOption is --vorbis-quality's VorbisQuality when given. The
// options read 0 as the encoder's default, so a typed 0 is refused rather than
// encoded at 3, as is a value off the -1 to 10 scale.
func vorbisQualityOption(cmd *cobra.Command, q float64) (float64, error) {
	if !cmd.Flags().Changed("vorbis-quality") {
		return 0, nil
	}
	if q == 0 || !(q >= -1 && q <= 10) { // a NaN fails both comparisons
		return 0, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf(
			"--vorbis-quality %g: want -1 to 10 other than 0 (omit the flag for the encoder's default, %g)",
			q, vorbis.DefaultQuality))
	}
	return q, nil
}

// flacLevelOption maps --flac-level onto FLACLevel. The options cannot say
// "level 0" with a plain 0, which selects the default, so the flag's 0 maps to
// FLACLevelFastest. A level outside 0 to 8 is refused.
func flacLevelOption(cmd *cobra.Command, level int) (int, error) {
	return levelFlag(cmd, "flac-level", level, 8, waxflow.FLACLevelDefault, waxflow.FLACLevelFastest)
}

// opusComplexityOption maps --opus-complexity onto OpusComplexity, the flag's
// 0 to OpusComplexityLowest, as flacLevelOption does.
func opusComplexityOption(cmd *cobra.Command, complexity int) (int, error) {
	return levelFlag(cmd, "opus-complexity", complexity, 10, waxflow.OpusComplexityDefault, waxflow.OpusComplexityLowest)
}

// levelFlag maps a 0-to-top flag onto an option whose zero value is the
// default: unset is dflt, 0 is zero (the sentinel), and a value outside 0 to
// top is refused.
func levelFlag(cmd *cobra.Command, flag string, v, top, dflt, zero int) (int, error) {
	switch {
	case !cmd.Flags().Changed(flag):
		return dflt, nil
	case v < 0 || v > top:
		return dflt, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("--%s %d: want 0 to %d", flag, v, top))
	case v == 0:
		return zero, nil
	}
	return v, nil
}

// knobFlags are the flags transcode and split spell the engine's knobs with.
var knobFlags = map[waxflow.Knob]string{
	waxflow.KnobBitrate:        "--bitrate",
	waxflow.KnobVBR:            "--vbr",
	waxflow.KnobVorbisQuality:  "--vorbis-quality",
	waxflow.KnobHEAACv2:        "--he-v2",
	waxflow.KnobFLACLevel:      "--flac-level",
	waxflow.KnobWavPackLevel:   "--wavpack-level",
	waxflow.KnobAPELevel:       "--ape-level",
	waxflow.KnobOpusComplexity: "--opus-complexity",
	waxflow.KnobOpusSignal:     "--opus-signal",
}

// flagWording words the engine's *KnobError and *RangeError in flag names
// through the server's sentences, so a flag reads as the parameter's refusal
// does. A bit rate on a quality-driven format names --vorbis-quality. The
// flags whose scale differs from the engine's refuse their range themselves.
var flagWording = outshape.NewWording(knobFlags, knobFlags[waxflow.KnobVorbisQuality])

// givenKnobRefusal refuses the first knob flag in among that the user gave and
// format does not take, whatever its value: a typed flag the format cannot use
// is a caller error even at the value that means the encoder's default.
func givenKnobRefusal(cmd *cobra.Command, format string, among waxflow.Knob) error {
	var given waxflow.Knob
	for k, flag := range knobFlags {
		if among&k != 0 && cmd.Flags().Changed(strings.TrimPrefix(flag, "--")) {
			given |= k
		}
	}
	return flagWording.Reword(waxflow.CheckKnobs(format, given))
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
