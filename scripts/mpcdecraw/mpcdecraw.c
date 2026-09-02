/*
 * mpcdecraw: libmpcdec's float output, untouched, for the Musepack decoder
 * differential.
 *
 * The reference mpcdec tool writes 16-bit WAV and truncates on the way, which
 * is a coarser oracle than the library it wraps. This tool links the same
 * libmpcdec (from the pinned musepack_src_r475 tarball) and writes every
 * sample the library produces as little-endian float32, interleaved, after
 * the library's own delay and tail trimming. It also prints the stream
 * properties as key=value lines so a test reads them without parsing prose.
 *
 * Built by `make mpc-tools` beside the other reference tools:
 *
 *   cc -O2 -fcommon -Iinclude -Ilibmpcdec libmpcdec/*.c common/crc32.c \
 *      mpcdecraw.c -o mpcdecraw -lm
 *
 * Usage: mpcdecraw [-i] <in.mpc> [<out.f32>]
 *   -i  print the stream properties only (no decode)
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <mpc/mpcdec.h>
#include "decoder.h"
#include "internal.h"

/* put_float_le writes one sample; a short write is a failure the test must
 * see as the tool's, not as a decoder disagreement. */
static int put_float_le(FILE *out, float v) {
	unsigned char b[4];
	unsigned int n;
	memcpy(&n, &v, 4);
	b[0] = (unsigned char)n;
	b[1] = (unsigned char)(n >> 8);
	b[2] = (unsigned char)(n >> 16);
	b[3] = (unsigned char)(n >> 24);
	return fwrite(b, 1, 4, out) == 4;
}

static void print_info(const mpc_streaminfo *si) {
	printf("stream_version=%u\n", si->stream_version);
	printf("sample_freq=%u\n", si->sample_freq);
	printf("channels=%u\n", si->channels);
	printf("max_band=%u\n", si->max_band);
	printf("ms=%u\n", si->ms);
	printf("block_pwr=%u\n", si->block_pwr);
	printf("is_true_gapless=%u\n", si->is_true_gapless);
	printf("samples=%llu\n", (unsigned long long)si->samples);
	printf("beg_silence=%llu\n", (unsigned long long)si->beg_silence);
	printf("length_samples=%lld\n", (long long)mpc_streaminfo_get_length_samples((mpc_streaminfo *)si));
	printf("pns=%u\n", si->pns);
	printf("profile=%g\n", si->profile);
	printf("encoder_version=%u\n", si->encoder_version);
	printf("gain_title=%u\n", si->gain_title);
	printf("peak_title=%u\n", si->peak_title);
	printf("gain_album=%u\n", si->gain_album);
	printf("peak_album=%u\n", si->peak_album);
	printf("header_position=%d\n", si->header_position);
	printf("tag_offset=%d\n", si->tag_offset);
	printf("total_file_length=%d\n", si->total_file_length);
}

int main(int argc, char **argv) {
	mpc_reader reader;
	mpc_demux *demux;
	mpc_streaminfo si;
	mpc_status err;
	int info_only = 0, argi = 1;
	FILE *out = NULL;
	unsigned long long total = 0;
	MPC_SAMPLE_FORMAT buffer[MPC_DECODER_BUFFER_LENGTH];

	if (argc > 1 && strcmp(argv[1], "-i") == 0) {
		info_only = 1;
		argi = 2;
	}
	if (argc - argi < 1 || argc - argi > 2) {
		fprintf(stderr, "usage: mpcdecraw [-i] <in.mpc> [<out.f32>]\n");
		return 2;
	}
	if (mpc_reader_init_stdio(&reader, argv[argi]) < 0) {
		fprintf(stderr, "mpcdecraw: cannot open %s\n", argv[argi]);
		return 1;
	}
	demux = mpc_demux_init(&reader);
	if (!demux) {
		fprintf(stderr, "mpcdecraw: %s is not a Musepack stream libmpcdec accepts\n", argv[argi]);
		mpc_reader_exit_stdio(&reader);
		return 1;
	}
	mpc_demux_get_info(demux, &si);
	print_info(&si);
	if (info_only) {
		mpc_demux_exit(demux);
		mpc_reader_exit_stdio(&reader);
		return 0;
	}
	if (argc - argi == 2) {
		out = fopen(argv[argi + 1], "wb");
		if (!out) {
			perror("mpcdecraw: output");
			mpc_demux_exit(demux);
			mpc_reader_exit_stdio(&reader);
			return 1;
		}
	}
	for (;;) {
		mpc_frame_info frame;
		unsigned int i, n;
		frame.buffer = buffer;
		err = mpc_demux_decode(demux, &frame);
		if (frame.bits == -1)
			break;
		n = frame.samples * si.channels;
		if (out)
			for (i = 0; i < n; i++)
				if (!put_float_le(out, buffer[i])) {
					perror("mpcdecraw: write");
					fclose(out);
					mpc_demux_exit(demux);
					mpc_reader_exit_stdio(&reader);
					return 1;
				}
		total += frame.samples;
	}
	printf("decoded_samples=%llu\n", total);
	printf("status=%s\n", err == MPC_STATUS_OK ? "ok" : "error");
	if (out && fclose(out) != 0) {
		perror("mpcdecraw: close");
		err = MPC_STATUS_FAIL;
	}
	mpc_demux_exit(demux);
	mpc_reader_exit_stdio(&reader);
	return err == MPC_STATUS_OK ? 0 : 1;
}
