package waxflow_test

import (
	"bytes"
	"context"
	"math"
	"testing"

	waxflow "github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/container"
)

// TestTranscodeOpusChannels pins the opus output row's channel policy: a
// source with more than two channels downmixes to stereo only when the
// request leaves the channel count unset, an explicit request for mono or
// stereo is always honored, and an explicit request the encoder cannot
// satisfy fails loudly instead of being silently rewritten.
func TestTranscodeOpusChannels(t *testing.T) {
	e := waxflow.New()
	synth := func(channels int) []byte {
		const rate, n = 48000, 48000
		f := audio.Format{Rate: rate, Channels: channels, Layout: audio.DefaultLayout(channels), Type: audio.Float, BitDepth: 32}
		samples := make([]float32, channels*n)
		for i := 0; i < n; i++ {
			for c := 0; c < channels; c++ {
				samples[i*channels+c] = float32(0.2 * math.Sin(2*math.Pi*(220+110*float64(c))*float64(i)/rate))
			}
		}
		return synthWAVFromSamples(t, f, samples)
	}
	outChannels := func(t *testing.T, og []byte) int {
		t.Helper()
		med, err := e.OpenStream(container.BytesSource(og), "opus")
		if err != nil {
			t.Fatal(err)
		}
		defer med.Close()
		return med.Info().Default().Fmt.Channels
	}

	quad := synth(4)
	stereo := synth(2)
	for _, tc := range []struct {
		name    string
		src     []byte
		request int
		want    int
		wantErr bool
	}{
		{"multichannel defaults to stereo", quad, 0, 2, false},
		{"multichannel honors explicit mono", quad, 1, 1, false},
		{"multichannel honors explicit stereo", quad, 2, 2, false},
		{"stereo honors explicit mono", stereo, 1, 1, false},
		{"unsupported explicit count fails loudly", quad, 4, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := e.Transcode(context.Background(), container.BytesSource(tc.src), "wav", &out,
				waxflow.TranscodeOptions{Format: "opus", Channels: tc.request})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Channels=%d: want an error, got %d-channel output", tc.request, outChannels(t, out.Bytes()))
				}
				return
			}
			if err != nil {
				t.Fatalf("Channels=%d: %v", tc.request, err)
			}
			if got := outChannels(t, out.Bytes()); got != tc.want {
				t.Errorf("Channels=%d: output has %d channels, want %d", tc.request, got, tc.want)
			}
		})
	}
}
