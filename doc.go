// Package waxflow will expose the transcoding engine facade (New, Probe,
// Transcode, OpenStream), the library-first entry point to the pure-Go
// audio pipeline: request -> decode -> DSP -> encode -> stream.
//
// The facade lands with the audio core. Through v1.0 the public,
// stdlib-only packages live under this module:
//
//	waxerr        - error taxonomy: codes, sentinels, exit-code contract
//	audio         - PCM model (planar buffers, formats, layouts)
//	dsp/...       - resample, mix, gain, dither, loudness, psy, fft
//	codec/...     - pcm, flac, alac, mp3, aac, opus, vorbis
//	container/... - riff, aiff, ogg, mp4, mka, adts, mpa, flacn
//	format        - probe + registry + Open
//	source        - source-ref model + Resolver interface
//	server        - HTTP service
//	client        - Go client for the HTTP service
//
// The codec/DSP tree is stdlib-only, enforced in CI by `make depcheck`.
// See docs/adr/ for the architecture decision records that pin the
// invariants this module promises.
package waxflow
