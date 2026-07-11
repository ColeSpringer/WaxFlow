package sign

import (
	"net/url"
	"testing"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// FuzzVerify drives the verifier with hostile query strings and checks the
// boundary contract: Verify never panics, and every rejection carries one
// of the two documented signature codes. When the fuzzer happens to build
// a verifiable query it must also survive an encode/parse cycle (the wire
// round trip every real URL takes). A Sign->Verify round trip over the
// fuzzer's own parameters pins the mint side against the same inputs.
func FuzzVerify(f *testing.F) {
	s, err := New([]Key{{ID: "k1", Secret: []byte("fuzz-secret")}})
	if err != nil {
		f.Fatal(err)
	}
	minted := s.Sign("GET", "/stream", url.Values{"src": {"lib/a.flac"}}, time.Unix(1<<40, 0))
	f.Add("GET", "/stream", minted.Encode())
	f.Add("HEAD", "/stream", "src=lib%2Fa.flac&exp=999999999999&kid=k1&sig=AAAA")
	f.Add("GET", "/hls/master.m3u8", "exp=&kid=&sig=")
	f.Add("PUT", "//", "sig=%zz&exp=+1&kid=k1&kid=k2")

	f.Fuzz(func(t *testing.T, method, path, rawQuery string) {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return
		}
		verr := s.Verify(method, path, q)
		if verr != nil {
			code := waxerr.CodeOf(verr)
			if code != waxerr.CodeSignatureInvalid && code != waxerr.CodeSignatureExpired {
				t.Fatalf("Verify returned an undocumented code %q: %v", code, verr)
			}
		} else {
			reparsed, perr := url.ParseQuery(q.Encode())
			if perr != nil {
				t.Fatalf("verified query does not re-parse: %v", perr)
			}
			if rerr := s.Verify(method, path, reparsed); rerr != nil {
				t.Fatalf("verified query fails after encode/parse: %v", rerr)
			}
		}

		signed := s.Sign(method, path, q, time.Unix(1<<40, 0))
		if err := s.Verify(method, path, signed); err != nil {
			t.Fatalf("fresh signature does not verify: %v", err)
		}
	})
}
