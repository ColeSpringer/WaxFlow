// Package waxflow exposes the transcoding engine facade (New, Probe,
// Transcode, OpenStream), the library-first entry point to the pure-Go
// audio pipeline: request -> decode -> DSP -> encode -> stream.
//
// The public, stdlib-only packages live under this module, whose
// require block is empty by construction (importing any of them pulls
// in nothing):
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
// The codec/DSP tree is stdlib-only, enforced in CI by `make depcheck`
// and structurally by this module's empty require block; the CLI/daemon
// binary (cobra + waxlabel) lives in the nested cli/ module, the WaxBin
// resolver flavor in resolver/, and third-party-oracle tests in
// oracletest/. See docs/adr/ for the architecture decision records that
// pin the invariants this module promises.
package waxflow
