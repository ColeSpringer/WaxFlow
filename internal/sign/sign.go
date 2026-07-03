// Package sign implements ADR-0003 signed playback URLs: exp + kid + sig
// query parameters carrying a base64url HMAC-SHA256 over the canonical
// string
//
//	"waxflow-v1" "\n" method "\n" path "\n" canonicalQuery
//
// where canonicalQuery is every query parameter except sig, sorted by key
// then value and percent-encoded per RFC 3986. Every playback-affecting
// parameter (src, format, bits, the source identity id, exp, kid, ...) is
// inside the signature, so no part of a signed URL can be altered. HEAD
// signs and verifies as GET so players' preflight requests cannot fail
// verification.
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxflow/waxerr"
)

// Scheme is the canonical-string version prefix. Changing the canonical
// string means a new scheme, never a silent break (ADR-0003).
const Scheme = "waxflow-v1"

// Leeway is the fixed clock-skew allowance on expiry checks, pinned here
// per ADR-0003. Sixty seconds tolerates unsynchronized self-hosted boxes
// without meaningfully extending a URL's life.
const Leeway = 60 * time.Second

// DefaultTTL is the minting default when the source duration is unknown;
// with a known duration the default is max(DefaultTTL, 2 x duration).
const DefaultTTL = 6 * time.Hour

// MaxTTL caps requested URL lifetimes: ten years is beyond any sane
// bearer-token life and far inside duration-arithmetic overflow.
const MaxTTL = 10 * 365 * 24 * time.Hour

// DefaultTTLFor is the ADR-0003 default TTL policy, in one place for the
// daemon's /sign and the CLI's offline mint: max(DefaultTTL, 2 x source
// duration), with non-positive durations (unknown length) getting the
// floor.
func DefaultTTLFor(durationSeconds float64) time.Duration {
	ttl := DefaultTTL
	if durationSeconds > 0 {
		ttl = max(ttl, 2*time.Duration(durationSeconds*float64(time.Second)))
	}
	return min(ttl, MaxTTL)
}

// Reserved query parameter names.
const (
	ParamExp = "exp"
	ParamKID = "kid"
	ParamSig = "sig"
)

// Key is one HMAC key in the rotation list.
type Key struct {
	ID     string
	Secret []byte
}

// Signer signs and verifies playback URLs. The first key mints; every key
// verifies, which is what makes rotation graceful.
type Signer struct {
	active Key
	keys   map[string]Key
	// now is the clock, injectable for expiry tests.
	now func() time.Time
}

// New returns a Signer over the rotation list; keys[0] is the minting key.
func New(keys []Key) (*Signer, error) {
	if len(keys) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalidRequest, "sign: no signing keys")
	}
	s := &Signer{active: keys[0], keys: make(map[string]Key, len(keys)), now: time.Now}
	for _, k := range keys {
		if k.ID == "" || len(k.Secret) == 0 {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, "sign: key with empty id or secret")
		}
		if _, dup := s.keys[k.ID]; dup {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, fmt.Sprintf("sign: duplicate key id %q", k.ID))
		}
		s.keys[k.ID] = k
	}
	return s, nil
}

// Sign returns a copy of q with exp, kid, and sig added, valid until exp.
func (s *Signer) Sign(method, path string, q url.Values, exp time.Time) url.Values {
	signed := make(url.Values, len(q)+3)
	for k, vs := range q {
		signed[k] = slices.Clone(vs)
	}
	signed.Set(ParamExp, strconv.FormatInt(exp.Unix(), 10))
	signed.Set(ParamKID, s.active.ID)
	mac := computeMAC(s.active.Secret, method, path, signed)
	signed.Set(ParamSig, base64.RawURLEncoding.EncodeToString(mac))
	return signed
}

// Verify checks q's signature for the given method and path. Errors carry
// waxerr.CodeSignatureInvalid, or waxerr.CodeSignatureExpired for a
// correctly signed URL past its exp (plus Leeway).
func (s *Signer) Verify(method, path string, q url.Values) error {
	sigStr := q.Get(ParamSig)
	expStr := q.Get(ParamExp)
	kid := q.Get(ParamKID)
	if sigStr == "" || expStr == "" || kid == "" {
		return waxerr.New(waxerr.CodeSignatureInvalid, "sign: missing exp, kid, or sig")
	}
	key, ok := s.keys[kid]
	if !ok {
		return waxerr.New(waxerr.CodeSignatureInvalid, fmt.Sprintf("sign: unknown key id %q", kid))
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil {
		return waxerr.New(waxerr.CodeSignatureInvalid, "sign: malformed sig encoding")
	}
	// Signature before expiry: a tampered URL is invalid no matter when it
	// is presented, and exp itself is only trustworthy once verified.
	if !hmac.Equal(sig, computeMAC(key.Secret, method, path, q)) {
		return waxerr.New(waxerr.CodeSignatureInvalid, "sign: signature mismatch")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return waxerr.New(waxerr.CodeSignatureInvalid, "sign: malformed exp")
	}
	if s.now().After(time.Unix(exp, 0).Add(Leeway)) {
		return waxerr.New(waxerr.CodeSignatureExpired, "sign: URL expired")
	}
	return nil
}

// computeMAC hashes the ADR-0003 canonical string. sig is excluded by
// CanonicalQuery, so passing the fully signed values back in verifies
// cleanly.
func computeMAC(secret []byte, method, path string, q url.Values) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(Scheme))
	h.Write([]byte{'\n'})
	h.Write([]byte(normalizeMethod(method)))
	h.Write([]byte{'\n'})
	h.Write([]byte(path))
	h.Write([]byte{'\n'})
	h.Write([]byte(CanonicalQuery(q)))
	return h.Sum(nil)
}

// normalizeMethod maps HEAD onto GET (ADR-0003) and uppercases the rest.
func normalizeMethod(method string) string {
	m := strings.ToUpper(method)
	if m == "HEAD" {
		return "GET"
	}
	return m
}

// CanonicalQuery serializes q per ADR-0003: every parameter except sig as
// "key=value" pairs, percent-encoded per RFC 3986, sorted by key then
// value, joined with '&'.
func CanonicalQuery(q url.Values) string {
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(q))
	for k, vs := range q {
		if k == ParamSig {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, pair{escape(k), escape(v)})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if c := strings.Compare(a.k, b.k); c != 0 {
			return c
		}
		return strings.Compare(a.v, b.v)
	})
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(p.k)
		sb.WriteByte('=')
		sb.WriteString(p.v)
	}
	return sb.String()
}

const upperhex = "0123456789ABCDEF"

// escape percent-encodes everything outside RFC 3986's unreserved set.
// url.QueryEscape is close but not it (space becomes '+', '~' is escaped),
// and the canonical form must be pinned exactly.
func escape(s string) string {
	// A plain byte scan: unreserved admits only ASCII, so every byte of
	// a multi-byte UTF-8 sequence (>= 0x80) correctly takes the escape
	// path, and no rune decoding is needed on this per-request path.
	clean := true
	for i := 0; i < len(s); i++ {
		if !unreserved(s[i]) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		b := s[i]
		if unreserved(b) {
			sb.WriteByte(b)
			continue
		}
		sb.WriteByte('%')
		sb.WriteByte(upperhex[b>>4])
		sb.WriteByte(upperhex[b&0xF])
	}
	return sb.String()
}

func unreserved(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' ||
		b == '-' || b == '.' || b == '_' || b == '~'
}
