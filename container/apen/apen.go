// Package apen reads and writes native Monkey's Audio framing: the descriptor,
// format header, and mandatory seek table a .ape file opens with, the run of
// frames behind them, and the APEv2 tag most files carry after the audio.
//
// A frame carries neither its length nor its block count, so unlike every
// self-framing format here nothing can be recovered by scanning: the seek
// table is the index, and it is exact. That makes seeking a table lookup and
// makes a file whose table is unusable undecodable rather than degraded. It
// is also why the muxer needs a destination it can seek: the table and the
// totals in front of the audio are only knowable once the audio has gone out.
//
// The package is named apen rather than ape because the registry imports
// codec/ape alongside it (the flacn precedent). The public container name and
// error prefix are "ape" either way.
package apen

import "github.com/colespringer/waxflow/codec/ape"

// MatchNeed is the sniff window Match wants.
const MatchNeed = ape.MatchNeed

// Match reports whether head begins with a Monkey's Audio file. It is the
// format sniff-table entry.
func Match(head []byte) bool { return ape.Match(head) }
