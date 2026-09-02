package mpc

import (
	"cmp"
	"fmt"
	"math"
	"slices"
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

// Chapters returns the SV8 chapter markers, resolved during the header parse,
// in start order.
func (d *Demuxer) Chapters() []container.Chapter { return d.chapters }

// maxChapterStart bounds a chapter's start sample: the bound the stream
// header's own count carries (sv8.go), past which the sample arithmetic
// overflows.
const maxChapterStart = 1 << 40

// readChapters reads the CT run the reference reads: the packets right after
// the seek table an SO packet points at, or else the run of CT packets
// ending at the end marker. A CT run interrupted by any other packet is not
// seen, which matches the reference rather than the spec's wider promise.
//
// The run is written in the order the editor's .ini listed the chapters, so
// it can be unsorted. The chapters are returned in start order, which the
// Chapterer contract promises: a caller derives a chapter's end from the next
// chapter's start and the mp4 chapter track needs starts that advance. The
// sort is stable so the order is a function of the file alone, the same
// answer the tag library gives. The spec's "presented in file order" is a
// player's rule and differs only on such a run.
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
		pkt := at
		at += int64(blk.hdrLen) + blk.payload
		if sample > maxChapterStart {
			if err := d.warn(pkt, "chapter start %d is past the %d-sample bound, skipped", sample, uint64(maxChapterStart)); err != nil {
				return err
			}
			continue
		}
		// The editor warns about a start past the end and writes it anyway;
		// a stream of unknown length has no end to be past.
		if end := s.count - s.begSilence; s.count != 0 && sample > end {
			if err := d.warn(pkt, "chapter starts at sample %d, past the end of the stream at %d", sample, end); err != nil {
				return err
			}
		}
		ch := container.Chapter{Start: time.Duration(float64(sample) / rate * float64(time.Second))}
		// Behind the gain and peak (two 16-bit fields) sits the tag: the
		// APEv2 header record without its preamble, then the items, no
		// footer, as the reference chapter editor writes it
		// (docs/notes/musepack-chapters.md). An untitled chapter carries no
		// tag bytes at all. The spec also allows the record as a footer
		// behind the items; no writer does that, and a tag not led by a
		// header record is reported rather than guessed at.
		if tag := p[n+4:]; len(tag) > 0 {
			count, ok := apev2.ParseHeaderRecord(tag)
			if !ok {
				if err := d.warn(pkt, "chapter tag does not start with an APEv2 header record, chapter listed untitled"); err != nil {
					return err
				}
			} else if items := apev2.ParseItems(tag[apev2.RecordLen:], count); len(items["TITLE"]) > 0 {
				ch.Title = items["TITLE"][0]
			}
		}
		d.chapters = append(d.chapters, ch)
	}
	if len(d.chapters) == maxChapters {
		if blk, ok := d.sv8Header(at); ok && blk.key == "CT" {
			if err := d.warn(at, "more than %d chapter packets, the rest are not read", maxChapters); err != nil {
				return err
			}
		}
	}
	slices.SortStableFunc(d.chapters, func(a, b container.Chapter) int { return cmp.Compare(a.Start, b.Start) })
	return nil
}
