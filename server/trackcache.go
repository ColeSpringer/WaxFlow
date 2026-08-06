package server

import (
	"math"
	"sync"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/source"
)

// trackCacheCap bounds the track memo. An entry is one container.Track: 112
// bytes plus its CodecConfig payload, which is the codec's header blob and runs
// to tens of bytes (an AAC ASC is ~5, a FLAC STREAMINFO 34, an OpusHead 19). So
// a full cache is well under a megabyte, and the cap is about lifetime rather
// than size: keyed by identity and invalidated only by replacement, the memo
// would otherwise grow once per file for the process's life, and a library-wide
// sweep would pin the whole catalog.
//
// The whole Track is stored, including the CodecConfig the HLS path does not
// read. Trimming it would save a rounding error of memory and hand out a Track
// that is quietly not a Track: anything later planning from one (a concatenated
// timeline, a remux) would find a nil config and have no way to know it was
// dropped on purpose.
const trackCacheCap = 4096

// trackCache is a bounded identity-keyed memo of probed, measured tracks.
//
// Eviction is least-recently-used, not oldest-inserted. metaCache next door
// evicts oldest-inserted, and the difference is deliberate: rebuilding a
// metadata entry is one cheap tag read, while rebuilding a track entry can cost
// a full decode (measureSamples on a source with no declared length). So
// evicting a hot entry is nearly free there and expensive here, and a
// library-wide sweep must not push a live session's track out of the memo. This
// follows resolver.Catalog's PID cache, which is LRU for the same reason.
type trackCache struct {
	mu      sync.Mutex
	entries map[string]*trackEntry
	clock   int64
}

type trackEntry struct {
	track container.Track
	// tags are the container's own tags from the same probe, memoized here
	// rather than in a memo of their own: one probe answers both, they share
	// an identity and so an invalidation, and the alternative is a second
	// probe on the path this exists to keep off the wire. Read-only by
	// contract, like format.Info's own map.
	tags map[string][]string
	used int64 // clock value at last access; the LRU order
}

func (c *trackCache) get(key string) (container.Track, bool) {
	t, _, ok := c.getWithTags(key)
	return t, ok
}

func (c *trackCache) getWithTags(key string) (container.Track, map[string][]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return container.Track{}, nil, false
	}
	c.clock++
	e.used = c.clock
	return e.track, e.tags, true
}

// put stores t, except that it never downgrades a measured entry to an
// advisory one.
//
// The two are not interchangeable and the writes race: an exact caller (a
// timeline mint) and an ordinary one can miss on the same source together,
// run separate flights, and both write, in either order. Letting the advisory
// write win would throw away a walk that already happened and make the next
// timeline request pay for it again. Neither caller is served a wrong track
// either way, since each returns its own flight's result; this is only about
// which one the memo keeps.
func (c *trackCache) put(key string, t container.Track, tags map[string][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*trackEntry, trackCacheCap)
	}
	if e, exists := c.entries[key]; exists {
		if e.track.SamplesExact && !t.SamplesExact {
			// The advisory write loses the track, but not the tags: they do
			// not differ between an exact probe and an advisory one, and the
			// kept entry may have been written before this file grew any.
			if len(e.tags) == 0 {
				e.tags = tags
			}
			return
		}
	} else if len(c.entries) >= trackCacheCap {
		c.evictOldestLocked()
	}
	c.clock++
	c.entries[key] = &trackEntry{track: t, tags: tags, used: c.clock}
}

// evictOldestLocked drops the least recently used entry: a linear scan, run
// only on insert at capacity, exactly as resolver.Catalog does it. The scan is
// microseconds against a miss that costs a probe at best and a full decode at
// worst, so it is not worth a heap.
func (c *trackCache) evictOldestLocked() {
	var oldest string
	var found bool
	minUsed := int64(math.MaxInt64)
	for k, e := range c.entries {
		// found, not oldest != "": an empty key is a legal map key, so the
		// zero value cannot double as "nothing selected". Identities are never
		// empty today, which is exactly what would make the bug silent.
		if !found || e.used < minUsed {
			minUsed, oldest, found = e.used, k, true
		}
	}
	if found {
		delete(c.entries, oldest)
	}
}

// trackFor returns src's default track, probed and (as exact requires)
// measured, memoized by source identity.
//
// prepareHLS runs per segment request, so a 70-minute stream is ~1050 probes
// without this and a 12-track album would be ~12,600. The key is the identity,
// which encodes its own invalidation: a replaced file has a new identity and
// misses, so a stale track cannot be served. The identity check itself still
// re-resolves per request, so the 410 source-changed guarantee is unaffected.
//
// exact asks for a length that is authoritative rather than declared. It is
// what a timeline member needs, whose positions are a prefix sum: two samples
// of drift desync every position after that member, so members are measured
// and not trusted. It covers more than an unknown length, which is why the
// flag is not "measure when Samples < 0": an MP3 with a Xing header declares a
// total from its headers and can still be wrong.
//
// A whole single source tolerates an advisory total (format.Media calls a
// lying FLAC STREAMINFO an oddity rather than a truncation) because nothing
// downstream of it depends on the number matching: the stream ends where the
// samples end. The tolerance is the whole source's alone, and does not extend
// to every single-source request. A span is validated against the total
// (SpanTrack refuses to > total), so its callers ask for exact even though
// they name one source: an under-declared length would refuse a span the file
// can serve, and an over-declared one would mint a span that dies part way.
// Whether the number is depended on is the question, not how many sources
// there are.
//
// A measured entry satisfies both callers, so the memo is shared. Only the
// flight keys differ, and they differ in both directions on purpose. An exact
// caller must not join a non-exact flight, which would hand it the advisory
// track it exists to avoid. And a non-exact caller must not join an exact one,
// which is the tempting half: it would save a probe, but flight blocks its
// waiters, so a segment request would stall behind a timeline mint's whole
// measure to be handed a length it never needed. The duplicate is one header
// parse in a race window; the alternative is a live request waiting on a walk.
//
// Misses are deduplicated per key, not just memoized. The memo alone leaves a
// stampede window that is small but lands at the worst moment: the memo is
// in-memory, so a daemon restart with live sessions is cold, and the resuming
// clients arrive as a burst of concurrent segment requests for one source. Each
// would otherwise run its own probe and, for a source with no declared length,
// its own full decode. flight's own package doc names probe as a case it exists
// for.
//
// The memo is in-memory only: a binary upgrade re-probes rather than trusting a
// memo written by a different decoder revision.
func (s *Server) trackFor(src *source.File, exact bool) (container.Track, error) {
	key := identityString(src.Ref, src.ID)
	if t, ok := s.trackCache.get(key); ok && (!exact || t.SamplesExact) {
		return t, nil
	}
	flightKey := key
	if exact {
		flightKey += "|exact"
	}
	return s.trackFlight.Do(flightKey, func() (container.Track, error) {
		// Re-check under the flight: flight is duplicate suppression, not
		// caching, so a caller that missed the memo just as the previous
		// flight filled it would otherwise probe again.
		if t, ok := s.trackCache.get(key); ok && (!exact || t.SamplesExact) {
			return t, nil
		}
		info, err := s.eng.Probe(src, src.Ext, nil)
		if err != nil {
			return container.Track{}, err
		}
		track := info.Default()
		if track.Samples < 0 || (exact && !track.SamplesExact) {
			// VOD playlists promise an exact segment count, so an unknown
			// length is measured, never estimated (estimates yield tail 404s
			// or an early ENDLIST). The walk is IO-bound and the index sidecar
			// keeps repeats cheap; this memo keeps them free.
			if track.Samples, err = s.measureSamples(src); err != nil {
				return container.Track{}, err
			}
			// The walk decoded to the true end of stream, so the length is
			// now authoritative and saying so is honest rather than a
			// convenience: it is what lets a later exact caller reuse this
			// entry, and what stops a measured track being measured again.
			track.SamplesExact = true
		}
		s.trackCache.put(key, track, info.Tags)
		return track, nil
	})
}

// trackIsExact reports whether src's track is already known to have an
// authoritative length: either its headers say so, or a previous request
// measured it. It is the mint's fast-path test, so an album of FLACs (or any
// album minted before) answers in one round trip instead of becoming a job.
func (s *Server) trackIsExact(src *source.File) bool {
	if t, ok := s.trackCache.get(identityString(src.Ref, src.ID)); ok {
		return t.SamplesExact
	}
	return false
}

// containerTagsFor returns the tags the container parsed off src's header,
// memoized beside its track. It is the HLS path's half of the fold prepareSource
// makes from its own probe: gain=track resolves against REPLAYGAIN_* tags, so
// without it the same source answers one gain on /stream and another on
// /hls/master.m3u8 whenever the tag library cannot read the file.
//
// A miss fills the memo through trackFor rather than probing separately, so the
// two facts keep coming from one probe. The advisory form is asked for because
// this needs no length at all; an exact caller has already populated the entry
// in every case that wanted one.
func (s *Server) containerTagsFor(src *source.File) map[string][]string {
	key := identityString(src.Ref, src.ID)
	if _, tags, ok := s.trackCache.getWithTags(key); ok {
		return tags
	}
	if _, err := s.trackFor(src, false); err != nil {
		return nil
	}
	_, tags, _ := s.trackCache.getWithTags(key)
	return tags
}
