# container/asf generated fixtures

The recipe for each fixture here that a tool in this tree regenerates. The
ffmpeg-made v1/v2 files and the WMA Pro, Voice and Lossless files predate this
manifest; the codec corpora under `codec/wma*/testdata` carry their own.

## `ms-v2-44100-2ch-128k.wma`

| Encoder | Rate | Ch | Bit rate | nBlockAlign | flags2 | Objects | Decoded samples per channel | Bytes |
|---|---|---|---|---|---|---|---|---|
| Media Foundation, WMA 8 (`Wma8`) | 44100 | 2 | 128016 | 5945 | 0x000f | 20 | 268288 | 121171 |

The first WMA v1/v2 file in the tree that Windows wrote. ffmpeg writes no bit
reservoir, so every other v1/v2 fixture here is one frame per media object and
its stamps land on the frame grid. This one carries the reservoir in every
packet, each object nearly fills its 5974-byte packet, and every object past
the first is stamped exactly two frames (4096 samples) under the sample a
decode resumed there first produces. The frame counts in the superframe
headers sum to 132; a linear decode is 131 frames, the last never completing.

Made by:

```sh
WAXFLOW_REGEN_ASF_FIXTURE=1 go test ./container/asf/ -run TestRegenerateWindowsFixture
```

which synthesizes the source with `chirpNoise` in `windows_fixture_test.go`
(6 s of 16-bit stereo at 44.1 kHz: the WMA corpus note's chirp plus seeded
noise, the right channel the left delayed a sample at 0.85) and encodes it
through `testutil.WMFEncode`, that is:

```sh
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/wmfenc/wmfenc.ps1 \
    -Subtype Wma8 -In source.wav -Out ms-v2-44100-2ch-128k.wma \
    -Rate 44100 -Channels 2 -BitRate 128000 -Bits 16
```

A regeneration is not byte-identical: the encoder writes a fresh File ID GUID
each run. The declared play duration (266188 samples) is advisory and short
of the decode, as on every Media Foundation file.
