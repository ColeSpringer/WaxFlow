// Package wv demuxes native WavPack framing: the self-delimiting run of
// "wvpk" blocks a .wv file is, plus the APEv2 tag most of them carry after
// the audio. Each block declares its own byte length, so a boundary is
// confirmed rather than guessed: a block is accepted only when the bytes
// right after it are either the end of the audio or another parsable block
// header. Seeking bisects on the blocks' own sample indexes, landing on the
// block containing the target; format.Media pre-rolls the rest, sample-exact.
//
// The package is named wv rather than wavpack because the registry imports
// codec/wavpack alongside it (the flacn precedent). The public container name
// and error prefix are "wavpack" either way.
package wv

import "github.com/colespringer/waxflow/codec/wavpack"

// Match reports whether head begins with a WavPack block. It is the format
// sniff-table entry.
func Match(head []byte) bool { return wavpack.Match(head) }
