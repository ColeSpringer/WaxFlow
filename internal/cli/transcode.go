package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/dither"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/waxerr"
)

func newTranscodeCmd() *cobra.Command {
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
	cmd := &cobra.Command{
		Use:   "transcode <input> <output>",
		Short: "Transcode an audio file locally through the engine",
		Long: `Transcode decodes the input and writes it to the output path via the
same engine the daemon uses: decode -> DSP -> encode -> mux. The output
format comes from --format or the output extension. Without conversion
flags the transcode is a bit-exact container rewrite; --rate,
--channels, --bits and --gain insert only the DSP nodes they need
(resampling, downmix, gain with true-peak limiting, dither).`,
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
			if outFormat == "" {
				// The engine's output table is the single source of truth
				// for extensions, so the CLI cannot drift from it.
				outFormat = waxflow.OutputFormatForExt(extHint(args[1]))
				if outFormat == "" {
					return waxerr.New(waxerr.CodeInvalidRequest,
						fmt.Sprintf("cannot infer output format from %q; pass --format (%s)",
							filepath.Base(args[1]), strings.Join(waxflow.OutputFormats(), ", ")))
				}
			}
			// A bare .aac output means the ADTS elementary stream (the
			// .m4a extension is the fMP4 default); an explicit
			// --container always wins.
			if containerName == "" && outFormat == "aac" && extHint(args[1]) == "aac" {
				containerName = "adts"
			}

			src, cleanup, err := openSource(args[0])
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
			res, err := e.Transcode(cmd.Context(), src, extHint(args[0]), out, waxflow.TranscodeOptions{
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
	cmd.Flags().StringVar(&containerName, "container", "", "container override where the format has one: adts for aac (default: the format's native container; a bare .aac output implies adts)")
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
	return cmd
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
