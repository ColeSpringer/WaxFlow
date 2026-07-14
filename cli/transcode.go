package cli

import (
	"errors"
	"fmt"
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
	"github.com/colespringer/waxflow/waxerr"
)

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
		Args: cobra.ExactArgs(2),
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
			out, err := os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				if !force && errors.Is(err, os.ErrExist) {
					return waxerr.Wrap(waxerr.CodeInvalidRequest, "output exists (use --force to overwrite)", err)
				}
				return waxerr.Wrap(waxerr.CodeOutputUnwritable, "creating output", err)
			}

			if loudness != "" && loudness != "analyze" {
				out.Close()
				os.Remove(writePath)
				return waxerr.New(waxerr.CodeInvalidRequest,
					fmt.Sprintf("loudness %q: want analyze (or omit)", loudness))
			}
			if loudness == "analyze" && gainDB != 0 {
				out.Close()
				os.Remove(writePath)
				return waxerr.New(waxerr.CodeInvalidRequest, "--loudness analyze replaces --gain; drop one")
			}

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

			e := waxflow.New(waxflow.WithLogger(logger))

			// The file-output passthrough matrix: full tags, chapters,
			// and art flow onto the output (the MP4 muxer embeds them;
			// every other format gets the mapping post-pass below).
			mapper := label.New()
			var info *meta.Info
			if !noTags {
				if m, merr := mapper.Read(cmd.Context(), src, srcHint, meta.ReadOptions{Pictures: true}); merr == nil {
					info = m
					for _, warn := range m.Warnings {
						fmt.Fprintf(cmd.ErrOrStderr(), "metadata: %s\n", warn)
					}
				}
			}
			analyzeLoudness := loudness == "analyze"
			var srcRes *waxflow.AnalyzeResult
			if analyzeLoudness {
				res, aerr := e.Analyze(cmd.Context(), src, srcHint, waxflow.AnalyzeOptions{})
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
			//
			// isMP4 must mean "written by the mp4 muxer" (fragmented default or
			// progressive), the only path that embeds tags in moov and takes the
			// mp4-specific ReplayGain patch. AAC also rides in adts (elementary)
			// and mka (Matroska), which are NOT MP4: patching them as MP4 would
			// fail and delete the output. ALAC is always MP4.
			isMP4 := outFormat == "alac" ||
				(outFormat == "aac" && (containerName == "" || containerName == "progressive"))
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
				rg, outLUFS := predictedRG(srcRes, gainDB)
				tags = append(tags, rg...)
				fmt.Fprintf(cmd.ErrOrStderr(), "loudness: output %.2f LUFS, %s / %s\n",
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

			res, err := e.Transcode(cmd.Context(), src, srcHint, out, waxflow.TranscodeOptions{
				Format:          outFormat,
				Container:       containerName,
				Rate:            rate,
				Channels:        channels,
				BitDepth:        bits,
				GainDB:          gainDB,
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
				Tags:            tags,
				Chapters:        chapters,
				Art:             art,
			})
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
				if rg, err = analyzeOutputRG(cmd, e, writePath, extHint(outPath), isMP4, srcRes, gainDB); err != nil {
					os.Remove(writePath)
					return err
				}
			}
			// embedsTags names the outputs whose muxer already wrote the tags at
			// mux time, so the post-pass must skip them to avoid a redundant (or
			// conflicting) second write. The MP4 muxers embed an ilst in moov;
			// the Ogg muxer embeds the comment header at Begin (and the label
			// mapper has no Ogg-FLAC writer anyway). Every other output, incl.
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
				if err := os.Rename(writePath, outPath); err != nil {
					os.Remove(writePath)
					return waxerr.Wrap(waxerr.CodeOutputUnwritable, "replacing output", err)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s: %s %d samples (%.3fs)\n",
				outPath, res.Format, res.Samples, durationSeconds(res.Samples, res.Format.Rate))
			return nil
		},
	}
	cmd.Flags().StringVar(&formatName, "format", "", "output format: wav, aiff, flac, mp3, aac, alac, or opus (default: from output extension)")
	cmd.Flags().StringVar(&containerName, "container", "", "container override where the format has one: adts for aac, progressive for aac/alac (flat non-streaming MP4), mka/webm for opus/aac/flac/wav, ogg for flac (default: the format's native container; a bare .aac output implies adts)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output if it exists")
	cmd.Flags().IntVar(&rate, "rate", 0, "output sample rate in Hz (default: source rate)")
	cmd.Flags().IntVar(&channels, "channels", 0, "output channel count: 1 or 2 (default: source layout)")
	cmd.Flags().IntVar(&bits, "bits", 0, "output bit depth, dithered when reducing (default: source depth)")
	cmd.Flags().Float64Var(&gainDB, "gain", 0, "gain in dB; positive gain engages the true-peak limiter")
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
	cmd.Flags().StringVar(&loudness, "loudness", "", "analyze: two-pass loudness (exact gain to the ReplayGain reference, measured RG tags on the output)")
	cmd.Flags().BoolVar(&noTags, "no-tags", false, "skip the metadata passthrough (tags, chapters, art)")
	return cmd
}

// analyzeOutputRG returns (after patching MP4 headers in place) the
// ReplayGain tags for the finished output: measured from the file where
// the engine can decode it back, derived from the source measurement
// plus the applied gain for fragmented MP4, which has no read path
// (exact for lossless ALAC, within the encoder's fraction of a dB for
// AAC; positive gain caps the derived peak at the limiter ceiling).
// predictedRG estimates the output ReplayGain from the source loudness analysis
// and the gain being applied: after normalization the output sits at
// srcLUFS+gain, and its true peak follows the gain (clamped to the ceiling when
// boosting). It is the estimate the MP4 path patches into its placeholders and
// the value the Ogg path embeds at Begin (Ogg cannot be patched afterward).
func predictedRG(srcRes *waxflow.AnalyzeResult, gainDB float64) (rg []container.Tag, outLUFS float64) {
	outLUFS = math.Inf(-1)
	if !math.IsInf(srcRes.IntegratedLUFS, -1) {
		outLUFS = srcRes.IntegratedLUFS + gainDB
	}
	outTP := srcRes.TruePeakDB + gainDB
	if gainDB > 0 {
		outTP = min(outTP, gain.DefaultCeilingDB)
	}
	return meta.ReplayGainTags(outLUFS, outTP), outLUFS
}

func analyzeOutputRG(cmd *cobra.Command, e *waxflow.Engine, path, hint string, isMP4 bool, srcRes *waxflow.AnalyzeResult, gainDB float64) ([]container.Tag, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeOutputUnwritable, "reopening output", err)
	}
	defer f.Close()
	var rg []container.Tag
	var outLUFS float64
	if isMP4 {
		// The MP4 output edit list already normalized to the target; the RG is
		// predicted from the source and gain, then patched into placeholders.
		rg, outLUFS = predictedRG(srcRes, gainDB)
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
