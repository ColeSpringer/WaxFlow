# ADR-0009: Multi-source timelines and their identity

Status: Accepted (2026-07-15)

## Context

A player queue is several files that should sound like one: a gapless album,
an audiobook split across tracks. Delivering that over HLS means one
continuous stream, because HLS cannot change format mid-variant without an
`EXT-X-DISCONTINUITY` and a second init segment, which the architecture (one
chain, one init, one edit list) forbids structurally.

Everything the pipeline needs already exists. `format.Media` delivers
gapless-trimmed PCM, so concatenating two of them is sample-exact by
construction with no delay arithmetic at the seam. What does not exist is a
way to say *which* files, since ADR-0003 makes every playback URL
self-contained and identity-pinned, and a URL cannot carry a thousand
references.

This extends ADR-0003 and ADR-0004 rather than changing them: a timeline is a
new kind of thing a URL can name, and it needs an identity that behaves like a
source's.

## Decision

### The primitive is a lazy concat in the engine

`Concat([]ConcatSource, ConcatOptions) format.Media` sequences members into
one Media whose sample `len(a)` is `b`'s sample 0, unless a crossfade is asked
for. Members open on demand and close on advance, so a queue of any length
costs one file descriptor.

Lazy opening is the design and not an optimization. It makes planning and
running symmetric (both are driven by the members' `container.Track` alone),
and it removes rewind entirely: a member reached again is a member opened
again, from the top, with no state to have gone stale.

Mixed members are normalized, not refused. The envelope is the format no
member loses information to reach (max rate, max channels, the wider domain).
Refusing would not push the problem to the caller, it would delete the feature
for the normal case, because a play queue is mixed by nature. A member already
at the envelope is read straight through with no chain and no copy, which is
structural rather than tuned: it falls out of the envelope being a maximum, and
it is the case a gapless album (one master, one rate) always hits.

`ConcatTrack` is the single funnel. Both the plan and the run resolve the
delivered format through it, so they cannot disagree; without that, a plan's
`Format` and the concat's actual delivery drift, and the drift is invisible
until a cache entry holds segments at the wrong rate.

The synthetic track carries `Delay = 0, Padding = 0`. This is load-bearing:
`format.Media` already delivered trimmed PCM, so both trims happened inside
each member. A nonzero trim here would make a downstream consumer trim twice.

### A crossfade is an option, and zero is not a value

`ConcatOptions.Crossfade` makes the seam a zone of X samples where member i's
tail and member i+1's head are the same region of the timeline, blended
equal-power. That conditionalizes the sentence above, which is the primitive's
own definition, so it is worth saying exactly what stays true: at `Crossfade:
0` sample `len(a)` is `b`'s sample 0 exactly, and the zero value is what every
caller that does not ask gets.

**There is no nonzero default and there will not be one.** A gapless album must
never blend: the seam a crossfade would smear is the artifact this primitive
exists to deliver intact. A blend is an editorial decision about material (a
declick between two takes, a play queue of unrelated tracks), so it is a thing
a caller asks for, never one the library decides on their behalf.

The length identity is `sum(L) - (N-1)X`, and it is the whole of what the plan
and the run must agree about: one zone per seam, N-1 seams, subtracted after
every member's own ceil so the sum-of-ceils normalization is untouched. Both
resolve it through `ConcatTrack`, which is why that function takes the options.

Two bounds, both refused there so a plan and a run refuse identically. **Fit**:
`head + tail <= L` for every member, which is the exact rule rather than `2X <=
L` because the edge members carry one zone, and which makes N=1 pass with no
special case. **Memory**: `maxCrossfadeBytes = 16 MiB`, derived rather than
chosen. `audio/pool.go`'s top size class is 4 Mi samples, and `Get` sizes on
`frames * Channels`, so `X*ch*4 <= 16 MiB` is exactly the largest blend the
pool will hold; one sample more and every seam of every timeline becomes a
16 MB allocate-and-discard.

**The digest is untouched, and that is what keeps the identity section above
true.** The server never crossfades: the HTTP surface has no way to ask for
one, and `timelineOptions` is the single place that says so. A digest covering
the members alone is therefore still a complete identity, because there is no
second timeline the same members could name. The day something does thread a
crossfade to the wire is the day the digest has to cover it, and that is the
question to answer then rather than now.

### Advisory lengths are enforced, not trusted

`format.Media` tolerates a declared length the file does not deliver (a lying
FLAC STREAMINFO) as an oddity. That is right for one file and fatal for a
timeline, whose positions are a prefix sum: two samples of drift desyncs every
position after that member, and the playlist then promises segments the stream
cannot fill.

So `Concat` verifies delivery against declaration on advance and fails the run
otherwise, and the mint measures any member whose length is not authoritative
rather than reading it off the headers.

### The wire form is a content-addressed store, not inline sources

    POST /hls/timeline {"srcs":[{"src":"music/Artist/Album/01.flac"}, ...]}
                    -> {"tl":"kJ3n...pQ","durationSeconds":2998.5}

The digest is `base64url(sha256(canonical JSON of the {src, id} pairs))`. The
store holds `dataDir/timelines/<digest>.json`, written atomically.

**The digest is the identity, and that is the whole point of addressing it by
content.** Because it covers every member's reference *and* its ADR-0003
identity, any change to any member yields new identities, a new digest, and a
new cache key. There is no separate list digest that could disagree with the
members, and no migration if the members' identity rules change.

The cache key uses `"tl:" + digest` where a single source uses
`ref + "|" + identity` (ADR-0004's `sourceIdentity` position). The descriptor
gains `Tl`, with exactly one of `Src` and `Tl` set: a single track and a
timeline are different things, not two spellings of one fact (the way
`Bitrate` and `Bitrates` are), so the single-source URL keeps `Src` + `ID`
unchanged and keeps ADR-0003's "regenerates from nothing but this URL"
guarantee whole for the 99% of URLs that gain nothing from a store.

### The plan carries what the synthetic track cannot

`PlanSegments` over a synthetic `codec.PCM` track would name `pcm`'s decoder
and no member's, and its chain runs envelope to output and so names none of the
resampling a member does to *reach* the envelope. Both are silent under-keying
of exactly the kind ADR-0004 exists to prevent: a FLAC decoder fix, or a
resampler revision, would leave a timeline's cached segments stale.

`PlanSegmentsTimeline` prepends both lists, deduplicated. It prepends rather
than replacing the synthetic `pcm` entry, which would require knowing which
entry is the decode version. Over-keying is safe; under-keying is the sin.

### Lifetime is bounded by the signatures issued against it

A stored timeline expires no sooner than the longest-lived URL minted against
it: the mint sets a floor of what a default-TTL URL for that much audio would
carry, `/sign` extends it, and every segment read extends it. Nothing else
sweeps it.

So **no still-valid signed URL can outlive its timeline, by construction**, and
ADR-0003's "signed URLs pin bytes" stays exactly true rather than becoming a
documented weakening. A fixed TTL would have been arbitrary; this bound falls
straight out of the signing policy.

Reads touch, not just mints. Otherwise a long session would evict its own
timeline: a 40-hour audiobook keeps fetching segments against a timeline nobody
re-mints, and a sweep would take it mid-playback, 404ing the player at a buffer
refill rather than at a natural boundary.

An LRU cap is a backstop against pathological growth only; expiry is the real
bound. `CodeNotFound` survives as the answer for a genuinely cold digest, and a
client answers it by re-minting from the queue it still has, without resetting
its position.

`maxTimelineMembers = 1000`: a play queue, not a library.

### Tag-derived gain does not apply

A timeline is one chain, so it has one gain, and there is no honest single
answer to read out of N members' tags. `gain=track` is worse than ambiguous:
applied to a continuous timeline it steps the level at every seam, which is the
artifact album gain exists to prevent, so the mode that looks most reasonable
is the one that would sound wrong. A timeline takes `gain=off` or the dB the
caller wants, refused at mint time where a client can act on it.

## Alternatives rejected

- **Inline sources in the descriptor.** A 500-track queue's URL would be tens
  of kilobytes, in every playlist line. The store keeps playlists at today's
  size for any queue length.
- **A caller-pinned `ConcatOptions.Format`** to collapse the
  44.1 -> 96k -> 48k resample cascade. It opens a plan-versions vs
  chain-versions drift hole, and the mechanism does not work as proposed
  anyway: `opts.Rate` is not the output rate, since the output row's `adjust`
  hook owns it (which is how Opus forces 48k). The viable form is a second
  planning pass, deferred with the mechanism written down.
- **Capping the envelope's channels at 2.** It silently destroys a surround
  member, and it looks cheaper only because the output is usually stereo, which
  is output-aware knowledge `ConcatTrack` does not have.
- **Progressive concat** (repeated `src=` parameters). It would break
  `q.Get("src")` (which returns the first, so every existing read site would
  silently see only track 1), the canonical params, `directPlayable`, and
  `/stream` signing, for a consumer that does not exist: client-side gapless
  covers direct-play, and one long file is a merge job's output.

## Consequences

- A timeline is planned and keyed from headers alone, so a segment request
  regenerates from the URL plus the store, and nothing else.
- The mint is where a queue's problems surface: a member that cannot be
  resolved, decoded, measured, or concatenated fails there rather than at the
  first segment request.
- The store is a second piece of durable state beside the jobs store. It is
  content-addressed and re-mintable, so losing it costs a round trip and never
  correctness, which is why an unreadable timeline is dropped rather than
  quarantined the way a job directory is.
- `Concat` failing a run on length drift means a lying header takes down a
  timeline's playback rather than one file's. The mint measuring is what keeps
  that theoretical.
