package waxflow

import (
	"errors"
	"fmt"
	"testing"

	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/format"
)

// closeFailMedia is a fixedMedia whose Close fails with err.
type closeFailMedia struct {
	*fixedMedia
	err error
}

func (m closeFailMedia) Close() error { return m.err }

// TestConcatMemberCloseErrorFailsTheReadPastIt: a member whose Close fails
// fails the read that advances past it, the last member's where the end of
// the stream would have been.
func TestConcatMemberCloseErrorFailsTheReadPastIt(t *testing.T) {
	const samples = 3000
	errClose := errors.New("the member did not close")
	for failing := range 2 {
		t.Run(fmt.Sprintf("member %d", failing), func(t *testing.T) {
			members := []ConcatSource{fixedMember(stereo48, samples), fixedMember(stereo48, samples)}
			members[failing].Open = func() (format.Media, error) {
				return closeFailMedia{fixedMedia: newFixedMedia(stereo48, samples), err: errClose}, nil
			}
			med, err := Concat(members, ConcatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer med.Close()
			buf := audio.Get(stereo48, audio.StandardChunk)
			defer audio.Put(buf)
			var read int64
			for err = med.ReadChunk(buf); err == nil; err = med.ReadChunk(buf) {
				read += int64(buf.N)
			}
			if err != errClose {
				t.Fatalf("the read ended with %v after %d samples, want the member's close error", err, read)
			}
			// Every sample of the member was delivered, and none of the next.
			if want := (int64(failing) + 1) * samples; read != want {
				t.Errorf("the close error came after %d samples, want after the member's last at %d", read, want)
			}
		})
	}
}
