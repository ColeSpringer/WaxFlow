# Musepack SV8 chapter tags

ADR-0001 black-box analysis artifact for `container/mpc`. The chapter editor
in the reference tarball, `mpcchap` (GPL), is the only writer of chapter
packets there has ever been. It is reached as a binary: `make mpc-tools`
builds it and the fixture generator runs it. What the reader may assume about
the bytes it writes is recorded here from three permitted places: the SV8
specification, libmpcdec (BSD), and walking files the binary wrote.

## The packet

An SV8 chapter is one `CT` packet: the start sample as a varlen number, a
16-bit gain and a 16-bit peak (big-endian, as libmpcdec's bit reader takes
them), then the tag. The specification, as mirrored at
mutagen-specs.readthedocs.io ("Chapter-Tag Packet"), describes the tag field
as an "APEv2 tag without the preamble { 'A', 'P', 'E', 'T', 'A', 'G', 'E', 'X' }
in the header or footer, preferably without footer. This field is optional."
libmpcdec hands the tag over as the packet's remaining bytes and reads nothing
of it, so the extent is the packet's own.

## What mpcchap writes

Observed on files written by `mpcchap` r475 from an `.ini` chapter file
(`mpcchap in.mpc chapters.ini`, the tool editing in place). The committed
`container/mpc/testdata/chapters.mpc` is one such file.

- A chapter with items carries the 32-byte APEv2 header record less its
  8-byte preamble, so 24 bytes: version 2000, size, item count, flags
  0xA0000000 (has-header and is-header), 8 reserved zero bytes. The items
  follow in the usual APEv2 form (little-endian value size, flags, the key, a
  NUL, the value). There is no footer.
- The size field counts the items only, not the 24-byte record. The item
  count is the field to walk by; the size is not needed.
- Items are written in ascending order of value length, so `Title` is rarely
  first. Key case is whatever the `.ini` said (`Title`, `TITLE`).
- A chapter with no items carries no tag bytes at all: the packet ends after
  the peak.
- The `.ini` section names are the start samples and the run is written in
  that order, unsorted. A start past the end of the stream is warned about on
  stderr and written anyway.

Payload of a one-item chapter at sample 0, as written:

```
00                          start sample
00 00 00 00                 gain, peak
d0 07 00 00                 version 2000
13 00 00 00                 size 19: the one item, not the record
01 00 00 00                 item count
00 00 00 a0                 flags
00 00 00 00 00 00 00 00     reserved
05 00 00 00 00 00 00 00     value size 5, item flags 0
54 69 74 6c 65 00           "Title" NUL
49 6e 74 72 6f              "Intro"
```

## What a reader may assume

A tag, when present, starts with the 24-byte header record (version 1000 or
2000, the is-header flag set), and the item count at bytes 8..12 bounds the
items that begin at byte 24. A tag shorter than the record, or not led by one,
is not one the editor wrote; the specification would also allow the record as
a footer behind the items, which no writer produces. The title is the item
whose key folds to TITLE, its first NUL-separated value. The specification says chapters are
presented in file order, and the editor writes them in `.ini` order, so a run
can be unsorted; what a reader does with that order is its own decision.

## Where the run is read

Unchanged, from libmpcdec: the packets right after the seek table an `SO`
packet points at, or else the run of consecutive `CT` packets that ends at
the `SE` marker.

## Building mpcchap

The tarball does not ship libcuefile, which mpcchap's cue-sheet path links
against. `scripts/mpcchap/` carries a stub of that API that reports failure,
so the built tool serves `.ini` chapter files and refuses `.cue` and `.toc`.
The stub's six declarations were reconstructed from the editor's own calls to
them; libcuefile itself, source or header, was not opened, and the enumerator
values in the stub are placeholders it never consults.
