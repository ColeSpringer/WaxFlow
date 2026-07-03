package sign

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxflow/waxerr"
)

// SecretFileName is the persisted-secret file under dataDir. One
// constant: the daemon and the offline `waxflow sign` must resolve the
// identical file or offline-minted URLs would fail daemon verification.
const SecretFileName = "signing-secret"

// ResolveKeys resolves signing key material exactly one way for every
// consumer: an explicit signingSecret spec wins, otherwise the secret
// persisted under dataDir (generated on first use).
func ResolveKeys(secretSpec, dataDir string) ([]Key, error) {
	if secretSpec != "" {
		return ParseKeys(secretSpec)
	}
	k, err := LoadOrCreate(filepath.Join(dataDir, SecretFileName))
	if err != nil {
		return nil, err
	}
	return []Key{k}, nil
}

// ParseKeys parses the signingSecret config value into a rotation list.
// Entries are comma-separated. An entry with a colon is "<kid>:<hex>", a
// hex-encoded secret under that key id; an entry without one is a literal
// secret under key id "1" (single-key convenience) and must then be the
// only entry. The first entry mints.
func ParseKeys(spec string) ([]Key, error) {
	entries := strings.Split(spec, ",")
	keys := make([]Key, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			return nil, waxerr.New(waxerr.CodeInvalidRequest, "sign: empty signingSecret entry")
		}
		kid, hexSecret, ok := strings.Cut(e, ":")
		if !ok {
			if len(entries) > 1 {
				return nil, waxerr.New(waxerr.CodeInvalidRequest,
					"sign: a signingSecret rotation list needs kid:hex entries")
			}
			return []Key{{ID: "1", Secret: []byte(e)}}, nil
		}
		secret, err := hex.DecodeString(hexSecret)
		if err != nil || len(secret) == 0 {
			return nil, waxerr.New(waxerr.CodeInvalidRequest,
				fmt.Sprintf("sign: signingSecret key %q is not kid:hex", kid))
		}
		keys = append(keys, Key{ID: kid, Secret: secret})
	}
	return keys, nil
}

// LoadOrCreate returns the persisted signing key at path, generating a
// fresh 32-byte secret on first use (mode 0600, per ADR-0003). The file
// holds one "kid:hex" line in ParseKeys syntax, so promoting it into the
// config's rotation list is a copy-paste.
//
// Creation is both crash-safe and race-safe: the content lands in a
// unique temp file first (a crash can never leave a partial secret at the
// final path), and publication uses a no-replace hard link so concurrent
// creators (the daemon and `waxflow sign` on first run) converge on one
// winner, with the loser re-reading the winner's key.
func LoadOrCreate(path string) (Key, error) {
	for range 2 {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			keys, err := ParseKeys(strings.TrimSpace(string(b)))
			if err != nil {
				return Key{}, waxerr.Wrap(waxerr.CodeInvalidRequest,
					fmt.Sprintf("sign: persisted secret %s", path), err)
			}
			return keys[0], nil
		case !errors.Is(err, fs.ErrNotExist):
			return Key{}, waxerr.Wrap(waxerr.CodeInternal, "sign: reading persisted secret", err)
		}

		secret := make([]byte, 32)
		rand.Read(secret) // never fails per crypto/rand's Go 1.24 contract
		published, err := publishSecret(path, secret)
		if err != nil {
			return Key{}, err
		}
		if !published {
			continue // lost the creation race: read the winner's key
		}
		return Key{ID: "1", Secret: secret}, nil
	}
	return Key{}, waxerr.New(waxerr.CodeInternal, "sign: persisted secret flapping")
}

// publishSecret writes the secret to a unique temp file and links it to
// path without replacing an existing file. It reports false when another
// creator won the race. Filesystems without hard links fall back to a
// rename, trading the (first-run-only) race guarantee for portability.
func publishSecret(path string, secret []byte) (bool, error) {
	fail := func(err error) (bool, error) {
		return false, waxerr.Wrap(waxerr.CodeOutputUnwritable, "sign: persisting secret", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename fallback
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fail(err)
	}
	if _, err := fmt.Fprintf(tmp, "1:%s\n", hex.EncodeToString(secret)); err != nil {
		tmp.Close()
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}

	switch err := os.Link(tmp.Name(), path); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	default:
		// Hard links unsupported here (exFAT, some network mounts):
		// rename still gives crash-atomicity, only the concurrent
		// first-creation race loses its guard.
		if rerr := os.Rename(tmp.Name(), path); rerr != nil {
			return fail(rerr)
		}
		return true, nil
	}
}
