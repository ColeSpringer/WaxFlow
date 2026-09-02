package mpc

import (
	"fmt"
	"math"
	"time"

	"github.com/colespringer/waxflow/codec/musepack"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/internal/apev2"
)

// Tags returns the APEv2 tag's fields under canonical uppercase keys, with
// the stream's own replay gain filled in where the tag has none. The
// spellings are the ones the tag library projects for the same fields, so the
// fold in the engine sees one value per key.
func (d *Demuxer) Tags() map[string][]string {
	out := make(map[string][]string, len(d.tags)+4)
	for k, v := range d.tags {
		out[k] = v
	}
	add := func(key string, gain, peak uint16) {
		if gain == 0 {
			return
		}
		if _, ok := out[key+"_GAIN"]; !ok {
			out[key+"_GAIN"] = []string{fmt.Sprintf("%+.2f dB", oldGainRef-float64(gain)/256)}
		}
		if _, ok := out[key+"_PEAK"]; !ok && peak != 0 {
			// The stored peak is 20*log10 of the 16-bit sample peak in Q8.8;
			// the tag convention is linear, full scale 1.0.
			out[key+"_PEAK"] = []string{fmt.Sprintf("%.6f", math.Pow(10, float64(peak)/256/20)/32768)}
		}
	}
	add("REPLAYGAIN_TRACK", d.rg.titleGain, d.rg.titlePeak)
	add("REPLAYGAIN_ALBUM", d.rg.albumGain, d.rg.albumPeak)
	if len(out) == 0 {
		return nil
	}
	return out
}

// Chapters returns the SV8 chapter markers, resolved during the header parse.
func (d *Demuxer) Chapters() []container.Chapter { return d.chapters }

// readChapters reads the CT run the reference reads: the packets right after
// the seek table an SO packet points at, or else the run of CT packets
// ending at the end marker. A CT run interrupted by any other packet is not
// seen, which matches the reference rather than the spec's wider promise.
func (d *Demuxer) readChapters() error {
	s := &d.sv8
	at := s.ctAt
	if s.soAt >= 0 && s.stHead == s.soAt {
		blk, ok := d.sv8Header(s.stHead)
		if !ok {
			return nil
		}
		at = s.stHead + int64(blk.hdrLen) + blk.payload
	}
	if at < 0 {
		return nil
	}
	rate := float64(d.cfg.Rate)
	for len(d.chapters) < maxChapters {
		blk, ok := d.sv8Header(at)
		if !ok || blk.key != "CT" {
			break
		}
		if blk.payload > maxHeaderPacket {
			if err := d.warn(at, "chapter packet of %d bytes exceeds the %d-byte bound, skipped", blk.payload, maxHeaderPacket); err != nil {
				return err
			}
			break
		}
		p := d.w.BytesAt(at+int64(blk.hdrLen), int(blk.payload))
		if int64(len(p)) != blk.payload {
			break
		}
		sample, n, ok := musepack.ReadVarint(p)
		if !ok || len(p) < n+4 {
			if err := d.warn(at, "chapter packet truncated"); err != nil {
				return err
			}
			break
		}
		ch := container.Chapter{Start: time.Duration(float64(sample) / rate * float64(time.Second))}
		// The gain and peak (two 16-bit fields) are followed by raw APEv2
		// items without preamble or footer.
		if items := apev2.ParseItems(p[n+4:]); len(items["TITLE"]) > 0 {
			ch.Title = items["TITLE"][0]
		}
		d.chapters = append(d.chapters, ch)
		at += int64(blk.hdrLen) + blk.payload
	}
	return nil
}
