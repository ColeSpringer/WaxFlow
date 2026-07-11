package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/dsp/resample"
	"github.com/colespringer/waxflow/internal/config"
	"github.com/colespringer/waxflow/source"
	"github.com/colespringer/waxflow/waxerr"
)

// doctorCheck is one health-check result. Status is "ok", "warn", "fail",
// or "skip"; err carries the underlying failure for exit-code
// classification.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	err    error
}

// doctorReport is the --json body.
type doctorReport struct {
	SchemaVersion int           `json:"schemaVersion"`
	Healthy       bool          `json:"healthy"`
	Checks        []doctorCheck `json:"checks"`
}

// newDoctorCmd diagnoses the local environment a daemon would run in:
// configuration resolves, every configured root opens and reads, the
// cache/data/scratch directories accept writes, the WaxBin catalog opens
// (flavor builds with catalogDB set), a quick self-bench confirms the box
// transcodes faster than realtime, and the absence of ffmpeg is confirmed
// to be fine. It never contacts a running daemon; that is `waxflow ping`.
func newDoctorCmd(flavor Flavor) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the local environment a waxflow daemon needs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			logger, err := newLogger(cmd.ErrOrStderr(), cfg)
			if err != nil {
				return err
			}

			checks := []doctorCheck{configCheck(cmd)}
			checks = append(checks, rootChecks(cfg)...)
			checks = append(checks, dirChecks(cfg)...)
			checks = append(checks, catalogCheck(cfg, flavor, logger))
			checks = append(checks, benchChecks(cfg)...)
			checks = append(checks, ffmpegCheck())

			var nfail int
			var firstFail error
			for _, c := range checks {
				if c.Status == "fail" {
					nfail++
					if firstFail == nil {
						firstFail = c.err
					}
				}
			}

			if jsonOut {
				report := doctorReport{SchemaVersion: 1, Healthy: nfail == 0, Checks: checks}
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
					return err
				}
			} else {
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				for _, c := range checks {
					// Deliberate asymmetry: healthy states stay quiet
					// lowercase so the one actionable state jumps out of
					// the column. The --json shape is uniformly lowercase.
					status := c.Status
					if status == "fail" {
						status = "FAIL"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\n", status, c.Name, c.Detail)
				}
				w.Flush()
			}
			if nfail > 0 {
				code := waxerr.CodeOf(firstFail)
				if code == "" {
					code = waxerr.CodeInternal
				}
				return waxerr.Wrap(code, fmt.Sprintf("doctor: %d of %d checks failed", nfail, len(checks)), firstFail)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON")
	return cmd
}

// configCheck reports where the effective configuration came from.
func configCheck(cmd *cobra.Command) doctorCheck {
	path, _ := cmd.Flags().GetString("config")
	if path == "" {
		path = os.Getenv("WAXFLOW_CONFIG")
	}
	detail := "no config file (WAXFLOW_* environment + defaults)"
	if path != "" {
		detail = "loaded " + path
	}
	return doctorCheck{Name: "config", Status: "ok", Detail: detail}
}

// rootChecks opens every configured root the way the daemon does and
// additionally reads each root directory, so an unreadable mount (a
// permissions mistake, a container volume that failed to attach) fails
// here rather than on the first stream request.
func rootChecks(cfg config.Config) []doctorCheck {
	if len(cfg.Roots) == 0 {
		return []doctorCheck{{
			Name:   "roots",
			Status: "warn",
			Detail: "no roots configured; only upload: sources will resolve",
		}}
	}
	roots, err := source.OpenRoots(configRoots(cfg), cfg.ResolvedSourceMaxBytes())
	if err != nil {
		// One bad root fails the whole set at daemon startup too; report
		// the set-level error once.
		return []doctorCheck{{Name: "roots", Status: "fail", Detail: err.Error(), err: err}}
	}
	roots.Close()
	checks := make([]doctorCheck, 0, len(cfg.Roots))
	for _, r := range cfg.Roots {
		checks = append(checks, readRootCheck(r))
	}
	return checks
}

// readRootCheck lists one entry of the root directory: os.OpenRoot
// succeeds on a directory the daemon cannot actually read (execute
// permission without read), so opening is not enough.
func readRootCheck(r config.Root) doctorCheck {
	name := "root:" + r.Name
	dir, err := os.Open(r.Path)
	if err != nil {
		return doctorCheck{Name: name, Status: "fail", Detail: err.Error(),
			err: waxerr.Wrap(waxerr.CodeSourceUnreadable, "root "+r.Name, err)}
	}
	defer dir.Close()
	names, err := dir.Readdirnames(1)
	if err != nil && err != io.EOF {
		return doctorCheck{Name: name, Status: "fail", Detail: "directory not readable: " + err.Error(),
			err: waxerr.Wrap(waxerr.CodeSourceUnreadable, "root "+r.Name, err)}
	}
	if len(names) == 0 {
		return doctorCheck{Name: name, Status: "warn", Detail: r.Path + " is readable but empty"}
	}
	return doctorCheck{Name: name, Status: "ok", Detail: r.Path + " readable"}
}

// dirChecks verifies the three directories the daemon writes: the
// transcode cache, the data dir (job store), and the scratch dir (upload
// spool). Each is created if missing, exactly as the daemon would, then
// probed with a real write.
func dirChecks(cfg config.Config) []doctorCheck {
	var checks []doctorCheck
	cacheDir, err := cfg.ResolvedCacheDir()
	if err != nil {
		checks = append(checks, doctorCheck{Name: "cache", Status: "fail", Detail: err.Error(), err: err})
	} else {
		checks = append(checks, writableDirCheck("cache", cacheDir))
	}
	dataDir, err := cfg.ResolvedDataDir()
	if err != nil {
		checks = append(checks, doctorCheck{Name: "data", Status: "fail", Detail: err.Error(), err: err})
	} else {
		checks = append(checks, writableDirCheck("data", dataDir))
	}
	checks = append(checks, writableDirCheck("scratch", cfg.ResolvedScratchDir()))
	return checks
}

// writableDirCheck creates the directory if needed and round-trips a
// probe file through it.
func writableDirCheck(name, dir string) doctorCheck {
	fail := func(err error) doctorCheck {
		return doctorCheck{Name: name, Status: "fail", Detail: dir + ": " + err.Error(),
			err: waxerr.Wrap(waxerr.CodeOutputUnwritable, name+" dir", err)}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	f, err := os.CreateTemp(dir, ".waxflow-doctor-*")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(f.Name())
	_, werr := f.Write([]byte("waxflow doctor write probe\n"))
	cerr := f.Close()
	if werr != nil {
		return fail(werr)
	}
	if cerr != nil {
		return fail(cerr)
	}
	return doctorCheck{Name: name, Status: "ok", Detail: dir + " writable"}
}

// catalogCheck opens the WaxBin catalog when one is configured. The
// stock build fails loudly here, the same refusal the daemon gives; the
// resolver flavor actually opens the database read-only (daemon=false,
// so no poll goroutine starts).
func catalogCheck(cfg config.Config, flavor Flavor, logger *slog.Logger) doctorCheck {
	if cfg.CatalogDB == "" {
		detail := "no catalogDB configured"
		if flavor.OpenResolver == nil {
			detail += " (pid: sources need the waxbin flavor)"
		}
		return doctorCheck{Name: "catalog", Status: "skip", Detail: detail}
	}
	_, closeFn, err := flavor.openResolver(cfg, logger, false)
	if err != nil {
		return doctorCheck{Name: "catalog", Status: "fail", Detail: err.Error(), err: err}
	}
	closeFn()
	return doctorCheck{Name: "catalog", Status: "ok", Detail: cfg.CatalogDB + " opens read-only"}
}

// benchChecks transcodes a short synthesized WAV through the same engine
// the daemon uses, once to Opus (the primary lossy path, including the
// 44.1 to 48 kHz resample, run on the CONFIGURED resample profile so
// the measurement matches what the daemon will actually do) and once to
// FLAC (the lossless path), and reports the realtime factor. A box that
// cannot clear 2x has no headroom for even one live stream; the warn
// points at the remedies the configuration has not already taken.
func benchChecks(cfg config.Config) []doctorCheck {
	const seconds = 2
	src := benchWAV(seconds)
	eng := waxflow.New()
	profile := resample.Profile(cfg.ResolvedResampleProfile())
	var checks []doctorCheck
	for _, formatName := range []string{"opus", "flac"} {
		name := "bench:" + formatName
		start := time.Now()
		_, err := eng.Transcode(context.Background(), bytesSource{bytes.NewReader(src)}, "wav",
			io.Discard, waxflow.TranscodeOptions{Format: formatName, ResampleProfile: profile})
		elapsed := time.Since(start)
		if err != nil {
			checks = append(checks, doctorCheck{Name: name, Status: "fail", Detail: err.Error(), err: err})
			continue
		}
		// A coarse platform timer could measure the transcode as zero,
		// which would divide to +Inf and dodge the warn gate below; the
		// millisecond floor also keeps the printed factor sane.
		elapsed = max(elapsed, time.Millisecond)
		xrt := seconds * float64(time.Second) / float64(elapsed)
		status, note := "ok", ""
		if xrt < 2 {
			status = "warn"
			if profile == resample.Fast {
				note = "; below 2x on the fast resample profile, try a lower opus complexity"
			} else {
				note = "; below 2x, try resampleProfile=fast and a lower opus complexity"
			}
		}
		checks = append(checks, doctorCheck{Name: name, Status: status,
			Detail: fmt.Sprintf("%ds of 44.1 kHz stereo in %s (%.0fx realtime)%s",
				seconds, elapsed.Round(time.Millisecond), xrt, note)})
	}
	return checks
}

// ffmpegCheck confirms the runtime promise: WaxFlow needs no ffmpeg. The
// check exists so an operator migrating from an ffmpeg-based stack sees
// the absence acknowledged rather than wondering whether it was missed.
func ffmpegCheck() doctorCheck {
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return doctorCheck{Name: "ffmpeg", Status: "ok",
			Detail: "found at " + path + " (unused: waxflow runs pure Go)"}
	}
	return doctorCheck{Name: "ffmpeg", Status: "ok",
		Detail: "not installed, and none needed (pure-Go runtime)"}
}

// bytesSource adapts an in-memory buffer to container.Source.
type bytesSource struct{ *bytes.Reader }

func (b bytesSource) Size() int64 { return b.Reader.Size() }

// benchWAV synthesizes a deterministic 44.1 kHz stereo 16-bit WAV: a
// frequency sweep with a little noise, busy enough that the encoders do
// real work.
func benchWAV(seconds int) []byte {
	const rate, channels = 44100, 2
	frames := rate * seconds
	dataLen := frames * channels * 2
	buf := make([]byte, 44+dataLen)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataLen))
	copy(buf[8:], "WAVE")
	copy(buf[12:], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[22:], channels)
	binary.LittleEndian.PutUint32(buf[24:], rate)
	binary.LittleEndian.PutUint32(buf[28:], rate*channels*2)
	binary.LittleEndian.PutUint16(buf[32:], channels*2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataLen))

	rng := uint32(0x9E3779B9)
	noise := func() float64 {
		rng ^= rng << 13
		rng ^= rng >> 17
		rng ^= rng << 5
		return (float64(rng)/float64(math.MaxUint32) - 0.5) * 0.05
	}
	var phaseL, phaseR float64
	for i := 0; i < frames; i++ {
		t := float64(i) / rate
		f := 220 + 3000*t/float64(seconds)
		phaseL += 2 * math.Pi * f / rate
		phaseR += 2 * math.Pi * f * 1.5 / rate
		l := 0.5*math.Sin(phaseL) + noise()
		r := 0.5*math.Sin(phaseR) + noise()
		off := 44 + i*channels*2
		binary.LittleEndian.PutUint16(buf[off:], uint16(int16(l*32767)))
		binary.LittleEndian.PutUint16(buf[off+2:], uint16(int16(r*32767)))
	}
	return buf
}
