# Upstream requests

The standing list of things WaxFlow wants from the sibling Wax repos it
depends on. Only wax-series dependencies belong here; today that is
WaxLabel alone (`cli/go.mod` and `oracletest/go.mod`; the root module's
require block is empty by design, see ADR-0002). Every entry is a
candidate for whenever upstream work is next scheduled; nothing here
implies timing, and none of it is a WaxFlow prerequisite. Each entry
names the shipped workaround WaxFlow runs on today and, where one exists,
the test that will notice the upstream fix landing. Agents: when you
defer something because it needs upstream support, add it here in the
same change; do not bury it in a progress note.

## WaxLabel

- **The MP4 sample entry's samplerate is read verbatim for ALAC and
  FLAC.** The AudioSampleEntry's 16.16 field cannot hold a rate past
  65535 Hz, so a hi-res file's entry never carries the true rate: WaxFlow
  writes 65535 for ALAC (the magic cookie carries 96000) and, as the
  FLAC-in-ISOBMFF spec requires, the greatest whole halving for FLAC
  (48000 for 96 and 192 kHz), a value the same spec says a reader MUST
  override from the STREAMINFO in the `dfLa` box. waxlabel v1.6.2 reports
  the field as it finds it: `SampleRate` 65535 for a 96 kHz ALAC and
  48000 for a 96 kHz FLAC-in-MP4 (`ffmpeg` writes 0 for such an ALAC and
  waxlabel would report that). Wanted: `SampleRate` from the ALAC cookie,
  the way `BitsPerSample` has come from it since v1.4.2, and from the
  `dfLa` STREAMINFO for `fLaC` entries. Shipped workaround: none needed
  inside WaxFlow (its own demuxer reads the config boxes); the wrong
  rate reaches only a waxlabel consumer reading a WaxFlow-produced hi-res
  ALAC or FLAC-in-MP4 file, such as a catalog scan of a job output.
  `TestWaxlabelReadsTheSampleEntryRateVerbatim` (oracletest) pins the
  present behaviour and fails the day waxlabel fixes it, which is the cue
  to retire this entry and flip that cell to the true rate.
