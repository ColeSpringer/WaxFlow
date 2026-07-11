package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"sync"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/meta"
	"github.com/colespringer/waxflow/internal/uploads"
	"github.com/colespringer/waxflow/source"
)

// uploadResolver serves upload:<id> references from the spool and
// delegates everything else, replacing the source package's 501 stub
// when the upload store is configured.
type uploadResolver struct {
	next  source.Resolver
	store *uploads.Store
}

func (r uploadResolver) Resolve(ref string) (*source.File, error) {
	id, ok := strings.CutPrefix(ref, "upload:")
	if !ok {
		return r.next.Resolve(ref)
	}
	item, path, err := r.store.Get(id)
	if err != nil {
		return nil, err
	}
	return source.OpenLocal(ref, path, item.Name)
}

// readMeta parses source metadata when the mapper is wired, nil
// otherwise. A read failure yields nil too: metadata is best-effort and
// must never fail a playback request.
//
// Results without picture payloads are cached per source identity: the
// resolved gain and the tags fingerprint feed the cache key and the
// direct-play decision, so every transcode-shaped request needs the
// read, including ones a warm cache then serves without a pipeline; the
// cache keeps that from taxing hot sources with a metadata parse per
// request. Cached entries are shared and read-only by contract.
func (s *Server) readMeta(ctx context.Context, src *source.File, pictures bool) *meta.Info {
	if s.cfg.Meta == nil {
		return nil
	}
	if pictures {
		// Art payloads are large and rarely re-fetched; they bypass the
		// cache so it stays a few KB per entry.
		info, err := s.cfg.Meta.Read(ctx, src, src.Ext, meta.ReadOptions{Pictures: true})
		if err != nil {
			s.log.Debug("metadata read failed", "src", src.Ref, "err", err)
			return nil
		}
		return info
	}
	key := identityString(src.Ref, src.ID)
	if info, ok := s.metaCache.get(key); ok {
		return info
	}
	info, err := s.cfg.Meta.Read(ctx, src, src.Ext, meta.ReadOptions{})
	if err != nil {
		s.log.Debug("metadata read failed", "src", src.Ref, "err", err)
		return nil
	}
	s.metaCache.put(key, info)
	return info
}

// metaCacheCap bounds the metadata cache; entries are tags and chapter
// lists, a few KB each. Eviction is oldest-inserted: identities change
// with the file, so staleness is not the concern, only size.
const metaCacheCap = 256

// metaCache is a bounded identity-keyed cache of metadata reads. The
// zero value is ready to use.
type metaCache struct {
	mu      sync.Mutex
	entries map[string]*meta.Info
	order   []string
}

func (c *metaCache) get(key string) (*meta.Info, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.entries[key]
	return info, ok
}

func (c *metaCache) put(key string, info *meta.Info) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*meta.Info, metaCacheCap)
	}
	if _, exists := c.entries[key]; !exists {
		if len(c.order) >= metaCacheCap {
			delete(c.entries, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = info
}

// tagsSerial is the muxers' stream-form tag-rendering revision; bump it
// when any muxer changes how it serializes the same tag values, so
// cached bytes stay coherent (the values themselves are hashed below,
// and they derive from the source identity already in the key).
const tagsSerial = "tags-1"

// tagsFingerprint folds the embedded tag payload into the canonical
// cache-key params: cached bytes embed these tags, so a daemon with a
// different mapper revision (or none) must not share entries with one
// that mapped differently.
func tagsFingerprint(tags []container.Tag) string {
	h := sha256.New()
	io.WriteString(h, tagsSerial)
	for _, t := range tags {
		io.WriteString(h, "\x00"+t.Key+"\x01"+t.Value)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
