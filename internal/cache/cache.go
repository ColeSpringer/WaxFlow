// Package cache is the transcode cache and the delivery model built on
// it. The pipeline always encodes at full speed into a cache temp file,
// and every client, the first included, drains as a read-behind reader
// with append/complete notification: slow clients can never backpressure
// the encoder, a second request for the same key attaches to the same
// growing file, and completed entries serve with full HTTP range support.
//
// A cache-volume write failure (disk full) downgrades the entry to a
// bounded in-memory ring feeding only the attached readers; it never
// kills playback. The same ring machinery, constructed without a file,
// carries uncacheable sync one-shots.
//
// Keys follow ADR-0004; the layout is cacheDir/v1/<aa>/<hash>/ holding
// meta.json plus out.<ext>. Writes go to *.tmp with atomic rename, and
// only completed outputs promote, so a crash leaves nothing that could be
// mistaken for a finished transcode.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// SchemaVersion versions the key derivation and on-disk layout together;
// bumping it invalidates everything (ADR-0004). It is the "v1" in the
// directory layout.
const SchemaVersion = 1

// DefaultRingBytes bounds the degradation/one-shot ring.
const DefaultRingBytes = 4 << 20

// Key addresses one cache entry: the hex SHA-256 of the ADR-0004 tuple.
type Key string

// NewKey derives a Key from the source identity (ref plus size-mtimeNS),
// the canonical output parameters, and the version constants of every
// sample-affecting node in the chain.
func NewKey(identity, params string, versions []string) Key {
	h := sha256.New()
	fmt.Fprintf(h, "waxflow-cache-%d\n", SchemaVersion)
	io.WriteString(h, identity)
	io.WriteString(h, "\n")
	io.WriteString(h, params)
	io.WriteString(h, "\n")
	io.WriteString(h, strings.Join(versions, ","))
	return Key(hex.EncodeToString(h.Sum(nil)))
}

// Meta is the sidecar record for one completed entry. It exists to
// rebuild the eviction index at boot and to serve headers without opening
// the payload; the key itself never needs reversing. Ref, Identity, and
// Params are written for humans inspecting a cache directory (nothing
// reads them back; an opaque hash dir would otherwise be undebuggable).
type Meta struct {
	SchemaVersion int    `json:"schemaVersion"`
	Ref           string `json:"ref"`
	Identity      string `json:"identity"`
	Params        string `json:"params"`
	Ext           string `json:"ext"`
	ContentType   string `json:"contentType"`
	Bytes         int64  `json:"bytes"`
	// Samples and Rate carry the output timeline for duration headers.
	Samples   int64     `json:"samples"`
	Rate      int       `json:"rate"`
	CreatedAt time.Time `json:"createdAt"`
}

func (m Meta) marshal() ([]byte, error) {
	m.SchemaVersion = SchemaVersion
	return json.MarshalIndent(m, "", "  ")
}
