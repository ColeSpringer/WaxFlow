package waxflow_test

import (
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/adts"
	"github.com/colespringer/waxflow/container/aiff"
	"github.com/colespringer/waxflow/container/flacn"
	"github.com/colespringer/waxflow/container/mka"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/container/mpa"
	"github.com/colespringer/waxflow/container/ogg"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/container/wv"
)

// Muxer refusals travel to end users as verbatim text just like demuxer
// parse errors (format.TestParseErrorsUsePublicContainerNames pins that
// side), so each container package must stamp mux errors with its public
// container name too, not the Go package name ("mpa", "flacn", "riff").
// Begin with no tracks is the one refusal every muxer shares.
func TestMuxerErrorsUsePublicContainerNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(dst io.Writer) container.Muxer
	}{
		{"wav", func(d io.Writer) container.Muxer { return riff.NewMuxer(d, nil) }},
		{"aiff", func(d io.Writer) container.Muxer { return aiff.NewMuxer(d) }},
		{"flac", func(d io.Writer) container.Muxer { return flacn.NewMuxer(d, nil) }},
		{"ogg", func(d io.Writer) container.Muxer { return ogg.NewMuxer(d, nil) }},
		{"mp4", func(d io.Writer) container.Muxer { return mp4.NewMuxer(d, nil) }},
		{"mka", func(d io.Writer) container.Muxer { return mka.NewMuxer(d, nil) }},
		{"adts", func(d io.Writer) container.Muxer { return adts.NewMuxer(d) }},
		{"mp3", func(d io.Writer) container.Muxer { return mpa.NewMuxer(d, &mpa.MuxerOptions{}) }},
		{"wavpack", func(d io.Writer) container.Muxer { return wv.NewMuxer(d, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build(io.Discard).Begin(nil)
			if err == nil {
				t.Fatal("Begin with no tracks succeeded")
			}
			if !strings.HasPrefix(err.Error(), tc.name+": ") {
				t.Fatalf("error %q does not carry the public container name %q", err, tc.name)
			}
		})
	}
}
