package asf_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/asf"
	"github.com/colespringer/waxflow/internal/testutil"
)

// FuzzDemux asserts the hostile-input invariants on arbitrary bytes: no
// panics, errors instead of garbage tracks, bounded packet production, seeks
// that never come back past their target, and a frame count that walks,
// snapshots and restores without moving a landing.
func FuzzDemux(f *testing.F) {
	full := fixture(f, "sine-wmav2.wma")
	f.Add(full)
	f.Add(full[:1000])
	f.Add(full[:60])
	f.Add(fixture(f, "frag.wma"))
	f.Add(fixture(f, "tagged.wma"))
	f.Add(fixture(f, "chapters.wma"))
	f.Add(fixture(f, "mono-8k.wma"))
	// WMA Lossless, which is the one codec here whose track wiring parses a
	// config, derives a format from it and installs a sync predicate. Without
	// a seed that reaches 0x0163 the whole arm is unreachable from this
	// fuzzer, including the Fmt.Valid() invariant below.
	f.Add(fixture(f, "lossless-s16.wma"))
	f.Add(fixture(f, "lossless-s16.wma")[:2000])
	f.Add(fixture(f, "pro-s16.wma"))
	f.Add(fixture(f, "pro-s16.wma")[:2000])
	f.Add(fixture(f, "voice-mono.wma"))
	f.Add(fixture(f, "voice-mono.wma")[:1200])
	f.Add(appendSimpleIndex(fixture(f, "mono-8k.wma"), 8, 1_000_0000))
	// The bit reservoir, whose frame counts are the superframe headers'.
	f.Add(fixture(f, windowsFixture))
	f.Add(fixture(f, windowsFixture)[:30000])
	f.Add(guidHeader)

	f.Fuzz(func(t *testing.T, data []byte) {
		targets := []int64{0, 1000, 1 << 40}
		testutil.DemuxFuzz{
			Open: func(src container.Source, strict bool) (container.Demuxer, error) {
				return asf.NewDemuxer(src, &asf.DemuxerOptions{Strict: strict})
			},
			// A media object needs at least a length byte and a byte of its
			// own, so production is bounded by the input either way.
			MaxPackets: func(raw []byte) int64 { return int64(len(raw))/2 + 8 },
			Targets:    targets,
			// Landing past the target is only legitimate when the stream
			// itself starts after it.
			LateLanding: true,
			Opened: func(t testing.TB, dm container.Demuxer, _ bool) {
				d := dm.(*asf.Demuxer)
				if d.Tracks()[0].SamplesExact {
					t.Fatal("ASF states milliseconds; no track from it is sample-exact before a walk")
				}
				// The Chapterer contract: start order, the start-only form, no
				// negative time, and a list the cap bounds.
				chs := d.Chapters()
				if len(chs) > 1<<16 {
					t.Fatalf("%d chapters from %d bytes", len(chs), len(data))
				}
				for i, ch := range chs {
					if ch.Start < 0 || ch.End != 0 || (i > 0 && ch.Start < chs[i-1].Start) {
						t.Fatalf("chapter %d = %+v breaks the Chapterer contract", i, ch)
					}
				}
			},
			// A read to the end counts what a walk counts: a second open that
			// walks settles the same length, and the two counts snapshot alike.
			Read: func(t testing.TB, dm container.Demuxer, strict bool, end error) {
				if !errors.Is(end, io.EOF) {
					return
				}
				d := dm.(*asf.Demuxer)
				w, err := asf.NewDemuxer(container.BytesSource(data), &asf.DemuxerOptions{Strict: strict})
				if err != nil {
					t.Fatalf("a second open of the same bytes failed: %v", err)
				}
				if w.Walk(context.Background()) != nil {
					return
				}
				a, b := d.Tracks()[0], w.Tracks()[0]
				if a.Samples != b.Samples || a.SamplesExact != b.SamplesExact {
					t.Fatalf("a read to the end settled %d (exact %v), a walk %d (exact %v)",
						a.Samples, a.SamplesExact, b.Samples, b.SamplesExact)
				}
				if sa, sb := d.IndexSnapshot(), w.IndexSnapshot(); sa != nil && sb != nil && !bytes.Equal(sa, sb) {
					t.Fatal("a read to the end and a walk took different counts")
				}
			},
			After: func(t testing.TB, _ []byte, dm container.Demuxer, strict bool) {
				d := dm.(*asf.Demuxer)
				if err := d.Walk(context.Background()); err != nil {
					return
				}
				if !d.Walked() {
					t.Fatal("a walk that returned nil is not walked")
				}
				if tr := d.Tracks()[0]; tr.SamplesExact && (tr.Samples < 0 || tr.SamplesAdvisory) {
					t.Fatalf("the walk settled %d samples, exact %v advisory %v", tr.Samples, tr.SamplesExact, tr.SamplesAdvisory)
				}
				blob := d.IndexSnapshot()
				if blob == nil {
					return
				}
				// A restore of its own snapshot, accepted or refused, lands
				// where the count it was taken from does.
				e, err := asf.NewDemuxer(container.BytesSource(data), &asf.DemuxerOptions{Strict: strict})
				if err != nil {
					t.Fatalf("a second open of the same bytes failed: %v", err)
				}
				e.RestoreIndex(blob)
				for _, target := range targets {
					want, errD := d.SeekSample(context.Background(), 0, target)
					got, errE := e.SeekSample(context.Background(), 0, target)
					if (errD == nil) != (errE == nil) || got != want {
						t.Fatalf("seek to %d: the restored count landed at %d (%v), the walked one at %d (%v)",
							target, got, errE, want, errD)
					}
				}
			},
		}.Run(t, data)
	})
}

// FuzzRestoreIndex hands a clean file arbitrary sidecar blobs. A damaged
// blob fails its CRC or its checks, so every landing and the walked length
// are the ones a count of the data gives. Re-signed, the same bytes are a
// blob built to pass the CRC, which the probes check only in part: it can
// move a landing or the settled length, but never lands a seek past its
// target or fails a read.
func FuzzRestoreIndex(f *testing.F) {
	raw := fixture(f, windowsFixture)
	d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
	if err != nil {
		f.Fatal(err)
	}
	if err := d.Walk(context.Background()); err != nil {
		f.Fatal(err)
	}
	blob := d.IndexSnapshot()
	if blob == nil {
		f.Fatal("no snapshot to seed with")
	}
	f.Add(blob)
	f.Add(blob[:len(blob)/2])
	f.Add(append(append([]byte(nil), blob[:9]...), 0x80))
	f.Add([]byte("WXASFIDX"))
	total := d.Tracks()[0].Samples
	targets := []int64{0, total / 3, total / 2, total - 1, 1 << 40}
	want := make([]int64, len(targets))
	for i, target := range targets {
		if want[i], err = d.SeekSample(context.Background(), 0, target); err != nil {
			f.Fatal(err)
		}
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := asf.NewDemuxer(container.BytesSource(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		restored := d.RestoreIndex(b)
		for i, target := range targets {
			landed, err := d.SeekSample(context.Background(), 0, target)
			if err != nil {
				t.Fatalf("seek to %d: %v", target, err)
			}
			if landed != want[i] {
				t.Fatalf("seek to %d landed at %d (restored %v), a count of the data lands at %d",
					target, landed, restored, want[i])
			}
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatalf("read after a seek to %d: %v", target, err)
			}
		}
		if err := d.Walk(context.Background()); err != nil {
			t.Fatalf("walk: %v", err)
		}
		if got := d.Tracks()[0]; got.Samples != total || !got.SamplesExact {
			t.Fatalf("the walk settled %d (exact %v, restored %v), a count of the data %d",
				got.Samples, got.SamplesExact, restored, total)
		}

		if len(b) < 4 {
			return
		}
		body := bytes.Clone(b[:len(b)-4])
		forged := binary.LittleEndian.AppendUint32(body, crc32.ChecksumIEEE(body))
		if d, err = asf.NewDemuxer(container.BytesSource(raw), nil); err != nil {
			t.Fatal(err)
		}
		restored = d.RestoreIndex(forged)
		for _, target := range targets {
			landed, err := d.SeekSample(context.Background(), 0, target)
			if err != nil || landed > target {
				t.Fatalf("re-signed (restored %v): seek to %d landed at %d (%v)", restored, target, landed, err)
			}
			var pkt container.Packet
			if err := d.ReadPacket(&pkt); err != nil {
				t.Fatalf("re-signed (restored %v): read after a seek to %d: %v", restored, target, err)
			}
		}
		if err := d.Walk(context.Background()); err != nil {
			t.Fatalf("re-signed (restored %v): walk: %v", restored, err)
		}
	})
}
