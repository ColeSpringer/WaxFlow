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

> **codec/alac encoder**: the forward direction of the same reference
> (Apache-2.0): adaptive Golomb encode with the derived escape
> (ag_enc.c), the forward adaptive-FIR predictor run in lockstep with
> the decoder's adaptation (dp_enc.c / pc_block), the forward mixer
> (matrix_enc.c / mix), and the magic-cookie layout. Ported faithfully
> so decode(encode(x)) is bit-exact and third-party decoders accept the
> output; the mixRes search, verbatim fallback policy, and muxer
> integration are original.

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
> substitution is filled with local noise (non-reproducible by design).
> The HE-AAC v1 SBR decode stage (`codec/aac/sbr_*.go`) is likewise
> original code written against ISO 14496-3 4.6.18; its parameter tables
> in `sbr_tables.go` (the SBR envelope/noise Huffman pair tables, the
> 640-tap QMF prototype window, the noise phasor table, the limiter and
> smoothing constants) are normative spec data (Tables 4.A.85-4.A.90 and
> 4.6.18's constants), recorded in the same black-box parameter pass with
> the faad2 (GPL-2) and FFmpeg (LGPL) restatements of those tables as
> cross-checks. The HE-AAC v2 parametric stereo stage
> (`codec/aac/ps_*.go`) follows the identical pattern: original code
> written against ISO 14496-3 8.6.4, with `ps_tables.go` carrying the
> normative spec data (the PS parameter codebooks as canonical
> value/length listings, the IID/ICC quantizer values, the hybrid filter
> prototypes of Tables 8.32-8.35, the band maps of Tables 8.44-8.49, and
> the decorrelator constants of 8.6.4.6.4), recorded in the same
> black-box parameter pass with the FFmpeg restatement as the
> cross-check; the mixing matrices and phase tables are derived from
> those listings by the spec's formulas at init. No decoder logic was
> taken from either project; SBR and PS behavior was verified against
> ffmpeg's decoder output only. Enhanced SBR remains out of scope by
> deliberate keep-out.

> **codec/aac encoder**: original code written against ISO/IEC 14496-3
> (the informative encoder annex for the two-loop quantizer structure)
> and Bosi/Goldberg. AAC encoders are Tier B (ffmpeg aacenc is LGPL), so
> none were opened while implementing. It introduces no new third-party
> data: the forward Huffman tables, scalefactor-band boundaries, and
> window shapes are the decoder's already-attributed tables (above), and
> the psychoacoustic model is dsp/psy, written from the ISO 11172-3
> Annex D model 2 description. ffmpeg's native AAC encoder serves only
> as a black-box quality oracle in the encoder-quality gate; its source
> was not consulted.

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

> **codec/vorbis encoder**: original code written against the Xiph
> *Vorbis I specification*, the forward direction of the decoder above. Its
> codebooks are self-generated clean-room books: computed floor/class books,
> plus multi-dimensional product-lattice residue books whose codeword lengths
> are Huffman-trained offline (`codec/vorbis/books_gen.go`, produced by the
> `booksgen` generator run via `go generate`) on a corpus WaxFlow synthesizes
> from scratch (tones, chords, noise, sweeps). No external or libvorbis-derived
> audio or book tables are used; the encoder defines its own books in the setup
> header. libvorbis is a test-only
> quality oracle (its binary is invoked via ffmpeg; its source is never
> opened), and *jfreymuth/oggvorbis* (MIT) additionally serves as a test-only
> independent decoder oracle in the nested `oracletest` module, alongside
> *hajimehoshi/go-mp3*. Neither enters the runtime pipeline.

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

> **codec/opus SILK encoder, tonality analyser, and encoder
> integration**: `codec/opus/silk_enc_*.go`, `silk_nsq.go`,
> `silk_encode.go`, `analysis.go`, `mlp.go`, and `opus_encode.go` are a
> clean-room port of the *libopus* 1.6.1 encoder side (BSD-3-Clause),
> https://gitlab.xiph.org/xiph/opus: the SILK float analysis chain
> (`silk/float/*_FLP.c`: pitch analysis, Burg LPC, noise shaping, gain
> processing), the shared fixed-point quantization core (NSQ, NLSF VQ,
> gain and LTP quantizers, shell coder) so the bitstream side mirrors
> the bit-exact decoder, the encode-side bitstream writers
> (`encode_indices.c`, `encode_pulses.c`, `stereo_encode_pred.c`,
> `stereo_LR_to_MS.c`), the packet driver with the CBR gain loop
> (`enc_API.c`, `silk/float/encode_frame_FLP.c`), the tonality analyser
> and its MLP (`src/analysis.c`, `src/mlp.c`, weights mechanically
> extracted; its 480-point FFT is WaxFlow's own kernel, not kiss_fft),
> and the mode-decision/hybrid/redundancy integration
> (`src/opus_encoder.c`). Constant tables in `silk_enc_tables_gen.go`
> are extracted by script from the same source. Encoder bit-exactness
> with the reference is a non-goal; the produced bitstreams are
> verified through the reference decoder with per-packet range-coder
> final-state checks.

> **codec/wavpack decoder**: a clean-room port of the *WavPack* reference
> decoder (BSD-3-Clause), https://github.com/dbry/WavPack, 5.9.0. A lossless
> decoder has to reproduce the reference bit for bit, so the parts that define
> the bitstream are ported faithfully: the median-adaptive entropy decoder and
> its escape forms (`read_words.c`), the decorrelation passes with their two
> weight-application forms and the clipped cross-channel update
> (`unpack.c`, the `apply_weight`/`update_weight` macros in
> `wavpack_local.h`), the metadata sub-block handlers (`decorr_utils.c`,
> `entropy_utils.c`), the fixed-point exponential table (`entropy_utils.c`),
> and the block sync predicate (`open_utils.c`, `read_next_header`). The
> bitstream reader, the block walk and its boundary confirmation, the
> container demuxer, and the codec.Decoder integration are original. The
> reference `wavpack` and `wvunpack` binaries additionally serve as test-only
> fixture generators and oracles; they never enter the runtime pipeline.

> **codec/wavpack encoder**: original, and deliberately not a port. The
> encoding side of a lossless codec is under no obligation to match the
> reference byte for byte, so only the parts the *bitstream* fixes are
> mirrored, and they are mirrored from our own decoder rather than from
> libwavpack: the forward decorrelation passes are the algebraic inverses of
> the ported decode passes, and the entropy writer is the inverse of the
> ported reader, which is what makes the pair verifiable by round trip. The
> weight and log quantizers (`store_weight`, and the log form that
> `wp_exp2s` reads) are the two places the encoder must produce exactly what
> the decoder's tables consume; `store_weight` is the reference's, and the
> log direction is computed by searching the ported exponential table rather
> than porting the reference's second table. Everything else is a design
> choice of ours with no counterpart in the reference: the block length, the
> candidate cascades each compression level searches, the adaptation rate,
> the cost proxy that scores a candidate, the joint-stereo and false-stereo
> decisions, and the container writer.

> **internal/testutil opus_compare**: `internal/testutil/opuscompare.go` is
> a Go port of *libopus*'s `src/opus_compare.c` (BSD-3-Clause),
> https://gitlab.xiph.org/xiph/opus, the RFC 6716 section 6 decoder
> conformance metric. Test-support code only: it is compiled into test
> binaries, never into release builds. Validated bit-for-bit against the C
> tool on the official test vectors.

> **dsp/loudness K-weighting**: the rate-independent analog
> parametrization of the BS.1770 pre-filter pair (shelf and high-pass
> center frequencies, Q values, and gain, used to derive biquad
> coefficients at any sample rate) follows the published
> de-quantification also used by *libebur128* (MIT),
> https://github.com/jiixyj/libebur128. The meter itself (gating, LRA,
> true peak) is original code written against ITU-R BS.1770-4 and EBU
> Tech 3341/3342; no source was ported.
