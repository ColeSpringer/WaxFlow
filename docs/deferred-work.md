# Deferred work

Known gaps that are understood, bounded, and deliberately not fixed in the
change that found them. Each entry says where it lives, what it is, why it
waits, and what found it. Things that need a sibling Wax repo go to
docs/upstream-requests.md instead.

## QuickTime channel layouts ('chan') in MP4/MOV

**Where:** `container/mp4/stsdpcm.go`, `setPCM`.

**What:** the 'chan' box states a track's channel order, and it is not read.
Every track is described with `audio.DefaultLayout`, so a multichannel file
whose 'chan' box states an order other than the WAVE one plays with its
channels in the wrong places. Mono and stereo have only one order and are
unaffected; above two channels the entry raises a Note naming the assumption,
which probe prints.

**Why deferred:** the mapping is a table (AudioChannelLayoutTag and its
descriptions) against a layout vocabulary that is WAVE's, and nothing in the
tree yet reads a multichannel MP4 PCM track: no encoder writes one, no fixture
holds one, and the ffmpeg differential that would score the mapping needs a
file that ffmpeg itself writes a 'chan' box into. Guessing a layout is worse
than defaulting to one and saying so.

**Found by:** third-party review of the PCM-in-MP4 work, 2026-09-12.
