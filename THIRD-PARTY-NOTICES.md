# Third-party notices

Attributions for permissively licensed (Tier A) code studied closely or
ported into WaxFlow, per [ADR-0001](docs/adr/0001-clean-room-policy.md).
Module dependencies (e.g. spf13/cobra) carry their own licenses in the
module cache and are not vendored here.

Entries follow this format:

> **component**: derived from *project* (license), upstream URL, commit.

> **codec/flac encoder strategy**: the analysis design (Tukey
> apodization defaults, precision-15 coefficient quantization with error
> feedback, mean-based Rice parameter estimation, and the level presets'
> block/order/partition shape) was studied from *libFLAC* (BSD-3-Clause),
> https://github.com/xiph/flac, as permitted for Tier A sources. The
> implementation is original code written against RFC 9639; no source
> was ported line-by-line.

> **codec/mp3 decoder**: the granule pipeline structure and the ported
> table data (the Huffman tree tables in huffman.go and the ISO Table
> B.3 synthesis window in synthwin.go) derive from the public-domain
> PDMP3 via *hajimehoshi/go-mp3* (Apache-2.0),
> https://github.com/hajimehoshi/go-mp3, v0.3.4. The low sampling
> frequency handling (MPEG-2/2.5 scalefactor decoding, intensity stereo,
> band tables, and band-edge behavior) was ported from *minimp3* (CC0),
> https://github.com/lieff/minimp3. go-mp3 also serves as a test-only
> differential oracle per the testing policy; it is never imported by
> the public tree or the runtime pipeline.

> **codec/mp3 encoder**: original code written against ISO 11172-3 /
> 13818-3 and textbook filterbank/MDCT theory. It introduces no new
> third-party data: the forward Huffman tables are derived at init from
> the decoder's tree tables (attributed above), and the polyphase
> analysis window is derived from the synthesis window (attributed
> above). *Shine* (LGPL, Tier B) is used only as a black-box quality
> oracle through `ffmpeg -c:a libshine`; its source was not consulted.

> **codec/alac decoder**: a clean-room port of Apple's *ALAC* reference
> decoder (Apache-2.0), https://github.com/macosforge/alac. The adaptive
> Golomb decode (ag_dec.c), the cascaded adaptive-FIR predictor
> (dp_dec.c / unpc_block), and the lossless middle-side matrix
> (matrix_dec.c / unmix) are ported faithfully so decodes are bit-exact;
> the frame element structure follows ALACDecoder.cpp. The bitstream
> reader, buffer model, and codec.Decoder integration are original.

> **codec/aac decoder**: the decode logic (raw_data_block, ICS, section
> and scalefactor decode, dequantization, TNS, M/S and intensity stereo,
> the IMDCT filterbank) is original code written against ISO/IEC 14496-3
> and Bosi/Goldberg. AAC is Tier B, so faad/ffmpeg decoders were not
> opened while implementing it. The file `codec/aac/tables_hcb.go` is the
> ADR-0001 black-box PARAMETER artifact: the normative Huffman codeword
> and length tables and scalefactor-band boundaries (facts fixed by ISO
> 14496-3), extracted as a data-only table from *FFmpeg*'s `aactab.c`
> (LGPL) in a separate analysis pass per the ADR-0001 provision that
> permits parameter tables. No decoder logic was taken. Perceptual noise
> substitution is filled with local noise (non-reproducible by design);
> SBR/PS are out of scope.

> **codec/vorbis decoder**: original code written against the Xiph
> *Vorbis I specification*. The codebook Huffman codeword assignment
> (`assignCodewords`) follows the algorithm in the public-domain
> *stb_vorbis*, https://github.com/nothings/stb, and the overall decode
> structure (floor 1 curve synthesis, residue partition passes, channel
> coupling, the MDCT/overlap-add) was cross-checked for shape against
> *stb_vorbis* and *jfreymuth/oggvorbis* (MIT),
> https://github.com/jfreymuth/oggvorbis, both Tier A. No source was
> ported line-by-line; the floor1 inverse-dB table is computed rather
> than transcribed, and the IMDCT reuses WaxFlow's own transform.

> **codec/opus range decoder**: `codec/opus/rangedec.go` is a clean-room
> port of the Opus entropy decoder from *libopus* `entdec.c`
> (BSD-3-Clause), https://gitlab.xiph.org/xiph/opus, per RFC 6716
> section 4.1. The range coder must be bit-exact with the reference, so
> the arithmetic (renormalization, `ec_decode`/`ec_dec_update`, the
> inverse-CDF and raw-bit readers, and the `ec_tell` accounting) is
> ported faithfully. The TOC/framing (`opus.go`) is original code written
> against RFC 6716 section 3.

> **codec/opus CELT decoder**: `codec/opus/celt_*.go` is a clean-room port
> of the *libopus* CELT float decoder (BSD-3-Clause),
> https://gitlab.xiph.org/xiph/opus, per RFC 6716 section 4.3. The band
> energy dequantization (`quant_bands.c`), bit allocation (`rate.c`), PVQ
> shape decoding (`cwrs.c`, `vq.c`), Laplace decode (`laplace.c`), band
> synthesis (`bands.c`), and the top-level decode loop, comb filter, and
> de-emphasis (`celt_decoder.c`, `celt.c`) are ported faithfully because a
> decoder must interoperate with the reference. The interoperability
> constant tables (`modes.c`, `static_modes_float.h`, `rate.c`) are
> reproduced verbatim as required by RFC 6716. The inverse MDCT
> (`celt_mdct.go`) ports the reference's rotation and windowing but runs a
> direct DFT in place of libopus's mixed-radix FFT (mathematically
> equivalent).
>
> **codec/opus SILK decoder**: `codec/opus/silk_*.go` is a clean-room port
> of the *libopus* SILK decoder (BSD-3-Clause),
> https://gitlab.xiph.org/xiph/opus, per RFC 6716 section 4.2. SILK's
> decoder is integer-only in the reference, so the fixed-point arithmetic
> macros (`macros.h`, `SigProc_FIX.h`, `Inlines.h`), the index and
> parameter decode (`decode_indices.c`, `decode_parameters.c`,
> `gain_quant.c`), NLSF-to-LPC conversion (`NLSF_decode.c`, `NLSF2A.c`,
> `NLSF_stabilize.c`, `NLSF_unpack.c`, `LPC_fit.c`, `LPC_inv_pred_gain.c`),
> excitation (`decode_pulses.c`, `shell_coder.c`, `code_signs.c`), LTP+LPC
> synthesis (`decode_core.c`), pitch decode (`decode_pitch.c`), stereo
> unmixing (`stereo_decode_pred.c`, `stereo_MS_to_LR.c`), the internal-rate
> resampler (`resampler*.c`), and the top-level frame loop (`dec_API.c`,
> `decode_frame.c`, `decoder_set_fs.c`) are ported faithfully so decode is
> bit-exact with the reference. The constant tables (`tables_*.c`,
> `table_LSF_cos.c`, `pitch_est_tables.c`, `resampler_rom.c`) in
> `silk_tables_gen.go` are reproduced verbatim as required by RFC 6716.
> The hybrid stitch and top-level mode dispatch (`decoder.go`) follow
> libopus `src/opus_decoder.c`. Loss concealment (PLC), comfort noise, DTX,
> and the neural OSCE enhancer are out of scope (file decode, not RTC).

> **codec/opus CELT encoder**: `codec/opus/rangeenc.go`,
> `celt_encode.go`, `celt_encanalysis.go`, `celt_encpitch.go`, and the
> encode branches of the shared band/energy/PVQ/allocation code are a
> clean-room port of the *libopus* CELT float encoder (BSD-3-Clause),
> https://gitlab.xiph.org/xiph/opus: the entropy encoder (`entenc.c`), the
> analysis stages, rate control (CBR, VBR, constrained VBR), theta RDO, and
> the pitch pre-filter with its pitch estimator and tone detection
> (`celt_encoder.c`, `bands.c`, `quant_bands.c`, `vq.c`, `pitch.c`,
> `celt_lpc.c`, `celt.c`). The forward MDCT ports the reference's rotation
> and windowing over the same direct DFT as the decoder. Encoder
> bit-exactness with the reference is a non-goal (the port uses exact math
> where the reference approximates); the produced bitstreams are verified
> against the reference decoder instead.

> **internal/testutil opus_compare**: `internal/testutil/opuscompare.go` is
> a Go port of *libopus*'s `src/opus_compare.c` (BSD-3-Clause),
> https://gitlab.xiph.org/xiph/opus, the RFC 6716 section 6 decoder
> conformance metric. Test-support code only: it is compiled into test
> binaries, never into release builds. Validated bit-for-bit against the C
> tool on the official test vectors.
