# ADR-0012: Live pipelines end when their audience does

Status: Accepted (2026-10-30)

## Context

The read-behind model (the cache package doc) runs every live encode at full
speed into a cache temp file while its clients read behind it. A client leaving
detached only its reader: the encode ran to the end on the server's base
context, holding its live slot and writing a cache entry, on the bet that the
next request for the same output would be served from that entry.

For a player the bet mostly loses. A scrub is a new `t=` and so a new cache
key, which left the previous encode running to the end of the track; so did
every Subsonic `timeOffset`, every track skipped after a few seconds, every
failure probe. Each held a live slot for its whole run (jobs pause while the
live pool is saturated) and wrote a full entry nobody would read: a listener
sampling the first seconds of many long tracks left one complete cached encode
per track.

HLS had the same shape one level up. A variant worker runs from the segment it
started at to the end of the stream, and only a seek outside its window stopped
one. A player that went away left its worker encoding, and holding a slot, to
the last segment.

## Decision

### A progressive encode stops a grace after its last reader leaves

`liveIdleGrace` (default 5 s, `0` never) bounds how long an in-flight entry
keeps encoding with no reader attached. The last reader leaving arms a timer for
the grace, and a reader attaching disarms it. When it fires, the entry abandons
under its lock: it leaves the registry, refuses new readers, and runs the hook
the server gave it, which gives the live slot back and cancels the run with
`cache.ErrAbandoned` as the cause. A run stalled where no write and no context
check comes (a hung source, a long seek walk) so frees its slot on time all the
same. The temp file stays the writer's to drop: its next call fails the entry,
which keeps the entry single-writer. A request whose flight found the entry just
before it abandoned starts the encode afresh and finds the slot already free.
An encode never abandons before its first reader attaches.

### An HLS worker stops when nobody fetches from its variant

`hlsIdleGrace` (default 60 s, `0` never) bounds how long a variant worker runs
with nobody fetching from its variant, and every fetch counts, cache hits
included. A hit never reaches the worker manager, and an unpaced worker is ahead
of its player after a segment or two, so from then on every fetch of a player
still listening is a hit: a timeout armed only when the last waiting fetch left
would fire on it. So the manager arms the timeout when nobody waits on the
worker, a waiting fetch disarms it, and a hit restarts it. When it fires, the
worker is canceled with `hls.ErrIdle` as the cause and gives its live slot back
at once, stalled or not. A seek restart keeps its slot until the old worker has
left, since the seek waiting on it needs that slot next.

### Accounting

Both stops log once at Info as "abandoned" and count in
`waxflow_sessions_abandoned_total{kind}` (`live`, `hls`), apart from seek
restarts and failures. A progressive one is graded by its entry, not by the
error the run surfaces on its way out. The live count also takes a ring-fed
session whose ring filled with nobody reading, the older rule a cache-volume
failure falls back to, which runs whatever the grace. `server.Config` takes both graces as durations whose
zero never stops, so an embedder keeps running encodes to completion; the CLI
wires 5 s and 60 s.

## Alternatives rejected

**A run-ahead budget for HLS.** Stopping a worker a fixed time after it starts
bounds how far it runs ahead of its player, which could save slots on very long
timelines. It is a different policy: it ends a worker whose listener is still
there, and the next miss pays a restart at the cache's edge. It would need its
own setting, and this ADR does not take it.

**Stopping the moment the last reader leaves.** That cannot tell a listener who
left from a reconnect, a retry or a second client arriving a moment later. The
grace is that distinction.

## Consequences

- An encode whose clients all leave before it finishes caches nothing, and the
  next request for it encodes again. Scrubbing and skipping no longer fill the
  cache with entries nobody reads; a listener sampling many long tracks leaves
  nothing behind instead of a full encode per track. An encode that finishes
  inside the grace caches as before.
- The slot an unread encode held for the rest of its track comes back a grace
  after its listener leaves, so jobs wait behind abandoned playback no longer
  than that.
- HLS loses nothing: segments are written whole and any segment is a restart
  point, so a worker stopped between fetches costs at most a restart at the
  next miss.
- `0` keeps the old behaviour on either surface.
