/*
 * fdkenc: minimal libfdk-aac CLI encoder for the encoder-quality gate.
 *
 * The HE-AAC gate scores our encoder against libfdk, but libfdk is
 * non-free so no distribution ffmpeg carries it. This tool reaches the
 * library directly instead: raw s16le PCM in, an ADTS elementary stream
 * out, the same operating point ffmpeg's libfdk wrapper uses (CBR,
 * afterburner on, implicit signalling via the ADTS transport).
 *
 * Build (MSYS2 UCRT64 with mingw-w64-ucrt-x86_64-fdk-aac installed):
 *
 *   gcc -O2 fdkenc.c -o fdkenc.exe -lfdk-aac -lstdc++ -static
 *
 * Point WAXFLOW_FDKENC at the binary and the testutil harness uses it
 * whenever ffmpeg itself has no libfdk_aac (internal/testutil/fdkenc.go).
 *
 * Usage: fdkenc <rate> <channels> <bitrate> <aot> <in.raw> <out.aac>
 *   aot 2 = AAC-LC, 5 = HE-AAC v1, 29 = HE-AAC v2
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <fdk-aac/aacenc_lib.h>

static void die(const char *what, int err) {
	fprintf(stderr, "fdkenc: %s failed (%d)\n", what, err);
	exit(1);
}

int main(int argc, char **argv) {
	if (argc != 7) {
		fprintf(stderr, "usage: fdkenc <rate> <channels> <bitrate> <aot> <in.raw s16le> <out.aac adts>\n");
		return 2;
	}
	int rate = atoi(argv[1]);
	int channels = atoi(argv[2]);
	int bitrate = atoi(argv[3]);
	int aot = atoi(argv[4]);
	if (channels < 1 || channels > 2) {
		fprintf(stderr, "fdkenc: %d channels unsupported\n", channels);
		return 2;
	}

	FILE *in = fopen(argv[5], "rb");
	if (!in) { perror("fdkenc: input"); return 1; }
	FILE *out = fopen(argv[6], "wb");
	if (!out) { perror("fdkenc: output"); return 1; }

	HANDLE_AACENCODER h;
	AACENC_ERROR err;
	if ((err = aacEncOpen(&h, 0, channels)) != AACENC_OK) die("aacEncOpen", err);
	if ((err = aacEncoder_SetParam(h, AACENC_AOT, aot)) != AACENC_OK) die("AOT", err);
	if ((err = aacEncoder_SetParam(h, AACENC_SAMPLERATE, rate)) != AACENC_OK) die("SAMPLERATE", err);
	if ((err = aacEncoder_SetParam(h, AACENC_CHANNELMODE, channels == 1 ? MODE_1 : MODE_2)) != AACENC_OK) die("CHANNELMODE", err);
	if ((err = aacEncoder_SetParam(h, AACENC_BITRATE, bitrate)) != AACENC_OK) die("BITRATE", err);
	if ((err = aacEncoder_SetParam(h, AACENC_TRANSMUX, TT_MP4_ADTS)) != AACENC_OK) die("TRANSMUX", err);
	if ((err = aacEncoder_SetParam(h, AACENC_AFTERBURNER, 1)) != AACENC_OK) die("AFTERBURNER", err);
	if ((err = aacEncEncode(h, NULL, NULL, NULL, NULL)) != AACENC_OK) die("init", err);

	AACENC_InfoStruct info;
	if ((err = aacEncInfo(h, &info)) != AACENC_OK) die("aacEncInfo", err);

	int frameSamples = (int)info.frameLength * channels;
	INT_PCM *pcm = malloc(sizeof(INT_PCM) * frameSamples);
	unsigned char obuf[20480];
	if (!pcm) { fprintf(stderr, "fdkenc: oom\n"); return 1; }

	for (;;) {
		size_t got = fread(pcm, sizeof(INT_PCM), frameSamples, in);

		void *inPtr = pcm;
		INT inId = IN_AUDIO_DATA, inSize = (INT)(got * sizeof(INT_PCM)), inElem = sizeof(INT_PCM);
		AACENC_BufDesc ibd = {0};
		ibd.numBufs = 1;
		ibd.bufs = &inPtr;
		ibd.bufferIdentifiers = &inId;
		ibd.bufSizes = &inSize;
		ibd.bufElSizes = &inElem;

		void *outPtr = obuf;
		INT outId = OUT_BITSTREAM_DATA, outSize = sizeof(obuf), outElem = 1;
		AACENC_BufDesc obd = {0};
		obd.numBufs = 1;
		obd.bufs = &outPtr;
		obd.bufferIdentifiers = &outId;
		obd.bufSizes = &outSize;
		obd.bufElSizes = &outElem;

		AACENC_InArgs ia = {0};
		AACENC_OutArgs oa = {0};
		ia.numInSamples = got == 0 ? -1 : (INT)got; /* -1 flushes */

		err = aacEncEncode(h, &ibd, &obd, &ia, &oa);
		if (err == AACENC_ENCODE_EOF) break;
		if (err != AACENC_OK) die("aacEncEncode", err);
		if (oa.numOutBytes > 0 &&
		    fwrite(obuf, 1, (size_t)oa.numOutBytes, out) != (size_t)oa.numOutBytes) {
			perror("fdkenc: write");
			return 1;
		}
	}

	aacEncClose(&h);
	fclose(in);
	if (fclose(out) != 0) { perror("fdkenc: close"); return 1; }
	return 0;
}
